package lsp

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
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

// celOperatorSymbol maps CEL operator function names to their operator symbols.
func celOperatorSymbol(funcName string) (string, bool) {
	if symbol, found := operators.FindReverse(funcName); found && symbol != "" {
		return symbol, true
	}
	if funcName == operators.Conditional {
		return "?", true
	}
	return "", false
}

// celRuneOffsetToByteOffset converts a CEL source position (rune offset)
// to a UTF-8 byte offset within the expression string.
func celRuneOffsetToByteOffset(s string, runeOffset int32) int {
	byteIdx := 0
	for runeIdx := int32(0); runeIdx < runeOffset && byteIdx < len(s); runeIdx++ {
		_, size := utf8.DecodeRuneInString(s[byteIdx:])
		byteIdx += size
	}
	return byteIdx
}

// celOffsetRangeToByteRange converts a CEL ast.OffsetRange to byte offsets.
func celOffsetRangeToByteRange(exprString string, r celast.OffsetRange) (byteStart, byteStop int) {
	byteStart = celRuneOffsetToByteOffset(exprString, r.Start)
	byteStop = byteStart + int(r.Stop-r.Start)
	return
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
