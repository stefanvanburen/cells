package lsp_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
)

// writeConfig writes a CEL environment configuration to a temporary file and
// returns its path.
func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cel.yaml")
	ok.MustNoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	return path
}

// loadOptions writes a configuration and returns Options naming it.
func loadOptions(t *testing.T, yaml string) lsp.Options {
	t.Helper()
	return lsp.Options{ConfigPath: writeConfig(t, yaml)}
}

func TestConfigDeclaresVariables(t *testing.T) {
	t.Parallel()

	opts := loadOptions(t, `
name: test
variables:
  - name: count
    type: "int"
  - name: request
    type: "map<string, dyn>"
`)

	tests := []struct {
		name string
		expr string
		want string // "" means the expression checks cleanly
	}{
		{"declared_variable", "count > 1", ""},
		{"declared_map_access", `request.method == "GET"`, ""},
		{"type_error", `count + "x"`, "no matching overload"},
		{"undeclared_variable", "nosuchvar > 1", "undeclared reference to 'nosuchvar'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			diags, err := lsp.Check(tt.expr, opts)
			ok.MustNoError(t, err)
			if tt.want == "" {
				ok.Equal(t, len(diags), 0, ok.Sprintf("diagnostics: %v", diags))
				return
			}
			if !ok.Equal(t, len(diags), 1, ok.Sprintf("diagnostics: %v", diags)) {
				return
			}
			ok.True(t, strings.Contains(diags[0].Message, tt.want), ok.Sprintf("message: %q", diags[0].Message))
		})
	}
}

// A configuration says what the expressions are allowed to name, so a failure
// to type-check against it is an error rather than the warning cells reports
// when it has been told nothing.
func TestConfigMakesCheckDiagnosticsErrors(t *testing.T) {
	t.Parallel()

	const expr = `count + "x"`

	withConfig := loadOptions(t, `
name: test
variables:
  - name: count
    type: "int"
`)
	diags, err := lsp.Check(expr, withConfig)
	ok.MustNoError(t, err)
	if ok.Equal(t, len(diags), 1) {
		ok.Equal(t, diags[0].Severity, "error")
	}

	// Without declarations "count" is undeclared, which is not something cells
	// can distinguish from a name it was never told about, so the same
	// expression is only a warning.
	diags, err = lsp.Check(expr, lsp.Options{})
	ok.MustNoError(t, err)
	if ok.Equal(t, len(diags), 1) {
		ok.Equal(t, diags[0].Severity, "warning")
		ok.True(t, strings.Contains(diags[0].Message, "undeclared reference"), ok.Sprintf("message: %q", diags[0].Message))
	}
}

// cells reads SourceInfo.MacroCalls() for hover and rename, so macro call
// tracking has to survive a configuration that disables it. Nothing errors
// when it does not — a macro simply stops being recognized — so the option
// order in newCELEnv is pinned here.
func TestConfigCannotDisableMacroCallTracking(t *testing.T) {
	t.Parallel()

	const expr = "[1, 2].map(x, x * 2)"
	opts := loadOptions(t, `
name: test
features:
  - name: cel.feature.macro_call_tracking
    enabled: false
`)

	// The loop variable is only renameable while the macro is still in call
	// form, which is what the tracking preserves.
	renamed, err := lsp.Rename(expr, 1, 12, "y", opts)
	ok.MustNoError(t, err)
	ok.Equal(t, renamed, "[1, 2].map(y, y * 2)")

	hover, err := lsp.Hover(expr, 1, 8, opts)
	ok.MustNoError(t, err)
	ok.True(t, strings.Contains(hover, "`map`"), ok.Sprintf("hover: %q", hover))
}

