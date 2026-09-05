package lsp

import (
	"context"
	"regexp"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/operators"
	"go.lsp.dev/protocol"
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
func publishDiagnostics(ctx context.Context, f *file, celEnv *cel.Env, checkSeverity protocol.DiagnosticSeverity) {
	client, ok := protocol.ClientFromContext(ctx)
	if !ok {
		return
	}
	_ = client.PublishDiagnostics(ctx, &protocol.PublishDiagnosticsParams{
		URI:         f.uri,
		Version:     protocol.NewOptional(f.version),
		Diagnostics: computeDiagnostics(f, celEnv, checkSeverity),
	})
}

// Diagnostic handles the pull diagnostic request (textDocument/diagnostic).
func (s *server) Diagnostic(_ context.Context, params *protocol.DocumentDiagnosticParams) (protocol.DocumentDiagnosticReport, error) {
	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	items := []protocol.Diagnostic{}
	if f != nil {
		items = computeDiagnostics(f, s.celEnv, s.checkSeverity)
	}
	return &protocol.RelatedFullDocumentDiagnosticReport{
		Kind:  string(protocol.DocumentDiagnosticReportKindFull),
		Items: items,
	}, nil
}

// computeDiagnostics parses and type-checks a CEL file, returning LSP
// diagnostics. Parse failures are always errors; checkSeverity says how to
// report a type-check failure, per the function of the same name.
func computeDiagnostics(f *file, celEnv *cel.Env, checkSeverity protocol.DiagnosticSeverity) []protocol.Diagnostic {
	if strings.TrimSpace(f.content) == "" {
		return []protocol.Diagnostic{}
	}

	if parsed, parseIssues := f.parse(celEnv); parsed == nil {
		return issuesToDiagnostics(f.content, parseIssues, protocol.DiagnosticSeverityError)
	}
	if checked, checkIssues := f.check(celEnv); checked == nil {
		return issuesToDiagnostics(f.content, checkIssues, checkSeverity)
	}

	// No errors — clear diagnostics.
	return []protocol.Diagnostic{}
}

// issuesToDiagnostics converts cel.Issues to LSP diagnostics.
func issuesToDiagnostics(content string, issues *cel.Issues, severity protocol.DiagnosticSeverity) []protocol.Diagnostic {
	errs := issues.Errors()
	diagnostics := make([]protocol.Diagnostic, 0, len(errs))
	for _, e := range errs {
		loc := e.Location
		// cel-go uses 1-based line, 0-based column. LSP uses 0-based for both.
		line := loc.Line() - 1
		col := loc.Column()
		if line < 0 {
			line = 0
		}
		if col < 0 {
			col = 0
		}
		startPos := protocol.Position{Line: uint32(line), Character: uint32(col)}
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

// operatorNameRe matches quoted cel-go internal operator names like '_+_', '-_', '!_', '@in'.
var operatorNameRe = regexp.MustCompile(`'([^']+)'`)

// cleanMessage rewrites cel-go internal operator names to user-friendly forms
// using operators.FindReverse.
func cleanMessage(msg string) string {
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
			return protocol.Position{Line: uint32(line), Character: uint32(end - i)}
		}
		if content[i] == '\n' {
			currentLine++
		}
		i++
	}
	// Fallback: end of file.
	return protocol.Position{Line: uint32(line), Character: 0}
}
