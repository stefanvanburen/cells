package lsp

import (
	"maps"
	"slices"
	"unicode/utf16"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/overloads"
	"github.com/google/cel-go/common/types"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// Semantic token types - indices into semanticTypeLegend.
const (
	semanticTypeProperty = iota
	semanticTypeStruct
	semanticTypeVariable
	semanticTypeEnum
	semanticTypeEnumMember
	semanticTypeInterface
	semanticTypeMethod
	semanticTypeFunction
	semanticTypeDecorator
	semanticTypeMacro
	semanticTypeNamespace
	semanticTypeKeyword
	semanticTypeModifier
	semanticTypeComment
	semanticTypeString
	semanticTypeNumber
	semanticTypeType
	semanticTypeOperator
)

// Semantic token modifiers - encoded as a bitset.
const (
	semanticModifierDeprecated = 1 << iota
	semanticModifierDefaultLibrary
)

var (
	semanticTypeLegend = []string{
		string(protocol.PropertyType),
		string(protocol.StructType),
		string(protocol.VariableType),
		string(protocol.EnumType),
		string(protocol.EnumMemberType),
		string(protocol.InterfaceType),
		string(protocol.MethodType),
		string(protocol.FunctionType),
		string(protocol.DecoratorType),
		string(protocol.MacroType),
		string(protocol.NamespaceType),
		string(protocol.KeywordType),
		string(protocol.ModifierType),
		string(protocol.CommentType),
		string(protocol.StringType),
		string(protocol.NumberType),
		string(protocol.TypeType),
		string(protocol.OperatorType),
	}
	semanticModifierLegend = []string{
		string(protocol.ModDeprecated),
		string(protocol.ModDefaultLibrary),
	}
)

// tokenInfo holds information about a single semantic token before encoding.
type tokenInfo struct {
	line    uint32
	col     uint32
	length  uint32
	semType uint32
	semMod  uint32
}

func computeSemanticTokens(f *file, celEnv *cel.Env) (*protocol.SemanticTokens, error) {
	if f == nil || f.content == "" {
		return nil, nil
	}

	var tokens []tokenInfo

	collectToken := func(byteStart, byteEnd int, semanticType, semanticModifier uint32) {
		if byteStart < 0 || byteEnd <= byteStart || byteEnd > len(f.content) {
			return
		}
		line, col := byteOffsetToLineCol(f.content, byteStart)
		// Calculate length in UTF-16 code units (what LSP expects)
		tokenText := f.content[byteStart:byteEnd]
		length := uint32(0)
		for _, r := range tokenText {
			length += uint32(utf16.RuneLen(r))
		}
		tokens = append(tokens, tokenInfo{
			line:    line,
			col:     col,
			length:  length,
			semType: semanticType,
			semMod:  semanticModifier,
		})
	}

	// Parse the CEL expression
	parsed, issues := celEnv.Parse(f.content)
	if issues.Err() != nil {
		return nil, nil
	}

	nativeAST := parsed.NativeRep()
	sourceInfo := nativeAST.SourceInfo()

	// Walk the CEL AST and collect tokens
	walkCELExpr(nativeAST.Expr(), sourceInfo, f.content, collectToken, nil)

	// Process macro calls
	collectMacroTokens(sourceInfo, f.content, collectToken)

	// Sort tokens by position
	slices.SortFunc(tokens, func(a, b tokenInfo) int {
		if a.line != b.line {
			return int(a.line) - int(b.line)
		}
		return int(a.col) - int(b.col)
	})

	// Delta-encode
	var (
		encoded           []uint32
		prevLine, prevCol uint32
	)
	for _, tok := range tokens {
		deltaCol := tok.col
		if prevLine == tok.line {
			deltaCol -= prevCol
		}
		encoded = append(encoded, tok.line-prevLine, deltaCol, tok.length, tok.semType, tok.semMod)
		prevLine = tok.line
		prevCol = tok.col
	}
	if len(encoded) == 0 {
		return nil, nil
	}
	return &protocol.SemanticTokens{Data: encoded}, nil
}

// walkCELExpr recursively walks a CEL expression AST and collects semantic tokens.
func walkCELExpr(
	expr ast.Expr,
	sourceInfo *ast.SourceInfo,
	exprString string,
	collectToken func(byteStart, byteEnd int, semanticType, semanticModifier uint32),
	compVars map[string]bool,
) {
	if expr == nil || expr.Kind() == ast.UnspecifiedExprKind {
		return
	}

	offsetRange, hasOffset := sourceInfo.GetOffsetRange(expr.ID())
	startLoc := sourceInfo.GetStartLocation(expr.ID())

	switch expr.Kind() {
	case ast.IdentKind:
		identName := expr.AsIdent()

		var tokenType uint32
		if isCELKeyword(identName) {
			tokenType = semanticTypeKeyword
		} else if compVars != nil && compVars[identName] {
			tokenType = semanticTypeVariable
		} else {
			tokenType = semanticTypeProperty
		}

		if hasOffset {
			byteStart, byteStop := celOffsetRangeToByteRange(exprString, offsetRange)
			collectToken(byteStart, byteStop, tokenType, 0)
		}

	case ast.SelectKind:
		sel := expr.AsSelect()
		if sel.Operand() != nil {
			walkCELExpr(sel.Operand(), sourceInfo, exprString, collectToken, compVars)
		}
		if sel.Operand() != nil {
			operandStart := sourceInfo.GetStartLocation(sel.Operand().ID())
			if operandStart.Line() > 0 {
				targetByteOffset := celRuneOffsetToByteOffset(exprString, int32(operandStart.Column())+sourceInfo.ComputeOffset(int32(operandStart.Line()), 0))
				start, end := findMethodNameAfterDot(targetByteOffset, sel.FieldName(), exprString)
				if start >= 0 {
					collectToken(start, end, semanticTypeProperty, 0)
				}
			}
		}

	case ast.CallKind:
		call := expr.AsCall()
		if call.IsMemberFunction() {
			walkCELExpr(call.Target(), sourceInfo, exprString, collectToken, compVars)
		}

		funcName := call.FunctionName()

		if _, isOperator := celOperatorSymbol(funcName); isOperator {
			if hasOffset {
				byteStart, byteStop := celOffsetRangeToByteRange(exprString, offsetRange)
				collectToken(byteStart, byteStop, semanticTypeOperator, 0)
			}
		} else {
			var tokenType uint32
			var tokenModifier uint32

			if isCELMacroFunction(funcName) {
				tokenType = semanticTypeMacro
			} else if overloads.IsTypeConversionFunction(funcName) {
				tokenType = semanticTypeType
				tokenModifier = semanticModifierDefaultLibrary
			} else if call.IsMemberFunction() {
				tokenType = semanticTypeMethod
			} else {
				tokenType = semanticTypeFunction
			}

			if call.IsMemberFunction() {
				targetStart := sourceInfo.GetStartLocation(call.Target().ID())
				if targetStart.Line() > 0 {
					targetByteOffset := celRuneOffsetToByteOffset(exprString, int32(targetStart.Column())+sourceInfo.ComputeOffset(int32(targetStart.Line()), 0))
					start, end := findMethodNameAfterDot(targetByteOffset, funcName, exprString)
					if start >= 0 {
						collectToken(start, end, tokenType, tokenModifier)
					}
				}
			} else if startLoc.Line() > 0 {
				celByteOffset := celRuneOffsetToByteOffset(exprString, int32(startLoc.Column())+sourceInfo.ComputeOffset(int32(startLoc.Line()), 0))
				funcStart := celByteOffset - len(funcName)
				funcEnd := funcStart + len(funcName)
				if funcStart >= 0 && funcEnd <= len(exprString) {
					if exprString[funcStart:funcEnd] == funcName {
						collectToken(funcStart, funcEnd, tokenType, tokenModifier)
					}
				}
			}
		}

		for _, arg := range call.Args() {
			walkCELExpr(arg, sourceInfo, exprString, collectToken, compVars)
		}

	case ast.LiteralKind:
		lit := expr.AsLiteral()
		if hasOffset {
			byteStart, byteStop := celOffsetRangeToByteRange(exprString, offsetRange)
			switch lit.(type) {
			case types.Null:
				collectToken(byteStart, byteStop, semanticTypeKeyword, 0)
			case types.String:
				collectToken(byteStart, byteStop, semanticTypeString, 0)
			case types.Int, types.Uint, types.Double:
				collectToken(byteStart, byteStop, semanticTypeNumber, 0)
			case types.Bytes:
				collectToken(byteStart, byteStop, semanticTypeString, 0)
			case types.Bool:
				collectToken(byteStart, byteStop, semanticTypeKeyword, 0)
			}
		}

	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			walkCELExpr(elem, sourceInfo, exprString, collectToken, compVars)
		}

	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			walkCELExpr(mapEntry.Key(), sourceInfo, exprString, collectToken, compVars)
			walkCELExpr(mapEntry.Value(), sourceInfo, exprString, collectToken, compVars)
		}

	case ast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			walkCELExpr(field.AsStructField().Value(), sourceInfo, exprString, collectToken, compVars)
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		walkCELExpr(comp.IterRange(), sourceInfo, exprString, collectToken, compVars)
		walkCELExpr(comp.AccuInit(), sourceInfo, exprString, collectToken, compVars)

		extendedVars := compVars
		if comp.IterVar() != "" || comp.AccuVar() != "" {
			if compVars != nil {
				extendedVars = make(map[string]bool, len(compVars)+2)
				maps.Copy(extendedVars, compVars)
			} else {
				extendedVars = make(map[string]bool, 2)
			}
			if comp.IterVar() != "" {
				extendedVars[comp.IterVar()] = true
			}
			if comp.AccuVar() != "" {
				extendedVars[comp.AccuVar()] = true
			}
		}

		walkCELExpr(comp.LoopCondition(), sourceInfo, exprString, collectToken, extendedVars)
		walkCELExpr(comp.LoopStep(), sourceInfo, exprString, collectToken, extendedVars)
		walkCELExpr(comp.Result(), sourceInfo, exprString, collectToken, extendedVars)
	}
}

