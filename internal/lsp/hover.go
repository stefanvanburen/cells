package lsp

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/decls"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/overloads"
	"cel.dev/cel-go/common/types"
	"go.lsp.dev/protocol"
)

func (s *server) Hover(_ context.Context, params *protocol.HoverParams) (*protocol.Hover, error) {
	f, docEnv := s.document(params.TextDocument.URI)

	if f == nil || docEnv == nil || f.content == "" {
		return nil, nil
	}

	return computeHover(f, docEnv, params.Position)
}

// hoverInfo represents hover documentation for a CEL element.
type hoverInfo struct {
	byteStart int
	byteEnd   int
	markdown  string
}

func computeHover(f *file, docEnv *environment, pos protocol.Position) (*protocol.Hover, error) {
	celEnv := docEnv.celEnv
	nativeAST := f.ast(celEnv)
	if nativeAST == nil {
		return nil, nil
	}
	sourceInfo := nativeAST.SourceInfo()

	// Convert the LSP position (line, UTF-16 col) to a byte offset.
	targetOffset := lineColToByteOffset(f.content, pos.Line, pos.Character)
	if targetOffset < 0 || targetOffset >= len(f.content) {
		return nil, nil
	}

	// Collect all hoverable elements with their byte ranges and docs.
	var hovers []hoverInfo

	collectHover := func(byteStart, byteEnd int, markdown string) {
		if byteStart < 0 || byteEnd <= byteStart || byteEnd > len(f.content) {
			return
		}
		if markdown == "" {
			return
		}
		hovers = append(hovers, hoverInfo{byteStart: byteStart, byteEnd: byteEnd, markdown: markdown})
	}

	walkCELExprForHover(nativeAST.Expr(), sourceInfo, f.content, celEnv, collectHover, nil)
	collectMacroHovers(sourceInfo, f.content, celEnv, collectHover)
	collectDeclaredHovers(f, docEnv, nativeAST, collectHover)

	// Find the most specific (smallest) hover that contains the target offset.
	var best *hoverInfo
	for i := range hovers {
		h := &hovers[i]
		if targetOffset >= h.byteStart && targetOffset < h.byteEnd {
			if best == nil || (h.byteEnd-h.byteStart) < (best.byteEnd-best.byteStart) {
				best = h
			}
		}
	}

	if best == nil {
		return nil, nil
	}

	startLine, startCol := byteOffsetToLineCol(f.content, best.byteStart)
	endLine, endCol := byteOffsetToLineCol(f.content, best.byteEnd)

	return &protocol.Hover{
		Contents: &protocol.MarkupContent{
			Kind:  protocol.MarkupKindMarkdown,
			Value: best.markdown,
		},
		Range: &protocol.Range{
			Start: protocol.Position{Line: startLine, Character: startCol},
			End:   protocol.Position{Line: endLine, Character: endCol},
		},
	}, nil
}

// collectDeclaredHovers reports what the environment knows about the names in
// a file: the type of a variable a configuration declared, and the type of a
// field selected from one. Plain CEL declares no variables, so this finds
// nothing until a configuration does.
func collectDeclaredHovers(
	f *file,
	docEnv *environment,
	nativeAST *ast.AST,
	collectHover func(byteStart, byteEnd int, markdown string),
) {
	celEnv := docEnv.celEnv
	variables := make(map[string]*decls.VariableDecl)
	for _, variable := range celEnv.Variables() {
		variables[variable.Name()] = variable
	}

	// Types come from the type-check, which an expression has to resolve for.
	// Where it does not, a variable's declaration still gives its type; a
	// field's type is only knowable from the check.
	var typed *ast.AST
	if checked, _ := f.check(celEnv); checked != nil {
		typed = checked.NativeRep()
	}
	typeOf := func(id int64) *types.Type {
		if typed == nil {
			return nil
		}
		return typed.GetType(id)
	}

	sourceInfo := nativeAST.SourceInfo()
	ast.PreOrderVisit(nativeAST.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		offsetRange, hasOffset := sourceInfo.GetOffsetRange(e.ID())
		if !hasOffset {
			return
		}
		byteStart, byteStop := celOffsetRangeToByteRange(f.content, offsetRange)

		switch e.Kind() {
		case ast.IdentKind:
			variable, declared := variables[e.AsIdent()]
			if !declared {
				return
			}
			// The checked type is preferred, since it accounts for a
			// comprehension variable shadowing a declared name.
			varType := typeOf(e.ID())
			if varType == nil {
				varType = variable.Type()
			}
			collectHover(byteStart, byteStop, declaredVariableHover(variable.Name(), varType, variable.Description()))

		case ast.SelectKind:
			fieldType := typeOf(e.ID())
			if fieldType == nil {
				return
			}
			// A select's own range covers the dot, not the field name after
			// it, and the two need not be adjacent: "request . method" is one
			// selection.
			fieldName := e.AsSelect().FieldName()
			start, end, found := findWord(f.content, fieldName, byteStop)
			if !found {
				return
			}
			collectHover(start, end, declaredVariableHover(fieldName, fieldType,
				docEnv.fieldDoc(typeOf(e.AsSelect().Operand().ID()), fieldName)))
		}
	}))
}

