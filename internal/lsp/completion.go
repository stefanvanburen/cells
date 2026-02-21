package lsp

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/decls"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// celKeywords are the literal keywords in CEL. These are language-level
// constants that aren't discoverable through cel-go's function or macro APIs.
var celKeywords = []string{"true", "false", "null"}

func (s *server) completion(req *jsonrpc2.Request) (any, error) {
	var params protocol.CompletionParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	// Dot context: member completions filtered by receiver type.
	if f != nil && isDotContext(f.content, params.Position) {
		receiverType := receiverTypeAtDot(f.content, params.Position, s.celEnv)
		items := memberCompletionItems(s.celEnv, receiverType)
		return &protocol.CompletionList{
			IsIncomplete: false,
			Items:        items,
		}, nil
	}

	// Check for operator context to filter by expected type.
	var expectedType *types.Type
	if f != nil {
		expectedType = expectedTypeAfterOperator(f.content, params.Position, s.celEnv)
	}

	var items []protocol.CompletionItem
	items = append(items, globalCompletionItems(s.celEnv, expectedType)...)
	items = append(items, macroCompletionItems(s.celEnv, expectedType)...)
	items = append(items, keywordCompletionItems(s.celEnv, expectedType)...)
	return &protocol.CompletionList{
		IsIncomplete: false,
		Items:        items,
	}, nil
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

		snippet := protocol.SnippetTextFormat
		items = append(items, protocol.CompletionItem{
			Label:            name,
			Kind:             protocol.MethodCompletion,
			Detail:           formatOverloadSignature(fn.Name(), matchingOverloads[0]),
			Documentation:    docString(fn.Description()),
			InsertText:       name + "($1)",
			InsertTextFormat: &snippet,
		})
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

		snippet := protocol.SnippetTextFormat
		items = append(items, protocol.CompletionItem{
			Label:            name,
			Kind:             protocol.FunctionCompletion,
			Detail:           globalFunctionDetail(fn),
			Documentation:    docString(fn.Description()),
			InsertText:       name + "($1)",
			InsertTextFormat: &snippet,
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

		snippet := protocol.SnippetTextFormat
		items = append(items, protocol.CompletionItem{
			Label:            name,
			Kind:             protocol.FunctionCompletion,
			Detail:           "macro",
			InsertText:       name + "($1)",
			InsertTextFormat: &snippet,
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
			Label:  kw,
			Kind:   protocol.KeywordCompletion,
			Detail: kwType.String(),
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

// globalFunctionDetail produces a short detail string for a global function.
func globalFunctionDetail(fn *decls.FunctionDecl) string {
	for _, o := range fn.OverloadDecls() {
		if o.IsMemberFunction() {
			continue
		}
		return formatOverloadSignature(fn.Name(), o)
	}
	return ""
}

// formatOverloadSignature formats a single overload as "name(arg1, arg2) -> result"
// or "receiver.name(arg1) -> result" for member functions.
func formatOverloadSignature(name string, o *decls.OverloadDecl) string {
	args := o.ArgTypes()
	var parts []string
	start := 0
	prefix := ""
	if o.IsMemberFunction() && len(args) > 0 {
		prefix = args[0].String() + "."
		start = 1
	}
	for _, a := range args[start:] {
		parts = append(parts, a.String())
	}
	result := o.ResultType().String()
	return fmt.Sprintf("%s%s(%s) -> %s", prefix, name, strings.Join(parts, ", "), result)
}

func docString(s string) *protocol.Or_CompletionItem_documentation {
	if s == "" {
		return nil
	}
	return &protocol.Or_CompletionItem_documentation{
		Value: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: s,
		},
	}
}
