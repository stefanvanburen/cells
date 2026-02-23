package lsp_test

import (
	"slices"
	"testing"

	"github.com/nalgeon/be"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// semanticToken represents a decoded semantic token for easier testing.
type semanticToken struct {
	line      uint32
	startChar uint32
	length    uint32
	tokenType uint32
}

// decodeSemanticTokens converts the delta-encoded token array into absolute positions.
func decodeSemanticTokens(data []uint32) []semanticToken {
	var tokens []semanticToken
	var line, startChar uint32

	for i := 0; i < len(data); i += 5 {
		deltaLine := data[i]
		deltaStartChar := data[i+1]
		length := data[i+2]
		tokenType := data[i+3]

		line += deltaLine
		if deltaLine != 0 {
			startChar = deltaStartChar
		} else {
			startChar += deltaStartChar
		}

		tokens = append(tokens, semanticToken{
			line:      line,
			startChar: startChar,
			length:    length,
			tokenType: tokenType,
		})
	}
	return tokens
}

// findToken searches for a token at the specified position with the given type.
func findToken(tokens []semanticToken, line, startChar, length, tokenType uint32) bool {
	return slices.ContainsFunc(tokens, func(token semanticToken) bool {
		return token.line == line && token.startChar == startChar &&
			token.length == length && token.tokenType == tokenType
	})
}

// Semantic token types - must match semantic_tokens.go constants.
const (
	stProperty   = 0
	stStruct     = 1
	stVariable   = 2
	stEnum       = 3
	stEnumMember = 4
	stInterface  = 5
	stMethod     = 6
	stFunction   = 7
	stDecorator  = 8
	stMacro      = 9
	stNamespace  = 10
	stKeyword    = 11
	stModifier   = 12
	stComment    = 13
	stString     = 14
	stNumber     = 15
	stType       = 16
	stOperator   = 17
)

// expectedToken represents an expected semantic token at a specific position.
type expectedToken struct {
	line      uint32
	startChar uint32
	length    uint32
	tokenType uint32
	desc      string
}

func getSemanticTokens(t *testing.T, celFile string) []semanticToken {
	t.Helper()
	ctx := t.Context()
	testPath := getAbsPath(t, celFile)
	clientConn, testURI := setupLSPServer(t, testPath)

	var result *protocol.SemanticTokens
	err := clientConn.Call(ctx, "textDocument/semanticTokens/full", protocol.SemanticTokensParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: testURI,
		},
	}, &result)
	be.Err(t, err, nil)
	be.True(t, result != nil)
	be.True(t, len(result.Data) > 0)
	return decodeSemanticTokens(result.Data)
}

func getNilSemanticTokens(t *testing.T, celFile string) {
	t.Helper()
	ctx := t.Context()
	testPath := getAbsPath(t, celFile)
	clientConn, testURI := setupLSPServer(t, testPath)

	var result *protocol.SemanticTokens
	err := clientConn.Call(ctx, "textDocument/semanticTokens/full", protocol.SemanticTokensParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: testURI,
		},
	}, &result)
	be.Err(t, err, nil)
	be.True(t, result == nil)
}

func assertTokens(t *testing.T, tokens []semanticToken, expected []expectedToken) {
	t.Helper()
	for _, exp := range expected {
		be.True(t, findToken(tokens, exp.line, exp.startChar, exp.length, exp.tokenType))
	}
}

func TestSemanticTokensBasic(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/basic.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 1, stProperty, "'x' property"},
		{0, 2, 1, stOperator, "'>' operator"},
		{0, 4, 1, stNumber, "'0' number"},
		{0, 6, 2, stOperator, "'&&' operator"},
		{0, 9, 1, stProperty, "'x' property (2nd)"},
		{0, 11, 4, stMethod, "'size' method"},
		{0, 18, 1, stOperator, "'<' operator"},
		{0, 20, 3, stNumber, "'100' number"},
	})
}

