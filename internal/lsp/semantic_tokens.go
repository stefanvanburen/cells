package lsp

import (
	"context"
	"maps"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/containers"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/overloads"
	"cel.dev/cel-go/common/stdlib"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/parser/gen"
	"go.lsp.dev/protocol"
)

func (s *server) SemanticTokensFull(ctx context.Context, params *protocol.SemanticTokensParams) (*protocol.SemanticTokens, error) {
	f, docEnv := s.document(ctx, params.TextDocument.URI)

	if f == nil || docEnv == nil {
		return nil, nil
	}
	return computeSemanticTokens(f, docEnv.celEnv), nil
}

// Semantic token types - indices into semanticTypeLegend.
const (
	semanticTypeNamespace = iota
	semanticTypeType
	semanticTypeEnumMember
	semanticTypeVariable
	semanticTypeProperty
	semanticTypeFunction
	semanticTypeMethod
	semanticTypeMacro
	semanticTypeKeyword
	semanticTypeComment
	semanticTypeString
	semanticTypeNumber
	semanticTypeOperator
)

// Semantic token modifiers - encoded as a bitset.
const (
	semanticModifierDeclaration = 1 << iota
	semanticModifierDefaultLibrary
)

var (
	semanticTypeLegend = []string{
		string(protocol.SemanticTokenTypesNamespace),
		string(protocol.SemanticTokenTypesType),
		string(protocol.SemanticTokenTypesEnumMember),
		string(protocol.SemanticTokenTypesVariable),
		string(protocol.SemanticTokenTypesProperty),
		string(protocol.SemanticTokenTypesFunction),
		string(protocol.SemanticTokenTypesMethod),
		string(protocol.SemanticTokenTypesMacro),
		string(protocol.SemanticTokenTypesKeyword),
		string(protocol.SemanticTokenTypesComment),
		string(protocol.SemanticTokenTypesString),
		string(protocol.SemanticTokenTypesNumber),
		string(protocol.SemanticTokenTypesOperator),
	}
	semanticModifierLegend = []string{
		string(protocol.SemanticTokenModifiersDeclaration),
		string(protocol.SemanticTokenModifiersDefaultLibrary),
	}
)

// standardNames holds the names the standard library declares: its functions,
// and the identifiers that name its types, such as int and google.protobuf.Timestamp.
var standardNames = sync.OnceValue(func() (names struct{ functions, types map[string]bool }) {
	names.functions = make(map[string]bool)
	for _, fn := range stdlib.Functions() {
		names.functions[fn.Name()] = true
	}
	names.types = make(map[string]bool)
	for _, v := range stdlib.Types() {
		names.types[v.Name()] = true
	}
	return names
})

// semanticClass is the token type and modifiers of an identifier.
type semanticClass struct {
	semType, semMod uint32
}

// computeSemanticTokens returns the semantic tokens of f.
//
// The lexer decides where every token is and the classes that need no
// context: comments, literals, keywords and most operators. The AST decides
// the rest by pointing at tokens: each expression's recorded position is a
// token, such as the dot of a field selection or the parenthesis of a call,
// from which the names it involves are a step or two away. A file that does
// not parse still gets the lexical classes.
func computeSemanticTokens(f *file, celEnv *cel.Env) *protocol.SemanticTokens {
	if f == nil || f.content == "" {
		return nil
	}
	nativeAST := f.ast(celEnv)
	var sourceInfo *ast.SourceInfo
	if nativeAST != nil {
		sourceInfo = nativeAST.SourceInfo()
	}
	c := &tokenClassifier{
		sourceTokens: newSourceTokens(f.content, sourceInfo),
		env:          celEnv,
		classes:      make(map[int]semanticClass),
		ternaries:    make(map[int]bool),
		indexes:      make(map[int]bool),
	}
	if nativeAST != nil {
		c.walk(nativeAST.Expr(), nil)
		c.walkMacroCalls()
	}

	data := c.encode(f.content)
	if len(data) == 0 {
		return nil
	}
	return &protocol.SemanticTokens{Data: data}
}

// tokenClassifier assigns semantic classes to the tokens of one file.
type tokenClassifier struct {
	*sourceTokens
	env *cel.Env

	// classes holds the class of each identifier token, by token index.
	classes map[int]semanticClass
	// ternaries and indexes hold the ? and [ tokens that are operators rather
	// than optional-syntax markers and list brackets.
	ternaries, indexes map[int]bool
}