// Extensions named in a configuration resolve by the same names the --ext flag
// takes, including the ones cel-go's own factory does not know.
func TestConfigExtensions(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		expr string
	}{
		// cel-go's factory knows this one.
		{"strings", `"a,b".split(",")[0] == "a"`},
		// These four it does not; cells answers for them.
		{"network", `isIP("1.2.3.4")`},
		{"comprehensions", `[1, 2].transformList(i, v, v * 2) == [2, 4]`},
		{"jwt", `jwt.Token{issuer: "a"}.issuer == "a"`},
		// regex needs optional types installed first, which cells arranges.
		{"regex", `regex.extract("hello", "h(.*)o").hasValue()`},
		// cel-go resolves this one itself; optFlatMap is in its newest
		// version but not its first.
		{"optional", `optional.of(1).optFlatMap(v, optional.of(v)).hasValue()`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := loadOptions(t, "name: test\nextensions:\n  - name: "+tt.name+"\n")
			diags, err := lsp.Check(tt.expr, opts)
			ok.MustNoError(t, err)
			ok.Equal(t, len(diags), 0, ok.Sprintf("diagnostics: %v", diags))
		})
	}
}

// The --ext flag adds to whatever a configuration already asks for rather than
// replacing it.
func TestExtensionsAddToConfig(t *testing.T) {
	t.Parallel()

	opts := loadOptions(t, `
name: test
extensions:
  - name: math
`)
	opts.Extensions = []string{"strings"}

	diags, err := lsp.Check(`math.greatest(1, 2) == 2 && "a,b".split(",")[0] == "a"`, opts)
	ok.MustNoError(t, err)
	ok.Equal(t, len(diags), 0, ok.Sprintf("diagnostics: %v", diags))
}

// An extension pinned to a version is left to cel-go, which can supply older
// versions; cells' own constructors are always the newest.
func TestConfigExtensionVersion(t *testing.T) {
	t.Parallel()

	// reverse() arrived in version 2 of the strings extension.
	const expr = `"abc".reverse() == "cba"`

	v1 := loadOptions(t, "name: test\nextensions:\n  - name: strings\n    version: 1\n")
	diags, err := lsp.Check(expr, v1)
	ok.MustNoError(t, err)
	ok.True(t, len(diags) > 0, ok.Sprintf("diagnostics: %v", diags))

	latest := loadOptions(t, "name: test\nextensions:\n  - name: strings\n    version: latest\n")
	diags, err = lsp.Check(expr, latest)
	ok.MustNoError(t, err)
	ok.Equal(t, len(diags), 0, ok.Sprintf("diagnostics: %v", diags))
}

func TestConfigUnknownExtension(t *testing.T) {
	t.Parallel()

	opts := loadOptions(t, "name: test\nextensions:\n  - name: nope\n")

	_, err := lsp.Check("1 + 1", opts)
	if !ok.True(t, err != nil) {
		return
	}
	// The name cells uses, and the names it accepts.
	ok.True(t, strings.Contains(err.Error(), `unknown CEL extension "nope"`), ok.Sprintf("error: %v", err))
	ok.True(t, strings.Contains(err.Error(), "available:"), ok.Sprintf("error: %v", err))
}

func TestLoadConfigErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing_file", func(t *testing.T) {
		t.Parallel()

		_, err := lsp.LoadConfig(filepath.Join(t.TempDir(), "absent.yaml"))
		ok.True(t, err != nil)
	})

	t.Run("malformed_yaml", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, "name: [unterminated\n")
		_, err := lsp.LoadConfig(path)
		if !ok.True(t, err != nil) {
			return
		}
		// The path is named, so the reader knows which file to fix.
		ok.True(t, strings.Contains(err.Error(), path), ok.Sprintf("error: %v", err))
	})

	t.Run("invalid_config", func(t *testing.T) {
		t.Parallel()

		// A variable with no type is not a usable declaration.
		path := writeConfig(t, "name: test\nvariables:\n  - name: x\n")
		_, err := lsp.LoadConfig(path)
		ok.True(t, err != nil)
	})
}

