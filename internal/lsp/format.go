package lsp

import (
	"context"
	"errors"
	"slices"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/parser/gen"
	"github.com/bufbuild/protocompile/experimental/dom"
	"go.lsp.dev/protocol"
)

func (s *server) Formatting(ctx context.Context, params *protocol.DocumentFormattingParams) ([]protocol.TextEdit, error) {
	f, docEnv := s.document(ctx, params.TextDocument.URI)

	if f == nil || docEnv == nil {
		return nil, nil
	}

	formatted, err := formatCEL(f.content, docEnv.celEnv)
	if err != nil {
		// If formatting fails (e.g., parse error), return no edits.
		return nil, nil
	}

	if formatted == f.content {
		return nil, nil
	}

	// Replace the entire document: an edit from the start of the file to its
	// end. That end is a position like any other, so its column is measured in
	// UTF-16 code units rather than in bytes.
	endLine, endCharacter := byteOffsetToLineCol(f.content, len(f.content))

	return []protocol.TextEdit{{
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: endLine, Character: endCharacter},
		},
		NewText: formatted,
	}}, nil
}

const (
	// formatWidth is the column a group of the formatted expression breaks
	// across lines to stay within.
	formatWidth = 100
	// formatIndent indents what a broken group holds.
	formatIndent = "  "
)

// formatCEL formats a CEL expression.
//
// The output spells the same tokens as content, comments included, and
// differs only in the whitespace between them, so a literal keeps the form it
// was written in and no comment is lost; the one exception is the trailing
// comma of a list, map or message, which is there exactly when it is broken
// across lines. The AST decides where lines may break: between the operands
// of a chain of && or ||, after the arguments of a call or the elements of a
// list, map or message, around the branches of a conditional, and inside
// parentheses. Each of those is a group, laid out on one line when it fits
// within [formatWidth] columns and broken otherwise. A comment on its own
// line stays on its own line, and one at the end of a line stays at the end
// of it, which breaks every group around it.
func formatCEL(content string, celEnv *cel.Env) (string, error) {
	parsed, iss := celEnv.Parse(content)
	if iss.Err() != nil {
		return "", iss.Err()
	}
	native := parsed.NativeRep()
	b := newSpanBuilder(newSourceTokens(content, native.SourceInfo()))
	b.build(native.Expr())
	f := &formatter{spanBuilder: b}

	out := dom.Render(dom.Options{MaxWidth: formatWidth, OmitTrailingNewline: true}, func(push dom.Sink) {
		push(dom.Group(formatWidth, func(push dom.Sink) {
			f.expr(push, native.Expr())
			f.emitTo(push, len(f.tokens)-1)
			f.flush(push)
		}))
	})
	out = strings.TrimRight(out, " \n")
	if strings.HasSuffix(content, "\n") {
		out += "\n"
	}

	if !slices.Equal(comparableTokens(content), comparableTokens(out)) {
		return "", errors.New("formatting would change the tokens of the expression")
	}
	return out, nil
}

// comparableTokens returns the text of the tokens of content that formatting
// keeps: all of them but a comma before a closing bracket, with the trailing
// whitespace of comments trimmed.
func comparableTokens(content string) []string {
	tokens := lexCEL(content)
	var texts []string
	for i, t := range tokens {
		if t.kind == gen.CELLexerCOMMA {
			next := i + 1
			for next < len(tokens) && tokens[next].kind == gen.CELLexerCOMMENT {
				next++
			}
			if next < len(tokens) && isClosingBracket(tokens[next].kind) {
				continue
			}
		}
		texts = append(texts, strings.TrimRight(content[t.start:t.end], " \t\r"))
	}
	return texts
}

// gap is the whitespace before a token.
type gap int

const (
	gapDefault   gap = iota // whatever [formatter.defaultGap] decides
	gapNone                 // nothing
	gapSpace                // a space
	gapSoftline             // a space in a flat group, a line break in a broken one
	gapSoftbreak            // nothing in a flat group, a line break in a broken one
)

// formatter lays out the tokens of an expression, in order. The methods that
// walk the AST say where each part of an expression begins; [formatter.emitTo]
// writes every token up to a given one, so that a token no method mentions,
// such as the dot of a selection, is still written, with the spacing
// [formatter.defaultGap] gives it.
type formatter struct {
	*spanBuilder
	// next is the index of the next token to write.
	next int
	// gap is the gap before the next token that is not a comment.
	gap gap
	// started is whether anything has been written.
	started bool
	// trailing is the index of a comment ending the line of the last token
	// written, held back until what comes next, which a trailing comma may
	// have to precede; it is 0 when there is none.
	trailing int
}

