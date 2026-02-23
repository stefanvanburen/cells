package lsp

import (
	"encoding/json"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/ast"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

func (s *server) signatureHelp(req *jsonrpc2.Request) (any, error) {
	var params protocol.SignatureHelpParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	if f == nil || f.content == "" {
		return nil, nil
	}

	return computeSignatureHelp(f, s.celEnv, params.Position)
}

func computeSignatureHelp(f *file, celEnv *cel.Env, pos protocol.Position) (*protocol.SignatureHelp, error) {
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

	// Find the call expression that contains the cursor position.
	call, paramIndex := findCallAtPosition(nativeAST.Expr(), sourceInfo, f.content, targetOffset)
	if call == nil {
		return nil, nil
	}

	funcName := call.FunctionName()

	// Look up the function in the CEL environment.
	funcs := celEnv.Functions()
	funcDecl, ok := funcs[funcName]
	if !ok {
		// Unknown function - no signature help
		return nil, nil
	}

	// Generate signatures from the function declaration, filtered by call type.
	sigs := generateSignatures(funcDecl, call.IsMemberFunction())
	if len(sigs) == 0 {
		return nil, nil
	}

	// Return the first signature as the active one, with the computed active parameter.
	return &protocol.SignatureHelp{
		Signatures:      sigs,
		ActiveSignature: 0,
		ActiveParameter: paramIndex,
	}, nil
}

// findCallAtPosition walks the AST to find a call expression that contains the cursor,
// and returns the call and the active parameter index (0-based).
func findCallAtPosition(expr ast.Expr, sourceInfo *ast.SourceInfo, exprString string, targetOffset int) (ast.CallExpr, uint32) {
	var result ast.CallExpr
	var paramIndex uint32
	var bestByteRange [2]int

	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		if e == nil {
			return
		}

		if e.Kind() == ast.CallKind {
			call := e.AsCall()
			offsetRange, hasOffset := sourceInfo.GetOffsetRange(e.ID())

			// For a call expression, we want to find the opening paren and everything after.
			// The offsetRange might just be the function name part.
			if hasOffset {
				byteStart, _ := celOffsetRangeToByteRange(exprString, offsetRange)

				// Find the opening paren from byteStart
				parenIdx := strings.Index(exprString[byteStart:], "(")

				if parenIdx >= 0 {
					// Extend the range to include the whole call (from opening paren to closing paren)
					parenStart := byteStart + parenIdx
					parenEnd := parenStart + 1
					depth := 1
					for parenEnd < len(exprString) && depth > 0 {
						if exprString[parenEnd] == '(' {
							depth++
						} else if exprString[parenEnd] == ')' {
							depth--
						}
						parenEnd++
					}

					// Check if cursor is inside the parentheses
					if targetOffset >= parenStart && targetOffset < parenEnd {
						// Prefer more specific (smaller) ranges
						callRange := parenEnd - parenStart
						if result == nil || callRange < (bestByteRange[1]-bestByteRange[0]) {
							result = call
							paramIndex = countParametersBeforeCursor(exprString, parenStart, targetOffset, call)
							bestByteRange = [2]int{parenStart, parenEnd}
						}
					}
				}
			}

			// Walk arguments.
			for _, arg := range call.Args() {
				walk(arg)
			}
			if call.IsMemberFunction() {
				walk(call.Target())
			}
		} else if e.Kind() == ast.ListKind {
			for _, elem := range e.AsList().Elements() {
				walk(elem)
			}
		} else if e.Kind() == ast.MapKind {
			for _, entry := range e.AsMap().Entries() {
				mapEntry := entry.AsMapEntry()
				walk(mapEntry.Key())
				walk(mapEntry.Value())
			}
		} else if e.Kind() == ast.StructKind {
			for _, field := range e.AsStruct().Fields() {
				walk(field.AsStructField().Value())
			}
		} else if e.Kind() == ast.SelectKind {
			sel := e.AsSelect()
			if sel.Operand() != nil {
				walk(sel.Operand())
			}
		} else if e.Kind() == ast.ComprehensionKind {
			comp := e.AsComprehension()
			walk(comp.IterRange())
			walk(comp.AccuInit())
			walk(comp.LoopCondition())
			walk(comp.LoopStep())
			walk(comp.Result())
		}
	}

	walk(expr)
	return result, paramIndex
}

// countParametersBeforeCursor determines which parameter the cursor is on,
// by counting commas before the cursor within the argument list.
func countParametersBeforeCursor(exprString string, callByteStart, cursorOffset int, call ast.CallExpr) uint32 {
	// Find the opening paren.
	openParenIdx := strings.Index(exprString[callByteStart:], "(")
	if openParenIdx == -1 {
		return 0
	}
	openParenIdx += callByteStart

	// Count commas and parenthesis depth from opening paren to cursor.
	paramIndex := uint32(0)
	depth := 0

	for i := openParenIdx + 1; i < len(exprString) && i < cursorOffset; i++ {
		switch exprString[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				paramIndex++
			}
		}
	}

	// If the call has no arguments, return 0.
	if len(call.Args()) == 0 && paramIndex > 0 {
		return 0
	}

	// Don't exceed the actual number of parameters.
	if int(paramIndex) >= len(call.Args()) {
		return uint32(len(call.Args()) - 1)
	}

	return paramIndex
}

