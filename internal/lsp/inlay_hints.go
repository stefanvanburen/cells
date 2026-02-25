package lsp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

func (s *server) inlayHints(req *jsonrpc2.Request) (any, error) {
	var params protocol.InlayHintParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	if f == nil || f.content == "" {
		return []protocol.InlayHint{}, nil
	}

	hints, _ := computeInlayHints(f, s.celEnv)

	// Filter hints to only those within the requested range
	var filtered []protocol.InlayHint
	for _, hint := range hints {
		if hint.Position.Line >= params.Range.Start.Line && hint.Position.Line <= params.Range.End.Line {
			filtered = append(filtered, hint)
		}
	}
	return filtered, nil
}

// computeInlayHints returns inlay hints for CEL expressions.
// Currently shows evaluation results for valid expressions.
func computeInlayHints(f *file, celEnv *cel.Env) ([]protocol.InlayHint, error) {
	if f.content == "" {
		return []protocol.InlayHint{}, nil
	}

	parsed, issues := celEnv.Parse(f.content)
	if issues.Err() != nil {
		return []protocol.InlayHint{}, nil
	}

	// Type-check to ensure the expression is valid and get the type.
	checked, checkIssues := celEnv.Check(parsed)
	if checkIssues.Err() != nil {
		return []protocol.InlayHint{}, nil
	}

	// Try to evaluate the entire expression
	result, err := tryEvaluateExpression(f.content, celEnv)
	if err != nil {
		return []protocol.InlayHint{}, nil
	}

	// Get the type of the expression
	exprType := checked.OutputType()
	typeStr := exprType.String()

	// Create a hint at the end of the content (before any trailing newline)
	contentLen := len(strings.TrimRight(f.content, "\n\r"))
	endLine, endCol := byteOffsetToLineCol(f.content, contentLen)

	hint := protocol.InlayHint{
		Position: protocol.Position{Line: endLine, Character: endCol},
		Label: []protocol.InlayHintLabelPart{
			{
				Value: fmt.Sprintf("→ %s (%s)", result, typeStr),
			},
		},
		Kind:        protocol.InlayHintKind(1), // Type hint kind
		PaddingLeft: true,
	}

	return []protocol.InlayHint{hint}, nil
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
