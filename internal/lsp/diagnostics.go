package lsp

import (
	"cmp"
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/env"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/parser/gen"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// checkSeverity returns the severity to report a type-check failure at.
//
// Without declarations, every reference to a variable the expression's real
// environment provides is an undeclared reference, so cells cannot tell a typo
// from a name it was never told about and reports the whole check phase as a
// warning. Once a configuration declares the environment, the same diagnostic
// is a genuine error — including the type errors that would otherwise be
// buried among the undeclared-reference noise.
func checkSeverity(opts Options) protocol.DiagnosticSeverity {
	if opts.declared() {
		return protocol.DiagnosticSeverityError
	}
	return protocol.DiagnosticSeverityWarning
}

// publishDiagnostics computes and pushes diagnostics for the given file.
func (s *server) publishDiagnostics(ctx context.Context, f *file) {
	if s.pullDiagnostics {
		return
	}
	client, ok := protocol.ClientFromContext(ctx)
	if !ok {
		return
	}
	_ = client.PublishDiagnostics(ctx, &protocol.PublishDiagnosticsParams{
		URI:         f.uri,
		Version:     protocol.NewOptional(f.version),
		Diagnostics: s.diagnosticsFor(f),
	})
}

// clearDiagnostics pushes an empty set for a document, which is what retracts
// the diagnostics cells published for it.
//
// A client goes on showing whatever was last published against a URI until the
// server replaces it, so a closed document whose diagnostics are never cleared
// keeps them — in VS Code, in the Problems panel, after the editor is gone.
func (s *server) clearDiagnostics(ctx context.Context, docURI uri.URI) {
	if s.pullDiagnostics {
		return
	}
	client, ok := protocol.ClientFromContext(ctx)
	if !ok {
		return
	}
	// No version: the document this refers to is no longer open, so there is
	// no version of it for the client to match against.
	_ = client.PublishDiagnostics(ctx, &protocol.PublishDiagnosticsParams{
		URI:         docURI,
		Diagnostics: []protocol.Diagnostic{},
	})
}

// Diagnostic handles the pull diagnostic request (textDocument/diagnostic).
func (s *server) Diagnostic(ctx context.Context, params *protocol.DocumentDiagnosticParams) (protocol.DocumentDiagnosticReport, error) {
	items := []protocol.Diagnostic{}
	if f, _ := s.document(ctx, params.TextDocument.URI); f != nil {
		items = s.diagnosticsFor(f)
	}
	return &protocol.RelatedFullDocumentDiagnosticReport{
		Kind:  string(protocol.DocumentDiagnosticReportKindFull),
		Items: items,
	}, nil
}

// diagnosticsFor returns the diagnostics for an open document, which are the
// configuration's own if it does not load: an expression cannot be judged
// against an environment cells could not build, and a configuration error is
// otherwise invisible from inside an editor.
func (s *server) diagnosticsFor(f *file) []protocol.Diagnostic {
	docEnv, err := s.envs.forDocument(f.uri)
	if err != nil {
		return []protocol.Diagnostic{{
			Range:    protocol.Range{},
			Severity: protocol.DiagnosticSeverityError,
			Source:   protocol.NewOptional(serverName),
			Message:  protocol.String(err.Error()),
		}}
	}
	return computeDiagnostics(f, docEnv.celEnv, docEnv.checkSeverity)
}

// computeDiagnostics parses and type-checks a CEL file, returning LSP
// diagnostics. Parse failures are always errors; checkSeverity says how to
// report a type-check failure, per the function of the same name. An
// expression that parses also gets hints for the names its macros bind and
// never use.
func computeDiagnostics(f *file, celEnv *cel.Env, checkSeverity protocol.DiagnosticSeverity) []protocol.Diagnostic {
	if strings.TrimSpace(f.content) == "" {
		return []protocol.Diagnostic{}
	}

	parsed, parseIssues := f.parse(celEnv)
	if parsed == nil {
		return issuesToDiagnostics(f.content, parseIssues, protocol.DiagnosticSeverityError, nil)
	}
	diagnostics := []protocol.Diagnostic{}
	if checked, checkIssues := f.check(celEnv); checked == nil {
		tokens := newSourceTokens(f.content, parsed.NativeRep().SourceInfo())
		diagnostics = issuesToDiagnostics(f.content, checkIssues, checkSeverity, tokens)
	} else if val, ok := f.constantValue(celEnv); ok {
		if evalErr, isErr := val.(*types.Err); isErr {
			diagnostics = append(diagnostics, constantErrorDiagnostic(f.content, parsed.NativeRep(), evalErr))
		}
	}
	return append(diagnostics, unusedVariableDiagnostics(f, celEnv)...)
}

// constantErrorDiagnostic reports the error an expression that takes no input
// always fails with, such as a division by zero. It covers the subexpression
// the error arose in, when cel-go records which, and the whole expression
// otherwise.
func constantErrorDiagnostic(content string, parsed *celast.AST, evalErr *types.Err) protocol.Diagnostic {
	b := newSpanBuilder(newSourceTokens(content, parsed.SourceInfo()))
	b.build(parsed.Expr())
	var r protocol.Range
	if n := b.spans[evalErr.NodeID()]; n != nil && n.start >= 0 {
		r = lspRange(content, n.start, n.end)
	} else {
		trimmed := strings.TrimRight(content, " \t\r\n")
		start := len(trimmed) - len(strings.TrimLeft(trimmed, " \t\r\n"))
		r = lspRange(content, start, len(trimmed))
	}
	return protocol.Diagnostic{
		Range:    r,
		Severity: protocol.DiagnosticSeverityError,
		Source:   protocol.NewOptional(serverName),
		Message:  protocol.String("always fails: " + evalErr.Error()),
	}
}

// diagnosticData is the data an undeclared-reference diagnostic carries for
// its quick fixes.
type diagnosticData struct {
	// Suggestions are the names cel-go suggests in place of the undeclared
	// one, which the diagnostic's range covers exactly.
	Suggestions []string `json:"suggestions,omitempty"`

	// Extensions are the extensions that declare the undeclared name, or the
	// one name in Suggestions when there are any: enabling one of them
	// resolves the reference.
	Extensions []string `json:"extensions,omitempty"`
}

// issuesToDiagnostics converts cel.Issues to LSP diagnostics.
//
// tokens, when not nil, is the lexed source of the parse the issues were found
// in. With it, an undeclared reference is reported on the name itself, rather
// than from the position cel-go records to the end of the line, and carries the
// names cel-go suggests in its place.
func issuesToDiagnostics(content string, issues *cel.Issues, severity protocol.DiagnosticSeverity, tokens *sourceTokens) []protocol.Diagnostic {
	errs := issues.Errors()
	diagnostics := make([]protocol.Diagnostic, 0, len(errs))
	for _, e := range errs {
		if d, ok := undeclaredDiagnostic(content, e, tokens); ok {
			d.Severity = severity
			diagnostics = append(diagnostics, d)
			continue
		}
		loc := e.Location
		// cel-go uses 1-based lines and 0-based rune columns. LSP uses
		// 0-based lines and UTF-16 columns, so convert through the source
		// rather than copying the column directly (an astral character is
		// one rune but two UTF-16 code units).
		line := max(loc.Line()-1, 0)
		col := max(loc.Column(), 0)
		startOffset := max(runeLineColToByteOffset(content, line, col), 0)
		startLine, startCol := byteOffsetToLineCol(content, startOffset)
		startPos := protocol.Position{Line: startLine, Character: startCol}
		// cel-go errors don't include an end position, so we use the end of the line.
		endPos := endOfLine(content, line)

		diagnostics = append(diagnostics, protocol.Diagnostic{
			Range: protocol.Range{
				Start: startPos,
				End:   endPos,
			},
			Severity: severity,
			Source:   protocol.NewOptional(serverName),
			Message:  protocol.String(cleanMessage(e.Message)),
		})
	}
	return diagnostics
}

// undeclaredDiagnostic reports an undeclared reference on the name it refers
// to, and reports false for any other issue, or when tokens is nil or the name
// cannot be found.
//
// cel-go records an identifier at its name but a call at its opening
// parenthesis, so the name is either the token at the recorded position or the
// one before it.
func undeclaredDiagnostic(content string, e *common.Error, tokens *sourceTokens) (protocol.Diagnostic, bool) {
	m := undeclaredRe.FindStringSubmatch(e.Message)
	if m == nil || tokens == nil {
		return protocol.Diagnostic{}, false
	}
	name := m[2]
	i := tokens.anchor(e.ExprID)
	call := i >= 0 && tokens.kind(i) == gen.CELLexerLPAREN
	if call {
		i = tokens.step(i, -1)
	}
	if i < 0 || !tokens.isName(i) || tokens.text(i) != name {
		return protocol.Diagnostic{}, false
	}
	fixes := undeclaredFixes(m, call)
	d := protocol.Diagnostic{
		Range:   lspRange(content, tokens.tokens[i].start, tokens.tokens[i].end),
		Source:  protocol.NewOptional(serverName),
		Message: protocol.String(undeclaredMessage(m, fixes)),
	}
	if len(fixes.Suggestions)+len(fixes.Extensions) > 0 {
		// Marshaling a struct of strings cannot fail.
		d.Data, _ = json.Marshal(fixes)
	}
	return d, true
}

// didYouMeanRe matches the end of an undeclared-reference message that
// suggests one or two declared names. A name the environment lacks a library
// for is suggested in another form, which suggestionRe matches instead.
var didYouMeanRe = regexp.MustCompile(`\(did you mean '([^']+)'(?: or '([^']+)')?\?\)$`)

// operatorNameRe matches quoted cel-go internal operator names like '_+_', '-_', '!_', '@in'.
var operatorNameRe = regexp.MustCompile(`'([^']+)'`)

// undeclaredRe matches cel-go's undeclared-reference message, capturing the
// name and the parenthesized hint, if any, that follows it.
var undeclaredRe = regexp.MustCompile(`^(undeclared reference to '([^']+)')(?: \((.*)\))?$`)

// suggestionRe matches the hint cel-go gives for a name in a library the
// environment lacks: the suggested name, then the Go option enabling it.
var suggestionRe = regexp.MustCompile("^did you mean '([^']+)'\\?, enable with `[^`]*`$")

// cleanMessage rewrites cel-go internal operator names to user-friendly forms
// using operators.FindReverse, and names the cells extension that declares an
// undeclared reference.
func cleanMessage(msg string) string {
	return extensionHint(cleanOperators(msg))
}

// extensionHint replaces the hint on an undeclared-reference message with the
// cells extensions that declare the name, or the name cel-go suggests instead.
// cel-go's own hint names a Go option, which a cells user cannot act on.
func extensionHint(msg string) string {
	m := undeclaredRe.FindStringSubmatch(msg)
	if m == nil {
		return msg
	}
	return undeclaredMessage(m, undeclaredFixes(m, false))
}

// undeclaredMessage returns the message for the undeclared reference whose
// message undeclaredRe matched as m, with the hint cel-go gave replaced when
// fixes enables an extension.
func undeclaredMessage(m []string, fixes diagnosticData) string {
	switch {
	case len(fixes.Extensions) == 0:
		return m[0]
	case len(fixes.Suggestions) == 0:
		return m[1] + " (" + enableExtensions(fixes.Extensions) + ")"
	default:
		return m[1] + " (did you mean '" + fixes.Suggestions[0] + "'?, " + enableExtensions(fixes.Extensions) + ")"
	}
}

// undeclaredFixes returns what would resolve the undeclared reference whose
// message undeclaredRe matched as m:
//
//   - the extensions that declare the name, when it is in one;
//   - the name cel-go suggests in its place and the extensions that declare
//     that, when the suggestion is in one;
//   - the one or two declared names cel-go suggests in its place;
//   - when call is set, so that the name is a function's, the closest
//     function an extension declares and that extension, if one is close
//     enough. cel-go only suggests such names for the optional library.
func undeclaredFixes(m []string, call bool) diagnosticData {
	if exts := extensionsByName()[m[2]]; len(exts) > 0 {
		return diagnosticData{Extensions: exts}
	}
	if s := suggestionRe.FindStringSubmatch(m[3]); s != nil {
		if exts := extensionsByName()[s[1]]; len(exts) > 0 {
			return diagnosticData{Suggestions: []string{s[1]}, Extensions: exts}
		}
		return diagnosticData{}
	}
	if s := didYouMeanRe.FindStringSubmatch(m[0]); s != nil {
		if s[2] != "" {
			return diagnosticData{Suggestions: []string{s[1], s[2]}}
		}
		return diagnosticData{Suggestions: []string{s[1]}}
	}
	if call {
		if s, ok := extensionSuggestion(m[2]); ok {
			return diagnosticData{Suggestions: []string{s}, Extensions: extensionsByName()[s]}
		}
	}
	return diagnosticData{}
}

// extensionSuggestion returns the name an extension declares that is closest
// to name, within the edit distance cel-go allows its own suggestions. Unlike
// cel-go, it does not fall back to names that name is a prefix of: a variable
// cells was not told about is no misspelling of a longer function.
func extensionSuggestion(name string) (string, bool) {
	maxDist := 2
	if len(name) <= 3 {
		maxDist = 1
	} else if len(name) > 8 {
		maxDist = len(name) / 3
	}
	best, bestDist := "", maxDist+1
	for candidate := range extensionsByName() {
		if strings.Contains(candidate, ".") != strings.Contains(name, ".") {
			continue
		}
		dist := env.EditDistance(name, candidate)
		if dist < bestDist || dist == bestDist && cmp.Or(cmp.Compare(len(candidate), len(best)), strings.Compare(candidate, best)) < 0 {
			best, bestDist = candidate, dist
		}
	}
	return best, best != ""
}

// enableExtensions phrases the advice to enable one of exts.
func enableExtensions(exts []string) string {
	quoted := make([]string, len(exts))
	for i, ext := range exts {
		quoted[i] = "`" + ext + "`"
	}
	noun := "extension"
	if len(exts) > 1 {
		noun = "extensions"
	}
	return "enable the " + strings.Join(quoted, " or ") + " " + noun
}

// cleanOperators rewrites cel-go internal operator names to user-friendly
// forms using operators.FindReverse.
func cleanOperators(msg string) string {
	return operatorNameRe.ReplaceAllStringFunc(msg, func(match string) string {
		// Strip surrounding quotes.
		symbol := match[1 : len(match)-1]
		if display, ok := operators.FindReverse(symbol); ok && display != "" {
			return "'" + display + "'"
		}
		return match
	})
}

// endOfLine returns the Position at the end of the given 0-based line.
func endOfLine(content string, line int) protocol.Position {
	currentLine := 0
	i := 0
	for i < len(content) {
		if currentLine == line {
			// Find end of this line.
			end := i
			for end < len(content) && content[end] != '\n' {
				end++
			}
			_, character := byteOffsetToLineCol(content, end)
			return protocol.Position{Line: uint32(line), Character: character}
		}
		if content[i] == '\n' {
			currentLine++
		}
		i++
	}
	// Fallback: end of file.
	return protocol.Position{Line: uint32(line), Character: 0}
}