func TestSemanticTokensKeywords(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/keywords.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 4, stKeyword, "'true' keyword"},
		{0, 5, 2, stOperator, "'&&' operator"},
		{0, 8, 5, stKeyword, "'false' keyword"},
		{0, 14, 2, stOperator, "'||' operator"},
		{0, 17, 4, stKeyword, "'null' keyword"},
		{0, 22, 2, stOperator, "'==' operator"},
		{0, 25, 1, stProperty, "'x' property"},
	})
}

func TestSemanticTokensFunctions(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/functions.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 3, stType, "'int' type conversion"},
		{0, 4, 1, stProperty, "'x' in int()"},
		{0, 7, 1, stOperator, "'>' operator"},
		{0, 9, 1, stNumber, "'0' number"},
		{0, 11, 2, stOperator, "'&&' operator"},
		{0, 14, 6, stType, "'string' type conversion"},
		{0, 21, 1, stProperty, "'y' in string()"},
		{0, 24, 2, stOperator, "'==' operator"},
		{0, 27, 7, stString, "'hello' string"},
		{0, 35, 2, stOperator, "'&&' operator (2nd)"},
		{0, 38, 1, stProperty, "'x' before startsWith"},
		{0, 40, 10, stMethod, "'startsWith' method"},
		{0, 51, 5, stString, "'foo' string"},
	})
}

func TestSemanticTokensMacros(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/macros.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 3, stMacro, "'has' macro"},
		{0, 4, 1, stProperty, "'x' in has()"},
		{0, 6, 5, stProperty, "'field' property"},
		{0, 13, 2, stOperator, "'&&' operator"},
		{0, 17, 1, stNumber, "'1' number"},
		{0, 20, 1, stNumber, "'2' number"},
		{0, 23, 1, stNumber, "'3' number"},
		{0, 26, 3, stMacro, "'all' macro"},
		{0, 40, 2, stOperator, "'&&' operator (2nd)"},
		{0, 43, 1, stProperty, "'x' before exists"},
		{0, 45, 6, stMacro, "'exists' macro"},
	})
}

func TestSemanticTokensTernary(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/ternary.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 1, stProperty, "'x' property"},
		{0, 2, 1, stOperator, "'>' operator"},
		{0, 4, 1, stNumber, "'0' number"},
		{0, 6, 1, stOperator, "'?' ternary operator"},
		{0, 8, 10, stString, "'positive' string"},
		{0, 21, 14, stString, "'non-positive' string"},
	})
}

func TestSemanticTokensMapLiteral(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/map_literal.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 1, 5, stString, "'key' string key"},
		{0, 8, 7, stString, "'value' string value"},
		{0, 17, 5, stString, "'num' string key"},
		{0, 24, 2, stNumber, "42 number"},
	})
}

func TestSemanticTokensNegation(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/negation.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 1, stOperator, "'!' negation operator"},
		{0, 1, 1, stProperty, "'x' property"},
		{0, 3, 2, stOperator, "'&&' operator"},
		{0, 6, 1, stOperator, "'!' negation operator (2nd)"},
		{0, 8, 1, stProperty, "'y' property"},
		{0, 10, 1, stOperator, "'>' operator"},
		{0, 12, 1, stNumber, "'0' number"},
	})
}

func TestSemanticTokensUnaryMinus(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/unary_minus.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 1, stOperator, "'-' unary minus operator"},
		{0, 1, 1, stProperty, "'x' property"},
		{0, 3, 1, stOperator, "'+' operator"},
		{0, 5, 2, stNumber, "'-3' negative number literal"},
	})
}

func TestSemanticTokensIndex(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/index.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 4, stProperty, "'list' property"},
		{0, 5, 1, stNumber, "'0' number"},
		{0, 8, 1, stOperator, "'+' operator"},
		{0, 10, 7, stProperty, "'map_var' property"},
		{0, 18, 5, stString, "'key' string"},
	})
}

func TestSemanticTokensNumericTypes(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/numeric_types.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 4, stNumber, "3.14 double literal"},
		{0, 5, 1, stOperator, "'+' operator"},
		{0, 7, 5, stNumber, "0.5e2 double literal"},
		{0, 13, 1, stOperator, "'+' operator (2nd)"},
		{0, 15, 3, stNumber, "42u uint literal"},
		{0, 19, 1, stOperator, "'+' operator (3rd)"},
		{0, 21, 2, stNumber, "0u uint literal"},
	})
}

