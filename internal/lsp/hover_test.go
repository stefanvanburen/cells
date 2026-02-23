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

type hoverTestCase struct {
	name     string
	file     string
	line     uint32
	char     uint32
	contains string // empty string means expect no hover
	desc     string
}

func TestHover(t *testing.T) {
	t.Parallel()

	tests := []hoverTestCase{
		// Operators tests
		{name: "operators_greater_than", file: "testdata/hover/operators.cel", line: 0, char: 2, contains: "greater than", desc: "'>' operator description"},
		{name: "operators_greater_than_header", file: "testdata/hover/operators.cel", line: 0, char: 2, contains: "**Operator**: `>`", desc: "'>' operator header"},
		{name: "operators_greater_than_overloads", file: "testdata/hover/operators.cel", line: 0, char: 2, contains: "**Overloads**", desc: "'>' has overloads"},
		{name: "operators_and", file: "testdata/hover/operators.cel", line: 0, char: 6, contains: "logically AND", desc: "'&&' operator"},
		{name: "operators_less_than", file: "testdata/hover/operators.cel", line: 0, char: 11, contains: "less than", desc: "'<' operator"},
		{name: "operators_no_hover_x", file: "testdata/hover/operators.cel", line: 0, char: 0, contains: "", desc: "'x' identifier — no hover"},

		// In operator tests
		{name: "in_operator_exists", file: "testdata/hover/in_operator.cel", line: 0, char: 4, contains: "exists in a list", desc: "'in' operator"},
		{name: "in_operator_header", file: "testdata/hover/in_operator.cel", line: 0, char: 4, contains: "**Operator**", desc: "'in' has operator header"},

		// Logical operators tests
		{name: "logical_negation", file: "testdata/hover/logical.cel", line: 0, char: 0, contains: "negate", desc: "'!' unary operator"},
		{name: "logical_and", file: "testdata/hover/logical.cel", line: 0, char: 3, contains: "logically AND", desc: "'&&' operator"},
		{name: "logical_or", file: "testdata/hover/logical.cel", line: 0, char: 9, contains: "logically OR", desc: "'||' operator"},

		// Arithmetic tests
		{name: "arithmetic_unary_minus", file: "testdata/hover/arithmetic.cel", line: 0, char: 0, contains: "negate", desc: "unary '-'"},
		{name: "arithmetic_plus", file: "testdata/hover/arithmetic.cel", line: 0, char: 3, contains: "adds two numeric", desc: "'+'"},
		{name: "arithmetic_multiply", file: "testdata/hover/arithmetic.cel", line: 0, char: 7, contains: "multiply", desc: "'*'"},
		{name: "arithmetic_divide", file: "testdata/hover/arithmetic.cel", line: 0, char: 11, contains: "divide", desc: "'/'"},
		{name: "arithmetic_modulus", file: "testdata/hover/arithmetic.cel", line: 0, char: 15, contains: "modulus", desc: "'%'"},

		// Macros tests
		{name: "macros_all_header", file: "testdata/hover/macros.cel", line: 0, char: 10, contains: "**Macro**: `all`", desc: "'all' macro header"},
		{name: "macros_all_description", file: "testdata/hover/macros.cel", line: 0, char: 10, contains: "all elements", desc: "'all' macro description"},
		{name: "macros_all_examples", file: "testdata/hover/macros.cel", line: 0, char: 10, contains: "**Examples**", desc: "'all' has examples"},

		// Has macro tests
		{name: "has_macro_header", file: "testdata/hover/has_macro.cel", line: 0, char: 0, contains: "**Macro**: `has`", desc: "'has' macro header"},
		{name: "has_macro_description", file: "testdata/hover/has_macro.cel", line: 0, char: 0, contains: "presence of a field", desc: "'has' description"},

		// Exists_one macro tests
		{name: "exists_one_macro", file: "testdata/hover/exists_one.cel", line: 0, char: 10, contains: "exists_one", desc: "'exists_one' macro"},
		{name: "exists_one_description", file: "testdata/hover/exists_one.cel", line: 0, char: 10, contains: "exactly one", desc: "'exists_one' description"},

		// Map macro tests
		{name: "map_macro_header", file: "testdata/hover/map_macro.cel", line: 0, char: 10, contains: "**Macro**: `map`", desc: "'map' macro header"},
		{name: "map_macro_description", file: "testdata/hover/map_macro.cel", line: 0, char: 10, contains: "transform", desc: "'map' description"},

		// More macros tests
		{name: "exists_macro", file: "testdata/hover/more_macros.cel", line: 0, char: 10, contains: "exists", desc: "'exists' macro"},
		{name: "exists_description", file: "testdata/hover/more_macros.cel", line: 0, char: 10, contains: "any value", desc: "'exists' description"},
		{name: "filter_macro", file: "testdata/hover/more_macros.cel", line: 0, char: 41, contains: "filter", desc: "'filter' macro"},
		{name: "filter_description", file: "testdata/hover/more_macros.cel", line: 0, char: 41, contains: "satisfy the given predicate", desc: "'filter' description"},

		// Functions tests
		{name: "size_function", file: "testdata/hover/functions.cel", line: 0, char: 1, contains: "size", desc: "'size' function"},
		{name: "size_overloads", file: "testdata/hover/functions.cel", line: 0, char: 1, contains: "**Overloads**", desc: "'size' has overloads"},
		{name: "int_type_conversion", file: "testdata/hover/functions.cel", line: 0, char: 17, contains: "int", desc: "'int' type conversion"},
		{name: "int_description", file: "testdata/hover/functions.cel", line: 0, char: 17, contains: "convert a value to an int", desc: "'int' description"},

		// Type conversions tests
		{name: "string_conversion", file: "testdata/hover/type_conversions.cel", line: 0, char: 0, contains: "convert a value to a string", desc: "'string()' description"},
		{name: "string_type_header", file: "testdata/hover/type_conversions.cel", line: 0, char: 0, contains: "**Type**", desc: "'string' type header"},
		{name: "double_conversion", file: "testdata/hover/type_conversions.cel", line: 0, char: 13, contains: "convert a value to a double", desc: "'double()' description"},
		{name: "double_type_header", file: "testdata/hover/type_conversions.cel", line: 0, char: 13, contains: "**Type**", desc: "'double' type header"},

		// Methods tests
		{name: "startswith_method", file: "testdata/hover/methods.cel", line: 0, char: 8, contains: "startsWith", desc: "'startsWith' method"},
		{name: "startswith_description", file: "testdata/hover/methods.cel", line: 0, char: 8, contains: "prefix", desc: "'startsWith' description"},
		{name: "contains_method", file: "testdata/hover/methods.cel", line: 0, char: 38, contains: "contains", desc: "'contains' method"},
		{name: "contains_description", file: "testdata/hover/methods.cel", line: 0, char: 38, contains: "substring", desc: "'contains' description"},

		// More methods tests
		{name: "endswith_method", file: "testdata/hover/more_methods.cel", line: 0, char: 14, contains: "endsWith", desc: "'endsWith' method"},
		{name: "endswith_description", file: "testdata/hover/more_methods.cel", line: 0, char: 14, contains: "suffix", desc: "'endsWith' description"},
		{name: "matches_method", file: "testdata/hover/more_methods.cel", line: 0, char: 44, contains: "matches", desc: "'matches' method"},
		{name: "matches_description", file: "testdata/hover/more_methods.cel", line: 0, char: 44, contains: "RE2", desc: "'matches' description"},

		// Keywords tests
		{name: "true_keyword", file: "testdata/hover/keywords.cel", line: 0, char: 0, contains: "true", desc: "'true' keyword"},
		{name: "false_keyword", file: "testdata/hover/keywords.cel", line: 0, char: 9, contains: "false", desc: "'false' keyword"},
		{name: "null_keyword", file: "testdata/hover/keywords.cel", line: 0, char: 18, contains: "null", desc: "'null' keyword"},
		{name: "null_type_info", file: "testdata/hover/keywords.cel", line: 0, char: 18, contains: "null_type", desc: "'null' type info"},

		// Ternary tests
		{name: "ternary_operator", file: "testdata/hover/ternary.cel", line: 0, char: 6, contains: "ternary", desc: "'?' ternary operator"},
		{name: "ternary_operator_header", file: "testdata/hover/ternary.cel", line: 0, char: 6, contains: "**Operator**", desc: "'?' operator header"},

		// Literals tests (no hover)
		{name: "literals_no_hover", file: "testdata/hover/literals.cel", line: 0, char: 3, contains: "", desc: "string literal — no hover"},

		// Whitespace tests (no hover)
		{name: "whitespace_no_hover", file: "testdata/hover/operators.cel", line: 0, char: 1, contains: "", desc: "whitespace between tokens — no hover"},

		// Number literal tests (no hover)
		{name: "number_literal_0", file: "testdata/hover/operators.cel", line: 0, char: 4, contains: "", desc: "number literal '0' — no hover"},
		{name: "number_literal_10", file: "testdata/hover/operators.cel", line: 0, char: 13, contains: "", desc: "number literal '10' — no hover"},

		// Multiline tests
		{name: "multiline_eq", file: "testdata/hover/multiline.cel", line: 0, char: 2, contains: "equality", desc: "'==' on line 1"},
		{name: "multiline_ne", file: "testdata/hover/multiline.cel", line: 1, char: 2, contains: "inequality", desc: "'!=' on line 2"},
		{name: "multiline_gte", file: "testdata/hover/multiline.cel", line: 2, char: 2, contains: "greater than or equal", desc: "'>=' on line 3"},
		{name: "multiline_lte", file: "testdata/hover/multiline.cel", line: 3, char: 2, contains: "less than or equal", desc: "'<=' on line 4"},

		// Unknown function tests (no hover)
		{name: "unknown_function_no_hover", file: "testdata/hover/unknown_func.cel", line: 0, char: 0, contains: "", desc: "unknown function — no hover"},

		// Select tests (no hover)
		{name: "select_no_hover_msg", file: "testdata/hover/select.cel", line: 0, char: 0, contains: "", desc: "'msg' identifier — no hover"},
		{name: "select_no_hover_field", file: "testdata/hover/select.cel", line: 0, char: 4, contains: "", desc: "'field' property — no hover"},
		{name: "select_no_hover_nested", file: "testdata/hover/select.cel", line: 0, char: 10, contains: "", desc: "'nested' property — no hover"},

		// Comprehension variable tests (no hover)
		{name: "comp_var_no_hover", file: "testdata/hover/comp_var.cel", line: 0, char: 18, contains: "", desc: "'x' comprehension variable — no hover"},

		// Empty file tests (no hover)
		{name: "empty_file_no_hover", file: "testdata/semantic_tokens/empty.cel", line: 0, char: 0, contains: "", desc: "empty file — no hover"},

		// Parse error tests (no hover)
		{name: "parse_error_no_hover", file: "testdata/semantic_tokens/parse_error.cel", line: 0, char: 0, contains: "", desc: "parse error file — no hover"},

		// Token boundary tests
		{name: "token_boundary_and_first", file: "testdata/hover/operators.cel", line: 0, char: 6, contains: "logically AND", desc: "'&&' first char"},
		{name: "token_boundary_and_second", file: "testdata/hover/operators.cel", line: 0, char: 7, contains: "logically AND", desc: "'&&' second char"},
		{name: "token_boundary_space_before", file: "testdata/hover/operators.cel", line: 0, char: 5, contains: "", desc: "space before '&&'"},
		{name: "token_boundary_space_after", file: "testdata/hover/operators.cel", line: 0, char: 8, contains: "", desc: "space after '&&'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.contains == "" {
				requireNoHover(t, tt.file, tt.line, tt.char, tt.desc)
			} else {
				requireHoverContains(t, tt.file, tt.line, tt.char, tt.contains, tt.desc)
			}
		})
	}
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
