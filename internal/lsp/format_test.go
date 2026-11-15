package lsp_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

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

			// Formatted source is left as it is.
			ok.Equal(t, len(requestFormatting(t, tt.golden)), 0)
		})
	}
}

// TestFormatEditRangeNonASCII pins the end of the edit of the input's only
// line to UTF-16 code units, which is what LSP positions are measured in. The
// input is 11 bytes but 10 code units long, and has no trailing newline, so a
// range counted in bytes would run past the end of the line.
func TestFormatEditRangeNonASCII(t *testing.T) {
	t.Parallel()

	edits := requestFormatting(t, "testdata/format/non_ascii.input.cel")
	if !ok.Equal(t, len(edits), 1) {
		return
	}
	ok.Equal(t, edits[0].Range.Start, protocol.Position{Line: 0, Character: 0})
	ok.Equal(t, edits[0].Range.End, protocol.Position{Line: 0, Character: 10})
}

// TestFormatInvalidUTF8 formats string literals holding bytes that are not
// UTF-8, which the parser reads as U+FFFD and so measures as three bytes long.
func TestFormatInvalidUTF8(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ input, want string }{
		{"'\x95'+1", "'\x95' + 1"},
		{"'\x95\xff'+1", "'\x95\xff' + 1"},
		{"'é\x95'+'\x95'", "'é\x95' + '\x95'"},
		{"['\x95',1]", "['\x95', 1]"},
	} {
		got, err := lsp.Format(tt.input, lsp.Options{})
		ok.NoError(t, err)
		ok.Equal(t, got, tt.want)
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
	ok.True(t, result.Capabilities.DocumentRangeFormattingProvider != nil)
}

// TestRangeFormatting formats a document with two changed lines, 1 and 3,
// around an unchanged one, and checks which of their edits each selection
// gets.
func TestRangeFormatting(t *testing.T) {
	t.Parallel()

	const path = "testdata/range_formatting/two_changes.cel"
	input, err := os.ReadFile(path)
	ok.MustNoError(t, err)

	tests := []struct {
		name      string
		selection protocol.Range
		edits     int
		want      string
	}{
		{
			name:      "line_of_one_change",
			selection: protocol.Range{Start: protocol.Position{Line: 1}, End: protocol.Position{Line: 2}},
			edits:     1,
			want:      "[\n  1 + 2,\n  3,\n  4  +  5\n]\n",
		},
		{
			name:      "part_of_a_line",
			selection: protocol.Range{Start: protocol.Position{Line: 1, Character: 2}, End: protocol.Position{Line: 1, Character: 3}},
			edits:     1,
			want:      "[\n  1 + 2,\n  3,\n  4  +  5\n]\n",
		},
		{
			name:      "cursor_in_change",
			selection: protocol.Range{Start: protocol.Position{Line: 3, Character: 3}, End: protocol.Position{Line: 3, Character: 3}},
			edits:     1,
			want:      "[\n  1+2,\n  3,\n  4 + 5,\n]\n",
		},
		{
			name:      "unchanged_line_between",
			selection: protocol.Range{Start: protocol.Position{Line: 2}, End: protocol.Position{Line: 3}},
			edits:     0,
			want:      string(input),
		},
		{
			name:      "whole_document",
			selection: protocol.Range{End: protocol.Position{Line: 5}},
			edits:     2,
			want:      "[\n  1 + 2,\n  3,\n  4 + 5,\n]\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clientConn, testURI := setupLSPServer(t, getAbsPath(t, path))
			var edits []protocol.TextEdit
			_, err := clientConn.Call(t.Context(), "textDocument/rangeFormatting", protocol.DocumentRangeFormattingParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: testURI},
				Range:        tt.selection,
				Options:      protocol.FormattingOptions{TabSize: 2, InsertSpaces: true},
			}, &edits)
			ok.MustNoError(t, err)

			ok.Equal(t, len(edits), tt.edits)
			ok.Equal(t, applyEdits(string(input), edits), tt.want)
		})
	}
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

// applyEdits applies edits, which must not overlap, to content.
func applyEdits(content string, edits []protocol.TextEdit) string {
	edits = slices.Clone(edits)
	slices.SortFunc(edits, func(a, b protocol.TextEdit) int {
		return positionOffset(content, b.Range.Start) - positionOffset(content, a.Range.Start)
	})
	for _, edit := range edits {
		start, end := positionOffset(content, edit.Range.Start), positionOffset(content, edit.Range.End)
		content = content[:start] + edit.NewText + content[end:]
	}
	return content
}

// positionOffset returns the byte offset in content of pos, whose character
// is measured in UTF-16 code units.
func positionOffset(content string, pos protocol.Position) int {
	offset := 0
	for range pos.Line {
		offset += strings.IndexByte(content[offset:], '\n') + 1
	}
	for character := uint32(0); character < pos.Character; {
		r, size := utf8.DecodeRuneInString(content[offset:])
		character += uint32(utf16.RuneLen(r))
		offset += size
	}
	return offset
}
