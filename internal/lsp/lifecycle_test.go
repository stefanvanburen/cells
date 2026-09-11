package lsp_test

import (
	"testing"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
)

// hoverRequest asks for hover over a document that is not open, which any
// initialized server answers with a null result rather than an error. That
// makes it a way to ask whether a request was let through at all.
func hoverRequest(t *testing.T, conn jsonrpc2.Conn) error {
	t.Helper()

	var result *protocol.Hover
	_, err := conn.Call(t.Context(), "textDocument/hover", protocol.HoverParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///nonexistent.cel"},
	}, &result)
	return err
}

// wireCode returns the JSON-RPC error code err was refused with.
func wireCode(t *testing.T, err error) jsonrpc2.Code {
	t.Helper()

	wireErr, found := ok.ErrorAs[*jsonrpc2.Error](t, err)
	if !found {
		return 0
	}
	return wireErr.Code
}

func TestLifecycleRequestBeforeInitialize(t *testing.T) {
	t.Parallel()

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	ok.Equal(t, wireCode(t, hoverRequest(t, conn)), jsonrpc2.ServerNotInitialized)

	// The session is still usable once it is initialized properly.
	initializeServer(t, conn, "")
	ok.MustNoError(t, hoverRequest(t, conn))
}

func TestLifecycleNotificationBeforeInitializeIsDropped(t *testing.T) {
	t.Parallel()

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})

	// A notification has nobody to refuse to, so it is dropped rather than
	// answered. The server has to survive it either way.
	ok.MustNoError(t, conn.Notify(t.Context(), "textDocument/didOpen", protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: "file:///dropped.cel", LanguageID: "cel", Version: 1, Text: "1 +",
		},
	}))

	initializeServer(t, conn, "")
	ok.MustNoError(t, hoverRequest(t, conn))
}

func TestLifecycleSecondInitialize(t *testing.T) {
	t.Parallel()

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	initializeServer(t, conn, "")

	// The second one carries options that would rebuild the environment, which
	// is what makes accepting it more than a formality.
	var result protocol.InitializeResult
	_, err := conn.Call(t.Context(), "initialize", protocol.InitializeParams{
		InitializationOptions: protocol.LSPAny(`{"extensions": ["strings"]}`),
	}, &result)
	ok.Equal(t, wireCode(t, err), jsonrpc2.InvalidRequest)
}

func TestLifecycleRequestAfterShutdown(t *testing.T) {
	t.Parallel()

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	initializeServer(t, conn, "")

	var nothing any
	_, err := conn.Call(t.Context(), "shutdown", nil, &nothing)
	ok.MustNoError(t, err)

	ok.Equal(t, wireCode(t, hoverRequest(t, conn)), jsonrpc2.InvalidRequest)

	// Including another initialize: a shut down session cannot be restarted.
	var result protocol.InitializeResult
	_, err = conn.Call(t.Context(), "initialize", protocol.InitializeParams{}, &result)
	ok.Equal(t, wireCode(t, err), jsonrpc2.InvalidRequest)
}
