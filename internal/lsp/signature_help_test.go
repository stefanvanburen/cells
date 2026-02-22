package lsp_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nalgeon/be"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// setupSignatureServer initializes an LSP server and opens celFile for signature help testing.
// Returns the client connection and document URI.
func setupSignatureServer(t *testing.T, celFile string) (*jsonrpc2.Conn, protocol.DocumentURI) {
	t.Helper()
	ctx := t.Context()

	testPath, err := filepath.Abs(celFile)
	be.Err(t, err, nil)

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	go func() {
		_ = lsp.ServeStream(ctx, serverConn)
	}()

	noop := jsonrpc2.HandlerFunc(func(_ context.Context, _ *jsonrpc2.Conn, _ *jsonrpc2.Request) (any, error) {
		return nil, nil
	})
	clientRPC := jsonrpc2.NewConn(ctx, clientConn, noop)
	t.Cleanup(func() {
		_ = clientRPC.Close()
	})

	testURI := protocol.URIFromPath(testPath)

	var initResult protocol.InitializeResult
	err = clientRPC.Call(ctx, "initialize", protocol.InitializeParams{}, &initResult)
	be.Err(t, err, nil)

	err = clientRPC.Notify(ctx, "initialized", protocol.InitializedParams{})
	be.Err(t, err, nil)

	content, err := os.ReadFile(testPath)
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

// requestSignatureHelp sends a textDocument/signatureHelp request at the given position.
func requestSignatureHelp(t *testing.T, conn *jsonrpc2.Conn, uri protocol.DocumentURI, pos protocol.Position) *protocol.SignatureHelp {
	t.Helper()
	var result *protocol.SignatureHelp
	err := conn.Call(t.Context(), "textDocument/signatureHelp", protocol.SignatureHelpParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			Position:     pos,
		},
	}, &result)
	if err != nil {
		t.Fatalf("textDocument/signatureHelp call failed: %v", err)
	}
	return result
}

// --- Basic signature help tests ---

func TestSignatureHelp(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name              string
		file              string
		pos               protocol.Position
		wantSignatures    bool
		wantExactLabel    string // If set, first signature label must match exactly
		wantLabelContains string // If set, first signature label must contain this
		wantNotContains   string // If set, first signature label must NOT contain this
		wantActiveParam   *uint32
	}{
		{
			name:              "global_function",
			file:              "testdata/signature_help/global_function.cel",
			pos:               protocol.Position{Line: 0, Character: 5},
			wantSignatures:    true,
			wantLabelContains: "size(",
			wantNotContains:   ".size()",
		},
		{
			name:              "member_function",
			file:              "testdata/signature_help/member_function.cel",
			pos:               protocol.Position{Line: 0, Character: 19},
			wantSignatures:    true,
			wantLabelContains: ".startsWith",
		},
		{
			name:           "type_conversion",
			file:           "testdata/signature_help/type_conversion.cel",
			pos:            protocol.Position{Line: 0, Character: 4},
			wantSignatures: true,
		},
		{
			name:            "multiple_params_member",
			file:            "testdata/signature_help/multiple_params.cel",
			pos:             protocol.Position{Line: 0, Character: 15},
			wantSignatures:  true,
			wantExactLabel:  "string.matches(string) -> bool",
			wantActiveParam: new(uint32(0)),
		},
		{
			name:           "not_a_call",
			file:           "testdata/signature_help/not_a_call.cel",
			pos:            protocol.Position{Line: 0, Character: 8},
			wantSignatures: false,
		},
		{
			name:           "after_comma",
			file:           "testdata/signature_help/after_comma.cel",
			pos:            protocol.Position{Line: 0, Character: 17},
			wantSignatures: true,
		},
		{
			name:           "nested_calls",
			file:           "testdata/signature_help/nested_calls.cel",
			pos:            protocol.Position{Line: 0, Character: 11},
			wantSignatures: true,
		},
		{
			name:           "unknown_function",
			file:           "testdata/signature_help/unknown_function.cel",
			pos:            protocol.Position{Line: 0, Character: 15},
			wantSignatures: false,
		},
		{
			name:           "outside_call",
			file:           "testdata/signature_help/outside_call.cel",
			pos:            protocol.Position{Line: 0, Character: 13},
			wantSignatures: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			conn, uri := setupSignatureServer(t, tc.file)
			sig := requestSignatureHelp(t, conn, uri, tc.pos)

			if !tc.wantSignatures {
				be.Equal(t, sig, (*protocol.SignatureHelp)(nil))
				return
			}

			if sig == nil {
				t.Fatal("expected signature help, got nil")
			}

			be.True(t, len(sig.Signatures) > 0)

			if tc.wantExactLabel != "" {
				be.Equal(t, sig.Signatures[0].Label, tc.wantExactLabel)
			}

			if tc.wantLabelContains != "" {
				be.True(t, strings.Contains(sig.Signatures[0].Label, tc.wantLabelContains))
			}

			if tc.wantNotContains != "" {
				be.True(t, !strings.Contains(sig.Signatures[0].Label, tc.wantNotContains))
			}

			if tc.wantActiveParam != nil {
				be.Equal(t, sig.ActiveParameter, *tc.wantActiveParam)
			}
		})
	}
}

// --- Capabilities test ---

func TestSignatureHelpCapabilities(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	go func() {
		_ = lsp.ServeStream(ctx, serverConn)
	}()

	noop := jsonrpc2.HandlerFunc(func(_ context.Context, _ *jsonrpc2.Conn, _ *jsonrpc2.Request) (any, error) {
		return nil, nil
	})
	clientRPC := jsonrpc2.NewConn(ctx, clientConn, noop)
	t.Cleanup(func() {
		_ = clientRPC.Close()
	})

	var result protocol.InitializeResult
	err := clientRPC.Call(ctx, "initialize", protocol.InitializeParams{}, &result)
	be.Err(t, err, nil)

	be.True(t, result.Capabilities.SignatureHelpProvider != nil)
	be.True(t, len(result.Capabilities.SignatureHelpProvider.TriggerCharacters) > 0)
}
