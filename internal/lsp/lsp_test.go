package lsp_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/nalgeon/be"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.vanburen.xyz/cells/internal/lsp"
)

// newLSPClient starts a fresh LSP server on one end of an in-memory pipe and
// returns a client connection to it, wired with the union-aware LSP codec
// (required for Optional/Nullable/union fields to round-trip correctly).
// It does not perform the initialize handshake, so callers that only need to
// inspect InitializeResult (or that supply their own protocol.Client, e.g. to
// capture pushed notifications) can do so without setupLSPServer's extra
// didOpen step.
//
// defaultExtensions is passed through to ServeStream, standing in for the
// extension names `cells serve --ext=...` would supply.
func newLSPClient(t *testing.T, client protocol.Client, defaultExtensions ...string) jsonrpc2.Conn {
	t.Helper()
	ctx := t.Context()

	// Create a pipe — server reads/writes one end, client the other.
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	// Run the LSP server on the server side of the pipe.
	go func() {
		_ = lsp.ServeStream(ctx, serverConn, defaultExtensions...)
	}()

	_, clientRPC, _ := protocol.NewClient(ctx, client, jsonrpc2.NewStream(clientConn))
	t.Cleanup(func() {
		_ = clientRPC.Close()
	})
	return clientRPC
}

// setupLSPServer creates and initializes an LSP server for testing.
// Returns the client JSON-RPC connection and the test file URI.
func setupLSPServer(t *testing.T, testFilePath string) (jsonrpc2.Conn, uri.URI) {
	t.Helper()
	ctx := t.Context()

	clientRPC := newLSPClient(t, protocol.UnimplementedClient{})

	testURI := uri.File(testFilePath)

	var initResult protocol.InitializeResult
	_, err := clientRPC.Call(ctx, "initialize", protocol.InitializeParams{}, &initResult)
	be.Err(t, err, nil)

	err = clientRPC.Notify(ctx, "initialized", protocol.InitializedParams{})
	be.Err(t, err, nil)

	content, err := os.ReadFile(testFilePath)
	be.Err(t, err, nil)

	err = clientRPC.Notify(ctx, "textDocument/didOpen", protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        testURI,
			LanguageID: "cel",
			Version:    1,
			Text:       string(content),
		},
	})
	be.Err(t, err, nil)

	return clientRPC, testURI
}

// getAbsPath returns the absolute path for a test file.
func getAbsPath(t *testing.T, relPath string) string {
	t.Helper()
	absPath, err := filepath.Abs(relPath)
	be.Err(t, err, nil)
	return absPath
}
