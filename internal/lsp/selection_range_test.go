package lsp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"go.lsp.dev/protocol"
	"go.vanburen.xyz/ok"
)

func TestSelectionRange(t *testing.T) {
	t.Parallel()

	// Each source marks the cursor with ‸; want is the text of each
	// selection, innermost first.
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"precedence", "x + y * ‸z", []string{"z", "y * z", "x + y * z"}},
		{"operator", "x ‸+ y * z", []string{"x + y * z"}},
		{"end_of_name", "abc‸ + d", []string{"abc", "abc + d"}},
		{"grouping", "(‸x + y) * z", []string{"x", "x + y", "(x + y)", "(x + y) * z"}},
		{"nested_grouping", "!((‸x || y))", []string{"x", "x || y", "(x || y)", "((x || y))", "!((x || y))"}},
		{"function", "size(‸xs) > 0", []string{"xs", "size(xs)", "size(xs) > 0"}},
		{"function_name", "si‸ze(xs) > 0", []string{"size", "size(xs)", "size(xs) > 0"}},
		{"method", `"abc".starts‸With("a")`, []string{"startsWith", `"abc".startsWith("a")`}},
		{"select", "a.‸b.c == 1", []string{"b", "a.b", "a.b.c", "a.b.c == 1"}},
		{"index", "xs[‸0] + 1", []string{"0", "xs[0]", "xs[0] + 1"}},
		{"list", "[1, ‸2, 3]", []string{"2", "[1, 2, 3]"}},
		{"map_entry", `{"a": 1, "b": ‸x}`, []string{"x", `"b": x`, `{"a": 1, "b": x}`}},
		{"message", "google.protobuf.Duration{seconds: ‸1}", []string{"1", "seconds: 1", "google.protobuf.Duration{seconds: 1}"}},
		{"message_name", "google.protobuf.Dur‸ation{seconds: 1}", []string{"google.protobuf.Duration", "google.protobuf.Duration{seconds: 1}"}},
		{"ternary", `x ? ‸"a" : "b"`, []string{`"a"`, `x ? "a" : "b"`}},
		{"macro", "[1, 2].all(n, ‸n > 0)", []string{"n", "n > 0", "[1, 2].all(n, n > 0)"}},
		{"macro_declaration", "[1, 2].all(‸n, n > 0)", []string{"n", "[1, 2].all(n, n > 0)"}},
		{"nested_macro", "xs.filter(x, x > 0).map(‸y, y * 2)", []string{"y", "xs.filter(x, x > 0).map(y, y * 2)"}},
		{"inside_nested_macro", "xs.filter(x, ‸x > 0).map(y, y * 2)", []string{"x", "x > 0", "xs.filter(x, x > 0)", "xs.filter(x, x > 0).map(y, y * 2)"}},
		{"has", "has(a.‸b) && c", []string{"b", "a.b", "has(a.b)", "has(a.b) && c"}},
		{"and_chain", "a && b && ‸c && d", []string{"c", "a && b && c && d"}},
		{"grouped_chain", "a && (b && ‸c) && d", []string{"c", "b && c", "(b && c)", "a && (b && c) && d"}},
		{"mixed_chain", "a || b && ‸c || d", []string{"c", "b && c", "a || b && c || d"}},
		{"multiline", "// comment\nx &&\n  (‸y || z)", []string{"y", "y || z", "(y || z)", "x &&\n  (y || z)"}},
		{"unicode", `"é" + ‸x`, []string{"x", `"é" + x`}},
		{"comment", "x + y // ‸note", []string{""}},
		{"parse_error", "x + ‸", []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ok.DeepEqual(t, selections(t, tt.src), tt.want)
		})
	}
}

// selections returns the text of each selection range around the cursor
// marked ‸ in src, innermost first.
func selections(t *testing.T, src string) []string {
	t.Helper()
	before, after, found := strings.Cut(src, "‸")
	ok.True(t, found)
	content := before + after
	lines := strings.Split(before, "\n")
	cursor := protocol.Position{
		Line:      uint32(len(lines) - 1),
		Character: uint32(len(utf16.Encode([]rune(lines[len(lines)-1])))),
	}

	path := filepath.Join(t.TempDir(), "test.cel")
	ok.MustNoError(t, os.WriteFile(path, []byte(content), 0o600))
	conn, uri := setupLSPServer(t, path)

	var result []protocol.SelectionRange
	_, err := conn.Call(t.Context(), "textDocument/selectionRange", protocol.SelectionRangeParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Positions:    []protocol.Position{cursor},
	}, &result)
	ok.MustNoError(t, err)
	ok.Equal(t, len(result), 1)

	var texts []string
	for r := &result[0]; r != nil; r = r.Parent {
		texts = append(texts, rangeText(content, r.Range))
	}
	return texts
}

// rangeText returns the text of content that r covers.
func rangeText(content string, r protocol.Range) string {
	offset := func(p protocol.Position) int {
		lines := strings.SplitAfter(content, "\n")
		n := 0
		for _, line := range lines[:p.Line] {
			n += len(line)
		}
		units := utf16.Encode([]rune(lines[p.Line]))
		return n + len(string(utf16.Decode(units[:p.Character])))
	}
	return content[offset(r.Start):offset(r.End)]
}
