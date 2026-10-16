package lsp

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/decls"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/parser/gen"
	"go.lsp.dev/protocol"
)

// celKeywords are the literal keywords in CEL. These are language-level
// constants that aren't discoverable through cel-go's function or macro APIs.
var celKeywords = []string{"true", "false", "null"}

func (s *server) Completion(ctx context.Context, params *protocol.CompletionParams) (protocol.CompletionResult, error) {
	f, docEnv := s.document(ctx, params.TextDocument.URI)
	if docEnv == nil {
		// The configuration covering this document does not load, so there is
		// no environment to complete against. Diagnostics report why.
		return &protocol.CompletionList{}, nil
	}

	var tokens []celToken
	var offset int
	if f != nil {
		offset = lineColToByteOffset(f.content, params.Position.Line, params.Position.Character)
		offset = max(0, min(offset, len(f.content)))
		var completable bool
		tokens, completable = tokensBefore(f.content, offset)
		if !completable {
			return s.completionList(nil), nil
		}
	}

	// Member completions, filtered by the receiver's type.
	if dot, ok := memberDot(tokens, offset); ok {
		receiverType := receiverType(f.content, tokens[:dot], docEnv.celEnv)
		items := fieldCompletionItems(docEnv, receiverType)
		items = append(items, memberCompletionItems(docEnv.celEnv, receiverType)...)
		return s.completionList(items), nil
	}

	// After a complete operand only an operator can come, and cells offers
	// none.
	if endsOperand(tokens, offset) {
		return s.completionList(nil), nil
	}

	// Check for operator context to filter by expected type.
	var expectedType *types.Type
	if f != nil {
		expectedType = expectedOperandType(f.content, tokens, offset, docEnv.celEnv)
	}

	var bound map[string]string
	if f != nil {
		bound = boundNamesAt(f.content[:offset], tokens, docEnv.celEnv)
	}

	var items []protocol.CompletionItem
	items = append(items, boundCompletionItems(bound)...)
	items = append(items, variableCompletionItems(docEnv.celEnv, expectedType, bound)...)
	items = append(items, globalCompletionItems(docEnv.celEnv, expectedType)...)
	items = append(items, macroCompletionItems(docEnv.celEnv, expectedType)...)
	items = append(items, keywordCompletionItems(docEnv.celEnv, expectedType)...)
	return s.completionList(items), nil
}

// completionList returns the items as a complete list. An item's labelDetails
// say what its detail does, so a client that shows them gets them in place of
// the detail, and any other client gets the detail alone.
func (s *server) completionList(items []protocol.CompletionItem) *protocol.CompletionList {
	for i := range items {
		switch {
		case !s.completionLabelDetails:
			items[i].LabelDetails = nil
		case items[i].LabelDetails != nil:
			items[i].Detail = protocol.Optional[string]{}
		}
	}
	return &protocol.CompletionList{
		IsIncomplete: false,
		Items:        items,
	}
}

// typeLabelDetails is the labelDetails of an item that has a type, which the
// client shows apart from the item's label.
func typeLabelDetails(t *types.Type) *protocol.CompletionItemLabelDetails {
	return &protocol.CompletionItemLabelDetails{Description: new(t.String())}
}

// signatureLabelDetails is the labelDetails of a function with the given
// overloads: the first's parameters and result and how many others there are,
// which the client shows directly after the function's name, and a member
// function's receiver type, which it shows apart.
func signatureLabelDetails(overloads []*decls.OverloadDecl) *protocol.CompletionItemLabelDetails {
	o := overloads[0]
	details := &protocol.CompletionItemLabelDetails{Detail: new(formatOverloadParams(o) + moreOverloads(overloads))}
	if args := o.ArgTypes(); o.IsMemberFunction() && len(args) > 0 {
		details.Description = new(args[0].String())
	}
	return details
}

// tokensBefore returns the tokens before offset, leaving out comments. It
// reports false where there is nothing to complete: inside a comment, or after
// text the lexer makes no token of, which is a string or bytes literal still
// being typed.
func tokensBefore(content string, offset int) (tokens []celToken, completable bool) {
	tokens = lexCEL(content[:offset])
	end := 0
	if len(tokens) > 0 {
		last := tokens[len(tokens)-1]
		if last.kind == gen.CELLexerCOMMENT && last.end == offset {
			return nil, false
		}
		end = last.end
	}
	if strings.TrimSpace(content[end:offset]) != "" {
		return nil, false
	}
	tokens = slices.DeleteFunc(tokens, func(tok celToken) bool { return tok.kind == gen.CELLexerCOMMENT })
	return tokens, true
}

