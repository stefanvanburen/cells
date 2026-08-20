package lsp_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/nalgeon/be"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.vanburen.xyz/cells/internal/lsp"
)

// networkExpr type-checks only when the network extension is enabled.
const networkExpr = `cidr('10.0.0.0/8').containsIP(ip('10.1.2.3'))`

// diagnosticsForExtensions starts a server whose default extensions are
// defaultExtensions (standing in for `cells serve --ext=...`), initializes it
// with initOptions as raw initializationOptions JSON (omitted entirely when
// empty), opens a document holding expr, and returns the reported diagnostics.
func diagnosticsForExtensions(t *testing.T, defaultExtensions []string, initOptions, expr string) []protocol.Diagnostic {
	t.Helper()
	ctx := t.Context()

	clientRPC := newLSPClient(t, protocol.UnimplementedClient{}, defaultExtensions...)

	params := protocol.InitializeParams{}
	if initOptions != "" {
		params.InitializationOptions = protocol.LSPAny(initOptions)
	}
	var initResult protocol.InitializeResult
	_, err := clientRPC.Call(ctx, "initialize", params, &initResult)
	be.Err(t, err, nil)

	err = clientRPC.Notify(ctx, "initialized", protocol.InitializedParams{})
	be.Err(t, err, nil)

	testURI := uri.File("/test.cel")
	err = clientRPC.Notify(ctx, "textDocument/didOpen", protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        testURI,
			LanguageID: "cel",
			Version:    1,
			Text:       expr,
		},
	})
	be.Err(t, err, nil)

	var diagResult protocol.RelatedFullDocumentDiagnosticReport
	_, err = clientRPC.Call(ctx, "textDocument/diagnostic", protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: testURI},
	}, &diagResult)
	be.Err(t, err, nil)

	return diagResult.Items
}

// TestExtensionResolution covers where the enabled extension set comes from —
// the server's own defaults, the client's initializationOptions, and the
// precedence between them.
func TestExtensionResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		defaults    []string
		initOptions string
		wantClean   bool
	}{
		{"no defaults and no options", nil, "", false},
		{"client opts in", nil, `{"extensions":["network"]}`, true},
		{"server default applies", []string{"network"}, "", true},
		{"server default survives unrelated options", []string{"network"}, `{"somethingElse":true}`, true},
		{"empty list clears server default", []string{"network"}, `{"extensions":[]}`, false},
		{"client list replaces server default", []string{"network"}, `{"extensions":["strings"]}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			diags := diagnosticsForExtensions(t, tt.defaults, tt.initOptions, networkExpr)
			if tt.wantClean {
				be.Equal(t, len(diags), 0)
			} else {
				be.True(t, len(diags) > 0)
			}
		})
	}
}

func TestInitializeUnknownExtensionFails(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	clientRPC := newLSPClient(t, protocol.UnimplementedClient{})

	var initResult protocol.InitializeResult
	_, err := clientRPC.Call(ctx, "initialize", protocol.InitializeParams{
		InitializationOptions: protocol.LSPAny(`{"extensions":["nope"]}`),
	}, &initResult)
	be.Err(t, err, "unknown CEL extension")
}

// TestExtensionFactories exercises every name in extensionFactories through
// the public Check API: an expression that only that extension declares must
// type-check with it enabled and fail without it.
func TestExtensionFactories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ext  string
		expr string
	}{
		{"bindings", `cel.bind(x, 1, x + 1)`},
		{"comprehensions", `[1, 2].transformList(i, v, v * 2)`},
		{"encoders", `base64.encode(b"x")`},
		{"hmac", `hmac.compute(b"msg", b"key", hmac.HS256)`},
		{"jwt", `jwt.parse("a.b.c")`},
		{"lists", `[1, 2].distinct()`},
		{"math", `math.greatest(1, 2, 3)`},
		{"network", `cidr("10.0.0.0/8").containsIP(ip("10.1.2.3"))`},
		{"optional", `optional.of(1).value()`},
		{"regex", `regex.extract("a1", "a(\\d)")`},
		{"sets", `sets.contains([1, 2], [1])`},
		{"strings", `"abc".reverse()`},
	}

	// Guard against a new extension being registered without a test here.
	covered := make([]string, 0, len(tests)+1)
	for _, tt := range tests {
		covered = append(covered, tt.ext)
	}
	covered = append(covered, "protos") // exercised by TestExtensionProtos
	slices.Sort(covered)
	be.Equal(t, covered, lsp.ExtensionNames())

	for _, tt := range tests {
		t.Run(tt.ext, func(t *testing.T) {
			t.Parallel()

			with, err := lsp.Check(tt.expr, tt.ext)
			be.Err(t, err, nil)
			be.Equal(t, len(with), 0)

			without, err := lsp.Check(tt.expr)
			be.Err(t, err, nil)
			be.True(t, len(without) > 0)
		})
	}
}

// protos declares macros rather than plain functions, and cells registers no
// proto types, so proto.getExt can never type-check cleanly. Assert the macro
// ran instead: with the extension the complaint is about the extension field,
// without it the "proto" namespace itself is undeclared.
func TestExtensionProtos(t *testing.T) {
	t.Parallel()

	const expr = `proto.getExt(1, foo)`

	with, err := lsp.Check(expr, "protos")
	be.Err(t, err, nil)
	be.Equal(t, len(with), 1)
	be.Equal(t, with[0].Message, "invalid extension field")

	without, err := lsp.Check(expr)
	be.Err(t, err, nil)
	be.True(t, len(without) > 0)
	be.True(t, strings.Contains(without[0].Message, "undeclared reference to 'proto'"))
}
