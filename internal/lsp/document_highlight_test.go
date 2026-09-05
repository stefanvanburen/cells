package lsp_test

import (
	"testing"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	lspuri "go.lsp.dev/uri"
	"go.vanburen.xyz/ok"
)

// requestDocumentHighlight sends a textDocument/documentHighlight request at the given position.
func requestDocumentHighlight(t *testing.T, conn jsonrpc2.Conn, uri lspuri.URI, pos protocol.Position) []protocol.DocumentHighlight {
	t.Helper()
	var result []protocol.DocumentHighlight
	_, err := conn.Call(t.Context(), "textDocument/documentHighlight", protocol.DocumentHighlightParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     pos,
	}, &result)
	ok.MustNoError(t, err)
	return result
}

func TestDocumentHighlight(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		file           string
		position       protocol.Position
		expectedRanges []protocol.Range // Exact ranges expected
		description    string
	}{
		// Top-level variable highlights - simple case
		{
			name:     "simple_variable_highlights",
			file:     "testdata/document_highlight/top_level_simple.cel",
			position: protocol.Position{Line: 0, Character: 0},
			expectedRanges: []protocol.Range{
				{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 1}},
				{Start: protocol.Position{Line: 0, Character: 4}, End: protocol.Position{Line: 0, Character: 5}},
				{Start: protocol.Position{Line: 0, Character: 8}, End: protocol.Position{Line: 0, Character: 9}},
			},
			description: "Highlight all occurrences of simple variable (x + x + x)",
		},

		// Multi-byte characters before a multi-character identifier; see the
		// matching case in TestReferences.
		{
			name:     "multibyte_before_ident",
			file:     "testdata/document_highlight/multibyte_before_ident.cel",
			position: protocol.Position{Line: 0, Character: 7},
			expectedRanges: []protocol.Range{
				{Start: protocol.Position{Line: 0, Character: 7}, End: protocol.Position{Line: 0, Character: 9}},
				{Start: protocol.Position{Line: 0, Character: 12}, End: protocol.Position{Line: 0, Character: 14}},
			},
			description: "Highlight xy in ('éé' + xy + xy)",
		},

		// Loop variable highlights
		{
			name:     "map_loop_var_highlights",
			file:     "testdata/document_highlight/map_loop_var.cel",
			position: protocol.Position{Line: 0, Character: 14},
			expectedRanges: []protocol.Range{
				{Start: protocol.Position{Line: 0, Character: 14}, End: protocol.Position{Line: 0, Character: 15}},
				{Start: protocol.Position{Line: 0, Character: 17}, End: protocol.Position{Line: 0, Character: 18}},
			},
			description: "Highlight loop variable in map ([1, 2, 3].map(x, x * 2))",
		},

		{
			name:     "exists_loop_var_highlights",
			file:     "testdata/document_highlight/exists_loop_var.cel",
			position: protocol.Position{Line: 0, Character: 17},
			expectedRanges: []protocol.Range{
				{Start: protocol.Position{Line: 0, Character: 17}, End: protocol.Position{Line: 0, Character: 18}},
				{Start: protocol.Position{Line: 0, Character: 20}, End: protocol.Position{Line: 0, Character: 21}},
			},
			description: "Highlight loop variable in exists ([1, 2, 3].exists(x, x == 2))",
		},

		// Emoji test - exact position validation
		{
			name:     "emoji_context",
			file:     "testdata/document_highlight/ascii_near_emoji.cel",
			position: protocol.Position{Line: 0, Character: 0},
			expectedRanges: []protocol.Range{
				{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 1}},
				{Start: protocol.Position{Line: 0, Character: 11}, End: protocol.Position{Line: 0, Character: 12}},
			},
			description: "Highlights with emoji nearby (x + \"🎉\" + x) - validates UTF-16 handling",
		},

		// Empty file
		{
			name:           "empty_file",
			file:           "testdata/document_highlight/empty.cel",
			position:       protocol.Position{Line: 0, Character: 0},
			expectedRanges: []protocol.Range{},
			description:    "Empty file returns no highlights",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testPath := getAbsPath(t, tc.file)
			conn, uri := setupLSPServer(t, testPath)

			highlights := requestDocumentHighlight(t, conn, uri, tc.position)

			// Verify exact count. Bail out on a mismatch: the position
			// checks below index into both slices.
			if !ok.Equal(t, len(highlights), len(tc.expectedRanges)) {
				return
			}

			// Sort both slices for comparison (order not guaranteed)
			actualRanges := make([]protocol.Range, len(highlights))
			for i, hl := range highlights {
				actualRanges[i] = hl.Range
			}
			sortRanges(actualRanges)
			sortRanges(tc.expectedRanges)

			// Verify exact positions
			for i, expected := range tc.expectedRanges {
				ok.Equal(t, actualRanges[i].Start.Line, expected.Start.Line)
				ok.Equal(t, actualRanges[i].Start.Character, expected.Start.Character)
				ok.Equal(t, actualRanges[i].End.Line, expected.End.Line)
				ok.Equal(t, actualRanges[i].End.Character, expected.End.Character)
			}
		})
	}
}