// memberDot returns the index of the dot a member is being completed after,
// which is the last token before offset, or the one before a name being typed
// up to offset.
func memberDot(tokens []celToken, offset int) (int, bool) {
	if n := len(tokens); n > 0 && tokens[n-1].kind == gen.CELLexerIDENTIFIER && tokens[n-1].end == offset {
		tokens = tokens[:n-1]
	}
	if n := len(tokens); n > 0 && tokens[n-1].kind == gen.CELLexerDOT {
		return n - 1, true
	}
	return 0, false
}

// endsOperand reports whether tokens end with a complete operand, such as a
// literal, a closing bracket, or a name followed by whitespace. A name that
// runs up to offset is one still being typed, which completion is there to
// finish.
func endsOperand(tokens []celToken, offset int) bool {
	if len(tokens) == 0 {
		return false
	}
	switch last := tokens[len(tokens)-1]; last.kind {
	case gen.CELLexerSTRING, gen.CELLexerBYTES,
		gen.CELLexerNUM_INT, gen.CELLexerNUM_UINT, gen.CELLexerNUM_FLOAT,
		gen.CELLexerESC_IDENTIFIER,
		gen.CELLexerRPAREN, gen.CELLexerRPRACKET, gen.CELLexerRBRACE:
		return true
	case gen.CELLexerIDENTIFIER, gen.CELLexerCEL_TRUE, gen.CELLexerCEL_FALSE, gen.CELLexerNUL:
		return last.end < offset
	}
	return false
}

// completionPlaceholder stands for the operand being completed when no name is
// being typed at the cursor, so that the source before the cursor can parse.
const completionPlaceholder = "cells_completion_placeholder"

// boundNamesAt returns the names macros bind at the end of before, the source
// up to the cursor, each mapped to the name of the macro that binds it.
//
// What is parsed is before with a placeholder name at the end if none is being
// typed there, made whole by [wholeSources]. Where that does not parse, there
// are no names to offer.
func boundNamesAt(before string, tokens []celToken, celEnv *cel.Env) map[string]string {
	src := before
	if n := len(tokens); n == 0 || tokens[n-1].kind != gen.CELLexerIDENTIFIER || tokens[n-1].end != len(before) {
		src += completionPlaceholder
	}
	nameEnd := len(src)

	var parsed *cel.Ast
	for _, whole := range wholeSources(src, tokens, completionPlaceholder) {
		if p, iss := celEnv.Parse(whole); iss.Err() == nil {
			parsed, src = p, whole
			break
		}
	}
	if parsed == nil {
		return nil
	}
	nativeAST := parsed.NativeRep()
	root, sourceInfo := nativeAST.Expr(), nativeAST.SourceInfo()
	var inScope map[string]int64
	walkScopes(root, func(ident ast.Expr, scope map[string]int64) {
		if _, end, ok := exprByteRange(src, sourceInfo, ident.ID()); ok && end == nameEnd {
			inScope = scope
		}
	})

	// The comprehensions also bind names of their own making, such as the
	// accumulator of [1, 2].map(x, x), which nobody wrote and nobody can
	// refer to. Only the names a macro declares are offered.
	bound := make(map[string]string)
	for name, id := range inScope {
		comp := findExprByID(root, id)
		if comp == nil {
			continue
		}
		for _, decl := range macroDeclarations(sourceInfo, comp) {
			if decl.AsIdent() == name {
				call, _ := sourceInfo.GetMacroCall(id)
				bound[name] = call.AsCall().FunctionName()
			}
		}
	}
	return bound
}