// set records the class of the name token at i, unless it already has one.
func (c *tokenClassifier) set(i int, semType, semMod uint32) {
	if !c.isName(i) {
		return
	}
	if _, ok := c.classes[i]; !ok {
		c.classes[i] = semanticClass{semType, semMod}
	}
}

// setQualified records the class of the name token at i, which ends the
// possibly dotted name, and marks the qualifiers written before it as
// namespaces. It stops early where the source spells fewer qualifiers than
// name has, as when a container resolved the name.
func (c *tokenClassifier) setQualified(i int, name string, semType, semMod uint32) {
	c.set(i, semType, semMod)
	for range strings.Count(strings.TrimPrefix(name, "."), ".") {
		dot := c.step(i, -1)
		if c.kind(dot) != gen.CELLexerDOT || !c.isName(c.step(dot, -1)) {
			return
		}
		i = c.step(dot, -1)
		c.set(i, semanticTypeNamespace, 0)
	}
}

// setCallName records the class of the name of the call with the given ID,
// which is recorded at its opening parenthesis. Macro expansion
// records the calls it synthesizes at the parenthesis of the macro too, so the
// name must match.
func (c *tokenClassifier) setCallName(id int64, name string, semType, semMod uint32) {
	if paren := c.anchor(id); c.kind(paren) == gen.CELLexerLPAREN {
		i := c.step(paren, -1)
		if c.isName(i) && c.text(i) == name[strings.LastIndex(name, ".")+1:] {
			c.setQualified(i, name, semType, semMod)
		}
	}
}

// walk classifies the names in expr. scope holds the loop variables bound
// where expr appears.
func (c *tokenClassifier) walk(expr ast.Expr, scope map[string]bool) {
	if expr == nil {
		return
	}
	switch expr.Kind() {
	case ast.IdentKind:
		semType, semMod := c.identClass(expr.AsIdent(), scope)
		c.set(c.anchor(expr.ID()), semType, semMod)

	case ast.SelectKind:
		sel := expr.AsSelect()
		// Like the checker, prefer the longest qualified name the environment
		// knows, such as google.protobuf.NullValue.NULL_VALUE, over selecting
		// fields from its prefix.
		if name, ok := c.qualifiedName(expr, scope); ok && !sel.IsTestOnly() {
			if semType, semMod, ok := c.resolve(name); ok {
				if dot := c.anchor(expr.ID()); c.kind(dot) == gen.CELLexerDOT {
					c.setQualified(c.step(dot, 1), name, semType, semMod)
				}
				return
			}
		}
		c.walk(sel.Operand(), scope)
		// A selection is recorded at its dot, except for the presence test
		// has() expands to, which is recorded at the macro and classified
		// through the call has() was written as.
		if dot := c.anchor(expr.ID()); c.kind(dot) == gen.CELLexerDOT {
			c.set(c.step(dot, 1), semanticTypeProperty, 0)
		}

	case ast.CallKind:
		call := expr.AsCall()
		fn := call.FunctionName()
		if call.IsMemberFunction() {
			// math.greatest(a, b) parses as a method of math, but names a
			// function in the math namespace.
			if namespace, ok := c.qualifiedName(call.Target(), scope); ok && c.env.HasFunction(namespace+"."+fn) {
				semType, semMod := functionClass(namespace+"."+fn, false)
				c.setCallName(expr.ID(), namespace+"."+fn, semType, semMod)
				for _, arg := range call.Args() {
					c.walk(arg, scope)
				}
				return
			}
			c.walk(call.Target(), scope)
		}
		switch fn {
		case operators.Conditional:
			c.ternaries[c.anchor(expr.ID())] = true
		case operators.Index, operators.OptIndex:
			c.indexes[c.anchor(expr.ID())] = true
		case operators.OptSelect:
			// x.?field: the field is a string literal recorded at its name.
			if args := call.Args(); len(args) == 2 {
				c.walk(args[0], scope)
				c.set(c.anchor(args[1].ID()), semanticTypeProperty, 0)
			}
			return
		default:
			if _, isOperator := celOperatorSymbol(fn); !isOperator {
				semType, semMod := functionClass(fn, call.IsMemberFunction())
				c.setCallName(expr.ID(), fn, semType, semMod)
			}
		}
		for _, arg := range call.Args() {
			c.walk(arg, scope)
		}

	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			c.walk(elem, scope)
		}

	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			c.walk(mapEntry.Key(), scope)
			c.walk(mapEntry.Value(), scope)
		}

	case ast.StructKind:
		s := expr.AsStruct()
		if brace := c.anchor(expr.ID()); c.kind(brace) == gen.CELLexerLBRACE {
			c.setQualified(c.step(brace, -1), s.TypeName(), semanticTypeType, 0)
		}
		for _, field := range s.Fields() {
			// A field initializer is recorded at its colon.
			if colon := c.anchor(field.ID()); c.kind(colon) == gen.CELLexerCOLON {
				c.set(c.step(colon, -1), semanticTypeProperty, 0)
			}
			c.walk(field.AsStructField().Value(), scope)
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		c.walk(comp.IterRange(), scope)
		c.walk(comp.AccuInit(), scope)

		bound := map[string]bool{comp.IterVar(): true, comp.AccuVar(): true}
		if comp.HasIterVar2() {
			bound[comp.IterVar2()] = true
		}
		// The declarations are arguments of the call the macro was written as.
		if call, ok := c.sourceInfo.GetMacroCall(expr.ID()); ok && call.Kind() == ast.CallKind {
			for _, arg := range call.AsCall().Args() {
				if arg.Kind() == ast.IdentKind && bound[arg.AsIdent()] {
					c.set(c.anchor(arg.ID()), semanticTypeVariable, semanticModifierDeclaration)
				}
			}
		}

		inner := maps.Clone(scope)
		if inner == nil {
			inner = make(map[string]bool, len(bound))
		}
		maps.Copy(inner, bound)
		c.walk(comp.LoopCondition(), inner)
		c.walk(comp.LoopStep(), inner)
		c.walk(comp.Result(), inner)
	}
}

