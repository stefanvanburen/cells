package lsp_test

import (
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
)

func TestLinkedEditingRange(t *testing.T) {
	t.Parallel()

	at := func(line, startCol, endCol uint32) protocol.Range {
		return protocol.Range{
			Start: protocol.Position{Line: line, Character: startCol},
			End:   protocol.Position{Line: line, Character: endCol},
		}
	}
	tests := []struct {
		name    string
		content string
		cursor  protocol.Position
		// want is nil when the name at the cursor is not linked.
		want []protocol.Range
	}{
		{
			name:    "loop variable at its declaration",
			content: "[1, 2].map(x, x * x)",
			cursor:  protocol.Position{Character: 11},
			want:    []protocol.Range{at(0, 11, 12), at(0, 14, 15), at(0, 18, 19)},
		},
		{
			name:    "loop variable at a use on a later line",
			content: "[1, 2].all(item,\n  item > 0)",
			cursor:  protocol.Position{Line: 1, Character: 4},
			want:    []protocol.Range{at(0, 11, 15), at(1, 2, 6)},
		},
		{
			name:    "bind variable",
			content: "cel.bind(v, 1, v + v)",
			cursor:  protocol.Position{Character: 15},
			want:    []protocol.Range{at(0, 9, 10), at(0, 15, 16), at(0, 19, 20)},
		},
		{
			name:    "inner loop variable shadowing an outer one",
			content: "[1].map(x, [2].map(x, x) + [x])",
			cursor:  protocol.Position{Character: 22},
			want:    []protocol.Range{at(0, 19, 20), at(0, 22, 23)},
		},
		{
			name:    "variable declared outside the file",
			content: "x + x",
			cursor:  protocol.Position{Character: 0},
		},
		{
			name:    "function",
			content: "size('a') + size('b')",
			cursor:  protocol.Position{Character: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			celPath := filepath.Join(t.TempDir(), "a.cel")
			conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{Extensions: []string{"bindings"}})
			initializeServer(t, conn, "")
			openAndDiagnose(t, conn, celPath, tt.content)

			var got *protocol.LinkedEditingRanges
			_, err := conn.Call(t.Context(), "textDocument/linkedEditingRange", protocol.LinkedEditingRangeParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(celPath)},
				Position:     tt.cursor,
			}, &got)
			ok.MustNoError(t, err)

			if tt.want == nil {
				ok.True(t, got == nil, ok.Sprintf("got %v", got))
				return
			}
			if got == nil {
				t.Fatal("got no ranges")
			}
			ok.Equal(t, got.Ranges, tt.want)
			ok.Equal(t, *got.WordPattern, `[_a-zA-Z][_a-zA-Z0-9]*`)
		})
	}
}