// boundCompletionItems offers the names macros bind where the cursor is, which
// bound maps to the names of those macros.
func boundCompletionItems(bound map[string]string) []protocol.CompletionItem {
	items := make([]protocol.CompletionItem, 0, len(bound))
	for name, macro := range bound {
		items = append(items, protocol.CompletionItem{
			Label:  name,
			Kind:   protocol.CompletionItemKindVariable,
			Detail: protocol.NewOptional("bound by " + macro),
		})
	}
	slices.SortFunc(items, func(a, b protocol.CompletionItem) int {
		return cmp.Compare(a.Label, b.Label)
	})
	return items
}

// wholeSources returns ways to make src, which tokens lead up to, a whole
// expression, since the source is rarely whole while it is being typed: src
// with the brackets tokens leave open closed, and the same with alternative
// after src as well, for a conditional they leave open.
func wholeSources(src string, tokens []celToken, alternative string) []string {
	var open []int
	for _, tok := range tokens {
		switch tok.kind {
		case gen.CELLexerLPAREN, gen.CELLexerLBRACKET, gen.CELLexerLBRACE:
			open = append(open, tok.kind)
		case gen.CELLexerRPAREN, gen.CELLexerRPRACKET, gen.CELLexerRBRACE:
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
		}
	}
	closers := map[int]string{gen.CELLexerLPAREN: ")", gen.CELLexerLBRACKET: "]", gen.CELLexerLBRACE: "}"}
	var b strings.Builder
	for _, kind := range slices.Backward(open) {
		b.WriteString(closers[kind])
	}
	closing := b.String()
	return []string{src + closing, src + " : " + alternative + closing}
}

// receiverType returns the type of the receiver a member is being completed
// on, which tokens, the tokens of content before the member's dot, end with.
// It returns nil if the type cannot be determined.
//
// What is type-checked is the source before the dot made whole by
// [wholeSources]. The parser records each expression at one token of its own,
// so the receiver is the outermost expression recorded at its anchor. It
// records nothing at the parenthesis of a grouping, so a receiver in
// parentheses is type-checked on its own instead.
func receiverType(content string, tokens []celToken, celEnv *cel.Env) *types.Type {
	anchor, ok := receiverAnchor(tokens)
	if !ok {
		return nil
	}
	end := tokens[len(tokens)-1].end
	for _, whole := range wholeSources(content[:end], tokens, "dyn(0)") {
		checked, iss := celEnv.Compile(whole)
		if iss.Err() != nil {
			continue
		}
		nativeAST := checked.NativeRep()
		var receiver ast.Expr
		ast.PreOrderVisit(nativeAST.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
			if r, ok := nativeAST.SourceInfo().GetOffsetRange(e.ID()); ok && receiver == nil && r.Start == anchor.runeStart {
				receiver = e
			}
		}))
		if receiver != nil {
			return nativeAST.GetType(receiver.ID())
		}
		if anchor.kind == gen.CELLexerLPAREN {
			if grouped, iss := celEnv.Compile(content[anchor.end:tokens[len(tokens)-1].start]); iss.Err() == nil {
				return grouped.OutputType()
			}
		}
		return nil
	}
	return nil
}

// receiverAnchor returns the token the parser records the expression that
// tokens end with at: the opening bracket of a list, map, message, call, macro
// or index that ends in its closing one, the dot of a field selection, and
// otherwise the last token itself, a name or a literal.
func receiverAnchor(tokens []celToken) (celToken, bool) {
	n := len(tokens)
	if n == 0 {
		return celToken{}, false
	}
	switch last := tokens[n-1]; last.kind {
	case gen.CELLexerRPAREN, gen.CELLexerRPRACKET, gen.CELLexerRBRACE:
		depth := 0
		for _, tok := range slices.Backward(tokens) {
			switch tok.kind {
			case gen.CELLexerRPAREN, gen.CELLexerRPRACKET, gen.CELLexerRBRACE:
				depth++
			case gen.CELLexerLPAREN, gen.CELLexerLBRACKET, gen.CELLexerLBRACE:
				depth--
				if depth == 0 {
					return tok, true
				}
			}
		}
		return celToken{}, false
	case gen.CELLexerIDENTIFIER:
		if n > 1 && tokens[n-2].kind == gen.CELLexerDOT {
			return tokens[n-2], true
		}
	}
	return tokens[n-1], true
}

