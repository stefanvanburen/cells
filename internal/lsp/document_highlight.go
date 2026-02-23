package lsp

import (
	"encoding/json"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/ast"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

func (s *server) documentHighlight(req *jsonrpc2.Request) (any, error) {
	var params protocol.DocumentHighlightParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	if f == nil || f.content == "" {
		return nil, nil
	}

	return computeDocumentHighlight(f, s.celEnv, params)
}

func computeDocumentHighlight(f *file, celEnv *cel.Env, params protocol.DocumentHighlightParams) ([]protocol.DocumentHighlight, error) {
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

	// Determine the scope of this identifier
	s := determineIdentifierScope(identInfo.exprID, identInfo.name, nativeAST.Expr(), sourceInfo, f.content)

	// Find all occurrences of this identifier within its scope
	highlights := collectHighlights(nativeAST.Expr(), sourceInfo, f.content, s, identInfo.name)

	return highlights, nil
}

// collectHighlights collects all highlight ranges for an identifier within its scope.
func collectHighlights(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, s scope, identName string) []protocol.DocumentHighlight {
	var highlights []protocol.DocumentHighlight

	switch sc := s.(type) {
	case loopVarScope:
		// Find the comprehension and only search within its scope
		// First try to find it as a CallExpr (macro invocation)
		comp := findComprehensionByID(expr, sc.comprehensionID)
		if comp != nil {
			collectHighlightsInComprehension(comp, sourceInfo, fileContent, identName, &highlights)
		} else {
			// Try to find it as a ComprehensionKind (expanded macro)
			compExpr := findComprehensionExprByID(expr, sc.comprehensionID)
			if compExpr != nil {
				collectHighlightsInComprehensionExpr(compExpr, sourceInfo, fileContent, identName, &highlights)
			}
		}

	case topLevelScope:
		// Search entire expression
		highlights = CollectIdentifierHighlights(expr, sourceInfo, fileContent, identName)
	}

	return highlights
}

// collectHighlightsInComprehension collects all highlight ranges in a comprehension's expressions.
func collectHighlightsInComprehension(comp ast.CallExpr, sourceInfo *ast.SourceInfo, fileContent string, identName string, highlights *[]protocol.DocumentHighlight) {
	if comp == nil || len(comp.Args()) < 2 {
		return
	}

	// The first argument is the loop variable declaration (e.g., "x" in ".map(x, ...)")
	firstArg := comp.Args()[0]
	if firstArg.Kind() == ast.IdentKind && firstArg.AsIdent() == identName {
		offsetRange, hasOffset := sourceInfo.GetOffsetRange(firstArg.ID())
		if hasOffset {
			// Loop variable has an offset range - use it directly
			byteStart, byteEnd := celOffsetRangeToByteRange(fileContent, offsetRange)
			startLine, startCol := byteOffsetToLineCol(fileContent, byteStart)
			endLine, endCol := byteOffsetToLineCol(fileContent, byteEnd)

			*highlights = append(*highlights, protocol.DocumentHighlight{
				Range: protocol.Range{
					Start: protocol.Position{Line: startLine, Character: startCol},
					End:   protocol.Position{Line: endLine, Character: endCol},
				},
			})
		} else {
			// Loop variable doesn't have an offset range - search for it in the source
			// Search within the entire file as a fallback
			loopVarIdx := strings.Index(fileContent, identName)
			if loopVarIdx >= 0 {
				byteStart := loopVarIdx
				byteEnd := byteStart + len(identName)
				// Verify it's a word boundary
				if (byteStart == 0 || !isIdentifierChar(rune(fileContent[byteStart-1]))) &&
					(byteEnd >= len(fileContent) || !isIdentifierChar(rune(fileContent[byteEnd]))) {
					startLine, startCol := byteOffsetToLineCol(fileContent, byteStart)
					endLine, endCol := byteOffsetToLineCol(fileContent, byteEnd)

					*highlights = append(*highlights, protocol.DocumentHighlight{
						Range: protocol.Range{
							Start: protocol.Position{Line: startLine, Character: startCol},
							End:   protocol.Position{Line: endLine, Character: endCol},
						},
					})
				}
			}
		}
	}

	// The second argument onward are expressions that use the loop variable
	for i := 1; i < len(comp.Args()); i++ {
		collectedHighlights := CollectIdentifierHighlights(comp.Args()[i], sourceInfo, fileContent, identName)
		*highlights = append(*highlights, collectedHighlights...)
	}
}

// collectHighlightsInComprehensionExpr collects highlights for an identifier within a ComprehensionKind expression.
func collectHighlightsInComprehensionExpr(compExpr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, highlights *[]protocol.DocumentHighlight) {
	if compExpr == nil || compExpr.Kind() != ast.ComprehensionKind {
		return
	}

	comp := compExpr.AsComprehension()
	loopVarName := comp.IterVar()

	// Add highlights for the loop variable declaration itself (if it matches)
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

						*highlights = append(*highlights, protocol.DocumentHighlight{
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
	collectedHighlights := CollectIdentifierHighlights(comp.IterRange(), sourceInfo, fileContent, identName)
	*highlights = append(*highlights, collectedHighlights...)
	collectedHighlights = CollectIdentifierHighlights(comp.AccuInit(), sourceInfo, fileContent, identName)
	*highlights = append(*highlights, collectedHighlights...)
	collectedHighlights = CollectIdentifierHighlights(comp.LoopCondition(), sourceInfo, fileContent, identName)
	*highlights = append(*highlights, collectedHighlights...)
	collectedHighlights = CollectIdentifierHighlights(comp.LoopStep(), sourceInfo, fileContent, identName)
	*highlights = append(*highlights, collectedHighlights...)
	collectedHighlights = CollectIdentifierHighlights(comp.Result(), sourceInfo, fileContent, identName)
	*highlights = append(*highlights, collectedHighlights...)
}
