package lsp

import (
	"maps"

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

// macroDeclarations returns the identifiers that declare the names an expanded
// macro binds: the x in [1, 2].map(x, x * 2), the k and v in m.all(k, v, v > 0),
// or the v in cel.bind(v, 1, v + v).
//
// Expanding a macro drops its declarations, leaving a comprehension that knows
// the names but holds no expression for where they were written. What the
// parser keeps is the call the macro was written as, whose leading arguments
// are those declarations, with source ranges of their own.
func macroDeclarations(sourceInfo *ast.SourceInfo, comp ast.Expr) []ast.Expr {
	call, retained := sourceInfo.GetMacroCall(comp.ID())
	if comp.Kind() != ast.ComprehensionKind || !retained || call.Kind() != ast.CallKind {
		return nil
	}
	c := comp.AsComprehension()
	declared := 1
	if c.HasIterVar2() {
		declared = 2
	}
	args := call.AsCall().Args()
	var decls []ast.Expr
	for _, arg := range args[:min(declared, len(args))] {
		if arg.Kind() != ast.IdentKind {
			continue
		}
		switch arg.AsIdent() {
		case c.IterVar(), c.IterVar2(), c.AccuVar():
			decls = append(decls, arg)
		}
	}
	return decls
}

// nameBindings maps each identifier in the expression rooted at root that a
// comprehension binds to that comprehension's ID. An identifier it leaves out
// names something the environment declares.
//
// A comprehension evaluates its range and initial accumulator outside itself,
// its condition and step with its iteration variables and accumulator bound,
// and its result with only the accumulator bound. cel.bind binds its name as
// the accumulator. An inner binding of a name shadows an outer one.
func nameBindings(root ast.Expr) map[int64]int64 {
	bindings := make(map[int64]int64)
	var walk func(e ast.Expr, scope map[string]int64)
	walk = func(e ast.Expr, scope map[string]int64) {
		switch e.Kind() {
		case ast.IdentKind:
			if id, bound := scope[e.AsIdent()]; bound {
				bindings[e.ID()] = id
			}
		case ast.SelectKind:
			walk(e.AsSelect().Operand(), scope)
		case ast.CallKind:
			call := e.AsCall()
			if call.IsMemberFunction() {
				walk(call.Target(), scope)
			}
			for _, arg := range call.Args() {
				walk(arg, scope)
			}
		case ast.ListKind:
			for _, elem := range e.AsList().Elements() {
				walk(elem, scope)
			}
		case ast.MapKind:
			for _, entry := range e.AsMap().Entries() {
				walk(entry.AsMapEntry().Key(), scope)
				walk(entry.AsMapEntry().Value(), scope)
			}
		case ast.StructKind:
			for _, field := range e.AsStruct().Fields() {
				walk(field.AsStructField().Value(), scope)
			}
		case ast.ComprehensionKind:
			c := e.AsComprehension()
			walk(c.IterRange(), scope)
			walk(c.AccuInit(), scope)
			withAccu := bind(scope, e.ID(), c.AccuVar())
			inLoop := bind(withAccu, e.ID(), c.IterVar(), c.IterVar2())
			walk(c.LoopCondition(), inLoop)
			walk(c.LoopStep(), inLoop)
			walk(c.Result(), withAccu)
		}
	}
	walk(root, map[string]int64{})
	return bindings
}

// bind returns scope with names bound to the comprehension with the given ID.
func bind(scope map[string]int64, id int64, names ...string) map[string]int64 {
	inner := maps.Clone(scope)
	for _, name := range names {
		if name != "" {
			inner[name] = id
		}
	}
	return inner
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

// identifierOccurrences returns the range of every occurrence of identName
// within the scope it belongs to: the identifiers that name what the
// environment declares, or a comprehension's declaration of the name and the
// identifiers it binds.
//
// References, document highlights and renames all act on exactly this set of
// ranges, and share this function so that the three cannot disagree about what
// a name refers to.
func identifierOccurrences(expr ast.Expr, sourceInfo *ast.SourceInfo, content string, s scope, identName string) []protocol.Range {
	var binder int64
	var ranges []protocol.Range
	if sc, ok := s.(loopVarScope); ok {
		binder = sc.comprehensionID
		comp := findExprByID(expr, binder)
		if comp == nil {
			return nil
		}
		for _, decl := range macroDeclarations(sourceInfo, comp) {
			if decl.AsIdent() != identName {
				continue
			}
			if r, ok := rangeOfExpr(content, sourceInfo, decl.ID()); ok {
				ranges = append(ranges, r)
			}
		}
	}

	bindings := nameBindings(expr)
	ast.PreOrderVisit(expr, ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() != ast.IdentKind || e.AsIdent() != identName || bindings[e.ID()] != binder {
			return
		}
		if r, ok := rangeOfExpr(content, sourceInfo, e.ID()); ok {
			ranges = append(ranges, r)
		}
	}))
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