// walkMacroCalls classifies the macro names, which expansion leaves out of the
// AST, along with anything else only the calls as written still hold: the
// argument of has(), and the namespace of a macro like cel.bind.
func (c *tokenClassifier) walkMacroCalls() {
	for id, call := range c.sourceInfo.MacroCalls() {
		if call.Kind() != ast.CallKind {
			continue
		}
		mc := call.AsCall()
		c.setCallName(id, mc.FunctionName(), semanticTypeMacro, 0)
		if mc.IsMemberFunction() {
			target := mc.Target()
			// A target the expansion kept was classified with it; one it
			// dropped, like the cel of cel.bind, only names the macro.
			if target.Kind() == ast.IdentKind {
				c.set(c.anchor(target.ID()), semanticTypeNamespace, 0)
			} else {
				c.walk(target, nil)
			}
		}
		for _, arg := range mc.Args() {
			c.walk(arg, nil)
		}
	}
}

// identClass returns the class of an identifier, which is a variable unless
// the environment knows the name as a type or an enum value.
func (c *tokenClassifier) identClass(name string, scope map[string]bool) (semType, semMod uint32) {
	if !scope[name] {
		if semType, semMod, ok := c.resolve(name); ok {
			return semType, semMod
		}
	}
	return semanticTypeVariable, 0
}

// resolve returns the class of a possibly qualified name the environment knows
// as a type or an enum value, and whether it knows the name at all.
func (c *tokenClassifier) resolve(name string) (semType, semMod uint32, ok bool) {
	value, found := c.env.CELTypeProvider().FindIdent(strings.TrimPrefix(name, "."))
	switch {
	case !found:
		return 0, 0, false
	case standardNames().types[name]:
		return semanticTypeType, semanticModifierDefaultLibrary, true
	}
	if _, isType := value.(*types.Type); isType {
		return semanticTypeType, 0, true
	}
	return semanticTypeEnumMember, 0, true
}

// qualifiedName returns the dotted name expr spells, if it is a chain of field
// selections from an identifier that is not a loop variable.
func (c *tokenClassifier) qualifiedName(expr ast.Expr, scope map[string]bool) (string, bool) {
	name, ok := containers.ToQualifiedName(expr)
	if !ok {
		return "", false
	}
	root, _, _ := strings.Cut(strings.TrimPrefix(name, "."), ".")
	return name, !scope[root]
}

// functionClass returns the class of the name of a call to fn.
func functionClass(fn string, member bool) (semType, semMod uint32) {
	if standardNames().functions[fn] {
		semMod = semanticModifierDefaultLibrary
	}
	switch {
	case overloads.IsTypeConversionFunction(fn):
		return semanticTypeType, semMod
	case member:
		return semanticTypeMethod, semMod
	}
	return semanticTypeFunction, semMod
}

