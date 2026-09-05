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

// definitionConfig declares one variable and one function, each on a line the
// tests below expect to be pointed at.
//
//	 1  name: test
//	 2  variables:
//	 3    - name: request
//	 4      type: "map<string, dyn>"
//	 5    - name: retries
//	 6      type: "int"
//	 7  functions:
//	 8    - name: isAdmin
//	 9      overloads:
//	10        - id: is_admin_string
//	11          args: ["string"]
//	12          return: "bool"
const definitionConfig = `name: test
variables:
  - name: request
    type: "map<string, dyn>"
  - name: retries
    type: "int"
functions:
  - name: isAdmin
    overloads:
      - id: is_admin_string
        args: ["string"]
        return: "bool"
`

func TestDefinitionPointsAtTheConfiguration(t *testing.T) {
	t.Parallel()

	opts := lsp.Options{ConfigPath: writeConfig(t, definitionConfig)}

	// Columns are 1-indexed: `retries < 3 && isAdmin(request.user)`.
	tests := []struct {
		name     string
		expr     string
		col      int
		wantLine int
		wantCol  int
	}{
		{"variable", "retries < 3", 1, 5, 11},
		{"second_variable", "request.user == 1", 1, 3, 11},
		{"function", `retries < 3 && isAdmin(request.user)`, 16, 8, 11},
		{"variable_inside_a_call", `isAdmin(request.user)`, 9, 3, 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, found, err := lsp.FindDefinition("p.cel", tt.expr, 1, tt.col, opts)
			ok.MustNoError(t, err)
			if !ok.True(t, found, ok.Sprintf("no definition for %q at col %d", tt.expr, tt.col)) {
				return
			}
			ok.Equal(t, got.Path, opts.ConfigPath)
			ok.Equal(t, got.Line, tt.wantLine)
			ok.Equal(t, got.Col, tt.wantCol)
		})
	}
}

// Nothing else in a CEL expression was declared anywhere cells can point at.
func TestDefinitionFindsNothingToPointAt(t *testing.T) {
	t.Parallel()

	opts := lsp.Options{ConfigPath: writeConfig(t, definitionConfig)}

	tests := []struct {
		name string
		expr string
		col  int
	}{
		// Defined in Go, by the standard library.
		{"builtin_function", `size("abc") > retries`, 1},
		// An operator is not a name.
		{"operator", "retries < 3", 9},
		// A literal is not a name either.
		{"literal", "retries < 3", 11},
		// Never declared at all.
		{"undeclared", "nosuchname > 1", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, found, err := lsp.FindDefinition("p.cel", tt.expr, 1, tt.col, opts)
			ok.MustNoError(t, err)
			ok.True(t, !found, ok.Sprintf("unexpected definition: %+v", got))
		})
	}
}

// With no configuration there is nowhere for a name to have been declared.
// A name a macro binds is declared in the expression itself, where the macro
// binds it, so it resolves without a configuration at all.
func TestDefinitionOfAMacroBinding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		expr    string
		exts    []string
		col     int
		wantCol int
	}{
		// [1, 2].exists(x, x > 1) — the use at 18 is bound at 15.
		{"loop_variable_from_use", "[1, 2].exists(x, x > 1)", nil, 18, 15},
		// Asking at the declaration answers with itself.
		{"loop_variable_from_declaration", "[1, 2].exists(x, x > 1)", nil, 15, 15},
		{"map_loop_variable", "[1, 2].map(item, item * 2)", nil, 18, 12},
		// A nested macro binds its own name.
		{"nested_macro", "[[1]].map(o, o.map(i, i * 2))", nil, 23, 20},
		// cel.bind(v, 1 + 1, v * v) — the use at 20 is bound at 10.
		{"cel_bind_from_use", "cel.bind(v, 1 + 1, v * v)", []string{"bindings"}, 20, 10},
		{"cel_bind_from_declaration", "cel.bind(v, 1 + 1, v * v)", []string{"bindings"}, 10, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := lsp.Options{Extensions: tt.exts}
			got, found, err := lsp.FindDefinition("p.cel", tt.expr, 1, tt.col, opts)
			ok.MustNoError(t, err)
			if !ok.True(t, found, ok.Sprintf("no definition for %q at col %d", tt.expr, tt.col)) {
				return
			}
			// The declaration is in the expression itself, not the config.
			ok.Equal(t, got.Path, "/p.cel")
			ok.Equal(t, got.Line, 1)
			ok.Equal(t, got.Col, tt.wantCol)
		})
	}
}

