package lsp

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/decls"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
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

	// Dot context: member completions filtered by receiver type.
	if f != nil && isDotContext(f.content, params.Position) {
		receiverType := receiverTypeAtDot(f.content, params.Position, docEnv.celEnv)
		items := fieldCompletionItems(docEnv, receiverType)
		items = append(items, memberCompletionItems(docEnv.celEnv, receiverType)...)
		return s.completionList(items), nil
	}

	// Check for operator context to filter by expected type.
	var expectedType *types.Type
	if f != nil {
		expectedType = expectedTypeAfterOperator(f.content, params.Position, docEnv.celEnv)
	}

	var items []protocol.CompletionItem
	items = append(items, variableCompletionItems(docEnv.celEnv, expectedType)...)
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

// signatureLabelDetails is the labelDetails of a function's overload: its
// parameters and result, which the client shows directly after the function's
// name, and a member function's receiver type, which it shows apart.
func signatureLabelDetails(o *decls.OverloadDecl) *protocol.CompletionItemLabelDetails {
	details := &protocol.CompletionItemLabelDetails{Detail: new(formatOverloadParams(o))}
	if args := o.ArgTypes(); o.IsMemberFunction() && len(args) > 0 {
		details.Description = new(args[0].String())
	}
	return details
}

// isDotContext returns true if the character immediately before the cursor
// position is a dot. This allows member completions to work regardless of
// whether the completion was triggered by typing '.' or by an explicit
// invocation (e.g. Ctrl+Space).
func isDotContext(content string, pos protocol.Position) bool {
	offset := lineColToByteOffset(content, pos.Line, pos.Character)
	return offset > 0 && offset <= len(content) && content[offset-1] == '.'
}

// receiverTypeAtDot extracts the expression before the dot at the given cursor
// position and tries to compile it to determine its type. Returns nil if the
// type cannot be determined.
func receiverTypeAtDot(content string, pos protocol.Position, celEnv *cel.Env) *types.Type {
	offset := lineColToByteOffset(content, pos.Line, pos.Character)
	if offset <= 0 || offset > len(content) {
		return nil
	}

	before := content[:offset-1]
	before = strings.TrimRight(before, " \t\r\n")
	if before == "" {
		return nil
	}

	ast, iss := celEnv.Compile(before)
	if iss.Err() != nil {
		return nil
	}
	return ast.OutputType()
}

// binaryOperatorSymbols returns a map from display symbol (e.g. "&&") to
// cel-go internal name (e.g. "_&&_"), derived from the environment's functions
// and operators.FindReverseBinaryOperator.
func binaryOperatorSymbols(celEnv *cel.Env) map[string]string {
	result := make(map[string]string)
	for name := range celEnv.Functions() {
		display, ok := operators.FindReverseBinaryOperator(name)
		if ok && display != "" {
			// Prefer the canonical form (e.g. "@in" over "_in_").
			if _, exists := result[display]; !exists {
				result[display] = name
			}
		}
	}
	return result
}

// expectedTypeAfterOperator checks if the cursor is positioned after a binary
// operator and determines the expected type for the right-hand operand.
// Returns nil if no operator context is detected or the type can't be determined.
func expectedTypeAfterOperator(content string, pos protocol.Position, celEnv *cel.Env) *types.Type {
	offset := lineColToByteOffset(content, pos.Line, pos.Character)
	if offset <= 0 {
		return nil
	}

	before := strings.TrimRight(content[:offset], " \t\r\n")
	if before == "" {
		return nil
	}

	opSymbols := binaryOperatorSymbols(celEnv)

	// Sort display symbols longest-first so multi-character operators
	// (e.g. "&&") match before single-character ones (e.g. ">").
	symbols := make([]string, 0, len(opSymbols))
	for sym := range opSymbols {
		symbols = append(symbols, sym)
	}
	slices.SortFunc(symbols, func(a, b string) int {
		return cmp.Compare(len(b), len(a))
	})

	// Find a binary operator at the end of the trimmed content.
	var celOp, leftExpr string
	for _, sym := range symbols {
		if strings.HasSuffix(before, sym) {
			candidate := strings.TrimRight(before[:len(before)-len(sym)], " \t\r\n")
			if candidate == "" {
				continue
			}
			celOp = opSymbols[sym]
			leftExpr = candidate
			break
		}
	}
	if celOp == "" {
		return nil
	}

	// Compile the left-hand expression to determine its type.
	ast, iss := celEnv.Compile(leftExpr)
	if iss.Err() != nil {
		return nil
	}
	leftType := ast.OutputType()

	// Find operator overloads that accept leftType and collect expected right types.
	fn, ok := celEnv.Functions()[celOp]
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
			LabelDetails:     signatureLabelDetails(matchingOverloads[0]),
			Detail:           protocol.NewOptional(formatOverloadSignature(fn.Name(), matchingOverloads[0])),
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

// variableCompletionItems offers the variables the environment declares. There
// are none until a configuration declares some, which is why plain CEL offers
// only functions and macros.
func variableCompletionItems(celEnv *cel.Env, expectedType *types.Type) []protocol.CompletionItem {
	var items []protocol.CompletionItem
	for _, variable := range celEnv.Variables() {
		if !typeMatches(expectedType, variable.Type()) {
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

		hasMatchingGlobal := false
		for _, o := range fn.OverloadDecls() {
			if o.IsMemberFunction() {
				continue
			}
			if typeMatches(expectedType, o.ResultType()) {
				hasMatchingGlobal = true
				break
			}
		}
		if !hasMatchingGlobal {
			continue
		}

		overload := firstGlobalOverload(fn)
		items = append(items, protocol.CompletionItem{
			Label:            name,
			LabelDetails:     signatureLabelDetails(overload),
			Kind:             protocol.CompletionItemKindFunction,
			Detail:           protocol.NewOptional(formatOverloadSignature(fn.Name(), overload)),
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

// firstGlobalOverload returns the first of fn's overloads that is not a
// member function.
func firstGlobalOverload(fn *decls.FunctionDecl) *decls.OverloadDecl {
	for _, o := range fn.OverloadDecls() {
		if !o.IsMemberFunction() {
			return o
		}
	}
	return nil
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