// Without a configuration, cells behaves exactly as it did before there was
// one: plain CEL, and check-phase diagnostics reported as warnings.
func TestNoConfigIsPlainCEL(t *testing.T) {
	t.Parallel()

	diags, err := lsp.Check(`1 + 2`, lsp.Options{})
	ok.MustNoError(t, err)
	ok.Equal(t, len(diags), 0, ok.Sprintf("diagnostics: %v", diags))

	diags, err = lsp.Check(`request.method`, lsp.Options{})
	ok.MustNoError(t, err)
	if ok.Equal(t, len(diags), 1) {
		ok.Equal(t, diags[0].Severity, "warning")
	}
}

// A client points the server at a configuration with the "config" key in its
// initializationOptions, the same way it names extensions.
func TestInitializeConfig(t *testing.T) {
	t.Parallel()

	config := writeConfig(t, `
name: test
variables:
  - name: count
    type: "int"
`)

	// Without a configuration, "count" is undeclared and only a warning.
	diags := diagnosticsFor(t, nil, "", "count > 1")
	if ok.Equal(t, len(diags), 1, ok.Sprintf("diagnostics: %v", diags)) {
		ok.Equal(t, diags[0].Severity, protocol.DiagnosticSeverityWarning)
	}

	// With one, it is declared and there is nothing to report.
	diags = diagnosticsFor(t, nil, `{"config":`+quote(config)+`}`, "count > 1")
	ok.Equal(t, len(diags), 0, ok.Sprintf("diagnostics: %v", diags))

	// And a type error against it is an error rather than a warning.
	diags = diagnosticsFor(t, nil, `{"config":`+quote(config)+`}`, `count + "x"`)
	if ok.Equal(t, len(diags), 1, ok.Sprintf("diagnostics: %v", diags)) {
		ok.Equal(t, diags[0].Severity, protocol.DiagnosticSeverityError)
	}
}

// An empty "config" clears whatever the server was started with, where
// omitting the key leaves it alone.
func TestInitializeConfigCleared(t *testing.T) {
	t.Parallel()

	diags := diagnosticsFor(t, nil, `{"config":""}`, "count > 1")
	if ok.Equal(t, len(diags), 1, ok.Sprintf("diagnostics: %v", diags)) {
		ok.Equal(t, diags[0].Severity, protocol.DiagnosticSeverityWarning)
	}
}

func TestInitializeConfigMissingFails(t *testing.T) {
	t.Parallel()

	clientRPC := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	var initResult protocol.InitializeResult
	_, err := clientRPC.Call(t.Context(), "initialize", protocol.InitializeParams{
		InitializationOptions: protocol.LSPAny(`{"config":"/no/such/cel.yaml"}`),
	}, &initResult)
	ok.ErrorContains(t, err, "/no/such/cel.yaml")
}

// quote renders s as a JSON string, so that a Windows-style path or a path
// with a quote in it survives being embedded in initializationOptions.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// The server discovers a configuration for each document, so documents in
// differently configured directories are checked against different
// environments.
func TestServerDiscoversConfigPerDocument(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ok.MustNoError(t, os.WriteFile(filepath.Join(dir, lsp.ConfigFileName), []byte(`
name: outer
variables:
  - name: count
    type: "int"
`), 0o600))
	inner := filepath.Join(dir, "inner")
	ok.MustNoError(t, os.MkdirAll(inner, 0o700))
	ok.MustNoError(t, os.WriteFile(filepath.Join(inner, lsp.ConfigFileName), []byte(`
name: inner
variables:
  - name: other
    type: "int"
`), 0o600))

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	initializeServer(t, conn, "")

	// The outer document sees "count"...
	ok.Equal(t, len(openAndDiagnose(t, conn, filepath.Join(dir, "a.cel"), "count > 1")), 0)
	// ...and the inner one does not, because its own configuration wins.
	innerDiags := openAndDiagnose(t, conn, filepath.Join(inner, "b.cel"), "count > 1")
	if ok.Equal(t, len(innerDiags), 1, ok.Sprintf("diagnostics: %v", innerDiags)) {
		ok.True(t, strings.Contains(diagnosticMessage(innerDiags[0]), "count"),
			ok.Sprintf("message: %v", innerDiags[0].Message))
	}
}

