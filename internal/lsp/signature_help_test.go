package lsp_test

import (
	"os"
	"path/filepath"
	"slices"
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

// TestSignatureHelpWhileTyping asks for signature help at the | in each source,
// where a call is still being written and the source may not parse.
func TestSignatureHelpWhileTyping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		src       string
		wantLabel string // empty for no signature help
		wantArg   uint32
	}{
		{"open_paren", "size(|", "size(", 0},
		{"after_comma", `"a".startsWith("b", |`, ".startsWith(", 1},
		{"auto_paired", `size("a", |)`, "size(", 1},
		{"member_call", `"a".startsWith(|`, ".startsWith(", 0},
		{"generic_receiver", "{1: 2}.size(|", "map(<A>, <B>).size(", 0},
		{"inside_list", "size([1, 2, |", "size(", 0},
		{"inside_parens", "size((1 + |", "size(", 0},
		{"nested", `size("a") + int(string(|`, "string(", 0},
		{"after_comment", "size( // the input\n|", "size(", 0},
		{"namespaced", "math.bitShiftLeft(1, |", "math.bitShiftLeft(", 1},
		{"closed", `size("a")|`, "", 0},
		{"macro", "[1].all(x, |", "", 0},
		{"grouping_only", "(1 + |", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			config := "name: test\nextensions:\n  - name: math\n"
			ok.MustNoError(t, os.WriteFile(filepath.Join(dir, lsp.ConfigFileName), []byte(config), 0o600))
			before, after, _ := strings.Cut(tt.src, "|")
			path := filepath.Join(dir, "test.cel")
			ok.MustNoError(t, os.WriteFile(path, []byte(before+after), 0o600))
			conn, uri := setupLSPServer(t, path)

			lines := strings.Split(before, "\n")
			cursor := protocol.Position{Line: uint32(len(lines) - 1), Character: uint32(len(lines[len(lines)-1]))}
			sig := requestSignatureHelp(t, conn, uri, cursor)
			if tt.wantLabel == "" {
				ok.Zero(t, sig)
				return
			}
			ok.NotNil(ok.Must(t), sig)
			arg, _ := sig.ActiveParameter.Get()
			ok.Equal(t, arg, tt.wantArg)

			// Some overload has the wanted label, and every parameter is
			// a type or a name with its parentheses balanced, never a piece of
			// the signature around it.
			var labels, params []string
			for _, s := range sig.Signatures {
				labels = append(labels, s.Label)
				for _, p := range s.Parameters {
					params = append(params, string(p.Label.(protocol.String)))
				}
			}
			ok.True(t, slices.ContainsFunc(labels, func(l string) bool { return strings.Contains(l, tt.wantLabel) }))
			ok.True(t, !slices.ContainsFunc(params, func(p string) bool { return strings.Count(p, "(") != strings.Count(p, ")") }))
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