// emitTo writes the tokens up to and including the one at index last.
func (f *formatter) emitTo(push dom.Sink, last int) {
	for ; f.next <= last && f.next < len(f.tokens); f.next++ {
		i := f.next
		f.flush(push)
		if f.kind(i) == gen.CELLexerCOMMENT {
			// A comment on a line of its own, since one at the end of a line
			// is written with the token it follows.
			if f.started {
				push(dom.Text(newlines(f.blankLineBefore(i))))
			}
			push(dom.Text(f.comment(i)), dom.Text("\n"))
			f.started = true
			continue
		}

		g := f.gap
		f.gap = gapDefault
		if g == gapDefault {
			g = f.defaultGap(i)
		}
		f.writeGap(push, g, i)
		push(dom.Text(f.text(i)))
		f.started = true

		if f.endsLine(i + 1) {
			f.trailing = i + 1
			f.next = i + 1
		}
	}
}

// placeGap writes the gap set for the next token now, rather than with the
// token. A group that is about to open around the token would otherwise
// decide whether the gap breaks, where it is the group outside that should.
func (f *formatter) placeGap(push dom.Sink) {
	if f.gap != gapDefault {
		f.flush(push)
		f.writeGap(push, f.gap, f.nextToken())
		f.gap = gapNone
	}
}

// writeGap writes the gap g before the token at index i.
func (f *formatter) writeGap(push dom.Sink, g gap, i int) {
	blank := f.blankLineBefore(i)
	switch {
	case f.kind(i-1) == gen.CELLexerCOMMENT && blank:
		push(dom.Text("\n\n"))
	case g == gapSpace:
		push(dom.Text(" "))
	case g == gapSoftline:
		push(dom.TextIf(dom.Flat, " "), dom.TextIf(dom.Broken, newlines(blank)))
	case g == gapSoftbreak:
		push(dom.TextIf(dom.Broken, newlines(blank)))
	}
}

// endsLine reports whether the token at index i is a comment at the end of
// the line of the token before it.
func (f *formatter) endsLine(i int) bool {
	return i > 0 && f.kind(i) == gen.CELLexerCOMMENT && !strings.Contains(f.content[f.tokens[i-1].end:f.tokens[i].start], "\n")
}

// flush writes the comment held back to end the line, if any.
func (f *formatter) flush(push dom.Sink) {
	if f.trailing > 0 {
		push(dom.Text(" "), dom.Text(f.comment(f.trailing)), dom.Text("\n"))
		f.trailing = 0
	}
}

// comment returns the text of the comment at index i.
func (f *formatter) comment(i int) string {
	return strings.TrimRight(f.text(i), " \t\r")
}

// blankLineBefore reports whether a blank line separates the token at index i
// from the one before it.
func (f *formatter) blankLineBefore(i int) bool {
	if i <= 0 {
		return false
	}
	return strings.Count(f.content[f.tokens[i-1].end:f.tokens[i].start], "\n") >= 2
}

func newlines(blank bool) string {
	if blank {
		return "\n\n"
	}
	return "\n"
}

// defaultGap returns the gap before the token at index i when the structure of
// the expression does not call for a particular one: a space between words and
// around binary operators, and nothing inside brackets, around dots, before a
// comma or colon, after a unary operator, or between a name and the
// parenthesis, bracket or brace that follows it.
func (f *formatter) defaultGap(i int) gap {
	prev := f.step(i, -1)
	if prev < 0 {
		return gapNone
	}
	k, prevKind := f.kind(i), f.kind(prev)
	switch {
	case isClosingBracket(k), k == gen.CELLexerCOMMA, k == gen.CELLexerDOT, k == gen.CELLexerCOLON:
		return gapNone
	case prevKind == gen.CELLexerLPAREN, prevKind == gen.CELLexerLBRACKET, prevKind == gen.CELLexerLBRACE,
		prevKind == gen.CELLexerDOT, prevKind == gen.CELLexerEXCLAM:
		return gapNone
	case k == gen.CELLexerLPAREN || k == gen.CELLexerLBRACKET || k == gen.CELLexerLBRACE:
		if f.isName(prev) || prevKind == gen.CELLexerRPAREN || prevKind == gen.CELLexerRPRACKET {
			return gapNone
		}
	case prevKind == gen.CELLexerMINUS && !f.endsOperand(f.step(prev, -1)):
		return gapNone
	case prevKind == gen.CELLexerQUESTIONMARK && f.marksOptional(prev):
		return gapNone
	}
	return gapSpace
}

