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

func TestCodeActionQuickFixes(t *testing.T) {
	t.Parallel()

	// fix is a quick fix as the result of applying it.
	type fix struct {
		title     string
		preferred bool
		// content is the document after the fix.
		content string
		// config is the configuration after the fix, or "" when the fix
		// leaves it alone.
		config string
	}
	const declaresCount = "name: test\nvariables:\n  - name: count\n    type_name: int\n"
	const enablesMath = "name: test\nextensions:\n  - name: math\n"
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
			want:    []fix{{"Change to 'size'", true, "size([1])", ""}},
		},
		{
			name:    "member function",
			content: "'a'.startWith('a')",
			cursor:  protocol.Position{Character: 6},
			want:    []fix{{"Change to 'startsWith'", true, "'a'.startsWith('a')", ""}},
		},
		{
			name:    "cursor just after the name",
			content: "sise([1])",
			cursor:  protocol.Position{Character: 4},
			want:    []fix{{"Change to 'size'", true, "size([1])", ""}},
		},
		{
			name:    "variable on a later line",
			config:  declaresCount,
			content: "1 < 2 &&\n  cuont > 1",
			cursor:  protocol.Position{Line: 1, Character: 3},
			want:    []fix{{"Change to 'count'", true, "1 < 2 &&\n  count > 1", ""}},
		},
		{
			name:    "two suggestions",
			config:  "name: test\nvariables:\n  - name: count\n    type_name: int\n  - name: counter\n    type_name: int\n",
			content: "countr > 1",
			cursor:  protocol.Position{Character: 1},
			want: []fix{
				{"Change to 'count'", false, "count > 1", ""},
				{"Change to 'counter'", false, "counter > 1", ""},
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
			name:    "extension added to the list",
			config:  enablesMath,
			content: "'a'.upperAscii()",
			cursor:  protocol.Position{Character: 6},
			want: []fix{{
				"Enable the `strings` extension", true, "'a'.upperAscii()",
				"name: test\nextensions:\n  - name: strings\n  - name: math\n",
			}},
		},
		{
			name:    "extension list keeps its indentation",
			config:  "name: test\nextensions:\n- name: math\n  version: 1\n",
			content: "'a'.upperAscii()",
			cursor:  protocol.Position{Character: 6},
			want: []fix{{
				"Enable the `strings` extension", true, "'a'.upperAscii()",
				"name: test\nextensions:\n- name: strings\n- name: math\n  version: 1\n",
			}},
		},
		{
			name:    "extension list started",
			config:  declaresCount,
			content: "'a'.upperAscii()",
			cursor:  protocol.Position{Character: 6},
			want: []fix{{
				"Enable the `strings` extension", true, "'a'.upperAscii()",
				declaresCount + "extensions:\n  - name: strings\n",
			}},
		},
		{
			name:    "extension list started after a last line without a newline",
			config:  "name: test",
			content: "'a'.upperAscii()",
			cursor:  protocol.Position{Character: 6},
			want: []fix{{
				"Enable the `strings` extension", true, "'a'.upperAscii()",
				"name: test\nextensions:\n  - name: strings\n",
			}},
		},
		{
			name:    "misspelled name in an extension",
			config:  "name: test\n",
			content: "'a'.upperAsci()",
			cursor:  protocol.Position{Character: 6},
			want: []fix{{
				"Change to 'upperAscii' and enable the `strings` extension", true, "'a'.upperAscii()",
				"name: test\nextensions:\n  - name: strings\n",
			}},
		},
		{
			// A name that is not called is a variable, however close it is
			// to a function an extension declares.
			name:    "variable near an extension function",
			config:  "name: test\n",
			content: "indexO > 1",
			cursor:  protocol.Position{Character: 2},
		},
		{
			// With no configuration there is nowhere to enable it.
			name:    "extension without a configuration",
			content: "'a'.upperAscii()",
			cursor:  protocol.Position{Character: 6},
		},
		{
			name:    "extension list in flow style",
			config:  "name: test\nextensions: [{name: math}]\n",
			content: "'a'.upperAscii()",
			cursor:  protocol.Position{Character: 6},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			dir := t.TempDir()
			configPath := filepath.Join(dir, lsp.ConfigFileName)
			if tt.config != "" {
				ok.MustNoError(t, os.WriteFile(configPath, []byte(tt.config), 0o600))
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
				applied := fix{title: a.Title, preferred: *a.IsPreferred, content: tt.content}
				for target, edits := range a.Edit.Changes {
					switch target {
					case uri.File(celPath):
						applied.content = applyEdits(t, tt.content, edits)
					case uri.File(configPath):
						applied.config = applyEdits(t, tt.config, edits)
					default:
						t.Errorf("edit to %s", target)
					}
				}
				got = append(got, applied)
			}
			ok.Equal(t, got, tt.want)
		})
	}
}
