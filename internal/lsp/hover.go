package lsp

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/overloads"
	"github.com/google/cel-go/common/types"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

func (s *server) hover(req *jsonrpc2.Request) (any, error) {
	var params protocol.HoverParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	if f == nil || f.content == "" {
		return nil, nil
	}

	return computeHover(f, s.celEnv, params.Position)
}

// hoverInfo represents hover documentation for a CEL element.
type hoverInfo struct {
	byteStart int
	byteEnd   int
	markdown  string
}

func computeHover(f *file, celEnv *cel.Env, pos protocol.Position) (*protocol.Hover, error) {
	parsed, issues := celEnv.Parse(f.content)
	if issues.Err() != nil {
		return nil, nil
	}

	nativeAST := parsed.NativeRep()
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
		Contents: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: best.markdown,
		},
		Range: protocol.Range{
			Start: protocol.Position{Line: startLine, Character: startCol},
			End:   protocol.Position{Line: endLine, Character: endCol},
		},
	}, nil
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
	doc := funcDecl.Documentation()
	if doc != nil {
		// For operators, use the display symbol instead of the internal name.
		if symbol, isOp := celOperatorSymbol(funcName); isOp {
			return formatCELDoc(doc, "**Operator**: ", symbol)
		}
		if overloads.IsTypeConversionFunction(funcName) {
			return formatCELDoc(doc, "**Type**: ", "")
		}
		return formatCELDoc(doc, "", "")
	}
	// Fallback to simple description.
	if desc := funcDecl.Description(); desc != "" {
		return fmt.Sprintf("`%s` — %s", funcName, desc)
	}
	return fmt.Sprintf("`%s()` — function", funcName)
}

// celMacroHover returns hover markdown for a CEL macro.
// It looks up macro documentation from the CEL environment.
func celMacroHover(macroName string, celEnv *cel.Env) string {
	for _, m := range celEnv.Macros() {
		if m.Function() != macroName {
			continue
		}
		if doc, ok := m.(common.Documentor); ok {
			if documentation := doc.Documentation(); documentation != nil {
				return formatCELDoc(documentation, "**Macro**: ", "")
			}
		}
		break
	}
	return fmt.Sprintf("`%s` — macro", macroName)
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
