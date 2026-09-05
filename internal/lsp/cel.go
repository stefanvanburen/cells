package lsp

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
)

// isCELKeyword returns true if the identifier is a CEL reserved keyword.
func isCELKeyword(name string) bool {
	switch name {
	case "true", "false", "null":
		return true
	}
	return false
}

// isCELMacroFunction returns true if the function name is a CEL macro.
func isCELMacroFunction(funcName string) bool {
	return funcName == operators.Has ||
		funcName == operators.All ||
		funcName == operators.Exists ||
		funcName == operators.ExistsOne ||
		funcName == operators.Map ||
		funcName == operators.Filter
}

// isCELComprehensionMacro reports whether the function name is a CEL macro
// that binds a loop variable. It is every macro but has(), which takes a field
// selection rather than a variable and a body.
func isCELComprehensionMacro(funcName string) bool {
	return isCELMacroFunction(funcName) && funcName != operators.Has
}

// celOperatorSymbol maps CEL operator function names to their operator symbols.
func celOperatorSymbol(funcName string) (string, bool) {
	if symbol, found := operators.FindReverse(funcName); found && symbol != "" {
		return symbol, true
	}
	if funcName == operators.Conditional {
		return "?", true
	}
	// FindReverse returns ("", true) for _[_], so handle it explicitly.
	if funcName == operators.Index {
		return "[", true
	}
	return "", false
}

// celRuneOffsetToByteOffset converts a rune offset to a UTF-8 byte offset within a string.
func celRuneOffsetToByteOffset(s string, runeOffset int32) int {
	byteIdx := 0
	for runeIdx := int32(0); runeIdx < runeOffset && byteIdx < len(s); runeIdx++ {
		_, size := utf8.DecodeRuneInString(s[byteIdx:])
		byteIdx += size
	}
	return byteIdx
}

// celOffsetRangeToByteRange converts a CEL ast.OffsetRange to UTF-8 byte offsets.
//
// The two ends of an OffsetRange are not measured in the same units. cel-go
// parses over a code-point buffer (common.NewStringSource wraps the source in
// a runes.Buffer), so Start is a rune offset; Stop is Start plus the token's
// *byte* length, because ANTLR reports token lengths against the underlying
// UTF-8 text. The two units coincide for an all-ASCII token, and only a string
// literal can be anything else — CEL identifiers and operators are ASCII — so
// the difference shows up as soon as a multi-byte character precedes a token.
//
// The conversion is therefore exact: translate Start from runes to bytes, then
// add the length unchanged.
func celOffsetRangeToByteRange(exprString string, r celast.OffsetRange) (byteStart, byteStop int) {
	byteStart = celRuneOffsetToByteOffset(exprString, r.Start)
	byteStop = min(byteStart+int(r.Stop-r.Start), len(exprString))
	return byteStart, byteStop
}

// findMethodNameAfterDot finds ".methodName" after targetByteOffset.
func findMethodNameAfterDot(targetByteOffset int, methodName string, exprString string) (start, end int) {
	searchStart := targetByteOffset
	searchRegion := exprString[searchStart:]
	if idx := strings.Index(searchRegion, "."+methodName); idx >= 0 {
		nameStart := searchStart + idx + 1
		nameEnd := nameStart + len(methodName)
		return nameStart, nameEnd
	}
	return -1, -1
}

// byteOffsetToLineCol converts a byte offset in text to 0-indexed line and
// column, where the column is measured in UTF-16 code units (as required by LSP).
func byteOffsetToLineCol(text string, offset int) (line, col uint32) {
	i := 0
	for i < offset && i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == '\n' {
			line++
			col = 0
		} else {
			col += uint32(utf16.RuneLen(r))
		}
		i += size
	}
	return
}

// lineColToByteOffset converts an LSP position (0-indexed line, UTF-16 col) to a byte offset.
func lineColToByteOffset(text string, line, utf16Col uint32) int {
	currentLine := uint32(0)
	i := 0
	for i < len(text) && currentLine < line {
		if text[i] == '\n' {
			currentLine++
		}
		i++
	}
	if currentLine != line {
		return -1
	}
	col := uint32(0)
	for i < len(text) && col < utf16Col {
		if text[i] == '\n' {
			return -1
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		col += uint32(utf16.RuneLen(r))
		i += size
	}
	return i
}
