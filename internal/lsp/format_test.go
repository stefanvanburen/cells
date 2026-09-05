package lsp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
)

func TestFormat(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir("testdata/format")
	ok.MustNoError(t, err)

	// Collect test cases from *.input.cel / *.golden.cel pairs.
	type testCase struct {
		name   string
		input  string
		golden string
	}
	var tests []testCase
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".input.cel") {
			continue
		}
		base := strings.TrimSuffix(name, ".input.cel")
		goldenFile := base + ".golden.cel"

		inputPath := filepath.Join("testdata/format", name)
		goldenPath := filepath.Join("testdata/format", goldenFile)

		tests = append(tests, testCase{
			name:   base,
			input:  inputPath,
			golden: goldenPath,
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			golden, err := os.ReadFile(tt.golden)
			ok.MustNoError(t, err)

			edits := requestFormatting(t, tt.input)
			input, err := os.ReadFile(tt.input)
			ok.MustNoError(t, err)

			got := applyEdits(string(input), edits)
			ok.Equal(t, got, string(golden))
		})
	}
}

func TestFormatParseError(t *testing.T) {
	t.Parallel()
	// Parse errors should return no edits (not crash).
	edits := requestFormatting(t, "testdata/semantic_tokens/parse_error.cel")
	ok.Equal(t, len(edits), 0)
}

func TestFormatCapabilities(t *testing.T) {
	t.Parallel()

	clientRPC := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})

	var result protocol.InitializeResult
	_, err := clientRPC.Call(t.Context(), "initialize", protocol.InitializeParams{}, &result)
	ok.MustNoError(t, err)

	ok.True(t, result.Capabilities.DocumentFormattingProvider != nil)
}

// requestFormatting opens a file and sends a textDocument/formatting request.
func requestFormatting(t *testing.T, celFile string) []protocol.TextEdit {
	t.Helper()
	ctx := t.Context()

	testPath := getAbsPath(t, celFile)
	clientConn, testURI := setupLSPServer(t, testPath)

	var edits []protocol.TextEdit
	_, err := clientConn.Call(ctx, "textDocument/formatting", protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: testURI},
		Options: protocol.FormattingOptions{
			TabSize:      2,
			InsertSpaces: true,
		},
	}, &edits)
	ok.MustNoError(t, err)

	return edits
}

// applyEdits applies text edits to the original content.
// For our formatter, there's at most one whole-document edit.
func applyEdits(content string, edits []protocol.TextEdit) string {
	if len(edits) == 0 {
		return content
	}
	// We always emit a single whole-document replacement.
	return edits[0].NewText
}