// endsOperand reports whether the token at index i can end an operand, which
// makes a - after it a binary operator rather than a negation.
func (f *formatter) endsOperand(i int) bool {
	switch f.kind(i) {
	case gen.CELLexerIDENTIFIER, gen.CELLexerESC_IDENTIFIER, gen.CELLexerRPAREN, gen.CELLexerRPRACKET,
		gen.CELLexerRBRACE, gen.CELLexerNUM_INT, gen.CELLexerNUM_UINT, gen.CELLexerNUM_FLOAT,
		gen.CELLexerSTRING, gen.CELLexerBYTES, gen.CELLexerCEL_TRUE, gen.CELLexerCEL_FALSE, gen.CELLexerNUL:
		return true
	}
	return false
}

// marksOptional reports whether the ? at index i marks an optional selection,
// index, element or field, rather than being a conditional.
func (f *formatter) marksOptional(i int) bool {
	switch f.kind(f.step(i, -1)) {
	case gen.CELLexerDOT, gen.CELLexerLBRACKET, gen.CELLexerLBRACE, gen.CELLexerCOMMA:
		return true
	}
	return false
}

// nextToken returns the index of the next token to write that is not a
// comment.
func (f *formatter) nextToken() int {
	i := f.next
	for f.kind(i) == gen.CELLexerCOMMENT {
		i++
	}
	return i
}

// lastToken returns the index of the last token of expr, without any
// parentheses around it, or -1.
func (f *formatter) lastToken(expr ast.Expr) int {
	if n := f.spans[expr.ID()]; n != nil {
		return f.tokenAt(n.end - 1)
	}
	return -1
}

// parentheses returns the indexes of the parentheses written around expr,
// outermost first.
func (f *formatter) parentheses(expr ast.Expr) []int {
	n := f.spans[expr.ID()]
	if n == nil {
		return nil
	}
	var opens []int
	first, last := f.tokenAt(n.start), f.tokenAt(n.end-1)
	for {
		open, closing := f.step(first, -1), f.step(last, 1)
		// A call's parentheses hold its arguments rather than group them.
		if f.kind(open) != gen.CELLexerLPAREN || closing < 0 || f.partner(open) != closing || f.isName(f.step(open, -1)) {
			break
		}
		opens = append(opens, open)
		first, last = open, closing
	}
	slices.Reverse(opens)
	return opens
}

// expr writes expr, with any parentheses around it.
func (f *formatter) expr(push dom.Sink, expr ast.Expr) {
	f.parenthesized(push, f.parentheses(expr), func(push dom.Sink) { f.bare(push, expr) })
}

// parenthesized writes body inside the parentheses that open at opens,
// outermost first, each pair a group whose content is indented when broken.
func (f *formatter) parenthesized(push dom.Sink, opens []int, body func(dom.Sink)) {
	if len(opens) == 0 {
		body(push)
		return
	}
	f.emitTo(push, opens[0])
	push(dom.Group(formatWidth, func(push dom.Sink) {
		push(dom.Indent(formatIndent, func(push dom.Sink) {
			f.gap = gapSoftbreak
			f.parenthesized(push, opens[1:], body)
		}))
		f.gap = gapSoftbreak
		f.emitTo(push, f.partner(opens[0]))
	}))
}

// bare writes expr without the parentheses around it.
func (f *formatter) bare(push dom.Sink, expr ast.Expr) {
	// A macro is written as the call it was written as, not its expansion.
	if call, ok := f.sourceInfo.GetMacroCall(expr.ID()); ok && call.Kind() == ast.CallKind {
		f.call(push, expr.ID(), call.AsCall(), true)
		return
	}

	switch expr.Kind() {
	case ast.SelectKind:
		f.expr(push, expr.AsSelect().Operand())

	case ast.CallKind:
		call := expr.AsCall()
		switch fn := call.FunctionName(); fn {
		case operators.LogicalAnd, operators.LogicalOr:
			f.chain(push, expr)
			return
		case operators.Conditional:
			f.conditional(push, call)
			return
		case operators.Index, operators.OptIndex, operators.OptSelect:
		default:
			if _, isOperator := celOperatorSymbol(fn); !isOperator {
				f.call(push, expr.ID(), call, false)
				return
			}
		}
		for _, arg := range call.Args() {
			f.expr(push, arg)
		}

	case ast.ListKind:
		open := f.anchor(expr.ID())
		f.emitTo(push, open)
		var elements []func(dom.Sink)
		for _, elem := range expr.AsList().Elements() {
			elements = append(elements, func(push dom.Sink) { f.expr(push, elem) })
		}
		f.list(push, elements, f.partner(open), false, true)
		return

	case ast.MapKind:
		open := f.anchor(expr.ID())
		f.emitTo(push, open)
		var entries []func(dom.Sink)
		for _, entry := range expr.AsMap().Entries() {
			e := entry.AsMapEntry()
			entries = append(entries, func(push dom.Sink) {
				f.expr(push, e.Key())
				f.emitTo(push, f.nextToken()) // :
				f.expr(push, e.Value())
			})
		}
		f.list(push, entries, f.partner(open), false, true)
		return

	case ast.StructKind:
		open := f.anchor(expr.ID())
		f.emitTo(push, open)
		var fields []func(dom.Sink)
		for _, field := range expr.AsStruct().Fields() {
			fields = append(fields, func(push dom.Sink) {
				// A field initializer is recorded at its colon.
				f.emitTo(push, f.anchor(field.ID()))
				f.expr(push, field.AsStructField().Value())
			})
		}
		f.list(push, fields, f.partner(open), false, true)
		return
	}
	f.emitTo(push, f.lastToken(expr))
}

