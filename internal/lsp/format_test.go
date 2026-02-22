package lsp_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nalgeon/be"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

func TestFormat(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir("testdata/format")
	be.Err(t, err, nil)

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
			be.Err(t, err, nil)

			edits := requestFormatting(t, tt.input)
			input, err := os.ReadFile(tt.input)
			be.Err(t, err, nil)

			got := applyEdits(string(input), edits)
			be.Equal(t, got, string(golden))
		})
	}
}

func TestFormatParseError(t *testing.T) {
	t.Parallel()
	// Parse errors should return no edits (not crash).
	edits := requestFormatting(t, "testdata/semantic_tokens/parse_error.cel")
	be.Equal(t, len(edits), 0)
}

func TestFormatCapabilities(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	go func() {
		_ = lsp.ServeStream(ctx, serverConn)
	}()

	noop := jsonrpc2.HandlerFunc(func(_ context.Context, _ *jsonrpc2.Conn, _ *jsonrpc2.Request) (any, error) {
		return nil, nil
	})
	clientRPC := jsonrpc2.NewConn(ctx, clientConn, noop)
	t.Cleanup(func() {
		_ = clientRPC.Close()
	})

	var result protocol.InitializeResult
	err := clientRPC.Call(ctx, "initialize", protocol.InitializeParams{}, &result)
	be.Err(t, err, nil)

	be.True(t, result.Capabilities.DocumentFormattingProvider != nil)
}

// requestFormatting opens a file and sends a textDocument/formatting request.
func requestFormatting(t *testing.T, celFile string) []protocol.TextEdit {
	t.Helper()
	ctx := t.Context()

	testPath := getAbsPath(t, celFile)
	clientConn, testURI := setupLSPServer(t, testPath)

	var edits []protocol.TextEdit
	err := clientConn.Call(ctx, "textDocument/formatting", protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: testURI},
		Options: protocol.FormattingOptions{
			TabSize:      2,
			InsertSpaces: true,
		},
	}, &edits)
	be.Err(t, err, nil)

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
