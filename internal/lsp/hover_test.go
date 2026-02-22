package lsp_test

import (
	"strings"
	"testing"

	"github.com/nalgeon/be"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// getHover sends a textDocument/hover request and returns the result.
func getHover(t *testing.T, celFile string, line, character uint32) *protocol.Hover {
	t.Helper()
	ctx := t.Context()
	testPath := getAbsPath(t, celFile)
	clientConn, testURI := setupLSPServer(t, testPath)

	var result *protocol.Hover
	err := clientConn.Call(ctx, "textDocument/hover", protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{
				URI: testURI,
			},
			Position: protocol.Position{
				Line:      line,
				Character: character,
			},
		},
	}, &result)
	be.Err(t, err, nil)
	return result
}

// requireHoverContains asserts that hover at (line, char) produces markdown containing substr.
func requireHoverContains(t *testing.T, celFile string, line, character uint32, substr string, desc string) {
	t.Helper()
	result := getHover(t, celFile, line, character)
	be.True(t, result != nil)
	be.Equal(t, result.Contents.Kind, protocol.Markdown)
	be.True(t, strings.Contains(result.Contents.Value, substr))
}

// requireNoHover asserts that hover at (line, char) returns nil.
func requireNoHover(t *testing.T, celFile string, line, character uint32, desc string) {
	t.Helper()
	result := getHover(t, celFile, line, character)
	be.True(t, result == nil)
}

func TestHoverOperators(t *testing.T) {
	t.Parallel()
	// x > 0 && y < 10
	// 0123456789012345
	f := "testdata/hover/operators.cel"

	// Upstream descriptions from cel-go
	requireHoverContains(t, f, 0, 2, "greater than", "'>' operator description")
	requireHoverContains(t, f, 0, 2, "**Operator**: `>`", "'>' operator header")
	requireHoverContains(t, f, 0, 2, "**Overloads**", "'>' has overloads")
	requireHoverContains(t, f, 0, 6, "logically AND", "'&&' operator")
	requireHoverContains(t, f, 0, 11, "less than", "'<' operator")

	// Hover on an identifier — no hover
	requireNoHover(t, f, 0, 0, "'x' identifier — no hover")
}

func TestHoverInOperator(t *testing.T) {
	t.Parallel()
	// "a" in ["a", "b", "c"]
	// 0123456789
	f := "testdata/hover/in_operator.cel"

	requireHoverContains(t, f, 0, 4, "exists in a list", "'in' operator")
	requireHoverContains(t, f, 0, 4, "**Operator**", "'in' has operator header")
}

func TestHoverLogicalOperators(t *testing.T) {
	t.Parallel()
	// !x && (y || z)
	// 01234567890123
	f := "testdata/hover/logical.cel"

	requireHoverContains(t, f, 0, 0, "negate", "'!' unary operator")
	requireHoverContains(t, f, 0, 3, "logically AND", "'&&' operator")
	requireHoverContains(t, f, 0, 9, "logically OR", "'||' operator")
}

func TestHoverArithmetic(t *testing.T) {
	t.Parallel()
	// -x + y * z / w % 2
	// 0         1
	// 0123456789012345678
	f := "testdata/hover/arithmetic.cel"

	requireHoverContains(t, f, 0, 0, "negate", "unary '-'")
	requireHoverContains(t, f, 0, 3, "adds two numeric", "'+'")
	requireHoverContains(t, f, 0, 7, "multiply", "'*'")
	requireHoverContains(t, f, 0, 11, "divide", "'/'")
	requireHoverContains(t, f, 0, 15, "modulus", "'%'")
}

func TestHoverMacros(t *testing.T) {
	t.Parallel()
	// [1, 2, 3].all(x, x > 0)
	f := "testdata/hover/macros.cel"

	requireHoverContains(t, f, 0, 10, "**Macro**: `all`", "'all' macro header")
	requireHoverContains(t, f, 0, 10, "all elements", "'all' macro description")
	requireHoverContains(t, f, 0, 10, "**Examples**", "'all' has examples")
}

