package lsp_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"go.lsp.dev/protocol"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
)

var update = flag.Bool("update", false, "rewrite the semantic token golden files")

// TestSemanticTokens compares the semantic tokens of each .cel file under
// testdata/semantic_tokens with the .tokens file beside it, which lists every
// token on a line of its own: its 1-indexed line and UTF-16 column, its text,
// and its type followed by any modifiers. The files in configured/ are checked
// against the cel.yaml there.
func TestSemanticTokens(t *testing.T) {
	t.Parallel()
	legend := semanticTokensLegend(t)

	files, err := filepath.Glob("testdata/semantic_tokens/*.cel")
	ok.MustNoError(t, err)
	configured, err := filepath.Glob("testdata/semantic_tokens/configured/*.cel")
	ok.MustNoError(t, err)
	files = append(files, configured...)

	for _, file := range files {
		name := strings.TrimPrefix(strings.TrimSuffix(file, ".cel"), "testdata/semantic_tokens/")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := describeSemanticTokens(t, file, legend)
			golden := strings.TrimSuffix(file, ".cel") + ".tokens"
			if *update {
				ok.MustNoError(t, os.WriteFile(golden, []byte(got), 0o600))
				return
			}
			want, err := os.ReadFile(golden)
			ok.MustNoError(t, err)
			ok.Equal(t, got, string(want))
		})
	}
}

// semanticTokensLegend returns the legend the server advertises.
func semanticTokensLegend(t *testing.T) protocol.SemanticTokensLegend {
	t.Helper()
	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	var result struct {
		Capabilities struct {
			SemanticTokensProvider struct {
				Legend protocol.SemanticTokensLegend `json:"legend"`
			} `json:"semanticTokensProvider"`
		} `json:"capabilities"`
	}
	var raw json.RawMessage
	_, err := conn.Call(t.Context(), "initialize", protocol.InitializeParams{}, &raw)
	ok.MustNoError(t, err)
	ok.MustNoError(t, json.Unmarshal(raw, &result))
	return result.Capabilities.SemanticTokensProvider.Legend
}

// describeSemanticTokens returns the semantic tokens of celFile in the format
// of the golden files.
func describeSemanticTokens(t *testing.T, celFile string, legend protocol.SemanticTokensLegend) string {
	t.Helper()
	testPath := getAbsPath(t, celFile)
	conn, testURI := setupLSPServer(t, testPath)

	var result *protocol.SemanticTokens
	_, err := conn.Call(t.Context(), "textDocument/semanticTokens/full", protocol.SemanticTokensParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: testURI},
	}, &result)
	ok.MustNoError(t, err)
	if result == nil {
		return ""
	}

	content, err := os.ReadFile(testPath)
	ok.MustNoError(t, err)
	lines := strings.Split(string(content), "\n")

	var b strings.Builder
	var line, col uint32
	data := result.Data
	ok.Equal(t, len(data)%5, 0)
	for i := 0; i+5 <= len(data); i += 5 {
		deltaLine, deltaCol, length, semType, semMod := data[i], data[i+1], data[i+2], data[i+3], data[i+4]
		if deltaLine != 0 {
			col = 0
		}
		line += deltaLine
		col += deltaCol

		units := utf16.Encode([]rune(lines[line]))
		ok.True(t, col+length <= uint32(len(units)), ok.Sprintf("token at %d:%d runs past the end of its line", line+1, col+1))
		text := string(utf16.Decode(units[col : col+length]))

		class := []string{legend.TokenTypes[semType]}
		for bit, modifier := range legend.TokenModifiers {
			if semMod&(1<<bit) != 0 {
				class = append(class, modifier)
			}
		}
		fmt.Fprintf(&b, "%d:%d %s %s\n", line+1, col+1, text, strings.Join(class, "."))
	}
	return b.String()
}
