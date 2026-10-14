package lsp_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
)

// FuzzAPI runs each command on arbitrary source, at an arbitrary position. None
// of them panics, and formatting formatted source leaves it as it is. The seed
// corpus is every .cel file under testdata.
func FuzzAPI(f *testing.F) {
	paths, err := filepath.Glob("testdata/*/*.cel")
	ok.MustNoError(f, err)
	for _, path := range paths {
		content, err := os.ReadFile(path)
		ok.MustNoError(f, err)
		f.Add(string(content), uint8(1), uint8(1))
	}

	f.Fuzz(func(t *testing.T, content string, line, col uint8) {
		opts := lsp.Options{}
		if formatted, err := lsp.Format(content, opts); err == nil {
			again, err := lsp.Format(formatted, opts)
			ok.NoError(t, err)
			ok.Equal(t, again, formatted)
		}
		_, _ = lsp.Check(content, opts)

		l, c := int(line)+1, int(col)+1
		_, _ = lsp.Hover(content, l, c, opts)
		_, _ = lsp.References(content, l, c, opts)
		_, _, _ = lsp.FindDefinition("fuzz.cel", content, l, c, opts)
		_, _ = lsp.Rename(content, l, c, "renamed", opts)
	})
}
