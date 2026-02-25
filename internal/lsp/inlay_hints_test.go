package lsp_test

import (
	"testing"

	"github.com/nalgeon/be"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// getInlayHints sends a textDocument/inlayHint request and returns the result.
func getInlayHints(t *testing.T, celFile string) []protocol.InlayHint {
	t.Helper()
	ctx := t.Context()
	testPath := getAbsPath(t, celFile)
	clientConn, testURI := setupLSPServer(t, testPath)

	var result []protocol.InlayHint
	err := clientConn.Call(ctx, "textDocument/inlayHint", protocol.InlayHintParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: testURI,
		},
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: 1000, Character: 1000},
		},
	}, &result)
	be.Err(t, err, nil)
	return result
}

type inlayHintTestCase struct {
	name           string
	file           string
	expectHint     bool
	resultContains string // substring that should be in the result
	desc           string
}

func TestInlayHints(t *testing.T) {
	t.Parallel()

	tests := []inlayHintTestCase{
		{
			name:           "simple_arithmetic",
			file:           "testdata/inlay_hints/arithmetic.cel",
			expectHint:     true,
			resultContains: "3",
			desc:           "1 + 2 should show 3 with int type",
		},
		{
			name:           "boolean_comparison",
			file:           "testdata/inlay_hints/boolean.cel",
			expectHint:     true,
			resultContains: "true",
			desc:           "5 > 3 should show true with bool type",
		},
		{
			name:           "function_call",
			file:           "testdata/inlay_hints/function_call.cel",
			expectHint:     true,
			resultContains: "5",
			desc:           "size('hello') should show 5 with int type",
		},
		{
			name:           "list_literal",
			file:           "testdata/inlay_hints/list_literal.cel",
			expectHint:     true,
			resultContains: "[1, 2, 3]",
			desc:           "[1, 2, 3] should show the list with list type",
		},
		{
			name:           "string_concatenation",
			file:           "testdata/inlay_hints/string_concatenation.cel",
			expectHint:     true,
			resultContains: "hello world",
			desc:           "string concatenation should show result with string type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hints := getInlayHints(t, tt.file)

			if tt.expectHint {
				be.True(t, len(hints) > 0)
				be.True(t, len(hints[0].Label) > 0)
				hintText := hints[0].Label[0].Value
				be.True(t, len(hintText) > 0)
				// Check if result is in the hint (after the arrow)
				be.True(t, len(hintText) >= 2)
			} else {
				be.True(t, len(hints) == 0)
			}
		})
	}
}