// declaredVariableHover renders a name, its type, and whatever the
// configuration said about it.
func declaredVariableHover(name string, t *types.Type, description string) string {
	markdown := fmt.Sprintf("`%s`: `%s`", name, t)
	if description != "" {
		markdown += "\n\n" + description
	}
	return markdown
}

// walkCELExprForHover walks the CEL AST and collects hover info.
func walkCELExprForHover(
	expr ast.Expr,
	sourceInfo *ast.SourceInfo,
	exprString string,
	celEnv *cel.Env,
	collectHover func(byteStart, byteEnd int, markdown string),
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
		if hasOffset && isCELKeyword(identName) {
			byteStart, byteStop := celOffsetRangeToByteRange(exprString, offsetRange)
			collectHover(byteStart, byteStop, celKeywordHover(identName))
		}

	case ast.SelectKind:
		sel := expr.AsSelect()
		if sel.Operand() != nil {
			walkCELExprForHover(sel.Operand(), sourceInfo, exprString, celEnv, collectHover, compVars)
		}

	case ast.CallKind:
		call := expr.AsCall()
		if call.IsMemberFunction() {
			walkCELExprForHover(call.Target(), sourceInfo, exprString, celEnv, collectHover, compVars)
		}

		funcName := call.FunctionName()

		if _, isOperator := celOperatorSymbol(funcName); isOperator {
			if hasOffset {
				byteStart, byteStop := celOffsetRangeToByteRange(exprString, offsetRange)
				collectHover(byteStart, byteStop, celFunctionHover(funcName, celEnv))
			}
		} else if !isCELMacroFunction(funcName) {
			// Non-operator, non-macro function or method.
			if call.IsMemberFunction() {
				targetStart := sourceInfo.GetStartLocation(call.Target().ID())
				if targetStart.Line() > 0 {
					targetByteOffset := celRuneOffsetToByteOffset(exprString, int32(targetStart.Column())+sourceInfo.ComputeOffset(int32(targetStart.Line()), 0))
					start, end := findMethodNameAfterDot(targetByteOffset, funcName, exprString)
					if start >= 0 {
						collectHover(start, end, celFunctionHover(funcName, celEnv))
					}
				}
			} else if startLoc.Line() > 0 {
				celByteOffset := celRuneOffsetToByteOffset(exprString, int32(startLoc.Column())+sourceInfo.ComputeOffset(int32(startLoc.Line()), 0))
				funcStart := celByteOffset - len(funcName)
				funcEnd := funcStart + len(funcName)
				if funcStart >= 0 && funcEnd <= len(exprString) && exprString[funcStart:funcEnd] == funcName {
					collectHover(funcStart, funcEnd, celFunctionHover(funcName, celEnv))
				}
			}
		}

		for _, arg := range call.Args() {
			walkCELExprForHover(arg, sourceInfo, exprString, celEnv, collectHover, compVars)
		}

	case ast.LiteralKind:
		lit := expr.AsLiteral()
		if hasOffset {
			byteStart, byteStop := celOffsetRangeToByteRange(exprString, offsetRange)
			if byteStart >= 0 && byteStop <= len(exprString) {
				switch lit.(type) {
				case types.Bool:
					text := exprString[byteStart:byteStop]
					collectHover(byteStart, byteStop, celKeywordHover(text))
				case types.Null:
					collectHover(byteStart, byteStop, celKeywordHover("null"))
				}
			}
		}

	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			walkCELExprForHover(elem, sourceInfo, exprString, celEnv, collectHover, compVars)
		}

	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			walkCELExprForHover(mapEntry.Key(), sourceInfo, exprString, celEnv, collectHover, compVars)
			walkCELExprForHover(mapEntry.Value(), sourceInfo, exprString, celEnv, collectHover, compVars)
		}

	case ast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			walkCELExprForHover(field.AsStructField().Value(), sourceInfo, exprString, celEnv, collectHover, compVars)
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		walkCELExprForHover(comp.IterRange(), sourceInfo, exprString, celEnv, collectHover, compVars)
		walkCELExprForHover(comp.AccuInit(), sourceInfo, exprString, celEnv, collectHover, compVars)

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

		walkCELExprForHover(comp.LoopCondition(), sourceInfo, exprString, celEnv, collectHover, extendedVars)
		walkCELExprForHover(comp.LoopStep(), sourceInfo, exprString, celEnv, collectHover, extendedVars)
		walkCELExprForHover(comp.Result(), sourceInfo, exprString, celEnv, collectHover, extendedVars)
	}
}