func TestSemanticTokensBytes(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/bytes.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 8, stString, `b"hello" bytes literal`},
	})
}

func TestSemanticTokensMultiline(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/multiline.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 1, stProperty, "'x' property"},
		{0, 2, 1, stOperator, "'>' operator"},
		{0, 4, 1, stNumber, "'0' number"},
		{0, 6, 2, stOperator, "'&&' operator"},
		{1, 0, 1, stProperty, "'y' property"},
		{1, 2, 1, stOperator, "'<' operator"},
		{1, 4, 2, stNumber, "'10' number"},
		{1, 7, 2, stOperator, "'&&' operator (2nd)"},
		{2, 0, 1, stProperty, "'z' property"},
		{2, 2, 2, stOperator, "'==' operator"},
		{2, 5, 7, stString, "'hello' string"},
	})
}

func TestSemanticTokensNestedSelect(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/nested_select.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 1, stProperty, "'a' property"},
		{0, 2, 1, stProperty, "'b' property"},
		{0, 4, 1, stProperty, "'c' property"},
		{0, 6, 1, stProperty, "'d' property"},
	})
}

func TestSemanticTokensChainedMethods(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/chained_methods.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 1, stProperty, "'x' property"},
		{0, 2, 4, stMethod, "'trim' method"},
		{0, 9, 4, stMethod, "'size' method"},
	})
}

func TestSemanticTokensNestedMacros(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/nested_macros.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 1, 1, stNumber, "'1' number"},
		{0, 4, 1, stNumber, "'2' number"},
		{0, 7, 1, stNumber, "'3' number"},
		{0, 10, 6, stMacro, "'filter' macro"},
		{0, 27, 3, stMacro, "'all' macro"},
	})
}

func TestSemanticTokensInOperator(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/in_operator.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 3, stString, "'a' string"},
		{0, 4, 2, stOperator, "'in' operator"},
		{0, 8, 3, stString, "'a' string in list"},
		{0, 13, 3, stString, "'b' string in list"},
		{0, 18, 3, stString, "'c' string in list"},
	})
}

func TestSemanticTokensExistsOne(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/exists_one.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 1, 1, stNumber, "'1' number"},
		{0, 4, 1, stNumber, "'2' number"},
		{0, 7, 1, stNumber, "'3' number"},
		{0, 10, 10, stMacro, "'exists_one' macro"},
	})
}

func TestSemanticTokensStandaloneFunc(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/standalone_func.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 4, stFunction, "'size' standalone function"},
		{0, 5, 1, stProperty, "'x' property"},
		{0, 8, 1, stOperator, "'+' operator"},
		{0, 10, 4, stFunction, "'size' standalone function (2nd)"},
		{0, 15, 1, stProperty, "'y' property"},
	})
}

func TestSemanticTokensParseError(t *testing.T) {
	t.Parallel()
	getNilSemanticTokens(t, "testdata/semantic_tokens/parse_error.cel")
}

func TestSemanticTokensEmpty(t *testing.T) {
	t.Parallel()
	getNilSemanticTokens(t, "testdata/semantic_tokens/empty.cel")
}

func TestEdgeUnicodeString(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/unicode_string.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 7, stString, `"héllo" string (7 UTF-16 units)`},
		{0, 8, 1, stOperator, "'+' operator"},
		{0, 10, 5, stString, `"日本語" string (5 UTF-16 units)`},
	})
}

func TestEdgeRawString(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/raw_string.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 22, stString, "raw string"},
	})
}

func TestEdgeTripleQuoted(t *testing.T) {
	t.Parallel()
	// Triple-quoted string spans 2 lines; token starts at (0,0) with
	// UTF-16 length covering both lines including the newline.
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/triple_quoted.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 17, stString, `"""hello\nworld""" triple-quoted string`},
	})
}

func TestEdgeDoubleQuoted(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/double_quoted.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 7, stString, `"hello" double-quoted`},
		{0, 8, 1, stOperator, "'+' operator"},
		{0, 10, 7, stString, `"world" double-quoted`},
	})
}