// expectedOperandType returns the type a binary operator expects of the
// operand being completed after it, or nil if the cursor is not after one or
// the type cannot be narrowed to one. tokens are the tokens of content before
// offset.
//
// What is type-checked is the source with dyn(0) standing for the operand,
// made whole by [wholeSources]. The operator is the call that placeholder is
// the second argument of, and its first argument is the operand on its left,
// as precedence makes it: 2.0 in 1 + 2.0 *, but 1 == 2 in 1 == 2 &&.
func expectedOperandType(content string, tokens []celToken, offset int, celEnv *cel.Env) *types.Type {
	// A name being typed is the operand itself.
	if n := len(tokens); n > 0 && tokens[n-1].kind == gen.CELLexerIDENTIFIER && tokens[n-1].end == offset {
		tokens = tokens[:n-1]
	}
	if len(tokens) == 0 {
		return nil
	}
	switch tokens[len(tokens)-1].kind {
	case gen.CELLexerEQUALS, gen.CELLexerNOT_EQUALS, gen.CELLexerIN,
		gen.CELLexerLESS, gen.CELLexerLESS_EQUALS, gen.CELLexerGREATER_EQUALS, gen.CELLexerGREATER,
		gen.CELLexerLOGICAL_AND, gen.CELLexerLOGICAL_OR,
		gen.CELLexerPLUS, gen.CELLexerMINUS, gen.CELLexerSTAR, gen.CELLexerSLASH, gen.CELLexerPERCENT:
	default:
		return nil
	}

	before := content[:tokens[len(tokens)-1].end] + " "
	// The placeholder's call is recorded at its parenthesis.
	placeholder := int32(utf8.RuneCountInString(before) + len("dyn"))
	for _, whole := range wholeSources(before+"dyn(0)", tokens, "dyn(0)") {
		checked, iss := celEnv.Compile(whole)
		if iss.Err() != nil {
			continue
		}
		nativeAST := checked.NativeRep()
		var operator ast.Expr
		ast.PreOrderVisit(nativeAST.Expr(), ast.NewExprVisitor(func(e ast.Expr) {
			if e.Kind() != ast.CallKind || len(e.AsCall().Args()) != 2 {
				return
			}
			if r, ok := nativeAST.SourceInfo().GetOffsetRange(e.AsCall().Args()[1].ID()); ok && r.Start == placeholder {
				operator = e
			}
		}))
		if operator == nil {
			return nil
		}
		call := operator.AsCall()
		return rightOperandType(celEnv, call.FunctionName(), nativeAST.GetType(call.Args()[0].ID()))
	}
	return nil
}

// rightOperandType returns the one type the overloads of the binary operator
// function accept on the right of a left operand of leftType, or nil if they
// accept more than one.
func rightOperandType(celEnv *cel.Env, function string, leftType *types.Type) *types.Type {
	fn, ok := celEnv.Functions()[function]
	if !ok {
		return nil
	}

	rightTypes := make(map[string]*types.Type)
	for _, o := range fn.OverloadDecls() {
		args := o.ArgTypes()
		if len(args) != 2 {
			continue
		}
		if !args[0].IsAssignableType(leftType) && !leftType.IsAssignableType(args[0]) {
			continue
		}
		right := args[1]
		if isTypeParam(right) {
			right = leftType
		}
		rightTypes[right.String()] = right
	}

	// Only narrow if there's a single expected type.
	if len(rightTypes) == 1 {
		for _, t := range rightTypes {
			return t
		}
	}
	return nil
}

// isTypeParam returns true if the type is a type parameter (e.g. <A>, <B>).
func isTypeParam(t *types.Type) bool {
	s := t.String()
	return strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">")
}

// typeMatches returns true if resultType is compatible with expectedType.
// If expectedType is nil, all types match.
func typeMatches(expectedType, resultType *types.Type) bool {
	if expectedType == nil {
		return true
	}
	return expectedType.IsAssignableType(resultType)
}

