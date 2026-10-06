package lsp

import (
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/parser/gen"
	"github.com/antlr4-go/antlr/v4"
)

// celToken is a token as cel-go's lexer produces it, with the byte range of its
// text and the rune offset the parser's source positions are measured in.
type celToken struct {
	kind       int
	start, end int
	runeStart  int32
}

// lexCEL returns the tokens of content, comments included and whitespace left
// out. Lexing does not need content to parse.
func lexCEL(content string) []celToken {
	// ANTLR measures positions in runes; record the byte offset of each.
	runeBytes := make([]int, 0, len(content)+1)
	for i := range content {
		runeBytes = append(runeBytes, i)
	}
	runeBytes = append(runeBytes, len(content))

	lexer := gen.NewCELLexer(antlr.NewInputStream(content))
	lexer.RemoveErrorListeners()
	var tokens []celToken
	for {
		t := lexer.NextToken()
		if t.GetTokenType() == antlr.TokenEOF {
			return tokens
		}
		if t.GetTokenType() == gen.CELLexerWHITESPACE {
			continue
		}
		tokens = append(tokens, celToken{
			kind:      t.GetTokenType(),
			start:     runeBytes[t.GetStart()],
			end:       runeBytes[t.GetStop()+1],
			runeStart: int32(t.GetStart()),
		})
	}
}

// sourceTokens is the lexed form of a source, which finds the token an
// expression's recorded position points at: a name, an operator, or the
// bracket or dot a call, literal or selection is recorded at.
type sourceTokens struct {
	content string
	tokens  []celToken
	// at maps a rune offset to the token starting there.
	at map[int32]int
	// sourceInfo holds the recorded positions, and is nil if the source does
	// not parse.
	sourceInfo *ast.SourceInfo
}

func newSourceTokens(content string, sourceInfo *ast.SourceInfo) *sourceTokens {
	t := &sourceTokens{
		content:    content,
		tokens:     lexCEL(content),
		at:         make(map[int32]int),
		sourceInfo: sourceInfo,
	}
	for i, tok := range t.tokens {
		t.at[tok.runeStart] = i
	}
	return t
}

// anchor returns the index of the token at the recorded position of the
// expression with the given ID, or -1.
func (c *sourceTokens) anchor(id int64) int {
	r, ok := c.sourceInfo.GetOffsetRange(id)
	if !ok {
		return -1
	}
	if i, ok := c.at[r.Start]; ok {
		return i
	}
	return -1
}

// kind returns the kind of the token at index i, or 0 if there is none.
func (c *sourceTokens) kind(i int) int {
	if i < 0 || i >= len(c.tokens) {
		return 0
	}
	return c.tokens[i].kind
}

// step returns the index of the nearest token in direction dir (1 or -1) from
// i that is not a comment, or -1.
func (c *sourceTokens) step(i, dir int) int {
	for i += dir; i >= 0 && i < len(c.tokens); i += dir {
		if c.tokens[i].kind != gen.CELLexerCOMMENT {
			return i
		}
	}
	return -1
}

// text returns the source text of the token at index i.
func (c *sourceTokens) text(i int) string {
	return c.content[c.tokens[i].start:c.tokens[i].end]
}

func (c *sourceTokens) isName(i int) bool {
	k := c.kind(i)
	return k == gen.CELLexerIDENTIFIER || k == gen.CELLexerESC_IDENTIFIER
}

// brackets maps the index of each bracket, brace and parenthesis to the index
// of its partner, for those that have one.
func (c *sourceTokens) brackets() map[int]int {
	partners := make(map[int]int)
	var open []int
	for i, t := range c.tokens {
		switch t.kind {
		case gen.CELLexerLPAREN, gen.CELLexerLBRACKET, gen.CELLexerLBRACE:
			open = append(open, i)
		case gen.CELLexerRPAREN, gen.CELLexerRPRACKET, gen.CELLexerRBRACE:
			if n := len(open); n > 0 {
				partners[open[n-1]], partners[i] = i, open[n-1]
				open = open[:n-1]
			}
		}
	}
	return partners
}
