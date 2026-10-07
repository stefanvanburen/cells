package lsp

import (
	"context"
	"slices"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common"
	"cel.dev/cel-go/parser/gen"
	"go.lsp.dev/protocol"
)

func (s *server) SignatureHelp(ctx context.Context, params *protocol.SignatureHelpParams) (*protocol.SignatureHelp, error) {
	f, docEnv := s.document(ctx, params.TextDocument.URI)

	if f == nil || docEnv == nil || f.content == "" {
		return nil, nil
	}

	return computeSignatureHelp(f, docEnv.celEnv, params.Position)
}

func computeSignatureHelp(f *file, celEnv *cel.Env, pos protocol.Position) (*protocol.SignatureHelp, error) {
	// The cursor may sit at the very end of the content, after a call's
	// opening parenthesis or a comma.
	offset := lineColToByteOffset(f.content, pos.Line, pos.Character)
	if offset < 0 || offset > len(f.content) {
		return nil, nil
	}

	call, ok := enclosingCall(f.content, offset)
	if !ok {
		return nil, nil
	}

	// A call written with a dotted name is either a namespaced function, such
	// as math.abs, or a member function called on the names before it.
	funcs := celEnv.Functions()
	name := strings.Join(call.names, ".")
	funcDecl, ok := funcs[name]
	member := false
	if !ok || call.receiver {
		name = call.names[len(call.names)-1]
		funcDecl, ok = funcs[name]
		member = call.receiver || len(call.names) > 1
	}
	if !ok {
		return nil, nil
	}

	sigs := generateSignatures(funcDecl, name, member)
	if len(sigs) == 0 {
		return nil, nil
	}

	return &protocol.SignatureHelp{
		Signatures:      sigs,
		ActiveSignature: &firstSignature,
		ActiveParameter: protocol.NewNullable(call.arg),
	}, nil
}

// firstSignature is always the active signature index; shared across
// requests to avoid allocating a *uint32 per call.
var firstSignature uint32

// callSite is a call whose argument list is open at the cursor.
type callSite struct {
	// names is the dotted name the call is written with, such as
	// ["math", "abs"] or ["s", "startsWith"].
	names []string
	// receiver reports whether the names follow a dot, as in
	// "a".startsWith(, making the call a member call on what precedes it.
	receiver bool
	// arg is the index of the argument the cursor is in.
	arg uint32
}

// enclosingCall finds the innermost call whose argument list is open at
// offset. It reads the tokens rather than the AST, so that it finds a call
// still being typed: one with no closing parenthesis, or an argument yet to
// be written after a comma.
func enclosingCall(content string, offset int) (callSite, bool) {
	t := &sourceTokens{content: content, tokens: lexCEL(content)}

	// open holds each bracket open at offset, with the commas directly
	// inside it so far.
	type bracket struct {
		index  int
		commas uint32
	}
	var open []bracket
	for i, tok := range t.tokens {
		if tok.start >= offset {
			break
		}
		switch tok.kind {
		case gen.CELLexerLPAREN, gen.CELLexerLBRACKET, gen.CELLexerLBRACE:
			open = append(open, bracket{index: i})
		case gen.CELLexerRPAREN, gen.CELLexerRPRACKET, gen.CELLexerRBRACE:
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
		case gen.CELLexerCOMMA:
			if len(open) > 0 {
				open[len(open)-1].commas++
			}
		}
	}

	// Skip the lists, maps and parenthesized expressions the cursor is in, up
	// to the parenthesis that follows a function's name.
	for _, b := range slices.Backward(open) {
		if t.kind(b.index) != gen.CELLexerLPAREN {
			continue
		}
		i := t.step(b.index, -1)
		if !t.isName(i) {
			continue
		}
		names := []string{t.text(i)}
		for dot := t.step(i, -1); t.kind(dot) == gen.CELLexerDOT; dot = t.step(i, -1) {
			prev := t.step(dot, -1)
			if !t.isName(prev) {
				return callSite{names: names, receiver: true, arg: b.commas}, true
			}
			names = append([]string{t.text(prev)}, names...)
			i = prev
		}
		return callSite{names: names, arg: b.commas}, true
	}
	return callSite{}, false
}

