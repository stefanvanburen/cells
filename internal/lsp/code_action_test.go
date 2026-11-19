package lsp_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
)

func TestCodeActionDidYouMean(t *testing.T) {
	t.Parallel()

	type fix struct {
		title     string
		newText   string
		edit      protocol.Range
		preferred bool
	}
	at := func(line, startCol, endCol uint32) protocol.Range {
		return protocol.Range{
			Start: protocol.Position{Line: line, Character: startCol},
			End:   protocol.Position{Line: line, Character: endCol},
		}
	}
	tests := []struct {
		name    string
		config  string
		content string
		// cursor is the position code actions are requested at.
		cursor protocol.Position
		only   []protocol.CodeActionKind
		want   []fix
	}{
		{
			name:    "global function",
			content: "sise([1])",
			cursor:  protocol.Position{Character: 2},
			want:    []fix{{"Change to 'size'", "size", at(0, 0, 4), true}},
		},
		{
			name:    "member function",
			content: "'a'.startWith('a')",
			cursor:  protocol.Position{Character: 6},
			want:    []fix{{"Change to 'startsWith'", "startsWith", at(0, 4, 13), true}},
		},
		{
			name:    "cursor just after the name",
			content: "sise([1])",
			cursor:  protocol.Position{Character: 4},
			want:    []fix{{"Change to 'size'", "size", at(0, 0, 4), true}},
		},
		{
			name:    "variable on a later line",
			config:  "name: test\nvariables:\n  - name: count\n    type_name: int\n",
			content: "1 < 2 &&\n  cuont > 1",
			cursor:  protocol.Position{Line: 1, Character: 3},
			want:    []fix{{"Change to 'count'", "count", at(1, 2, 7), true}},
		},
		{
			name:    "two suggestions",
			config:  "name: test\nvariables:\n  - name: count\n    type_name: int\n  - name: counter\n    type_name: int\n",
			content: "countr > 1",
			cursor:  protocol.Position{Character: 1},
			want: []fix{
				{"Change to 'count'", "count", at(0, 0, 6), false},
				{"Change to 'counter'", "counter", at(0, 0, 6), false},
			},
		},
		{
			name:    "cursor elsewhere",
			content: "sise([1])",
			cursor:  protocol.Position{Character: 6},
		},
		{
			name:    "only refactorings requested",
			content: "sise([1])",
			cursor:  protocol.Position{Character: 2},
			only:    []protocol.CodeActionKind{protocol.CodeActionKindRefactor},
		},
		{
			name:    "no suggestion",
			content: "zzzzzz > 1",
			cursor:  protocol.Position{Character: 2},
		},
		{
			// The name is declared by an extension the environment lacks, so
			// the diagnostic advises enabling it rather than a spelling.
			name:    "extension not enabled",
			content: "'a'.upperAscii()",
			cursor:  protocol.Position{Character: 6},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			dir := t.TempDir()
			if tt.config != "" {
				ok.MustNoError(t, os.WriteFile(filepath.Join(dir, lsp.ConfigFileName), []byte(tt.config), 0o600))
			}
			celPath := filepath.Join(dir, "a.cel")
			conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
			initializeServer(t, conn, "")
			openAndDiagnose(t, conn, celPath, tt.content)

			var actions []*protocol.CodeAction
			_, err := conn.Call(ctx, "textDocument/codeAction", protocol.CodeActionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(celPath)},
				Range:        protocol.Range{Start: tt.cursor, End: tt.cursor},
				Context:      protocol.CodeActionContext{Only: tt.only},
			}, &actions)
			ok.MustNoError(t, err)

			var got []fix
			for _, a := range actions {
				ok.Equal(t, *a.Kind, protocol.CodeActionKindQuickFix)
				ok.Equal(t, len(a.Diagnostics), 1)
				edits := a.Edit.Changes[uri.File(celPath)]
				ok.Equal(t, len(edits), 1)
				got = append(got, fix{a.Title, edits[0].NewText, edits[0].Range, *a.IsPreferred})
			}
			ok.Equal(t, got, tt.want)
		})
	}
}
