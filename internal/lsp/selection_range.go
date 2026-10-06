package lsp

import (
	"context"
	"sort"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/parser/gen"
	"go.lsp.dev/protocol"
)

func (s *server) SelectionRange(ctx context.Context, params *protocol.SelectionRangeParams) ([]protocol.SelectionRange, error) {
	f, docEnv := s.document(ctx, params.TextDocument.URI)
	if f == nil || docEnv == nil {
		return nil, nil
	}
	return computeSelectionRanges(f, docEnv.celEnv, params.Positions), nil
}

// computeSelectionRanges returns, for each position, the chain of expressions
// enclosing it, innermost first. A position outside every expression, or in a
// file that does not parse, gets just the empty range at itself, since the
// protocol asks for one answer per position.
func computeSelectionRanges(f *file, celEnv *cel.Env, positions []protocol.Position) []protocol.SelectionRange {
	var root *spanNode
	if nativeAST := f.ast(celEnv); nativeAST != nil {
		b := newSpanBuilder(newSourceTokens(f.content, nativeAST.SourceInfo()))
		root = b.build(nativeAST.Expr())
	}

	ranges := make([]protocol.SelectionRange, len(positions))
	for i, pos := range positions {
		ranges[i] = protocol.SelectionRange{Range: protocol.Range{Start: pos, End: pos}}
		offset := lineColToByteOffset(f.content, pos.Line, pos.Character)
		if root == nil || offset < 0 || !root.contains(offset) {
			continue
		}
		var innermost *protocol.SelectionRange
		for n := root; n != nil; n = n.childAt(offset) {
			r := lspRange(f.content, n.start, n.end)
			if innermost != nil && innermost.Range == r {
				continue
			}
			innermost = &protocol.SelectionRange{Range: r, Parent: innermost}
		}
		ranges[i] = *innermost
	}
	return ranges
}

// spanNode is a piece of source worth selecting as a whole: an expression, a
// map or message entry, or a name within one. Its children are the pieces
// inside it.
type spanNode struct {
	// start and end are byte offsets; start is -1 while the node is empty.
	start, end int
	children   []*spanNode
	// chain is the logical operator, if any, that the node joins its children
	// with, written without parentheses.
	chain string
}

func (n *spanNode) contains(offset int) bool {
	return n.start <= offset && offset <= n.end
}

// childAt returns the child containing offset, or nil. A child the offset
// falls strictly inside wins over one it merely touches the end of, so that
// the cursor at the + of a+b belongs to the +, not to a.
func (n *spanNode) childAt(offset int) *spanNode {
	var touching *spanNode
	for _, child := range n.children {
		switch {
		case child.start <= offset && offset < child.end:
			return child
		case offset == child.end && touching == nil:
			touching = child
		}
	}
	return touching
}

// spanBuilder builds the tree of spanNodes for an AST. The parser records a
// single token for each expression — the operator of a binary expression, the
// parenthesis of a call — so a span is assembled from the spans of the
// expression's parts and the tokens around them.
type spanBuilder struct {
	*sourceTokens
	partners map[int]int
}

func newSpanBuilder(t *sourceTokens) *spanBuilder {
	return &spanBuilder{sourceTokens: t, partners: t.brackets()}
}

// newNode returns a node spanning children, which may include nils.
func newNode(children ...*spanNode) *spanNode {
	n := &spanNode{start: -1}
	for _, child := range children {
		n.add(child)
	}
	return n
}

// add makes child a child of n, extending n to cover it.
func (n *spanNode) add(child *spanNode) {
	if child == nil || child.start < 0 {
		return
	}
	n.children = append(n.children, child)
	n.extend(child.start, child.end)
}

func (n *spanNode) extend(start, end int) {
	if n.start < 0 || start < n.start {
		n.start = start
	}
	n.end = max(n.end, end)
}

// cover extends n over the token at index i, if there is one.
func (b *spanBuilder) cover(n *spanNode, i int) {
	if i >= 0 && i < len(b.tokens) {
		n.extend(b.tokens[i].start, b.tokens[i].end)
	}
}

// leaf returns a node for the token at index i, or nil.
func (b *spanBuilder) leaf(i int) *spanNode {
	if i < 0 || i >= len(b.tokens) {
		return nil
	}
	return &spanNode{start: b.tokens[i].start, end: b.tokens[i].end}
}

// partner returns the index of the bracket matching the one at i, or -1.
func (b *spanBuilder) partner(i int) int {
	if j, ok := b.partners[i]; ok {
		return j
	}
	return -1
}