func TestDefinitionWithoutAConfiguration(t *testing.T) {
	t.Parallel()

	// "retries" would have been declared by a configuration; there is none.
	_, found, err := lsp.FindDefinition("p.cel", "retries < 3", 1, 1, lsp.Options{})
	ok.MustNoError(t, err)
	ok.True(t, !found)
}

// A name the configuration declares but which is not in the file it points at
// — because the configuration on disk changed — is not pointed at either.
func TestDefinitionMissingFromConfiguration(t *testing.T) {
	t.Parallel()

	// A variable declared through an extension rather than a "variables"
	// entry has nothing in the file to point at.
	opts := lsp.Options{
		ConfigPath: writeConfig(t, "name: test\nextensions:\n  - name: math\n"),
	}

	_, found, err := lsp.FindDefinition("p.cel", "math.greatest(1, 2)", 1, 1, opts)
	ok.MustNoError(t, err)
	ok.True(t, !found)
}

// The server answers textDocument/definition with the location in the
// configuration it discovered for the document.
func TestServerDefinition(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, lsp.ConfigFileName)
	ok.MustNoError(t, os.WriteFile(configPath, []byte(definitionConfig), 0o600))
	celPath := filepath.Join(dir, "a.cel")

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	initializeServer(t, conn, "")
	openDocument(t, conn, celPath, "retries < 3")

	var locations []protocol.Location
	_, err := conn.Call(t.Context(), "textDocument/definition", protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(celPath)},
		Position:     protocol.Position{Line: 0, Character: 0},
	}, &locations)
	ok.MustNoError(t, err)

	if !ok.Equal(t, len(locations), 1, ok.Sprintf("locations: %v", locations)) {
		return
	}
	ok.Equal(t, locations[0].URI, uri.File(configPath))
	ok.Equal(t, locations[0].Range.Start.Line, uint32(4))
	ok.Equal(t, locations[0].Range.Start.Character, uint32(10))
	// The range covers the declared name.
	ok.Equal(t, locations[0].Range.End.Character, uint32(10+len("retries")))
}

func TestDefinitionCapability(t *testing.T) {
	t.Parallel()

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})

	var result protocol.InitializeResult
	_, err := conn.Call(t.Context(), "initialize", protocol.InitializeParams{}, &result)
	ok.MustNoError(t, err)
	ok.Equal(t, result.Capabilities.DefinitionProvider, protocol.DefinitionProvider(protocol.Boolean(true)))
}

// cel.bind does not name what it binds through the iteration variable the way
// a comprehension does, so cells did not recognize the binding at all. Renaming
// from a use wrote over the macro's own name — cel.bind(v, 1 + 1, v * v)
// renamed to w produced cel.bindw(v, 1 + 1, w * w) — because the expansion
// leaves behind a synthesized identifier with an empty source range, and an
// empty range accepted an edit at the offset it started from.
func TestCelBindRenameDoesNotCorrupt(t *testing.T) {
	t.Parallel()

	const expr = `cel.bind(v, 1 + 1, v * v)`
	opts := lsp.Options{Extensions: []string{"bindings"}}

	// Renaming from a use and from the declaration agree, and both rename the
	// binding along with its uses.
	for _, col := range []int{10, 20, 24} {
		got, err := lsp.Rename(expr, 1, col, "w", opts)
		ok.MustNoError(t, err)
		ok.Equal(t, got, `cel.bind(w, 1 + 1, w * w)`, ok.Sprintf("renaming at col %d", col))
	}
}

func TestCelBindReferences(t *testing.T) {
	t.Parallel()

	const expr = `cel.bind(v, 1 + 1, v * v)`
	opts := lsp.Options{Extensions: []string{"bindings"}}

	// The declaration and both uses, and nothing at column 9, which is the
	// parenthesis the synthesized identifier used to be reported at.
	want := []lsp.Reference{
		{Line: 1, Col: 10, EndLine: 1, EndCol: 11},
		{Line: 1, Col: 20, EndLine: 1, EndCol: 21},
		{Line: 1, Col: 24, EndLine: 1, EndCol: 25},
	}
	for _, col := range []int{10, 20, 24} {
		got, err := lsp.References(expr, 1, col, opts)
		ok.MustNoError(t, err)
		ok.DeepEqual(t, got, want)
	}
}
