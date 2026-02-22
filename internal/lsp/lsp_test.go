package lsp_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/nalgeon/be"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// setupLSPServer creates and initializes an LSP server for testing.
// Returns the client JSON-RPC connection and the test file URI.
func setupLSPServer(t *testing.T, testFilePath string) (*jsonrpc2.Conn, protocol.DocumentURI) {
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
		_ = lsp.ServeStream(ctx, serverConn)
	}()

	// Create a client connection on the client side.
	noop := jsonrpc2.HandlerFunc(func(_ context.Context, _ *jsonrpc2.Conn, _ *jsonrpc2.Request) (any, error) {
		return nil, nil
	})
	clientRPC := jsonrpc2.NewConn(ctx, clientConn, noop)
	t.Cleanup(func() {
		_ = clientRPC.Close()
	})

	testURI := protocol.URIFromPath(testFilePath)

	var initResult protocol.InitializeResult
	err := clientRPC.Call(ctx, "initialize", protocol.InitializeParams{}, &initResult)
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