// memberCompletionItems returns completion items for member functions.
// If receiverType is non-nil, only functions whose receiver matches that type
// are returned. If nil, all member functions are returned.
func memberCompletionItems(celEnv *cel.Env, receiverType *types.Type) []protocol.CompletionItem {
	var items []protocol.CompletionItem
	for name, fn := range celEnv.Functions() {
		if isOperatorOrInternal(name) {
			continue
		}

		var matchingOverloads []*decls.OverloadDecl
		for _, o := range fn.OverloadDecls() {
			if !o.IsMemberFunction() {
				continue
			}
			if receiverType != nil && len(o.ArgTypes()) > 0 {
				recvType := o.ArgTypes()[0]
				if !recvType.IsAssignableType(receiverType) {
					continue
				}
			}
			matchingOverloads = append(matchingOverloads, o)
		}
		if len(matchingOverloads) == 0 {
			continue
		}

		items = append(items, protocol.CompletionItem{
			Label:            name,
			Kind:             protocol.CompletionItemKindMethod,
			LabelDetails:     signatureLabelDetails(matchingOverloads),
			Detail:           protocol.NewOptional(formatOverloadSignature(fn.Name(), matchingOverloads[0]) + moreOverloads(matchingOverloads)),
			Documentation:    docString(fn.Description()),
			InsertText:       protocol.NewOptional(name + "($1)"),
			InsertTextFormat: protocol.InsertTextFormatSnippet,
		})
	}
	slices.SortFunc(items, func(a, b protocol.CompletionItem) int {
		return cmp.Compare(a.Label, b.Label)
	})
	return items
}

// variableCompletionItems offers the variables the environment declares,
// leaving out those a name in bound shadows. There are none until a
// configuration declares some, which is why plain CEL offers only functions
// and macros.
func variableCompletionItems(celEnv *cel.Env, expectedType *types.Type, bound map[string]string) []protocol.CompletionItem {
	var items []protocol.CompletionItem
	for _, variable := range celEnv.Variables() {
		if _, shadowed := bound[variable.Name()]; shadowed || !typeMatches(expectedType, variable.Type()) {
			continue
		}
		items = append(items, protocol.CompletionItem{
			Label:         variable.Name(),
			LabelDetails:  typeLabelDetails(variable.Type()),
			Kind:          protocol.CompletionItemKindVariable,
			Detail:        protocol.NewOptional(variable.Type().String()),
			Documentation: docString(variable.Description()),
		})
	}
	slices.SortFunc(items, func(a, b protocol.CompletionItem) int {
		return cmp.Compare(a.Label, b.Label)
	})
	return items
}

// fieldCompletionItems offers the fields of a message-typed receiver, so that
// completing after the dot in request. lists what the message actually holds.
// Anything else — a map, a list, a scalar — has no fields to offer.
func fieldCompletionItems(docEnv *environment, receiverType *types.Type) []protocol.CompletionItem {
	if receiverType == nil || receiverType.Kind() != types.StructKind {
		return nil
	}
	typeName := receiverType.TypeName()
	provider := docEnv.celEnv.CELTypeProvider()
	fieldNames, found := provider.FindStructFieldNames(typeName)
	if !found {
		return nil
	}

	items := make([]protocol.CompletionItem, 0, len(fieldNames))
	for _, name := range fieldNames {
		item := protocol.CompletionItem{
			Label:         name,
			Kind:          protocol.CompletionItemKindField,
			Documentation: docString(docEnv.fieldDoc(receiverType, name)),
		}
		if fieldType, ok := provider.FindStructFieldType(typeName, name); ok {
			item.LabelDetails = typeLabelDetails(fieldType.Type)
			item.Detail = protocol.NewOptional(fieldType.Type.String())
		}
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b protocol.CompletionItem) int {
		return cmp.Compare(a.Label, b.Label)
	})
	return items
}

// globalCompletionItems returns completion items for global functions and type
// conversions. If expectedType is non-nil, only functions that can return a
// compatible type are included.
func globalCompletionItems(celEnv *cel.Env, expectedType *types.Type) []protocol.CompletionItem {
	var items []protocol.CompletionItem
	for name, fn := range celEnv.Functions() {
		if isOperatorOrInternal(name) {
			continue
		}

		var matchingOverloads []*decls.OverloadDecl
		for _, o := range fn.OverloadDecls() {
			if !o.IsMemberFunction() && typeMatches(expectedType, o.ResultType()) {
				matchingOverloads = append(matchingOverloads, o)
			}
		}
		if len(matchingOverloads) == 0 {
			continue
		}

		items = append(items, protocol.CompletionItem{
			Label:            name,
			LabelDetails:     signatureLabelDetails(matchingOverloads),
			Kind:             protocol.CompletionItemKindFunction,
			Detail:           protocol.NewOptional(formatOverloadSignature(fn.Name(), matchingOverloads[0]) + moreOverloads(matchingOverloads)),
			Documentation:    docString(fn.Description()),
			InsertText:       protocol.NewOptional(name + "($1)"),
			InsertTextFormat: protocol.InsertTextFormatSnippet,
		})
	}
	slices.SortFunc(items, func(a, b protocol.CompletionItem) int {
		return cmp.Compare(a.Label, b.Label)
	})
	return items
}