// collectMacroHovers processes CEL macro calls for hover info.
func collectMacroHovers(
	sourceInfo *ast.SourceInfo,
	exprString string,
	celEnv *cel.Env,
	collectHover func(byteStart, byteEnd int, markdown string),
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

		doc := celMacroHover(funcName, celEnv)
		if doc == "" {
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
					collectHover(start, end, doc)
				}
			}
		} else {
			celByteOffset := celRuneOffsetToByteOffset(exprString, int32(startLoc.Column())+sourceInfo.ComputeOffset(int32(startLoc.Line()), 0))
			funcStart := celByteOffset - len(funcName)
			funcEnd := funcStart + len(funcName)
			if funcStart >= 0 && funcEnd <= len(exprString) && exprString[funcStart:funcEnd] == funcName {
				collectHover(funcStart, funcEnd, doc)
			}
		}
	}
}

// --- Documentation rendering ---

// celKeywordHover returns hover markdown for a CEL keyword.
func celKeywordHover(name string) string {
	switch name {
	case "true":
		return "`true` — boolean **true** literal"
	case "false":
		return "`false` — boolean **false** literal"
	case "null":
		return "`null` — **null** value\n\nRepresents the absence of a value. Type: `null_type`."
	default:
		return ""
	}
}

// celFunctionHover returns hover markdown for a CEL function (including operators and type conversions).
// It looks up the function declaration in the CEL environment for upstream documentation.
func celFunctionHover(funcName string, celEnv *cel.Env) string {
	funcs := celEnv.Functions()
	funcDecl, ok := funcs[funcName]
	if !ok {
		return ""
	}
	var result string
	doc := funcDecl.Documentation()
	if doc != nil {
		// For operators, use the display symbol instead of the internal name.
		if symbol, isOp := celOperatorSymbol(funcName); isOp {
			result = formatCELDoc(doc, "**Operator**: ", symbol)
		} else if overloads.IsTypeConversionFunction(funcName) {
			result = formatCELDoc(doc, "**Type**: ", "")
		} else {
			result = formatCELDoc(doc, "", "")
		}
	} else {
		// Fallback to simple description.
		if desc := funcDecl.Description(); desc != "" {
			result = fmt.Sprintf("`%s` — %s", funcName, desc)
		} else {
			result = fmt.Sprintf("`%s()` — function", funcName)
		}
	}
	if link := celFunctionDocumentationLink(funcName); link != "" {
		result += "\n\n" + link
	}
	return result
}

// celMacroHover returns hover markdown for a CEL macro.
// It looks up macro documentation from the CEL environment.
func celMacroHover(macroName string, celEnv *cel.Env) string {
	var result string
	for _, m := range celEnv.Macros() {
		if m.Function() != macroName {
			continue
		}
		if doc, ok := m.(common.Documentor); ok {
			if documentation := doc.Documentation(); documentation != nil {
				result = formatCELDoc(documentation, "**Macro**: ", "")
			}
		}
		break
	}
	if result == "" {
		result = fmt.Sprintf("`%s` — macro", macroName)
	}
	if link := celMacroDocumentationLink(macroName); link != "" {
		result += "\n\n" + link
	}
	return result
}

// celByExampleLink returns a markdown link to a page on celbyexample.com.
func celByExampleLink(path string) string {
	return "[CEL by Example](https://celbyexample.com/" + path + ")"
}