// Editing a configuration takes effect without restarting the server: the
// environment built from it is rebuilt when the file on disk changes.
func TestServerReloadsChangedConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, lsp.ConfigFileName)
	ok.MustNoError(t, os.WriteFile(configPath, []byte("name: test\n"), 0o600))
	celPath := filepath.Join(dir, "a.cel")

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	initializeServer(t, conn, "")

	// Nothing is declared yet.
	diags := openAndDiagnose(t, conn, celPath, "count > 1")
	ok.Equal(t, len(diags), 1, ok.Sprintf("diagnostics: %v", diags))

	// Declare it, and ask again.
	ok.MustNoError(t, os.WriteFile(configPath, []byte(`
name: test
variables:
  - name: count
    type: "int"
`), 0o600))
	diags = diagnoseOpen(t, conn, celPath)
	ok.Equal(t, len(diags), 0, ok.Sprintf("diagnostics: %v", diags))
}

// Diagnostics already published are made good when the configuration they were
// judged against is edited, without the document itself being touched: the
// editor is left showing errors for a name the configuration now declares
// otherwise.
func TestServerRefreshesDiagnosticsAfterConfigChange(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	dir := t.TempDir()
	configPath := filepath.Join(dir, lsp.ConfigFileName)
	ok.MustNoError(t, os.WriteFile(configPath, []byte("name: test\n"), 0o600))
	celPath := filepath.Join(dir, "a.cel")
	celURI := uri.File(celPath)

	dc := newDiagnosticCollector()
	conn := newLSPClient(t, dc, lsp.Options{})
	initializeServer(t, conn, "")

	ok.MustNoError(t, conn.Notify(ctx, "textDocument/didOpen", protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: celURI, LanguageID: "cel", Version: 1, Text: "count > 1",
		},
	}))
	dc.waitForDiagnostics(t, 1)
	ok.Equal(t, len(dc.latest().Diagnostics), 1, ok.Sprintf("diagnostics: %v", dc.latest().Diagnostics))

	ok.MustNoError(t, os.WriteFile(configPath, []byte(`
name: test
variables:
  - name: count
    type: "int"
`), 0o600))

	// Any request that resolves the document's environment notices the edit.
	// This one is the request an editor makes without being asked, as soon as
	// the file is on screen again.
	var tokens protocol.SemanticTokens
	_, err := conn.Call(ctx, "textDocument/semanticTokens/full", protocol.SemanticTokensParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: celURI},
	}, &tokens)
	ok.MustNoError(t, err)

	dc.waitForDiagnostics(t, 2)
	params := dc.latest()
	ok.Equal(t, string(params.URI), string(celURI))
	ok.Equal(t, len(params.Diagnostics), 0, ok.Sprintf("diagnostics: %v", params.Diagnostics))
}

// refreshRecorder is a client that supports the workspace refresh requests and
// records each one it receives.
type refreshRecorder struct {
	protocol.UnimplementedClient
	got chan string
}

func (r *refreshRecorder) PublishDiagnostics(context.Context, *protocol.PublishDiagnosticsParams) error {
	return nil
}

func (r *refreshRecorder) InlayHintRefresh(context.Context) error {
	r.got <- "inlayHint"
	return nil
}

func (r *refreshRecorder) SemanticTokensRefresh(context.Context) error {
	r.got <- "semanticTokens"
	return nil
}

func (r *refreshRecorder) DiagnosticRefresh(context.Context) error {
	r.got <- "diagnostic"
	return nil
}

