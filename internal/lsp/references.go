package lsp

import (
	"encoding/json"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/ast"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

func (s *server) references(req *jsonrpc2.Request) (any, error) {
	var params protocol.ReferenceParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	if f == nil || f.content == "" {
		return nil, nil
	}

	return computeReferences(f, s.celEnv, params)
}

func computeReferences(f *file, celEnv *cel.Env, params protocol.ReferenceParams) ([]protocol.Location, error) {
	parsed, issues := celEnv.Parse(f.content)
	if issues.Err() != nil {
		return nil, nil
	}

	nativeAST := parsed.NativeRep()
	sourceInfo := nativeAST.SourceInfo()

	// Convert LSP position to byte offset
	targetOffset := lineColToByteOffset(f.content, params.Position.Line, params.Position.Character)
	if targetOffset < 0 || targetOffset >= len(f.content) {
		return nil, nil
	}

	// Find the identifier at the cursor position
	identInfo := findIdentifierAtPosition(nativeAST.Expr(), sourceInfo, f.content, targetOffset)
	if identInfo == nil {
		// Fallback: try to find a word boundary
		if targetOffset < len(f.content) {
			start := targetOffset
			for start > 0 && isIdentifierChar(rune(f.content[start-1])) {
				start--
			}
			end := targetOffset
			for end < len(f.content) && isIdentifierChar(rune(f.content[end])) {
				end++
			}
			if start < end {
				possibleName := f.content[start:end]
				// Try to find this name in the AST
				identInfo = findIdentifierByName(nativeAST.Expr(), sourceInfo, f.content, possibleName, targetOffset)
			}
		}
		if identInfo == nil {
			return nil, nil
		}
	}

	// Can't get references to functions
	if identInfo.kind == identifierKindFunction {
		return nil, nil
	}

	// Determine the scope of this identifier
	s := determineIdentifierScope(identInfo.exprID, identInfo.name, nativeAST.Expr(), sourceInfo, f.content)

	// Find all occurrences of this identifier within its scope
	locations := findAllReferences(nativeAST.Expr(), sourceInfo, f.content, s, identInfo.name, params.TextDocument.URI)

	return locations, nil
}

// findAllReferences collects all locations of the identifier within its scope.
func findAllReferences(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, s scope, identName string, uri protocol.DocumentURI) []protocol.Location {
	var locations []protocol.Location

	switch sc := s.(type) {
	case loopVarScope:
		// Find the comprehension and only search within its scope
		// First try to find it as a CallExpr (macro invocation)
		comp := findComprehensionByID(expr, sc.comprehensionID)
		if comp != nil {
			collectReferencesInComprehension(comp, sourceInfo, fileContent, identName, uri, &locations)
		} else {
			// Try to find it as a ComprehensionKind (expanded macro)
			compExpr := findComprehensionExprByID(expr, sc.comprehensionID)
			if compExpr != nil {
				collectReferencesInComprehensionExpr(compExpr, sourceInfo, fileContent, identName, uri, &locations)
			}
		}

	case topLevelScope:
		// Search entire expression
		locations = CollectIdentifierReferences(expr, sourceInfo, fileContent, identName, uri)
	}

	return locations
}

// collectReferencesInComprehension collects all occurrences of identName in a comprehension's expressions.
func collectReferencesInComprehension(comp ast.CallExpr, sourceInfo *ast.SourceInfo, fileContent string, identName string, uri protocol.DocumentURI, locations *[]protocol.Location) {
	if comp == nil || len(comp.Args()) < 2 {
		return
	}

	// The first argument is the loop variable declaration (e.g., "x" in ".map(x, ...)")
	// Collect it directly
	firstArg := comp.Args()[0]
	if firstArg.Kind() == ast.IdentKind && firstArg.AsIdent() == identName {
		offsetRange, hasOffset := sourceInfo.GetOffsetRange(firstArg.ID())
		if hasOffset {
			byteStart, byteEnd := celOffsetRangeToByteRange(fileContent, offsetRange)
			startLine, startCol := byteOffsetToLineCol(fileContent, byteStart)
			endLine, endCol := byteOffsetToLineCol(fileContent, byteEnd)

			*locations = append(*locations, protocol.Location{
				URI: uri,
				Range: protocol.Range{
					Start: protocol.Position{Line: startLine, Character: startCol},
					End:   protocol.Position{Line: endLine, Character: endCol},
				},
			})
		}
	}

	// The second argument onward are expressions that use the loop variable
	for i := 1; i < len(comp.Args()); i++ {
		collectedRefs := CollectIdentifierReferences(comp.Args()[i], sourceInfo, fileContent, identName, uri)
		*locations = append(*locations, collectedRefs...)
	}
}

// collectReferencesInComprehensionExpr collects references for an identifier within a ComprehensionKind expression.
func collectReferencesInComprehensionExpr(compExpr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, uri protocol.DocumentURI, locations *[]protocol.Location) {
	if compExpr == nil || compExpr.Kind() != ast.ComprehensionKind {
		return
	}

	comp := compExpr.AsComprehension()
	loopVarName := comp.IterVar()

	// Add references for the loop variable declaration itself (if it matches)
	if loopVarName == identName {
		// The loop variable itself doesn't have an offset range in ComprehensionKind,
		// so we search the source for it. We look for the first occurrence after the IterRange.
		iterOffset, hasOffset := sourceInfo.GetOffsetRange(comp.IterRange().ID())
		if hasOffset {
			_, byteStop := celOffsetRangeToByteRange(fileContent, iterOffset)

			// For comprehensions like [1,2,3].map(x, ...), the loop variable appears AFTER
			// the entire call. Find the first occurrence of loopVarName that is surrounded
			// by word boundaries and comes after the IterRange.
			for i := byteStop; i < len(fileContent)-len(loopVarName)+1; i++ {
				if fileContent[i:i+len(loopVarName)] == loopVarName {
					// Check word boundaries
					if (i == 0 || !isIdentifierChar(rune(fileContent[i-1]))) &&
						(i+len(loopVarName) >= len(fileContent) || !isIdentifierChar(rune(fileContent[i+len(loopVarName)]))) {
						byteStart := i
						byteEnd := i + len(loopVarName)
						startLine, startCol := byteOffsetToLineCol(fileContent, byteStart)
						endLine, endCol := byteOffsetToLineCol(fileContent, byteEnd)

						*locations = append(*locations, protocol.Location{
							URI: uri,
							Range: protocol.Range{
								Start: protocol.Position{Line: startLine, Character: startCol},
								End:   protocol.Position{Line: endLine, Character: endCol},
							},
						})
						break // Found the first occurrence after IterRange
					}
				}
			}
		}
	}

	// Collect all occurrences in the comprehension's expressions
	collectedRefs := CollectIdentifierReferences(comp.IterRange(), sourceInfo, fileContent, identName, uri)
	*locations = append(*locations, collectedRefs...)
	collectedRefs = CollectIdentifierReferences(comp.AccuInit(), sourceInfo, fileContent, identName, uri)
	*locations = append(*locations, collectedRefs...)
	collectedRefs = CollectIdentifierReferences(comp.LoopCondition(), sourceInfo, fileContent, identName, uri)
	*locations = append(*locations, collectedRefs...)
	collectedRefs = CollectIdentifierReferences(comp.LoopStep(), sourceInfo, fileContent, identName, uri)
	*locations = append(*locations, collectedRefs...)
	collectedRefs = CollectIdentifierReferences(comp.Result(), sourceInfo, fileContent, identName, uri)
	*locations = append(*locations, collectedRefs...)
}