// celFunctionDocumentationLink returns a markdown link to external documentation for a CEL function
// or operator. Returns an empty string if no documentation link is available.
func celFunctionDocumentationLink(funcName string) string {
	switch funcName {
	// Logical operators
	case operators.LogicalAnd:
		return celByExampleLink("logical-operators/#and")
	case operators.LogicalOr:
		return celByExampleLink("logical-operators/#or")
	case operators.LogicalNot:
		return celByExampleLink("logical-operators/#not")
	// Comparison operators
	case operators.Equals, operators.NotEquals:
		return celByExampleLink("comparison/#equality")
	case operators.Less, operators.LessEquals, operators.Greater, operators.GreaterEquals:
		return celByExampleLink("comparison/#ordering")
	// Arithmetic operators
	case operators.Add, operators.Subtract, operators.Multiply, operators.Divide, operators.Modulo:
		return celByExampleLink("arithmetic/")
	// Collection operators
	case operators.In:
		return celByExampleLink("collections/#membership-and-access")
	case operators.Conditional:
		return celByExampleLink("ternary/")
	case operators.Index:
		return celByExampleLink("lists/")
	// String functions
	case overloads.Size:
		return celByExampleLink("strings/#size")
	case overloads.Contains, overloads.StartsWith, overloads.EndsWith:
		return celByExampleLink("strings/#substring-search")
	case overloads.Matches:
		return celByExampleLink("strings/#regular-expressions")
	// Timestamp functions
	case overloads.TimeGetFullYear, overloads.TimeGetMonth, overloads.TimeGetDate,
		overloads.TimeGetDayOfMonth, overloads.TimeGetDayOfWeek, overloads.TimeGetDayOfYear,
		overloads.TimeGetHours, overloads.TimeGetMinutes, overloads.TimeGetSeconds,
		overloads.TimeGetMilliseconds:
		return celByExampleLink("time/#timestamp-components")
	// Type conversions
	case overloads.TypeConvertInt, overloads.TypeConvertUint, overloads.TypeConvertDouble:
		return celByExampleLink("type-conversions/#numeric-conversions")
	case overloads.TypeConvertString:
		return celByExampleLink("type-conversions/#string-conversions")
	case overloads.TypeConvertBytes:
		return celByExampleLink("type-conversions/#bytes-conversions")
	case overloads.TypeConvertTimestamp, overloads.TypeConvertDuration:
		return celByExampleLink("type-conversions/#time-conversions")
	case overloads.TypeConvertDyn:
		return celByExampleLink("type-conversions/#dynamic-type")
	}
	return ""
}

// celMacroDocumentationLink returns a markdown link to external documentation for a CEL macro.
// Returns an empty string if no documentation link is available.
func celMacroDocumentationLink(macroName string) string {
	switch macroName {
	case operators.Has:
		return celByExampleLink("has/")
	case operators.All:
		return celByExampleLink("all/")
	case operators.Exists:
		return celByExampleLink("exists/")
	case operators.ExistsOne:
		return celByExampleLink("exists-one/")
	case operators.Filter:
		return celByExampleLink("filter/")
	case operators.Map:
		return celByExampleLink("map-macro/")
	}
	return ""
}

// formatCELDoc formats a common.Doc into markdown.
// headerPrefix is prepended before the name (e.g. "**Operator**: ").
// nameOverride replaces doc.Name if non-empty.
func formatCELDoc(doc *common.Doc, headerPrefix string, nameOverride string) string {
	if doc == nil {
		return ""
	}

	var b strings.Builder

	name := doc.Name
	if nameOverride != "" {
		name = nameOverride
	}
	if doc.Signature != "" {
		name = doc.Signature
	}

	if name != "" {
		if headerPrefix != "" {
			fmt.Fprintf(&b, "%s`%s`", headerPrefix, name)
		} else {
			fmt.Fprintf(&b, "`%s`", name)
		}
	}

	if doc.Description != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(doc.Description)
	}

	if len(doc.Children) > 0 {
		hasSignatures := false
		for _, child := range doc.Children {
			if child.Signature != "" {
				hasSignatures = true
				break
			}
		}

		if hasSignatures {
			b.WriteString("\n\n**Overloads**:")
			for _, child := range doc.Children {
				if child.Signature != "" {
					fmt.Fprintf(&b, "\n- `%s`", child.Signature)
				}
			}
		} else {
			b.WriteString("\n\n**Examples**:")
			for _, child := range doc.Children {
				if child.Description != "" {
					fmt.Fprintf(&b, "\n```cel\n%s\n```", child.Description)
				}
			}
		}
	}

	return b.String()
}