// What a client requests as a document changes and keeps until it next
// changes is requested again when the configuration it was computed under is
// edited, for each kind the client can be asked to refresh: inlay hints and
// semantic tokens, and diagnostics when the client pulls them.
func TestServerRequestsRefreshAfterConfigChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pull bool
		want []string
	}{
		{"push diagnostics", false, []string{"inlayHint", "semanticTokens"}},
		{"pull diagnostics", true, []string{"diagnostic", "inlayHint", "semanticTokens"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			dir := t.TempDir()
			configPath := filepath.Join(dir, lsp.ConfigFileName)
			ok.MustNoError(t, os.WriteFile(configPath, []byte("name: test\n"), 0o600))
			celPath := filepath.Join(dir, "a.cel")

			recorder := &refreshRecorder{got: make(chan string, 10)}
			conn := newLSPClient(t, recorder, lsp.Options{})
			params := protocol.InitializeParams{
				Capabilities: protocol.ClientCapabilities{
					Workspace: &protocol.WorkspaceClientCapabilities{
						InlayHint:      &protocol.InlayHintWorkspaceClientCapabilities{RefreshSupport: new(true)},
						SemanticTokens: &protocol.SemanticTokensWorkspaceClientCapabilities{RefreshSupport: new(true)},
						Diagnostics:    &protocol.DiagnosticWorkspaceClientCapabilities{RefreshSupport: new(true)},
					},
				},
			}
			if tt.pull {
				params.InitializationOptions = protocol.LSPAny(`{"pullDiagnostics": true}`)
			}
			var initResult protocol.InitializeResult
			_, err := conn.Call(ctx, "initialize", params, &initResult)
			ok.MustNoError(t, err)
			ok.MustNoError(t, conn.Notify(ctx, "initialized", protocol.InitializedParams{}))

			requestHints := func() {
				var hints []protocol.InlayHint
				_, err := conn.Call(ctx, "textDocument/inlayHint", protocol.InlayHintParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(celPath)},
				}, &hints)
				ok.MustNoError(t, err)
			}

			// Opening the document builds its environment for the first time,
			// which nothing the client holds depends on yet, so the requests
			// that follow have nothing to refresh.
			openAndDiagnose(t, conn, celPath, "items.all(x, x > 0)")
			requestHints()

			ok.MustNoError(t, os.WriteFile(configPath, []byte(`
name: test
variables:
  - name: items
    type_name: list
    params: [{type_name: int}]
`), 0o600))
			requestHints()

			var got []string
			for range tt.want {
				select {
				case method := <-recorder.got:
					got = append(got, method)
				case <-time.After(5 * time.Second):
					t.Fatalf("refreshes: got %v, want %v", got, tt.want)
				}
			}
			slices.Sort(got)
			ok.Equal(t, got, tt.want)
			select {
			case method := <-recorder.got:
				t.Errorf("unexpected %s refresh", method)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// A configuration that does not load is reported against the document that
// depends on it, since nothing else in an editor would show it.
func TestServerReportsBrokenConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, lsp.ConfigFileName)
	ok.MustNoError(t, os.WriteFile(configPath, []byte("name: [unterminated\n"), 0o600))

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	initializeServer(t, conn, "")

	diags := openAndDiagnose(t, conn, filepath.Join(dir, "a.cel"), "1 + 1")
	if ok.Equal(t, len(diags), 1, ok.Sprintf("diagnostics: %v", diags)) {
		ok.Equal(t, diags[0].Severity, protocol.DiagnosticSeverityError)
		ok.True(t, strings.Contains(diagnosticMessage(diags[0]), configPath),
			ok.Sprintf("message: %v", diags[0].Message))
	}
}

