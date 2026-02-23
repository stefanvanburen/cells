package lsp

import (
	"github.com/google/cel-go/common/ast"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// IdentifierVisitor is a callback function that processes each identifier found in the AST.
// It returns true if the walk should continue, false to stop.
type IdentifierVisitor func(expr ast.Expr, identName string) bool

// IdentifierHandler is a callback for processing identifier occurrences with full context.
type IdentifierHandler func(expr ast.Expr, identName string, sourceInfo *ast.SourceInfo, fileContent string)

// CollectIdentifierOccurrences walks the AST and collects all TextEdits for renaming an identifier.
func CollectIdentifierOccurrences(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, newName string) []protocol.TextEdit {
	var edits []protocol.TextEdit
	collectOccurrencesInExpr(expr, sourceInfo, fileContent, identName, newName, &edits)
	return edits
}

// CollectIdentifierHighlights walks the AST and collects all highlight ranges for an identifier.
func CollectIdentifierHighlights(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string) []protocol.DocumentHighlight {
	var highlights []protocol.DocumentHighlight
	collectHighlightsInExpr(expr, sourceInfo, fileContent, identName, &highlights)
	return highlights
}

// CollectIdentifierReferences walks the AST and collects all locations for an identifier.
func CollectIdentifierReferences(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, uri protocol.DocumentURI) []protocol.Location {
	var locations []protocol.Location
	collectReferencesInExpr(expr, sourceInfo, fileContent, identName, uri, &locations)
	return locations
}

// collectOccurrencesInExpr recursively collects all occurrences of identName.
func collectOccurrencesInExpr(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, newName string, edits *[]protocol.TextEdit) {
	if expr == nil {
		return
	}

	if expr.Kind() == ast.IdentKind && expr.AsIdent() == identName {
		offsetRange, hasOffset := sourceInfo.GetOffsetRange(expr.ID())
		if hasOffset {
			byteStart, byteEnd := celOffsetRangeToByteRange(fileContent, offsetRange)
			startLine, startCol := byteOffsetToLineCol(fileContent, byteStart)
			endLine, endCol := byteOffsetToLineCol(fileContent, byteEnd)

			*edits = append(*edits, protocol.TextEdit{
				Range: protocol.Range{
					Start: protocol.Position{Line: startLine, Character: startCol},
					End:   protocol.Position{Line: endLine, Character: endCol},
				},
				NewText: newName,
			})
		}
	}

	recurseAllExpr(expr, sourceInfo, fileContent, identName, newName, edits, collectOccurrencesInExpr)
}

// collectHighlightsInExpr recursively collects all highlight ranges for an identifier.
func collectHighlightsInExpr(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, highlights *[]protocol.DocumentHighlight) {
	if expr == nil {
		return
	}

	if expr.Kind() == ast.IdentKind && expr.AsIdent() == identName {
		offsetRange, hasOffset := sourceInfo.GetOffsetRange(expr.ID())
		if hasOffset {
			byteStart, byteEnd := celOffsetRangeToByteRange(fileContent, offsetRange)
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

	recurseAllHighlights(expr, sourceInfo, fileContent, identName, highlights)
}

// collectReferencesInExpr recursively collects all occurrences of identName.
func collectReferencesInExpr(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, uri protocol.DocumentURI, locations *[]protocol.Location) {
	if expr == nil {
		return
	}

	if expr.Kind() == ast.IdentKind && expr.AsIdent() == identName {
		offsetRange, hasOffset := sourceInfo.GetOffsetRange(expr.ID())
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

	recurseAllReferences(expr, sourceInfo, fileContent, identName, uri, locations)
}

// recurseAllExpr handles recursion for all expression types (for rename edits).
func recurseAllExpr(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, newName string, edits *[]protocol.TextEdit, callback func(ast.Expr, *ast.SourceInfo, string, string, string, *[]protocol.TextEdit)) {
	switch expr.Kind() {
	case ast.CallKind:
		call := expr.AsCall()
		for _, arg := range call.Args() {
			callback(arg, sourceInfo, fileContent, identName, newName, edits)
		}
		if call.IsMemberFunction() {
			callback(call.Target(), sourceInfo, fileContent, identName, newName, edits)
		}

	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			callback(elem, sourceInfo, fileContent, identName, newName, edits)
		}

	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			callback(mapEntry.Key(), sourceInfo, fileContent, identName, newName, edits)
			callback(mapEntry.Value(), sourceInfo, fileContent, identName, newName, edits)
		}

	case ast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			callback(field.AsStructField().Value(), sourceInfo, fileContent, identName, newName, edits)
		}

	case ast.SelectKind:
		sel := expr.AsSelect()
		if sel.Operand() != nil {
			callback(sel.Operand(), sourceInfo, fileContent, identName, newName, edits)
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		callback(comp.IterRange(), sourceInfo, fileContent, identName, newName, edits)
		callback(comp.AccuInit(), sourceInfo, fileContent, identName, newName, edits)
		callback(comp.LoopCondition(), sourceInfo, fileContent, identName, newName, edits)
		callback(comp.LoopStep(), sourceInfo, fileContent, identName, newName, edits)
		callback(comp.Result(), sourceInfo, fileContent, identName, newName, edits)
	}
}

// recurseAllHighlights handles recursion for all expression types (for highlights).
func recurseAllHighlights(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, highlights *[]protocol.DocumentHighlight) {
	switch expr.Kind() {
	case ast.CallKind:
		call := expr.AsCall()
		for _, arg := range call.Args() {
			collectHighlightsInExpr(arg, sourceInfo, fileContent, identName, highlights)
		}
		if call.IsMemberFunction() {
			collectHighlightsInExpr(call.Target(), sourceInfo, fileContent, identName, highlights)
		}

	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			collectHighlightsInExpr(elem, sourceInfo, fileContent, identName, highlights)
		}

	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			collectHighlightsInExpr(mapEntry.Key(), sourceInfo, fileContent, identName, highlights)
			collectHighlightsInExpr(mapEntry.Value(), sourceInfo, fileContent, identName, highlights)
		}

	case ast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			collectHighlightsInExpr(field.AsStructField().Value(), sourceInfo, fileContent, identName, highlights)
		}

	case ast.SelectKind:
		sel := expr.AsSelect()
		if sel.Operand() != nil {
			collectHighlightsInExpr(sel.Operand(), sourceInfo, fileContent, identName, highlights)
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		collectHighlightsInExpr(comp.IterRange(), sourceInfo, fileContent, identName, highlights)
		collectHighlightsInExpr(comp.AccuInit(), sourceInfo, fileContent, identName, highlights)
		collectHighlightsInExpr(comp.LoopCondition(), sourceInfo, fileContent, identName, highlights)
		collectHighlightsInExpr(comp.LoopStep(), sourceInfo, fileContent, identName, highlights)
		collectHighlightsInExpr(comp.Result(), sourceInfo, fileContent, identName, highlights)
	}
}