func TestHoverHasMacro(t *testing.T) {
	t.Parallel()
	// has(msg.field)
	// 01234567890123
	f := "testdata/hover/has_macro.cel"

	requireHoverContains(t, f, 0, 0, "**Macro**: `has`", "'has' macro header")
	requireHoverContains(t, f, 0, 0, "presence of a field", "'has' description")
}

func TestHoverExistsOneMacro(t *testing.T) {
	t.Parallel()
	// [1, 2, 3].exists_one(x, x > 2)
	// 0         1         2         3
	// 0123456789012345678901234567890
	f := "testdata/hover/exists_one.cel"

	requireHoverContains(t, f, 0, 10, "exists_one", "'exists_one' macro")
	requireHoverContains(t, f, 0, 10, "exactly one", "'exists_one' description")
}

func TestHoverMapMacro(t *testing.T) {
	t.Parallel()
	// [1, 2, 3].map(x, x * 2)
	// 0         1
	// 0123456789012345678901234
	f := "testdata/hover/map_macro.cel"

	requireHoverContains(t, f, 0, 10, "**Macro**: `map`", "'map' macro header")
	requireHoverContains(t, f, 0, 10, "transform", "'map' description")
}

func TestHoverMoreMacros(t *testing.T) {
	t.Parallel()
	// [1, 2, 3].exists(x, x > 2) && [1, 2, 3].filter(y, y > 1).size() > 0
	// 0         1         2         3         4
	// 0123456789012345678901234567890123456789012345678901234567890
	f := "testdata/hover/more_macros.cel"

	requireHoverContains(t, f, 0, 10, "exists", "'exists' macro")
	requireHoverContains(t, f, 0, 10, "any value", "'exists' description")
	requireHoverContains(t, f, 0, 41, "filter", "'filter' macro")
	requireHoverContains(t, f, 0, 41, "satisfy the given predicate", "'filter' description")
}

func TestHoverFunctions(t *testing.T) {
	t.Parallel()
	// size("hello") + int("42")
	f := "testdata/hover/functions.cel"

	requireHoverContains(t, f, 0, 1, "size", "'size' function")
	requireHoverContains(t, f, 0, 1, "**Overloads**", "'size' has overloads")
	requireHoverContains(t, f, 0, 17, "int", "'int' type conversion")
	requireHoverContains(t, f, 0, 17, "convert a value to an int", "'int' description")
}

func TestHoverTypeConversions(t *testing.T) {
	t.Parallel()
	// string(42) + double("3.14")
	// 0         1         2
	// 0123456789012345678901234567
	f := "testdata/hover/type_conversions.cel"

	requireHoverContains(t, f, 0, 0, "convert a value to a string", "'string()' description")
	requireHoverContains(t, f, 0, 0, "**Type**", "'string' type header")
	requireHoverContains(t, f, 0, 13, "convert a value to a double", "'double()' description")
	requireHoverContains(t, f, 0, 13, "**Type**", "'double' type header")
}

func TestHoverMethods(t *testing.T) {
	t.Parallel()
	// "hello".startsWith("he") && "hello".contains("ell")
	f := "testdata/hover/methods.cel"

	requireHoverContains(t, f, 0, 8, "startsWith", "'startsWith' method")
	requireHoverContains(t, f, 0, 8, "prefix", "'startsWith' description")
	requireHoverContains(t, f, 0, 38, "contains", "'contains' method")
	requireHoverContains(t, f, 0, 38, "substring", "'contains' description")
}

func TestHoverMoreMethods(t *testing.T) {
	t.Parallel()
	// "hello world".endsWith("world") && "abc123".matches("[a-z]+[0-9]+")
	// 0         1         2         3         4         5         6
	// 0123456789012345678901234567890123456789012345678901234567890123456789
	f := "testdata/hover/more_methods.cel"

	requireHoverContains(t, f, 0, 14, "endsWith", "'endsWith' method")
	requireHoverContains(t, f, 0, 14, "suffix", "'endsWith' description")
	requireHoverContains(t, f, 0, 44, "matches", "'matches' method")
	requireHoverContains(t, f, 0, 44, "RE2", "'matches' description")
}

