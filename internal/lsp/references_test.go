package lsp_test

import (
	"testing"

	"github.com/nalgeon/be"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// requestReferences sends a textDocument/references request at the given position.
func requestReferences(t *testing.T, conn *jsonrpc2.Conn, uri protocol.DocumentURI, pos protocol.Position) []protocol.Location {
	t.Helper()
	var result []protocol.Location
	err := conn.Call(t.Context(), "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": pos.Line, "character": pos.Character},
		"context":      map[string]any{"includeDeclaration": true},
	}, &result)
	be.Err(t, err, nil)
	return result
}

func TestReferences(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		file           string
		position       protocol.Position
		expectedRanges []protocol.Range // Exact ranges expected
		description    string
	}{
		// Top-level variable references - simple case
		{
			name:     "simple_variable_references",
			file:     "testdata/references/top_level_simple.cel",
			position: protocol.Position{Line: 0, Character: 0},
			expectedRanges: []protocol.Range{
				{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 1}},
				{Start: protocol.Position{Line: 0, Character: 4}, End: protocol.Position{Line: 0, Character: 5}},
				{Start: protocol.Position{Line: 0, Character: 8}, End: protocol.Position{Line: 0, Character: 9}},
			},
			description: "Find all references to simple variable (x + x + x)",
		},

		// Emoji test - exact position validation
		{
			name:     "emoji_context",
			file:     "testdata/references/ascii_near_emoji.cel",
			position: protocol.Position{Line: 0, Character: 0},
			expectedRanges: []protocol.Range{
				{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 1}},
				{Start: protocol.Position{Line: 0, Character: 11}, End: protocol.Position{Line: 0, Character: 12}},
			},
			description: "References with emoji nearby (x + \"🎉\" + x) - validates UTF-16 handling",
		},

		// Loop variable test
		{
			name:     "map_loop_var_references",
			file:     "testdata/references/map_loop_var.cel",
			position: protocol.Position{Line: 0, Character: 14},
			expectedRanges: []protocol.Range{
				{Start: protocol.Position{Line: 0, Character: 14}, End: protocol.Position{Line: 0, Character: 15}},
				{Start: protocol.Position{Line: 0, Character: 17}, End: protocol.Position{Line: 0, Character: 18}},
			},
			description: "Find references to loop variable in map",
		},

		// Empty file
		{
			name:           "empty_file",
			file:           "testdata/references/empty.cel",
			position:       protocol.Position{Line: 0, Character: 0},
			expectedRanges: []protocol.Range{},
			description:    "Empty file returns no references",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testPath := getAbsPath(t, tc.file)
			conn, uri := setupLSPServer(t, testPath)

			refs := requestReferences(t, conn, uri, tc.position)

			// Verify exact count
			be.Equal(t, len(refs), len(tc.expectedRanges))

			// Verify all references point to the same document
			for _, ref := range refs {
				be.Equal(t, ref.URI, uri)
			}

			// Sort both slices for comparison (order not guaranteed)
			actualRanges := make([]protocol.Range, len(refs))
			for i, ref := range refs {
				actualRanges[i] = ref.Range
			}
			sortRanges(actualRanges)
			sortRanges(tc.expectedRanges)

			// Verify exact positions
			for i, expected := range tc.expectedRanges {
				be.Equal(t, actualRanges[i].Start.Line, expected.Start.Line)
				be.Equal(t, actualRanges[i].Start.Character, expected.Start.Character)
				be.Equal(t, actualRanges[i].End.Line, expected.End.Line)
				be.Equal(t, actualRanges[i].End.Character, expected.End.Character)
			}
		})
	}
}

func sortRanges(ranges []protocol.Range) {
	for i := range ranges {
		for j := i + 1; j < len(ranges); j++ {
			if isRangeAfter(ranges[i], ranges[j]) {
				ranges[i], ranges[j] = ranges[j], ranges[i]
			}
		}
	}
}

func isRangeAfter(a, b protocol.Range) bool {
	if a.Start.Line != b.Start.Line {
		return a.Start.Line > b.Start.Line
	}
	return a.Start.Character > b.Start.Character
}

func TestReferencesConsistency(t *testing.T) {
	t.Parallel()

	// Verify that requesting references from different occurrences
	// of the same variable returns consistent results (same positions, possibly different order)
	testPath := getAbsPath(t, "testdata/references/top_level_simple.cel")
	conn, uri := setupLSPServer(t, testPath)

	// Request references from first occurrence (position 0)
	refs1 := requestReferences(t, conn, uri, protocol.Position{Line: 0, Character: 0})

	// Request references from middle occurrence (position 4)
	refs2 := requestReferences(t, conn, uri, protocol.Position{Line: 0, Character: 4})

	// Request references from last occurrence (position 8)
	refs3 := requestReferences(t, conn, uri, protocol.Position{Line: 0, Character: 8})

	// All should return the same count
	be.Equal(t, len(refs1), 3)
	be.Equal(t, len(refs2), 3)
	be.Equal(t, len(refs3), 3)

	// Collect ranges from all three requests
	ranges1 := extractRanges(refs1)
	ranges2 := extractRanges(refs2)
	ranges3 := extractRanges(refs3)

	// Sort all for comparison
	sortRanges(ranges1)
	sortRanges(ranges2)
	sortRanges(ranges3)

	// All three should find the same set of ranges
	expectedRanges := []protocol.Range{
		{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 1}},
		{Start: protocol.Position{Line: 0, Character: 4}, End: protocol.Position{Line: 0, Character: 5}},
		{Start: protocol.Position{Line: 0, Character: 8}, End: protocol.Position{Line: 0, Character: 9}},
	}

	for i, expected := range expectedRanges {
		be.Equal(t, ranges1[i].Start.Line, expected.Start.Line)
		be.Equal(t, ranges1[i].Start.Character, expected.Start.Character)
		be.Equal(t, ranges2[i].Start.Line, expected.Start.Line)
		be.Equal(t, ranges2[i].Start.Character, expected.Start.Character)
		be.Equal(t, ranges3[i].Start.Line, expected.Start.Line)
		be.Equal(t, ranges3[i].Start.Character, expected.Start.Character)
	}
}

func extractRanges(locations []protocol.Location) []protocol.Range {
	ranges := make([]protocol.Range, len(locations))
	for i, loc := range locations {
		ranges[i] = loc.Range
	}
	return ranges
}