// macroCompletionItems returns completion items for CEL macros, derived from
// cel-go's env.Macros(). Macros don't carry descriptions or return type
// information in cel-go, so they are not filtered by expectedType.
func macroCompletionItems(celEnv *cel.Env, _ *types.Type) []protocol.CompletionItem {
	seen := make(map[string]bool)
	var items []protocol.CompletionItem
	for _, m := range celEnv.Macros() {
		name := m.Function()
		if seen[name] {
			continue
		}
		seen[name] = true

		items = append(items, protocol.CompletionItem{
			Label:            name,
			Kind:             protocol.CompletionItemKindFunction,
			Detail:           protocol.NewOptional("macro"),
			InsertText:       protocol.NewOptional(name + "($1)"),
			InsertTextFormat: protocol.InsertTextFormatSnippet,
		})
	}
	slices.SortFunc(items, func(a, b protocol.CompletionItem) int {
		return cmp.Compare(a.Label, b.Label)
	})
	return items
}

// keywordCompletionItems returns completion items for CEL literal keywords
// (true, false, null). Types are derived by compiling each keyword against the
// environment. If expectedType is non-nil, only keywords whose type matches
// are included.
func keywordCompletionItems(celEnv *cel.Env, expectedType *types.Type) []protocol.CompletionItem {
	var items []protocol.CompletionItem
	for _, kw := range celKeywords {
		ast, iss := celEnv.Compile(kw)
		if iss.Err() != nil {
			continue
		}
		kwType := ast.OutputType()
		if !typeMatches(expectedType, kwType) {
			continue
		}
		items = append(items, protocol.CompletionItem{
			Label:        kw,
			LabelDetails: typeLabelDetails(kwType),
			Kind:         protocol.CompletionItemKindKeyword,
			Detail:       protocol.NewOptional(kwType.String()),
		})
	}
	return items
}

// isOperatorOrInternal returns true if the function name represents an
// operator or internal function that should not appear as a completion item.
func isOperatorOrInternal(name string) bool {
	if _, ok := operators.FindReverse(name); ok {
		return true
	}
	return strings.HasPrefix(name, "@") || strings.HasPrefix(name, "_")
}

// moreOverloads notes how many overloads there are besides the first one a
// completion item shows, if any.
func moreOverloads(overloads []*decls.OverloadDecl) string {
	if len(overloads) <= 1 {
		return ""
	}
	return fmt.Sprintf(", +%d more", len(overloads)-1)
}

// formatOverloadSignature formats a single overload as "name(arg1, arg2) -> result"
// or "receiver.name(arg1) -> result" for member functions.
func formatOverloadSignature(name string, o *decls.OverloadDecl) string {
	prefix := ""
	if args := o.ArgTypes(); o.IsMemberFunction() && len(args) > 0 {
		prefix = args[0].String() + "."
	}
	return prefix + name + formatOverloadParams(o)
}

// formatOverloadParams formats a single overload's parameters and result as
// "(arg1, arg2) -> result", leaving out a member function's receiver.
func formatOverloadParams(o *decls.OverloadDecl) string {
	args := o.ArgTypes()
	if o.IsMemberFunction() && len(args) > 0 {
		args = args[1:]
	}
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = a.String()
	}
	return fmt.Sprintf("(%s) -> %s", strings.Join(parts, ", "), o.ResultType().String())
}

func docString(s string) protocol.InlayHintTooltip {
	if s == "" {
		return nil
	}
	return &protocol.MarkupContent{
		Kind:  protocol.MarkupKindMarkdown,
		Value: s,
	}
}
