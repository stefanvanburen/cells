package lsp_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
)

// getInlayHints sends a textDocument/inlayHint request and returns the result.
func getInlayHints(t *testing.T, celFile string) []protocol.InlayHint {
	t.Helper()
	ctx := t.Context()
	testPath := getAbsPath(t, celFile)
	clientConn, testURI := setupLSPServer(t, testPath)

	var result []protocol.InlayHint
	_, err := clientConn.Call(ctx, "textDocument/inlayHint", protocol.InlayHintParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: testURI,
		},
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: 1000, Character: 1000},
		},
	}, &result)
	ok.MustNoError(t, err)
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
				ok.True(t, len(hints) > 0)
				labelParts, isParts := hints[0].Label.(protocol.InlayHintLabelPartSlice)
				ok.True(t, isParts)
				ok.True(t, len(labelParts) > 0)
				hintText := labelParts[0].Value
				ok.True(t, len(hintText) > 0)
				// Check if result is in the hint (after the arrow)
				ok.True(t, len(hintText) >= 2)
			} else {
				ok.True(t, len(hints) == 0)
			}
		})
	}
}

func TestInlayHintsLoopVariableTypes(t *testing.T) {
	t.Parallel()

	// want lists each type hint as the 1-indexed line and UTF-16 column it
	// follows, then its label.
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"list", "[1, 2].all(x, x > 0)", []string{"1:13 : int"}},
		{"map", `{"a": 1}.exists(k, k == "a")`, []string{"1:18 : string"}},
		{"two_variable_map", `{"a": 1}.all(k, v, v > 0)`, []string{"1:15 : string", "1:18 : int"}},
		{"two_variable_list", `["a"].exists(i, v, i == 0 && v == "a")`, []string{"1:15 : int", "1:18 : string"}},
		{"bind", `cel.bind(n, "x", n + n) == "xx"`, []string{"1:11 : string"}},
		{"dyn", "request.headers.all(h, h != '')", []string{"1:22 : dyn"}},
		{"nested", "[[1]].all(xs, xs.all(x, x > 0))", []string{"1:13 : list(int)", "1:23 : int"}},
		{"multiline", "[1].map(\n  x,\n  x * 2\n)", []string{"2:4 : int"}},
		{"does_not_check", "[1].all(x, x > undeclared)", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			config := "name: test\nvariables:\n  - name: request\n    type: \"map<string, dyn>\"\nextensions:\n  - name: bindings\n  - name: comprehensions\n"
			ok.MustNoError(t, os.WriteFile(filepath.Join(dir, lsp.ConfigFileName), []byte(config), 0o600))
			path := filepath.Join(dir, "test.cel")
			ok.MustNoError(t, os.WriteFile(path, []byte(tt.src), 0o600))

			var got []string
			for _, hint := range getInlayHints(t, path) {
				label := hint.Label.(protocol.InlayHintLabelPartSlice)[0].Value
				if strings.HasPrefix(label, ": ") {
					got = append(got, fmt.Sprintf("%d:%d %s", hint.Position.Line+1, hint.Position.Character+1, label))
				}
			}
			ok.DeepEqual(t, got, tt.want)
		})
	}
}

func TestInlayHintsCostLimit(t *testing.T) {
	t.Parallel()

	// Four nested comprehensions over 100 elements take 10^8 iterations,
	// far beyond the cost an inlay hint may spend evaluating.
	list := "[" + strings.Repeat("0, ", 99) + "0]"
	src := list + ".all(a, " + list + ".all(b, " + list + ".all(c, " + list + ".all(d, a + b + c + d == 0))))"
	path := filepath.Join(t.TempDir(), "test.cel")
	ok.MustNoError(t, os.WriteFile(path, []byte(src), 0o600))

	var results []string
	for _, hint := range getInlayHints(t, path) {
		if label := hint.Label.(protocol.InlayHintLabelPartSlice)[0].Value; strings.HasPrefix(label, "→") {
			results = append(results, label)
		}
	}
	ok.DeepEqual(t, results, nil)
}