func TestEdgeEscapedString(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/escaped_string.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 14, stString, `"hello\nworld" escaped string`},
	})
}

func TestEdgeMapMacro(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/map_macro.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 1, 1, stNumber, "'1' number"},
		{0, 4, 1, stNumber, "'2' number"},
		{0, 7, 1, stNumber, "'3' number"},
		{0, 10, 3, stMacro, "'map' macro"},
	})
}

func TestEdgeNestedTernary(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/nested_ternary.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 1, stProperty, "'x' property"},
		{0, 2, 1, stOperator, "'>' operator"},
		{0, 4, 1, stNumber, "'0' number"},
		{0, 6, 1, stOperator, "'?' ternary"},
		{0, 8, 5, stString, `"pos" string`},
		{0, 16, 1, stProperty, "'x' property (2nd)"},
		{0, 18, 1, stOperator, "'<' operator"},
		{0, 20, 1, stNumber, "'0' number"},
		{0, 22, 1, stOperator, "'?' ternary (2nd)"},
		{0, 24, 5, stString, `"neg" string`},
		{0, 32, 6, stString, `"zero" string`},
	})
}

func TestEdgeEmptyCollections(t *testing.T) {
	t.Parallel()
	// [] + {} == x — empty list and map produce no tokens themselves
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/empty_collections.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 3, 1, stOperator, "'+' operator"},
		{0, 8, 2, stOperator, "'==' operator"},
		{0, 11, 1, stProperty, "'x' property"},
	})
}

func TestEdgeNestedCalls(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/nested_calls.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 4, stFunction, "'size' function"},
		{0, 5, 6, stType, "'string' type conversion"},
		{0, 12, 1, stProperty, "'x' property"},
	})
}

func TestEdgeHex(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/hex.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 4, stNumber, "0xFF hex"},
		{0, 5, 1, stOperator, "'+' operator"},
		{0, 7, 4, stNumber, "0x10 hex"},
	})
}

func TestEdgeWhitespace(t *testing.T) {
	t.Parallel()
	getNilSemanticTokens(t, "testdata/semantic_tokens/whitespace.cel")
}

func TestEdgeMoreOperators(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/more_operators.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 1, stProperty, "'x' property"},
		{0, 2, 2, stOperator, "'!=' operator"},
		{0, 5, 1, stProperty, "'y' property"},
		{0, 7, 2, stOperator, "'&&' operator"},
		{0, 10, 1, stProperty, "'a' property"},
		{0, 12, 2, stOperator, "'>=' operator"},
		{0, 15, 1, stProperty, "'b' property"},
		{0, 17, 2, stOperator, "'&&' operator (2nd)"},
		{0, 20, 1, stProperty, "'c' property"},
		{0, 22, 2, stOperator, "'<=' operator"},
		{0, 25, 1, stProperty, "'d' property"},
		{0, 27, 2, stOperator, "'&&' operator (3rd)"},
		{0, 30, 1, stProperty, "'e' property"},
		{0, 32, 1, stOperator, "'%' operator"},
		{0, 34, 1, stProperty, "'f' property"},
		{0, 36, 2, stOperator, "'==' operator"},
		{0, 39, 1, stNumber, "'0' number"},
	})
}

func TestEdgeParenthesized(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/parenthesized.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 1, 1, stProperty, "'x' property"},
		{0, 3, 1, stOperator, "'+' operator"},
		{0, 5, 1, stProperty, "'y' property"},
		{0, 8, 1, stOperator, "'*' operator"},
		{0, 10, 1, stProperty, "'z' property"},
	})
}

func TestEdgeSelectOnCall(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/select_on_call.cel")
	assertTokens(t, tokens, []expectedToken{
		{0, 0, 6, stType, "'string' type conversion"},
		{0, 7, 1, stProperty, "'x' property"},
		{0, 10, 4, stMethod, "'size' method"},
	})
}

// findTokenOnLine checks that at least one token of the given type exists on the given line.
func findTokenOnLine(tokens []semanticToken, line, tokenType uint32) bool {
	return slices.ContainsFunc(tokens, func(token semanticToken) bool {
		return token.line == line && token.tokenType == tokenType
	})
}