func TestHoverKeywords(t *testing.T) {
	t.Parallel()
	// true && !false || null == null
	// 0         1         2
	// 0123456789012345678901234567890
	f := "testdata/hover/keywords.cel"

	requireHoverContains(t, f, 0, 0, "true", "'true' keyword")
	requireHoverContains(t, f, 0, 9, "false", "'false' keyword")
	requireHoverContains(t, f, 0, 18, "null", "'null' keyword")
	requireHoverContains(t, f, 0, 18, "null_type", "'null' type info")
}

func TestHoverTernary(t *testing.T) {
	t.Parallel()
	// x > 0 ? "positive" : "non-positive"
	f := "testdata/hover/ternary.cel"

	requireHoverContains(t, f, 0, 6, "ternary", "'?' ternary operator")
	requireHoverContains(t, f, 0, 6, "**Operator**", "'?' operator header")
}

func TestHoverLiterals(t *testing.T) {
	t.Parallel()
	// "hello"
	f := "testdata/hover/literals.cel"

	requireNoHover(t, f, 0, 3, "string literal — no hover")
}

func TestHoverWhitespace(t *testing.T) {
	t.Parallel()
	f := "testdata/hover/operators.cel"

	requireNoHover(t, f, 0, 1, "whitespace between tokens — no hover")
}

func TestHoverNumberLiteral(t *testing.T) {
	t.Parallel()
	f := "testdata/hover/operators.cel"

	requireNoHover(t, f, 0, 4, "number literal '0' — no hover")
	requireNoHover(t, f, 0, 13, "number literal '10' — no hover")
}

func TestHoverMultiline(t *testing.T) {
	t.Parallel()
	// x == 1 &&
	// y != 2 &&
	// z >= 3 &&
	// w <= 4
	f := "testdata/hover/multiline.cel"

	requireHoverContains(t, f, 0, 2, "equality", "'==' on line 1")
	requireHoverContains(t, f, 1, 2, "inequality", "'!=' on line 2")
	requireHoverContains(t, f, 2, 2, "greater than or equal", "'>=' on line 3")
	requireHoverContains(t, f, 3, 2, "less than or equal", "'<=' on line 4")
}

func TestHoverUnknownFunction(t *testing.T) {
	t.Parallel()
	// unknown_func(x, y)
	f := "testdata/hover/unknown_func.cel"

	// Unknown functions are not in the environment, so no hover
	requireNoHover(t, f, 0, 0, "unknown function — no hover")
}

func TestHoverSelect(t *testing.T) {
	t.Parallel()
	// msg.field.nested
	f := "testdata/hover/select.cel"

	requireNoHover(t, f, 0, 0, "'msg' identifier — no hover")
	requireNoHover(t, f, 0, 4, "'field' property — no hover")
	requireNoHover(t, f, 0, 10, "'nested' property — no hover")
}

func TestHoverComprehensionVariable(t *testing.T) {
	t.Parallel()
	// [1, 2, 3].all(x, x > 0)
	f := "testdata/hover/comp_var.cel"

	requireNoHover(t, f, 0, 18, "'x' comprehension variable — no hover")
}

func TestHoverEmptyFile(t *testing.T) {
	t.Parallel()
	f := "testdata/semantic_tokens/empty.cel"

	requireNoHover(t, f, 0, 0, "empty file — no hover")
}

func TestHoverParseError(t *testing.T) {
	t.Parallel()
	f := "testdata/semantic_tokens/parse_error.cel"

	requireNoHover(t, f, 0, 0, "parse error file — no hover")
}

func TestHoverTokenBoundary(t *testing.T) {
	t.Parallel()
	// x > 0 && y < 10
	// 0123456789012345
	f := "testdata/hover/operators.cel"

	// First char of '&&'
	requireHoverContains(t, f, 0, 6, "logically AND", "'&&' first char")
	// Second char of '&&'
	requireHoverContains(t, f, 0, 7, "logically AND", "'&&' second char")
	// Just before '&&' (space at col 5)
	requireNoHover(t, f, 0, 5, "space before '&&'")
	// Just after '&&' (space at col 8)
	requireNoHover(t, f, 0, 8, "space after '&&'")
}