// chain writes a chain of && or || as a group with a line for each operand
// when broken, the operator ending the line before it. The parser balances
// a chain into a tree; the operands are its leaves, short of parentheses.
func (f *formatter) chain(push dom.Sink, expr ast.Expr) {
	f.placeGap(push)
	fn := expr.AsCall().FunctionName()
	var operands []ast.Expr
	var collect func(ast.Expr)
	collect = func(e ast.Expr) {
		if e.Kind() == ast.CallKind && e.AsCall().FunctionName() == fn && len(f.parentheses(e)) == 0 {
			for _, arg := range e.AsCall().Args() {
				collect(arg)
			}
			return
		}
		operands = append(operands, e)
	}
	// The parentheses around expr itself, if any, are already written.
	for _, arg := range expr.AsCall().Args() {
		collect(arg)
	}

	push(dom.Group(formatWidth, func(push dom.Sink) {
		for i, operand := range operands {
			if i > 0 {
				f.emitTo(push, f.nextToken()) // the operator
				f.gap = gapSoftline
			}
			f.expr(push, operand)
		}
	}))
}

// conditional writes c ? a : b as a group that, broken, puts each branch on
// an indented line of its own.
func (f *formatter) conditional(push dom.Sink, call ast.CallExpr) {
	f.placeGap(push)
	args := call.Args()
	push(dom.Group(formatWidth, func(push dom.Sink) {
		f.expr(push, args[0])
		push(dom.Indent(formatIndent, func(push dom.Sink) {
			for _, branch := range args[1:] {
				f.gap = gapSoftline
				f.emitTo(push, f.nextToken()) // ? or :
				f.expr(push, branch)
			}
		}))
	}))
}

// call writes a call. The arguments of a macro that name the variables it
// binds stay on the line of its name, as in xs.all(x, ..., the rest
// following when the call is broken.
func (f *formatter) call(push dom.Sink, id int64, call ast.CallExpr, macro bool) {
	if call.IsMemberFunction() {
		f.expr(push, call.Target())
	}
	args := call.Args()
	paren := f.anchor(id)
	if f.kind(paren) != gen.CELLexerLPAREN {
		for _, arg := range args {
			f.expr(push, arg)
		}
		return
	}
	f.emitTo(push, paren)

	declarations := 0
	if macro {
		for declarations < len(args)-1 && args[declarations].Kind() == ast.IdentKind {
			declarations++
		}
	}
	for _, arg := range args[:declarations] {
		f.expr(push, arg)
		f.emitTo(push, f.nextToken()) // ,
	}
	var rest []func(dom.Sink)
	for _, arg := range args[declarations:] {
		rest = append(rest, func(push dom.Sink) { f.expr(push, arg) })
	}
	// CEL allows no trailing comma among arguments.
	f.list(push, rest, f.partner(paren), declarations > 0, false)
}

// list writes the comma-separated elements of a call, list, map or message as
// a group, then the bracket at index closing that ends it. Broken, each
// element is on an indented line of its own, the first one too unless
// continuing a line, and a trailing comma follows the last if trailingComma.
func (f *formatter) list(push dom.Sink, elements []func(dom.Sink), closing int, continuing, trailingComma bool) {
	push(dom.Group(formatWidth, func(push dom.Sink) {
		push(dom.Indent(formatIndent, func(push dom.Sink) {
			for i, element := range elements {
				if i > 0 {
					f.emitTo(push, f.nextToken()) // ,
				}
				f.gap = gapSoftline
				if i == 0 && !continuing {
					f.gap = gapSoftbreak
				}
				element(push)
			}
			// The trailing comma is written, or not, by the layout instead,
			// before a comment ending the line of the last element.
			if trailingComma && len(elements) > 0 {
				push(dom.TextIf(dom.Broken, ","))
			}
			if comma := f.nextToken(); f.kind(comma) == gen.CELLexerCOMMA {
				f.emitTo(push, comma-1)
				f.next = comma + 1
				if f.endsLine(comma + 1) {
					f.flush(push)
					f.trailing = comma + 1
					f.next = comma + 2
				}
			}
			f.flush(push)
		}))
		f.gap = gapSoftbreak
		f.emitTo(push, closing)
	}))
}