func TestSemanticTokensComprehensive(t *testing.T) {
	t.Parallel()
	tokens := getSemanticTokens(t, "testdata/semantic_tokens/comprehensive.cel")

	// The comprehensive file is 121 lines with comments and exercises every feature.
	// There should be a significant number of tokens.
	be.True(t, len(tokens) >= 80)

	// Spot-check specific token types on specific lines (0-indexed).
	// Line 7: (x + y) * 2 - z / w % 3 >= 10 &&
	be.True(t, findTokenOnLine(tokens, 7, stOperator))
	be.True(t, findTokenOnLine(tokens, 7, stNumber))

	// Line 10: "hello world".contains("world") &&
	be.True(t, findTokenOnLine(tokens, 10, stString))
	be.True(t, findTokenOnLine(tokens, 10, stMethod))

	// Line 11: "hello".startsWith("he") &&
	be.True(t, findTokenOnLine(tokens, 11, stMethod))

	// Line 14: size("test") == 4 &&
	be.True(t, findTokenOnLine(tokens, 14, stFunction))

	// Line 17: int("42") + double("3.14") > 0.0 &&
	be.True(t, findTokenOnLine(tokens, 17, stType))

	// Line 20: bool("true") == true &&
	be.True(t, findTokenOnLine(tokens, 20, stKeyword))

	// Line 33: has({"field": true}.field) &&
	be.True(t, findTokenOnLine(tokens, 33, stMacro))

	// Line 36: "a" in ["a", "b", "c"] &&
	be.True(t, findTokenOnLine(tokens, 36, stOperator))

	// Line 40: [1, 2, 3].all(i, i > 0) &&
	be.True(t, findTokenOnLine(tokens, 40, stMacro))

	// Line 43: [1, 2, 3].exists(i, i == 2) &&
	be.True(t, findTokenOnLine(tokens, 43, stMacro))

	// Line 46: [1, 2, 3].exists_one(i, i > 2) &&
	be.True(t, findTokenOnLine(tokens, 46, stMacro))

	// Line 49: [1, 2, 3].map(i, i * 2) == [2, 4, 6] &&
	be.True(t, findTokenOnLine(tokens, 49, stMacro))

	// Line 52: [1, 2, 3, 4, 5].filter(i, i > 3) == [4, 5] &&
	be.True(t, findTokenOnLine(tokens, 52, stMacro))

	// Line 55: nested macros
	be.True(t, findTokenOnLine(tokens, 55, stMacro))

	// Line 58: ternary
	be.True(t, findTokenOnLine(tokens, 58, stOperator))

	// Line 62: true || false &&
	be.True(t, findTokenOnLine(tokens, 62, stKeyword))

	// Line 73: !false &&
	be.True(t, findTokenOnLine(tokens, 73, stKeyword))

	// Line 81: null == null &&
	be.True(t, findTokenOnLine(tokens, 81, stKeyword))

	// Line 84: b"hello" == bytes("hello") &&
	be.True(t, findTokenOnLine(tokens, 84, stString))

	// Line 91: "Hello World".endsWith("World") &&
	be.True(t, findTokenOnLine(tokens, 91, stMethod))

	// Line 96: multi-line comprehension filter
	be.True(t, findTokenOnLine(tokens, 96, stMacro))

	// Line 102: duration("1h") > duration("30m") &&
	be.True(t, findTokenOnLine(tokens, 102, stType))

	// Line 103: timestamp type conversion
	be.True(t, findTokenOnLine(tokens, 103, stType))

	// Line 120: x + y == z
	be.True(t, findTokenOnLine(tokens, 120, stOperator))

	// Verify all major token types are present across the file.
	typesSeen := make(map[uint32]bool)
	for _, tok := range tokens {
		typesSeen[tok.tokenType] = true
	}
	for _, expected := range []uint32{stOperator, stNumber, stString, stMethod, stFunction, stType, stMacro, stKeyword, stVariable} {
		be.True(t, typesSeen[expected])
	}
}
