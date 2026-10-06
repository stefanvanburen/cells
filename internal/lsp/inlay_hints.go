package lsp

import (
	"context"
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"go.lsp.dev/protocol"
)

// paddingLeft is always true for the hints below; shared across requests to
// avoid allocating a *bool per hint.
var paddingLeft = true

func (s *server) InlayHint(ctx context.Context, params *protocol.InlayHintParams) ([]protocol.InlayHint, error) {
	f, docEnv := s.document(ctx, params.TextDocument.URI)

	if f == nil || docEnv == nil || f.content == "" {
		return []protocol.InlayHint{}, nil
	}

	hints, _ := computeInlayHints(f, docEnv.celEnv)

	// Filter hints to only those within the requested range
	var filtered []protocol.InlayHint
	for _, hint := range hints {
		if hint.Position.Line >= params.Range.Start.Line && hint.Position.Line <= params.Range.End.Line {
			filtered = append(filtered, hint)
		}
	}
	return filtered, nil
}

// computeInlayHints returns the types of the loop variables a macro declares,
// and the value of the whole expression when it evaluates without any input.
// Both need the expression to type-check.
func computeInlayHints(f *file, celEnv *cel.Env) ([]protocol.InlayHint, error) {
	if f.content == "" {
		return []protocol.InlayHint{}, nil
	}

	checked, _ := f.check(celEnv)
	if checked == nil {
		return []protocol.InlayHint{}, nil
	}
	hints := loopVariableHints(f.content, checked.NativeRep())

	// Try to evaluate the entire expression
	result, err := tryEvaluateExpression(f.content, celEnv)
	if err != nil {
		return hints, nil
	}

	// Get the type of the expression
	exprType := checked.OutputType()
	typeStr := exprType.String()

	// Create a hint at the end of the content (before any trailing newline)
	contentLen := len(strings.TrimRight(f.content, "\n\r"))
	endLine, endCol := byteOffsetToLineCol(f.content, contentLen)

	hint := protocol.InlayHint{
		Position: protocol.Position{Line: endLine, Character: endCol},
		Label: protocol.InlayHintLabelPartSlice{
			{
				Value: fmt.Sprintf("→ %s (%s)", result, typeStr),
			},
		},
		Kind:        protocol.InlayHintKind(1), // Type hint kind
		PaddingLeft: &paddingLeft,
	}

	return append(hints, hint), nil
}

// loopVariableHints returns a hint for the type of each loop variable a macro
// declares, placed after the declaration: the k and v of .all(k, v, ...).
func loopVariableHints(content string, checked *celast.AST) []protocol.InlayHint {
	hints := []protocol.InlayHint{}
	sourceInfo := checked.SourceInfo()
	celast.PreOrderVisit(checked.Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if e.Kind() != celast.ComprehensionKind {
			return
		}
		call, ok := sourceInfo.GetMacroCall(e.ID())
		if !ok || call.Kind() != celast.CallKind {
			return
		}
		comp := e.AsComprehension()
		for _, arg := range call.AsCall().Args() {
			if arg.Kind() != celast.IdentKind {
				continue
			}
			t := loopVariableType(comp, arg.AsIdent(), checked.TypeMap())
			_, end, ok := exprByteRange(content, sourceInfo, arg.ID())
			if t == nil || !ok {
				continue
			}
			line, col := byteOffsetToLineCol(content, end)
			hints = append(hints, protocol.InlayHint{
				Position: protocol.Position{Line: line, Character: col},
				Label:    protocol.InlayHintLabelPartSlice{{Value: ": " + t.String()}},
				Kind:     protocol.InlayHintKind(1), // Type hint kind
			})
		}
	}))
	return hints
}

// loopVariableType returns the type of the variable name that comp binds, or
// nil if comp does not bind it. Expansion leaves no expression for the
// variable itself, so its type follows from what the comprehension iterates
// over: the elements of a list, or the keys of a map, and with a second
// variable a list's indexes and elements, or a map's keys and values. A
// variable bound through the accumulator, as cel.bind does, has the type of
// its initial value.
func loopVariableType(comp celast.ComprehensionExpr, name string, typeMap map[int64]*types.Type) *types.Type {
	if name == comp.AccuVar() {
		return typeMap[comp.AccuInit().ID()]
	}
	rangeType := typeMap[comp.IterRange().ID()]
	if rangeType == nil {
		return nil
	}
	param := func(i int) *types.Type {
		if params := rangeType.Parameters(); i < len(params) {
			return params[i]
		}
		return types.DynType
	}
	list := rangeType.Kind() == types.ListKind
	switch name {
	case comp.IterVar():
		if list && comp.HasIterVar2() {
			return types.IntType
		}
		return param(0)
	case comp.IterVar2():
		if list {
			return param(0)
		}
		return param(1)
	}
	return nil
}

// tryEvaluateExpression attempts to parse, check, and evaluate a CEL expression.
// Returns a human-readable string representation of the result, or an error.
func tryEvaluateExpression(exprText string, celEnv *cel.Env) (string, error) {
	if strings.TrimSpace(exprText) == "" {
		return "", fmt.Errorf("empty expression")
	}

	// Parse the expression
	parsed, parseIssues := celEnv.Parse(exprText)
	if parseIssues.Err() != nil {
		return "", parseIssues.Err()
	}

	// Type-check the expression
	_, checkIssues := celEnv.Check(parsed)
	if checkIssues.Err() != nil {
		return "", checkIssues.Err()
	}

	// Compile the expression
	prog, compileErr := celEnv.Program(parsed)
	if compileErr != nil {
		return "", compileErr
	}

	// Evaluate with no variables
	val, _, evalErr := prog.Eval(map[string]any{})
	if evalErr != nil {
		return "", evalErr
	}

	// Convert the result to a human-readable string
	return resultToString(val), nil
}

// resultToString converts a CEL value to a human-readable string for inlay hints.
func resultToString(val any) string {
	// The val returned from Program.Eval is already a ref.Val (CEL value)
	// Format it using types.Format
	if refVal, ok := val.(ref.Val); ok {
		return types.Format(refVal)
	}
	// Fallback for non-ref.Val types
	return fmt.Sprintf("%v", val)
}
