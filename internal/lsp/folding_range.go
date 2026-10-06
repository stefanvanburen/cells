package lsp

import (
	"cmp"
	"context"
	"slices"

	"cel.dev/cel-go/parser/gen"
	"go.lsp.dev/protocol"
)

func (s *server) FoldingRanges(ctx context.Context, params *protocol.FoldingRangeParams) ([]protocol.FoldingRange, error) {
	f, _ := s.document(ctx, params.TextDocument.URI)
	if f == nil {
		return nil, nil
	}
	return computeFoldingRanges(f.content), nil
}

// computeFoldingRanges returns the foldable regions of content: whatever a
// pair of brackets holds — a call's arguments, a macro, a list, map or message,
// a parenthesized expression — when it spans lines, along with runs of comment
// lines and triple-quoted strings. Every one of those shows in the tokens, so
// the content need not parse.
//
// A closing bracket that starts its line, or follows only other closing
// brackets there, stays visible as the end of the fold.
// Of several regions starting on the same line, the largest is kept, since
// that line can only fold one way.
func computeFoldingRanges(content string) []protocol.FoldingRange {
	t := newSourceTokens(content, nil)
	lineStarts := []int{0}
	for i := range len(content) {
		if content[i] == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}
	line := func(offset int) uint32 {
		l, found := slices.BinarySearch(lineStarts, offset)
		if !found {
			l--
		}
		return uint32(l)
	}
	// startsLine reports whether the token at i is the first on its line.
	startsLine := func(i int) bool {
		return i == 0 || line(t.tokens[i-1].end) < line(t.tokens[i].start)
	}
	// closesLine reports whether the closing bracket at i begins its line, or
	// follows only other closing brackets there.
	closesLine := func(i int) bool {
		for i > 0 && isClosingBracket(t.kind(i-1)) && line(t.tokens[i-1].start) == line(t.tokens[i].start) {
			i--
		}
		return startsLine(i)
	}

	var ranges []protocol.FoldingRange
	add := func(start, end uint32, kind protocol.FoldingRangeKind) {
		if end > start {
			ranges = append(ranges, protocol.FoldingRange{StartLine: start, EndLine: end, Kind: kind})
		}
	}

	for open, closing := range t.brackets() {
		if open > closing {
			continue
		}
		end := line(t.tokens[closing].start)
		if closesLine(closing) {
			end--
		}
		add(line(t.tokens[open].start), end, protocol.FoldingRangeKindRegion)
	}

	for i := 0; i < len(t.tokens); i++ {
		tok := t.tokens[i]
		switch tok.kind {
		case gen.CELLexerSTRING, gen.CELLexerBYTES:
			add(line(tok.start), line(tok.end), protocol.FoldingRangeKindRegion)
		case gen.CELLexerCOMMENT:
			if !startsLine(i) {
				continue
			}
			// Extend over the comments on the lines that follow.
			last := i
			for last+1 < len(t.tokens) && t.tokens[last+1].kind == gen.CELLexerCOMMENT &&
				line(t.tokens[last+1].start) == line(t.tokens[last].start)+1 {
				last++
			}
			add(line(tok.start), line(t.tokens[last].start), protocol.FoldingRangeKindComment)
			i = last
		}
	}

	slices.SortFunc(ranges, func(a, b protocol.FoldingRange) int {
		return cmp.Or(cmp.Compare(a.StartLine, b.StartLine), cmp.Compare(b.EndLine, a.EndLine))
	})
	return slices.CompactFunc(ranges, func(a, b protocol.FoldingRange) bool {
		return a.StartLine == b.StartLine
	})
}

func isClosingBracket(kind int) bool {
	return kind == gen.CELLexerRPAREN || kind == gen.CELLexerRPRACKET || kind == gen.CELLexerRBRACE
}