func (b *spanBuilder) build(expr ast.Expr) *spanNode {
	if expr == nil {
		return nil
	}
	// A macro is selected as the call it was written as, not its expansion.
	if call, ok := b.sourceInfo.GetMacroCall(expr.ID()); ok && call.Kind() == ast.CallKind {
		return b.group(b.call(expr.ID(), call.AsCall()))
	}

	var n *spanNode
	switch expr.Kind() {
	case ast.IdentKind, ast.LiteralKind:
		if start, end, ok := exprByteRange(b.content, b.sourceInfo, expr.ID()); ok {
			n = &spanNode{start: start, end: end}
		}

	case ast.SelectKind:
		n = newNode(b.build(expr.AsSelect().Operand()))
		if dot := b.anchor(expr.ID()); b.kind(dot) == gen.CELLexerDOT {
			n.add(b.leaf(b.step(dot, 1)))
		}

	case ast.CallKind:
		n = b.call(expr.ID(), expr.AsCall())

	case ast.ListKind:
		n = newNode()
		for _, elem := range expr.AsList().Elements() {
			n.add(b.build(elem))
		}
		b.coverBrackets(n, b.anchor(expr.ID()))

	case ast.MapKind:
		n = newNode()
		for _, entry := range expr.AsMap().Entries() {
			e := entry.AsMapEntry()
			n.add(newNode(b.build(e.Key()), b.build(e.Value())))
		}
		b.coverBrackets(n, b.anchor(expr.ID()))

	case ast.StructKind:
		n = newNode()
		brace := b.anchor(expr.ID())
		// The message name, possibly qualified, comes before the brace.
		name := newNode()
		for i := b.step(brace, -1); b.isName(i); {
			b.cover(name, i)
			dot := b.step(i, -1)
			if b.kind(dot) != gen.CELLexerDOT {
				break
			}
			b.cover(name, dot)
			i = b.step(dot, -1)
		}
		n.add(name)
		for _, field := range expr.AsStruct().Fields() {
			entry := newNode()
			// A field initializer is recorded at its colon.
			if colon := b.anchor(field.ID()); b.kind(colon) == gen.CELLexerCOLON {
				entry.add(b.leaf(b.step(colon, -1)))
			}
			entry.add(b.build(field.AsStructField().Value()))
			n.add(entry)
		}
		b.coverBrackets(n, brace)

	case ast.ComprehensionKind:
		// A comprehension the parser kept no call for; its range is the only
		// part known to be written in the source.
		n = newNode(b.build(expr.AsComprehension().IterRange()))
	}
	return b.group(n)
}

// call returns the node for a call, which may be an operator.
func (b *spanBuilder) call(id int64, call ast.CallExpr) *spanNode {
	n := newNode()
	if call.IsMemberFunction() {
		n.add(b.build(call.Target()))
	}
	anchor := b.anchor(id)
	if b.kind(anchor) == gen.CELLexerLPAREN {
		// A function is recorded at its parenthesis, after its name.
		n.add(b.leaf(b.step(anchor, -1)))
	}
	// The parser balances a chain like a && b && c && d into a tree, whose
	// subtrees are not anything written; select the chain as one.
	fn := call.FunctionName()
	if fn == operators.LogicalAnd || fn == operators.LogicalOr {
		n.chain = fn
	}
	for _, arg := range call.Args() {
		child := b.build(arg)
		if n.chain != "" && child != nil && child.chain == n.chain {
			for _, grandchild := range child.children {
				n.add(grandchild)
			}
			continue
		}
		n.add(child)
	}
	// An operator is recorded at its symbol, which a unary operator leads with.
	b.cover(n, anchor)
	if k := b.kind(anchor); k == gen.CELLexerLPAREN || k == gen.CELLexerLBRACKET {
		b.cover(n, b.partner(anchor))
	}
	return n
}

// coverBrackets extends n over the bracket at index open and its partner.
func (b *spanBuilder) coverBrackets(n *spanNode, open int) {
	b.cover(n, open)
	b.cover(n, b.partner(open))
}

// group wraps n in a node for each pair of grouping parentheses around it, as
// in (a + b) * c.
func (b *spanBuilder) group(n *spanNode) *spanNode {
	if n == nil || n.start < 0 {
		return n
	}
	for {
		first, last := b.tokenAt(n.start), b.tokenAt(n.end-1)
		open, closing := b.step(first, -1), b.step(last, 1)
		if b.kind(open) != gen.CELLexerLPAREN || b.partner(open) != closing || closing < 0 {
			return n
		}
		// A call's parentheses hold its arguments rather than group them.
		if b.isName(b.step(open, -1)) {
			return n
		}
		grouped := newNode(n)
		b.coverBrackets(grouped, open)
		n = grouped
	}
}

// tokenAt returns the index of the token containing byte offset, or -1.
func (b *spanBuilder) tokenAt(offset int) int {
	i := sort.Search(len(b.tokens), func(i int) bool { return b.tokens[i].end > offset })
	if i < len(b.tokens) && b.tokens[i].start <= offset {
		return i
	}
	return -1
}