func sortRangesDoc(ranges []protocol.Range) {
	for i := range ranges {
		for j := i + 1; j < len(ranges); j++ {
			if isRangeAfterDoc(ranges[i], ranges[j]) {
				ranges[i], ranges[j] = ranges[j], ranges[i]
			}
		}
	}
}

func isRangeAfterDoc(a, b protocol.Range) bool {
	if a.Start.Line != b.Start.Line {
		return a.Start.Line > b.Start.Line
	}
	return a.Start.Character > b.Start.Character
}

func TestDocumentHighlightConsistency(t *testing.T) {
	t.Parallel()

	// Verify that requesting highlights from different occurrences
	// of the same variable returns consistent results (same positions, possibly different order)
	testPath := getAbsPath(t, "testdata/document_highlight/top_level_simple.cel")
	conn, uri := setupLSPServer(t, testPath)

	// Request highlights from first occurrence (position 0)
	highlights1 := requestDocumentHighlight(t, conn, uri, protocol.Position{Line: 0, Character: 0})

	// Request highlights from middle occurrence (position 4)
	highlights2 := requestDocumentHighlight(t, conn, uri, protocol.Position{Line: 0, Character: 4})

	// Request highlights from last occurrence (position 8)
	highlights3 := requestDocumentHighlight(t, conn, uri, protocol.Position{Line: 0, Character: 8})

	// All should return the same count
	ok.Equal(t, len(highlights1), 3)
	ok.Equal(t, len(highlights2), 3)
	ok.Equal(t, len(highlights3), 3)

	// Collect ranges from all three requests
	ranges1 := extractHighlightRanges(highlights1)
	ranges2 := extractHighlightRanges(highlights2)
	ranges3 := extractHighlightRanges(highlights3)

	// Sort all for comparison
	sortRangesDoc(ranges1)
	sortRangesDoc(ranges2)
	sortRangesDoc(ranges3)

	// All three should find the same set of ranges
	expectedRanges := []protocol.Range{
		{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 1}},
		{Start: protocol.Position{Line: 0, Character: 4}, End: protocol.Position{Line: 0, Character: 5}},
		{Start: protocol.Position{Line: 0, Character: 8}, End: protocol.Position{Line: 0, Character: 9}},
	}

	for i, expected := range expectedRanges {
		ok.Equal(t, ranges1[i].Start.Line, expected.Start.Line)
		ok.Equal(t, ranges1[i].Start.Character, expected.Start.Character)
		ok.Equal(t, ranges2[i].Start.Line, expected.Start.Line)
		ok.Equal(t, ranges2[i].Start.Character, expected.Start.Character)
		ok.Equal(t, ranges3[i].Start.Line, expected.Start.Line)
		ok.Equal(t, ranges3[i].Start.Character, expected.Start.Character)
	}
}

func extractHighlightRanges(highlights []protocol.DocumentHighlight) []protocol.Range {
	ranges := make([]protocol.Range, len(highlights))
	for i, hl := range highlights {
		ranges[i] = hl.Range
	}
	return ranges
}
