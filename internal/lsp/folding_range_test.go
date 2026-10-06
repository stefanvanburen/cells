package lsp_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.vanburen.xyz/ok"
)

func TestFoldingRange(t *testing.T) {
	t.Parallel()

	// want lists each fold as its 1-indexed first and last lines and its kind.
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"single_line", "size([1, 2]) > 0", nil},
		{"list", "[\n  1,\n  2,\n]", []string{"1-3 region"}},
		{"bracket_ends_last_line", "size([1,\n  2]) > 0", []string{"1-2 region"}},
		{"nested", "f(\n  [\n    1,\n  ],\n)", []string{"1-4 region", "2-3 region"}},
		{"same_start_line", "f([\n  1,\n])", []string{"1-2 region"}},
		{"macro", "xs.all(x,\n  x > 0\n)", []string{"1-2 region"}},
		{"message", "google.protobuf.Duration{\n  seconds: 1,\n}", []string{"1-2 region"}},
		{"comments", "// a\n// b\nx && // c\n// d\ny", []string{"1-2 comment"}},
		{"triple_quoted", "\"\"\"a\nb\nc\"\"\" == x", []string{"1-3 region"}},
		{"parse_error", "f(\n  1,\n  +", nil},
		{"unbalanced_close", "x)\n(\n  y\n)", []string{"2-3 region"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "test.cel")
			ok.MustNoError(t, os.WriteFile(path, []byte(tt.src), 0o600))
			conn, uri := setupLSPServer(t, path)

			var result []protocol.FoldingRange
			_, err := conn.Call(t.Context(), "textDocument/foldingRange", protocol.FoldingRangeParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			}, &result)
			ok.MustNoError(t, err)

			var got []string
			for _, r := range result {
				got = append(got, fmt.Sprintf("%d-%d %s", r.StartLine+1, r.EndLine+1, r.Kind))
			}
			ok.DeepEqual(t, got, tt.want)
		})
	}
}
