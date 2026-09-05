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
// with the given ID. It reports false when there is no such text: the parser
// records no range at all for some expressions macro expansion synthesizes,
// and an empty one for others, and neither stands for anything a user wrote.
func rangeOfExpr(content string, sourceInfo *ast.SourceInfo, id int64) (protocol.Range, bool) {
	byteStart, byteEnd, ok := exprByteRange(content, sourceInfo, id)
	if !ok {
		return protocol.Range{}, false
	}
	return lspRange(content, byteStart, byteEnd), true
}

// exprByteRange returns the byte range of an expression's source text, and
// whether it has any.
func exprByteRange(content string, sourceInfo *ast.SourceInfo, id int64) (byteStart, byteEnd int, ok bool) {
	offsetRange, hasRange := sourceInfo.GetOffsetRange(id)
	if !hasRange {
		return 0, 0, false
	}
	byteStart, byteEnd = celOffsetRangeToByteRange(content, offsetRange)
	if byteEnd <= byteStart {
		return 0, 0, false
	}
	return byteStart, byteEnd, true
}

// macroDeclarationRange returns the range of the name a macro binds.
//
// Expanding a macro drops the declaration: [1, 2].map(x, ...) becomes a
// comprehension that knows the name x but holds no expression for where it was
// written. What the parser keeps is the call the macro was written as, whose
// first argument is that declaration, with its own source range.
func macroDeclaration(sourceInfo *ast.SourceInfo, macroID int64) (name string, declID int64, ok bool) {
	call, retained := sourceInfo.GetMacroCall(macroID)
	if !retained || call.Kind() != ast.CallKind {
		return "", 0, false
	}
	args := call.AsCall().Args()
	if len(args) == 0 || args[0].Kind() != ast.IdentKind {
		return "", 0, false
	}
	return args[0].AsIdent(), args[0].ID(), true
}

// macroDeclarationRange returns the range of the declaration of identName in
// the macro with the given ID.
func macroDeclarationRange(sourceInfo *ast.SourceInfo, content string, macroID int64, identName string) (protocol.Range, bool) {
	name, declID, ok := macroDeclaration(sourceInfo, macroID)
	if !ok || name != identName {
		return protocol.Range{}, false
	}
	return rangeOfExpr(content, sourceInfo, declID)
}

// comprehensionBinds reports whether an expanded macro binds identName.
//
// A comprehension names its iteration variable directly, but cel.bind does not
// — it binds through the accumulator and leaves IterVar as a placeholder — so
// the call the macro was written as has the last word.
func comprehensionBinds(sourceInfo *ast.SourceInfo, expr ast.Expr, identName string) bool {
	if expr.Kind() != ast.ComprehensionKind {
		return false
	}
	if expr.AsComprehension().IterVar() == identName {
		return true
	}
	name, _, ok := macroDeclaration(sourceInfo, expr.ID())
	return ok && name == identName
}

// macroName returns the name of the macro an expanded comprehension came from,
// or "" when the parser did not retain the call.
func macroName(sourceInfo *ast.SourceInfo, macroID int64) string {
	call, ok := sourceInfo.GetMacroCall(macroID)
	if !ok || call.Kind() != ast.CallKind {
		return ""
	}
	return call.AsCall().FunctionName()
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
	// The declaration, which the expansion dropped.
	if decl, ok := macroDeclarationRange(sourceInfo, content, expr.ID(), identName); ok {
		ranges = append(ranges, decl)
	} else if comp.IterVar() == identName {
		// No call was retained — macro call tracking can be off in an
		// environment cells did not build. Fall back to the source text, where
		// the declaration is the first word-delimited occurrence of the name
		// after the range being iterated over.
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