// generateSignatures creates protocol.SignatureInformation for overloads of a function.
// funcDecl must implement the interface with Documentation() method.
// If isMemberFunction is true, only member function overloads are included;
// if false, only global function overloads are included.
func generateSignatures(funcDecl any, isMemberFunction bool) []protocol.SignatureInformation {
	// Try to get documentation if available.
	var doc *common.Doc
	if documenter, ok := funcDecl.(interface{ Documentation() *common.Doc }); ok {
		doc = documenter.Documentation()
	}

	if doc == nil {
		// Fallback for functions without documentation.
		return []protocol.SignatureInformation{{
			Label: "function()",
		}}
	}

	// If the main doc has a signature, use it as the primary signature.
	// Check if it matches the expected call type.
	if doc.Signature != "" {
		if isSignatureMatchingCallType(doc.Signature, isMemberFunction) {
			sig := protocol.SignatureInformation{
				Label:      doc.Signature,
				Parameters: extractParametersFromSignature(doc.Signature, doc),
			}
			if doc.Description != "" {
				sig.Documentation = &protocol.Or_SignatureInformation_documentation{
					Value: doc.Description,
				}
			}
			return []protocol.SignatureInformation{sig}
		}
	}

	// Use child signatures as overloads, filtering by call type.
	var sigs []protocol.SignatureInformation
	for _, child := range doc.Children {
		if child.Signature != "" && isSignatureMatchingCallType(child.Signature, isMemberFunction) {
			sig := protocol.SignatureInformation{
				Label:      child.Signature,
				Parameters: extractParametersFromSignature(child.Signature, child),
			}
			if child.Description != "" {
				sig.Documentation = &protocol.Or_SignatureInformation_documentation{
					Value: child.Description,
				}
			}
			sigs = append(sigs, sig)
		}
	}

	if len(sigs) > 0 {
		return sigs
	}

	// Final fallback.
	return []protocol.SignatureInformation{{
		Label: "function()",
	}}
}

// isSignatureMatchingCallType checks if a signature matches the call type.
// Member function signatures typically have a receiver (e.g., "string.matches(string) -> bool").
// Global function signatures don't (e.g., "matches(string, string) -> bool").
func isSignatureMatchingCallType(signature string, isMemberFunction bool) bool {
	// A simple heuristic: member functions have a dot before the opening paren.
	// E.g., "string.matches(string) -> bool" contains a dot.
	before, _, ok := strings.Cut(signature, "(")
	if !ok {
		return true // Can't determine, so include it
	}

	beforeParen := before
	hasDot := strings.Contains(beforeParen, ".")

	// If it's a member call, we want signatures with dots.
	// If it's a global call, we want signatures without dots.
	return hasDot == isMemberFunction
}

// extractParametersFromSignature parses parameter information from a signature string.
// For now, this is a simple implementation that extracts parameter names from the signature.
func extractParametersFromSignature(signature string, doc *common.Doc) []protocol.ParameterInformation {
	// Simple extraction: assume signature is like "func(param1, param2) -> type"
	openIdx := strings.Index(signature, "(")
	closeIdx := strings.LastIndex(signature, ")")
	if openIdx == -1 || closeIdx == -1 || openIdx >= closeIdx {
		return nil
	}

	paramsStr := signature[openIdx+1 : closeIdx]
	if paramsStr == "" {
		return nil
	}

	// Split by comma, handling nested parens/brackets.
	var params []protocol.ParameterInformation
	var current strings.Builder
	depth := 0

	for _, ch := range paramsStr {
		switch ch {
		case '(':
			depth++
			current.WriteRune(ch)
		case ')':
			depth--
			current.WriteRune(ch)
		case ',':
			if depth == 0 {
				paramStr := strings.TrimSpace(current.String())
				if paramStr != "" {
					paramName := extractParamName(paramStr)
					params = append(params, protocol.ParameterInformation{
						Label: protocol.Or_ParameterInformation_label{
							Value: paramName,
						},
					})
				}
				current.Reset()
			} else {
				current.WriteRune(ch)
			}
		default:
			current.WriteRune(ch)
		}
	}

	// Add the last parameter.
	paramStr := strings.TrimSpace(current.String())
	if paramStr != "" {
		paramName := extractParamName(paramStr)
		params = append(params, protocol.ParameterInformation{
			Label: protocol.Or_ParameterInformation_label{
				Value: paramName,
			},
		})
	}

	return params
}

// extractParamName extracts just the parameter name from a parameter declaration like "string x" or "string".
func extractParamName(paramDecl string) string {
	// For now, return the last word (the parameter name, if present).
	parts := strings.Fields(paramDecl)
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return paramDecl
}