// encode returns the classified tokens of content in the LSP's relative
// encoding.
func (c *tokenClassifier) encode(content string) []uint32 {
	e := tokenEncoder{content: content}
	// brackets holds, for each bracket open at this point, whether it is an
	// index operator; ternaryDepths holds the bracket depth of each ? whose :
	// is still to come.
	var (
		brackets      []bool
		ternaryDepths []int
	)
	for i, t := range c.tokens {
		switch k := t.kind; {
		case k == gen.CELLexerCOMMENT:
			e.emit(t.start, t.end, semanticTypeComment, 0)
		case k == gen.CELLexerSTRING || k == gen.CELLexerBYTES:
			e.emit(t.start, t.end, semanticTypeString, 0)
		case k == gen.CELLexerNUM_INT || k == gen.CELLexerNUM_UINT || k == gen.CELLexerNUM_FLOAT:
			e.emit(t.start, t.end, semanticTypeNumber, 0)
		case k == gen.CELLexerCEL_TRUE || k == gen.CELLexerCEL_FALSE || k == gen.CELLexerNUL:
			e.emit(t.start, t.end, semanticTypeKeyword, 0)
		case k >= gen.CELLexerEQUALS && k <= gen.CELLexerLOGICAL_OR,
			k == gen.CELLexerMINUS, k == gen.CELLexerEXCLAM, k == gen.CELLexerPLUS,
			k == gen.CELLexerSTAR, k == gen.CELLexerSLASH, k == gen.CELLexerPERCENT:
			e.emit(t.start, t.end, semanticTypeOperator, 0)
		case k == gen.CELLexerQUESTIONMARK:
			if c.ternaries[i] {
				e.emit(t.start, t.end, semanticTypeOperator, 0)
				ternaryDepths = append(ternaryDepths, len(brackets))
			}
		case k == gen.CELLexerCOLON:
			if n := len(ternaryDepths); n > 0 && ternaryDepths[n-1] == len(brackets) {
				e.emit(t.start, t.end, semanticTypeOperator, 0)
				ternaryDepths = ternaryDepths[:n-1]
			}
		case k == gen.CELLexerLBRACKET:
			if c.indexes[i] {
				e.emit(t.start, t.end, semanticTypeOperator, 0)
			}
			brackets = append(brackets, c.indexes[i])
		case k == gen.CELLexerLPAREN || k == gen.CELLexerLBRACE:
			brackets = append(brackets, false)
		case k == gen.CELLexerRPRACKET || k == gen.CELLexerRPAREN || k == gen.CELLexerRBRACE:
			if n := len(brackets); n > 0 {
				if brackets[n-1] && k == gen.CELLexerRPRACKET {
					e.emit(t.start, t.end, semanticTypeOperator, 0)
				}
				brackets = brackets[:n-1]
			}
		case k == gen.CELLexerIDENTIFIER || k == gen.CELLexerESC_IDENTIFIER:
			if class, ok := c.classes[i]; ok {
				e.emit(t.start, t.end, class.semType, class.semMod)
			}
		}
	}
	return e.data
}

// tokenEncoder appends tokens, which must arrive in order, in the LSP's
// relative encoding.
type tokenEncoder struct {
	content string
	data    []uint32

	// pos is the byte offset that line and col, in UTF-16 code units, describe.
	pos       int
	line, col uint32
	// prevLine and prevCol are the start of the last token emitted.
	prevLine, prevCol uint32
}

// emit appends the token covering content[start:end], split at line breaks:
// clients need not support tokens that span lines, and a triple-quoted string
// can.
func (e *tokenEncoder) emit(start, end int, semType, semMod uint32) {
	for start < end {
		lineEnd := end
		if nl := strings.IndexByte(e.content[start:end], '\n'); nl >= 0 {
			lineEnd = start + nl
		}
		segment := strings.TrimSuffix(e.content[start:lineEnd], "\r")
		if segment != "" {
			e.advance(start)
			length := uint32(0)
			for _, r := range segment {
				length += uint32(utf16.RuneLen(r))
			}
			deltaCol := e.col
			if e.line == e.prevLine {
				deltaCol -= e.prevCol
			}
			e.data = append(e.data, e.line-e.prevLine, deltaCol, length, semType, semMod)
			e.prevLine, e.prevCol = e.line, e.col
		}
		start = lineEnd + 1
	}
}

// advance moves the position forward to the byte offset to.
func (e *tokenEncoder) advance(to int) {
	for e.pos < to {
		r, size := utf8.DecodeRuneInString(e.content[e.pos:])
		if r == '\n' {
			e.line++
			e.col = 0
		} else {
			e.col += uint32(utf16.RuneLen(r))
		}
		e.pos += size
	}
}