// recurseAllReferences handles recursion for all expression types (for references).
func recurseAllReferences(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, uri protocol.DocumentURI, locations *[]protocol.Location) {
	switch expr.Kind() {
	case ast.CallKind:
		call := expr.AsCall()
		for _, arg := range call.Args() {
			collectReferencesInExpr(arg, sourceInfo, fileContent, identName, uri, locations)
		}
		if call.IsMemberFunction() {
			collectReferencesInExpr(call.Target(), sourceInfo, fileContent, identName, uri, locations)
		}

	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			collectReferencesInExpr(elem, sourceInfo, fileContent, identName, uri, locations)
		}

	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			collectReferencesInExpr(mapEntry.Key(), sourceInfo, fileContent, identName, uri, locations)
			collectReferencesInExpr(mapEntry.Value(), sourceInfo, fileContent, identName, uri, locations)
		}

	case ast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			collectReferencesInExpr(field.AsStructField().Value(), sourceInfo, fileContent, identName, uri, locations)
		}

	case ast.SelectKind:
		sel := expr.AsSelect()
		if sel.Operand() != nil {
			collectReferencesInExpr(sel.Operand(), sourceInfo, fileContent, identName, uri, locations)
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		collectReferencesInExpr(comp.IterRange(), sourceInfo, fileContent, identName, uri, locations)
		collectReferencesInExpr(comp.AccuInit(), sourceInfo, fileContent, identName, uri, locations)
		collectReferencesInExpr(comp.LoopCondition(), sourceInfo, fileContent, identName, uri, locations)
		collectReferencesInExpr(comp.LoopStep(), sourceInfo, fileContent, identName, uri, locations)
		collectReferencesInExpr(comp.Result(), sourceInfo, fileContent, identName, uri, locations)
	}
}

// WalkIdentifiers walks the AST and calls visitor for each identifier.
// It handles both regular identifiers and loop variables in comprehensions.
func WalkIdentifiers(expr ast.Expr, visitor IdentifierVisitor) {
	walkIdentifiersImpl(expr, visitor)
}

func walkIdentifiersImpl(expr ast.Expr, visitor IdentifierVisitor) bool {
	if expr == nil {
		return true
	}

	// Check for identifier
	if expr.Kind() == ast.IdentKind {
		if !visitor(expr, expr.AsIdent()) {
			return false
		}
	}

	// Check for loop variables in comprehensions
	if expr.Kind() == ast.ComprehensionKind {
		comp := expr.AsComprehension()
		loopVar := comp.IterVar()
		if !visitor(expr, loopVar) {
			return false
		}
	}

	// Recurse into children
	switch expr.Kind() {
	case ast.CallKind:
		call := expr.AsCall()
		for _, arg := range call.Args() {
			if !walkIdentifiersImpl(arg, visitor) {
				return false
			}
		}
		if call.IsMemberFunction() {
			if !walkIdentifiersImpl(call.Target(), visitor) {
				return false
			}
		}

	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			if !walkIdentifiersImpl(elem, visitor) {
				return false
			}
		}

	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			if !walkIdentifiersImpl(mapEntry.Key(), visitor) {
				return false
			}
			if !walkIdentifiersImpl(mapEntry.Value(), visitor) {
				return false
			}
		}

	case ast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			if !walkIdentifiersImpl(field.AsStructField().Value(), visitor) {
				return false
			}
		}

	case ast.SelectKind:
		sel := expr.AsSelect()
		if sel.Operand() != nil {
			if !walkIdentifiersImpl(sel.Operand(), visitor) {
				return false
			}
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		if !walkIdentifiersImpl(comp.IterRange(), visitor) {
			return false
		}
		if !walkIdentifiersImpl(comp.AccuInit(), visitor) {
			return false
		}
		if !walkIdentifiersImpl(comp.LoopCondition(), visitor) {
			return false
		}
		if !walkIdentifiersImpl(comp.LoopStep(), visitor) {
			return false
		}
		if !walkIdentifiersImpl(comp.Result(), visitor) {
			return false
		}
	}

	return true
}
