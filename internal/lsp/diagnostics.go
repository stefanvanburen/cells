package lsp

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common"
	"cel.dev/cel-go/common/operators"
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
// report a type-check failure, per the function of the same name.
func computeDiagnostics(f *file, celEnv *cel.Env, checkSeverity protocol.DiagnosticSeverity) []protocol.Diagnostic {
	if strings.TrimSpace(f.content) == "" {
		return []protocol.Diagnostic{}
	}

	parsed, parseIssues := f.parse(celEnv)
	if parsed == nil {
		return issuesToDiagnostics(f.content, parseIssues, protocol.DiagnosticSeverityError, nil)
	}
	if checked, checkIssues := f.check(celEnv); checked == nil {
		tokens := newSourceTokens(f.content, parsed.NativeRep().SourceInfo())
		return issuesToDiagnostics(f.content, checkIssues, checkSeverity, tokens)
	}

	// No errors — clear diagnostics.
	return []protocol.Diagnostic{}
}

// diagnosticData is the data an undeclared-reference diagnostic carries for
// its quick fixes.
type diagnosticData struct {
	// Suggestions are the declared names cel-go suggests in place of the
	// undeclared one, which the diagnostic's range covers exactly.
	Suggestions []string `json:"suggestions"`
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
	if i >= 0 && tokens.kind(i) == gen.CELLexerLPAREN {
		i = tokens.step(i, -1)
	}
	if i < 0 || !tokens.isName(i) || tokens.text(i) != name {
		return protocol.Diagnostic{}, false
	}
	d := protocol.Diagnostic{
		Range:   lspRange(content, tokens.tokens[i].start, tokens.tokens[i].end),
		Source:  protocol.NewOptional(serverName),
		Message: protocol.String(cleanMessage(e.Message)),
	}
	if s := didYouMeanRe.FindStringSubmatch(e.Message); s != nil {
		suggestions := []string{s[1]}
		if s[2] != "" {
			suggestions = append(suggestions, s[2])
		}
		// Marshaling a struct of strings cannot fail.
		d.Data, _ = json.Marshal(diagnosticData{Suggestions: suggestions})
	}
	return d, true
}

// didYouMeanRe matches the end of an undeclared-reference message that
// suggests one or two declared names. A name the environment lacks a library
// for is suggested in another form, and is left out: replacing the reference
// with it would only trade one error for another.
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
	if exts := extensionsByName()[m[2]]; len(exts) > 0 {
		return m[1] + " (" + enableExtensions(exts) + ")"
	}
	if s := suggestionRe.FindStringSubmatch(m[3]); s != nil {
		if exts := extensionsByName()[s[1]]; len(exts) > 0 {
			return m[1] + " (did you mean '" + s[1] + "'?, " + enableExtensions(exts) + ")"
		}
	}
	return msg
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