// initializeServer completes the initialize handshake, sending initOptions as
// raw initializationOptions JSON when it is not empty.
func initializeServer(t *testing.T, conn jsonrpc2.Conn, initOptions string) {
	t.Helper()

	params := protocol.InitializeParams{}
	if initOptions != "" {
		params.InitializationOptions = protocol.LSPAny(initOptions)
	}
	var initResult protocol.InitializeResult
	_, err := conn.Call(t.Context(), "initialize", params, &initResult)
	ok.MustNoError(t, err)
	ok.MustNoError(t, conn.Notify(t.Context(), "initialized", protocol.InitializedParams{}))
}

// openAndDiagnose opens a document at path holding content and returns the
// diagnostics reported for it.
func openAndDiagnose(t *testing.T, conn jsonrpc2.Conn, path, content string) []protocol.Diagnostic {
	t.Helper()

	err := conn.Notify(t.Context(), "textDocument/didOpen", protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri.File(path),
			LanguageID: "cel",
			Version:    1,
			Text:       content,
		},
	})
	ok.MustNoError(t, err)
	return diagnoseOpen(t, conn, path)
}

// diagnoseOpen pulls the diagnostics for an already-open document.
func diagnoseOpen(t *testing.T, conn jsonrpc2.Conn, path string) []protocol.Diagnostic {
	t.Helper()

	var report protocol.RelatedFullDocumentDiagnosticReport
	_, err := conn.Call(t.Context(), "textDocument/diagnostic", protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
	}, &report)
	ok.MustNoError(t, err)
	return report.Items
}

// diagnosticMessage returns a diagnostic's message text.
func diagnosticMessage(d protocol.Diagnostic) string {
	message, _ := d.Message.(protocol.String)
	return string(message)
}

// Every request against a document whose configuration does not load must
// still answer. Only diagnostics have anything to say in that case, but none
// of the others may fall over.
func TestServerRequestsWithBrokenConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ok.MustNoError(t, os.WriteFile(filepath.Join(dir, lsp.ConfigFileName), []byte("name: [unterminated\n"), 0o600))
	celPath := filepath.Join(dir, "a.cel")

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	initializeServer(t, conn, "")

	docURI := uri.File(celPath)
	err := conn.Notify(t.Context(), "textDocument/didOpen", protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: docURI, LanguageID: "cel", Version: 1, Text: "x + x",
		},
	})
	ok.MustNoError(t, err)

	doc := protocol.TextDocumentIdentifier{URI: docURI}
	pos := protocol.Position{Line: 0, Character: 0}
	for _, tt := range []struct {
		method string
		params any
	}{
		{"textDocument/hover", protocol.HoverParams{TextDocument: doc, Position: pos}},
		{"textDocument/completion", protocol.CompletionParams{TextDocument: doc, Position: pos}},
		{"textDocument/formatting", protocol.DocumentFormattingParams{TextDocument: doc}},
		{"textDocument/references", map[string]any{
			"textDocument": map[string]any{"uri": docURI},
			"position":     map[string]any{"line": 0, "character": 0},
			"context":      map[string]any{"includeDeclaration": true},
		}},
		{"textDocument/documentHighlight", protocol.DocumentHighlightParams{TextDocument: doc, Position: pos}},
		{"textDocument/rename", protocol.RenameParams{TextDocument: doc, Position: pos, NewName: "y"}},
		{"textDocument/prepareRename", map[string]any{
			"textDocument": map[string]any{"uri": docURI},
			"position":     map[string]any{"line": 0, "character": 0},
		}},
		{"textDocument/signatureHelp", protocol.SignatureHelpParams{TextDocument: doc, Position: pos}},
		{"textDocument/semanticTokens/full", protocol.SemanticTokensParams{TextDocument: doc}},
		{"textDocument/inlayHint", protocol.InlayHintParams{TextDocument: doc}},
		{"textDocument/codeAction", protocol.CodeActionParams{TextDocument: doc}},
	} {
		t.Run(tt.method, func(t *testing.T) {
			var result any
			_, err := conn.Call(t.Context(), tt.method, tt.params, &result)
			ok.NoError(t, err)
		})
	}
}