// collectMacroTokens processes CEL macro calls to highlight macro function names.
func collectMacroTokens(
	sourceInfo *ast.SourceInfo,
	exprString string,
	collectToken func(byteStart, byteEnd int, semanticType, semanticModifier uint32),
) {
	for macroID, macroExpr := range sourceInfo.MacroCalls() {
		if macroExpr.Kind() != ast.CallKind {
			continue
		}
		call := macroExpr.AsCall()
		funcName := call.FunctionName()
		if !isCELMacroFunction(funcName) {
			continue
		}

		startLoc := sourceInfo.GetStartLocation(macroID)
		if startLoc.Line() <= 0 {
			continue
		}

		if call.IsMemberFunction() {
			targetStart := sourceInfo.GetStartLocation(call.Target().ID())
			if targetStart.Line() > 0 {
				targetByteOffset := celRuneOffsetToByteOffset(exprString, int32(targetStart.Column())+sourceInfo.ComputeOffset(int32(targetStart.Line()), 0))
				start, end := findMethodNameAfterDot(targetByteOffset, funcName, exprString)
				if start >= 0 {
					collectToken(start, end, semanticTypeMacro, 0)
				}
			}
		} else {
			celByteOffset := celRuneOffsetToByteOffset(exprString, int32(startLoc.Column())+sourceInfo.ComputeOffset(int32(startLoc.Line()), 0))
			funcStart := celByteOffset - len(funcName)
			funcEnd := funcStart + len(funcName)
			if funcStart >= 0 && funcEnd <= len(exprString) {
				if exprString[funcStart:funcEnd] == funcName {
					collectToken(funcStart, funcEnd, semanticTypeMacro, 0)
				}
			}
		}
	}
}
