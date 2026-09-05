package lsp

import (
	"cel.dev/cel-go/common/ast"
	"go.lsp.dev/protocol"
)

// lspRange converts a [byteStart, byteEnd) range within content to an LSP
// range, whose columns are UTF-16 code units.
func lspRange(content string, byteStart, byteEnd int) protocol.Range {
	startLine, startCol := byteOffsetToLineCol(content, byteStart)
	endLine, endCol := byteOffsetToLineCol(content, byteEnd)
	return protocol.Range{
		Start: protocol.Position{Line: startLine, Character: startCol},
		End:   protocol.Position{Line: endLine, Character: endCol},
	}
}

// rangeOfExpr returns the LSP range covering the source text of the expression
// with the given ID. It reports false when the parser recorded no source range
// for that ID, which happens for expressions synthesized by macro expansion.
func rangeOfExpr(content string, sourceInfo *ast.SourceInfo, id int64) (protocol.Range, bool) {
	offsetRange, ok := sourceInfo.GetOffsetRange(id)
	if !ok {
		return protocol.Range{}, false
	}
	byteStart, byteEnd := celOffsetRangeToByteRange(content, offsetRange)
	return lspRange(content, byteStart, byteEnd), true
}

// findExprByID returns the expression with the given ID, or nil if the subtree
// rooted at expr does not contain it.
func findExprByID(expr ast.Expr, targetID int64) ast.Expr {
	var found ast.Expr
	ast.PreOrderVisit(expr, ast.NewExprVisitor(func(e ast.Expr) {
		if found == nil && e.ID() == targetID {
			found = e
		}
	}))
	return found
}

// identifierRanges returns the range of every reference to identName within
// the subtree rooted at expr.
func identifierRanges(expr ast.Expr, sourceInfo *ast.SourceInfo, content string, identName string) []protocol.Range {
	var ranges []protocol.Range
	ast.PreOrderVisit(expr, ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() != ast.IdentKind || e.AsIdent() != identName {
			return
		}
		if r, ok := rangeOfExpr(content, sourceInfo, e.ID()); ok {
			ranges = append(ranges, r)
		}
	}))
	return ranges
}

// identifierOccurrences returns the range of every occurrence of identName
// within the scope it belongs to: the whole expression for a top-level
// identifier, or just the enclosing comprehension for a loop variable.
//
// References, document highlights and renames all act on exactly this set of
// ranges, and share this function so that the three cannot disagree about what
// a name refers to.
func identifierOccurrences(expr ast.Expr, sourceInfo *ast.SourceInfo, content string, s scope, identName string) []protocol.Range {
	sc, ok := s.(loopVarScope)
	if !ok {
		// topLevelScope: the identifier refers to the same thing everywhere.
		return identifierRanges(expr, sourceInfo, content, identName)
	}

	comp := findExprByID(expr, sc.comprehensionID)
	if comp == nil {
		return nil
	}
	// A macro the parser left in call form, e.g. list.map(x, x * 2), keeps the
	// loop variable as the call's first argument. An expanded macro is a
	// ComprehensionKind, whose iteration variable is not an expression at all.
	if comp.Kind() == ast.CallKind {
		return macroCallOccurrences(comp.AsCall(), sourceInfo, content, identName)
	}
	if comp.Kind() == ast.ComprehensionKind {
		return comprehensionOccurrences(comp, sourceInfo, content, identName)
	}
	return nil
}

// macroCallOccurrences returns the occurrences of identName within an
// unexpanded macro call, whose first argument declares the loop variable.
func macroCallOccurrences(call ast.CallExpr, sourceInfo *ast.SourceInfo, content string, identName string) []protocol.Range {
	args := call.Args()
	if len(args) < 2 {
		return nil
	}

	var ranges []protocol.Range
	// The declaration itself, e.g. the "x" in .map(x, ...).
	if decl := args[0]; decl.Kind() == ast.IdentKind && decl.AsIdent() == identName {
		if r, ok := rangeOfExpr(content, sourceInfo, decl.ID()); ok {
			ranges = append(ranges, r)
		}
	}
	// Every use of it in the macro body.
	for _, arg := range args[1:] {
		ranges = append(ranges, identifierRanges(arg, sourceInfo, content, identName)...)
	}
	return ranges
}

// comprehensionOccurrences returns the occurrences of identName within an
// expanded macro.
func comprehensionOccurrences(expr ast.Expr, sourceInfo *ast.SourceInfo, content string, identName string) []protocol.Range {
	comp := expr.AsComprehension()

	var ranges []protocol.Range
	// Macro expansion drops the iteration variable's declaration: it is named
	// by IterVar() but has no expression, and so no source range, of its own.
	// Recover it from the source text, where it is the first word-delimited
	// occurrence of the name after the range being iterated over.
	if comp.IterVar() == identName {
		var searchFrom int
		if iterRange, ok := sourceInfo.GetOffsetRange(comp.IterRange().ID()); ok {
			_, searchFrom = celOffsetRangeToByteRange(content, iterRange)
		}
		if start, end, ok := findWord(content, identName, searchFrom); ok {
			ranges = append(ranges, lspRange(content, start, end))
		}
	}

	for _, sub := range []ast.Expr{
		comp.IterRange(),
		comp.AccuInit(),
		comp.LoopCondition(),
		comp.LoopStep(),
		comp.Result(),
	} {
		ranges = append(ranges, identifierRanges(sub, sourceInfo, content, identName)...)
	}
	return ranges
}

// findWord returns the byte range of the first occurrence of word in content
// at or after start that is delimited by non-identifier characters.
func findWord(content string, word string, start int) (wordStart, wordEnd int, found bool) {
	if word == "" {
		return 0, 0, false
	}
	for i := max(start, 0); i+len(word) <= len(content); i++ {
		if content[i:i+len(word)] != word {
			continue
		}
		end := i + len(word)
		if i > 0 && isIdentifierChar(rune(content[i-1])) {
			continue
		}
		if end < len(content) && isIdentifierChar(rune(content[end])) {
			continue
		}
		return i, end, true
	}
	return 0, 0, false
}
