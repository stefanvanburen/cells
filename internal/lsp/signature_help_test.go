package lsp_test

import (
	"strings"
	"testing"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	lspuri "go.lsp.dev/uri"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
)

// requestSignatureHelp sends a textDocument/signatureHelp request at the given position.
func requestSignatureHelp(t *testing.T, conn jsonrpc2.Conn, uri lspuri.URI, pos protocol.Position) *protocol.SignatureHelp {
	t.Helper()
	var result *protocol.SignatureHelp
	_, err := conn.Call(t.Context(), "textDocument/signatureHelp", protocol.SignatureHelpParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     pos,
	}, &result)
	ok.MustNoError(t, err)
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
			testPath := getAbsPath(t, tc.file)
			conn, uri := setupLSPServer(t, testPath)
			sig := requestSignatureHelp(t, conn, uri, tc.pos)

			if !tc.wantSignatures {
				ok.Zero(t, sig)
				return
			}

			ok.True(t, sig != nil)
			ok.True(t, len(sig.Signatures) > 0)

			if tc.wantExactLabel != "" {
				ok.Equal(t, sig.Signatures[0].Label, tc.wantExactLabel)
			}

			if tc.wantLabelContains != "" {
				ok.True(t, strings.Contains(sig.Signatures[0].Label, tc.wantLabelContains))
			}

			if tc.wantNotContains != "" {
				ok.True(t, !strings.Contains(sig.Signatures[0].Label, tc.wantNotContains))
			}

			if tc.wantActiveParam != nil {
				got, _ := sig.ActiveParameter.Get()
				ok.Equal(t, got, *tc.wantActiveParam)
			}
		})
	}
}

// --- Capabilities test ---

func TestSignatureHelpCapabilities(t *testing.T) {
	t.Parallel()

	clientRPC := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})

	var result protocol.InitializeResult
	_, err := clientRPC.Call(t.Context(), "initialize", protocol.InitializeParams{}, &result)
	ok.MustNoError(t, err)

	ok.True(t, result.Capabilities.SignatureHelpProvider != nil)
	ok.True(t, len(result.Capabilities.SignatureHelpProvider.TriggerCharacters) > 0)
}