func TestHoverComprehensive(t *testing.T) {
	t.Parallel()
	f := "testdata/hover/comprehensive.cel"

	// --- Operators ---
	// Line 7: (x + y) * 2 - z / w % 3 >= 10 &&
	requireHoverContains(t, f, 7, 3, "adds two numeric", "'+' operator")
	requireHoverContains(t, f, 7, 8, "multiply", "'*' operator")
	requireHoverContains(t, f, 7, 12, "subtract", "'-' operator")
	requireHoverContains(t, f, 7, 16, "divide", "'/' operator")
	requireHoverContains(t, f, 7, 20, "modulus", "'%' operator")
	requireHoverContains(t, f, 7, 24, "greater than or equal", "'>=' operator")
	requireHoverContains(t, f, 7, 31, "logically AND", "'&&' operator")

	// --- String methods ---
	// Line 10: "hello world".contains("world") &&
	requireHoverContains(t, f, 10, 14, "contains", "'.contains' method")
	requireHoverContains(t, f, 10, 14, "substring", "'.contains' description")

	// Line 11: "hello".startsWith("he") &&
	requireHoverContains(t, f, 11, 8, "startsWith", "'.startsWith' method")

	// Line 12: "hello".endsWith("lo") &&
	requireHoverContains(t, f, 12, 8, "endsWith", "'.endsWith' method")

	// Line 13: "abc123".matches("[a-z]+[0-9]+") &&
	requireHoverContains(t, f, 13, 9, "matches", "'.matches' method")

	// Line 14: size("test") == 4 &&
	requireHoverContains(t, f, 14, 0, "size", "'size' function")
	requireHoverContains(t, f, 14, 0, "**Overloads**", "'size' has overloads")

	// --- Type conversions ---
	// Line 17: int("42") + double("3.14") > 0.0 &&
	requireHoverContains(t, f, 17, 0, "convert a value to an int", "'int()' hover")
	requireHoverContains(t, f, 17, 0, "**Type**", "'int' type header")
	requireHoverContains(t, f, 17, 12, "convert a value to a double", "'double()' hover")

	// Line 18: string(42) != "" &&
	requireHoverContains(t, f, 18, 0, "convert a value to a string", "'string()' hover")

	// Line 19: uint(10) > 0u &&
	requireHoverContains(t, f, 19, 0, "convert a value to a uint", "'uint()' hover")

	// Line 20: bool("true") == true &&
	requireHoverContains(t, f, 20, 0, "convert a value to a boolean", "'bool()' hover")
	requireHoverContains(t, f, 20, 19, "true", "'true' keyword")

	// Line 21: bytes("hello") == b"hello" &&
	requireHoverContains(t, f, 21, 0, "convert a value to bytes", "'bytes()' hover")

	// Line 22: type(42) == int &&
	requireHoverContains(t, f, 22, 0, "type identifier", "'type()' hover")

	// Line 23: dyn(1) == 1 &&
	requireHoverContains(t, f, 23, 0, "dynamic", "'dyn()' hover")

	// --- List/Map operations ---
	// Line 26: [1, 2, 3].size() == 3 &&
	requireHoverContains(t, f, 26, 10, "size", "'.size()' method on list")

	// --- Macros ---
	// Line 33: has({"field": true}.field) &&
	requireHoverContains(t, f, 33, 0, "**Macro**: `has`", "'has' macro")
	requireHoverContains(t, f, 33, 0, "presence", "'has' description")

	// Line 36: "a" in ["a", "b", "c"] &&
	requireHoverContains(t, f, 36, 4, "exists in a list", "'in' operator")

	// Line 40: [1, 2, 3].all(i, i > 0) &&
	requireHoverContains(t, f, 40, 10, "**Macro**: `all`", "'all' macro")
	requireHoverContains(t, f, 40, 10, "all elements", "'all' description")

	// Line 43: [1, 2, 3].exists(i, i == 2) &&
	requireHoverContains(t, f, 43, 10, "exists", "'exists' macro")

	// Line 46: [1, 2, 3].exists_one(i, i > 2) &&
	requireHoverContains(t, f, 46, 10, "exists_one", "'exists_one' macro")
	requireHoverContains(t, f, 46, 10, "exactly one", "'exists_one' description")

	// Line 49: [1, 2, 3].map(i, i * 2) == [2, 4, 6] &&
	requireHoverContains(t, f, 49, 10, "**Macro**: `map`", "'map' macro")

	// Line 52: [1, 2, 3, 4, 5].filter(i, i > 3) == [4, 5] &&
	requireHoverContains(t, f, 52, 16, "filter", "'filter' macro")

	// --- Ternary ---
	// Line 58: (x > 0 ? "positive" : "non-positive") != "" &&
	requireHoverContains(t, f, 58, 7, "ternary", "ternary '?'")

	// --- Logical operators ---
	// Line 61: !(false) &&
	requireHoverContains(t, f, 61, 0, "negate", "'!' operator")

	// Line 62: true || false &&
	requireHoverContains(t, f, 62, 5, "logically OR", "'||' operator")

	// --- Comparison operators ---
	// Line 65: 1 == 1 &&
	requireHoverContains(t, f, 65, 2, "equality", "'==' operator")

	// Line 66: 1 != 2 &&
	requireHoverContains(t, f, 66, 2, "inequality", "'!=' operator")

	// Line 67: 1 < 2 &&
	requireHoverContains(t, f, 67, 2, "less than", "'<' operator")

	// Line 68: 1 <= 2 &&
	requireHoverContains(t, f, 68, 2, "less than or equal", "'<=' operator")

	// Line 69: 2 > 1 &&
	requireHoverContains(t, f, 69, 2, "greater than", "'>' operator")

	// Line 70: 2 >= 1 &&
	requireHoverContains(t, f, 70, 2, "greater than or equal", "'>=' operator")

	// --- Negation and unary minus ---
	// Line 73: !false &&
	requireHoverContains(t, f, 73, 0, "negate", "'!' negation")

	// Line 74: -x + 1 > 0 &&
	requireHoverContains(t, f, 74, 0, "negate", "unary '-'")

	// --- Keywords ---
	// Line 80: true != false &&
	requireHoverContains(t, f, 80, 0, "true", "'true' keyword")
	requireHoverContains(t, f, 80, 8, "false", "'false' keyword")

	// Line 81: null == null &&
	requireHoverContains(t, f, 81, 0, "null", "'null' keyword")
	requireHoverContains(t, f, 81, 0, "null_type", "'null' type info")

	// --- No hover cases ---
	requireNoHover(t, f, 7, 1, "'x' identifier — no hover")
	requireNoHover(t, f, 10, 1, "string literal — no hover")
	requireNoHover(t, f, 7, 10, "number '2' — no hover")
	requireNoHover(t, f, 0, 5, "comment — no hover")
	requireNoHover(t, f, 77, 0, "'msg' identifier — no hover")

	// --- Duration/Timestamp ---
	// Line 102: duration("1h") > duration("30m") &&
	requireHoverContains(t, f, 102, 0, "duration", "'duration()' type")
	requireHoverContains(t, f, 102, 0, "**Type**", "'duration' type header")

	// Line 103: timestamp("2023-01-01T00:00:00Z") < timestamp("2024-01-01T00:00:00Z") &&
	requireHoverContains(t, f, 103, 0, "timestamp", "'timestamp()' type")

	// --- Size method/function forms ---
	// Line 112: size("hello") == 5 &&
	requireHoverContains(t, f, 112, 0, "size", "'size()' function form")
	// Line 113: "hello".size() == 5 &&
	requireHoverContains(t, f, 113, 8, "size", "'.size()' method form")

	// --- Final line ---
	// Line 120: x + y == z
	requireHoverContains(t, f, 120, 2, "adds two numeric", "'+' on final line")
	requireHoverContains(t, f, 120, 6, "equality", "'==' on final line")

	// --- All macros get proper headers ---
	macroPositions := [][3]any{
		{uint32(33), uint32(0), "has"},
		{uint32(40), uint32(10), "all"},
		{uint32(43), uint32(10), "exists"},
		{uint32(46), uint32(10), "exists_one"},
		{uint32(49), uint32(10), "map"},
		{uint32(52), uint32(16), "filter"},
	}
	for _, mp := range macroPositions {
		line, col, name := mp[0].(uint32), mp[1].(uint32), mp[2].(string)
		requireHoverContains(t, f, line, col, "**Macro**", name+" macro header")
		requireHoverContains(t, f, line, col, name, name+" macro name")
	}
}