// generateSignatures creates protocol.SignatureInformation for overloads of a function.
// funcDecl must implement the interface with Documentation() method.
// If isMemberFunction is true, only member function overloads of name are
// included; if false, only global function overloads are included.
func generateSignatures(funcDecl any, name string, isMemberFunction bool) []protocol.SignatureInformation {
	// Try to get documentation if available.
	var doc *common.Doc
	if documenter, ok := funcDecl.(interface{ Documentation() *common.Doc }); ok {
		doc = documenter.Documentation()
	}

	if doc == nil {
		// Fallback for functions without documentation.
		return []protocol.SignatureInformation{{
			Label: "function()",
		}}
	}

	// If the main doc has a signature, use it as the primary signature.
	// Check if it matches the expected call type.
	if doc.Signature != "" {
		if isSignatureMatchingCallType(doc.Signature, name, isMemberFunction) {
			sig := protocol.SignatureInformation{
				Label:      doc.Signature,
				Parameters: extractParametersFromSignature(doc.Signature, name),
			}
			if doc.Description != "" {
				sig.Documentation = protocol.String(doc.Description)
			}
			return []protocol.SignatureInformation{sig}
		}
	}

	// Use child signatures as overloads, filtering by call type.
	var sigs []protocol.SignatureInformation
	for _, child := range doc.Children {
		if child.Signature != "" && isSignatureMatchingCallType(child.Signature, name, isMemberFunction) {
			sig := protocol.SignatureInformation{
				Label:      child.Signature,
				Parameters: extractParametersFromSignature(child.Signature, name),
			}
			if child.Description != "" {
				sig.Documentation = protocol.String(child.Description)
			}
			sigs = append(sigs, sig)
		}
	}

	if len(sigs) > 0 {
		return sigs
	}

	// Final fallback.
	return []protocol.SignatureInformation{{
		Label: "function()",
	}}
}

// isSignatureMatchingCallType reports whether a signature of the function
// name is for the given kind of call. A member signature names the receiver's
// type before the function, as in "string.matches(string) -> bool", and a
// global one starts with the name, as in "matches(string, string) -> bool" or
// "math.abs(int) -> int".
func isSignatureMatchingCallType(signature, name string, isMemberFunction bool) bool {
	if isMemberFunction {
		return strings.Contains(signature, "."+name+"(")
	}
	return strings.HasPrefix(signature, name+"(")
}

// extractParametersFromSignature parses the parameters from a signature of the
// function name, such as "list(<A>).join(string) -> string": those in the
// parentheses after the name, before the result type.
func extractParametersFromSignature(signature, name string) []protocol.ParameterInformation {
	_, rest, ok := strings.Cut(signature, name+"(")
	if !ok {
		return nil
	}
	rest, _, _ = strings.Cut(rest, " -> ")
	closeIdx := strings.LastIndex(rest, ")")
	if closeIdx == -1 {
		return nil
	}

	paramsStr := rest[:closeIdx]
	if paramsStr == "" {
		return nil
	}

	// Split by comma, handling nested parens/brackets.
	var params []protocol.ParameterInformation
	var current strings.Builder
	depth := 0

	for _, ch := range paramsStr {
		switch ch {
		case '(':
			depth++
			current.WriteRune(ch)
		case ')':
			depth--
			current.WriteRune(ch)
		case ',':
			if depth == 0 {
				paramStr := strings.TrimSpace(current.String())
				if paramStr != "" {
					params = append(params, protocol.ParameterInformation{
						Label: protocol.String(paramStr),
					})
				}
				current.Reset()
			} else {
				current.WriteRune(ch)
			}
		default:
			current.WriteRune(ch)
		}
	}

	// Add the last parameter.
	paramStr := strings.TrimSpace(current.String())
	if paramStr != "" {
		params = append(params, protocol.ParameterInformation{
			Label: protocol.String(paramStr),
		})
	}

	return params
}
