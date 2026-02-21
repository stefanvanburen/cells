package lsp

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/operators"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// publishDiagnostics computes and pushes diagnostics for the given file.
func publishDiagnostics(conn *jsonrpc2.Conn, uri protocol.DocumentURI, version int32, content string, celEnv *cel.Env) {
	diagnostics := computeDiagnostics(content, celEnv)
	_ = conn.Notify(context.Background(), "textDocument/publishDiagnostics", protocol.PublishDiagnosticsParams{
		URI:         uri,
		Version:     version,
		Diagnostics: diagnostics,
	})
}

// diagnosticFull handles the pull diagnostic request (textDocument/diagnostic).
func (s *server) diagnosticFull(req *jsonrpc2.Request) (any, error) {
	var params protocol.DocumentDiagnosticParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	var content string
	if f != nil {
		content = f.content
	}
	s.mu.Unlock()

	if f == nil {
		return protocol.RelatedFullDocumentDiagnosticReport{
			FullDocumentDiagnosticReport: protocol.FullDocumentDiagnosticReport{
				Kind:  string(protocol.DiagnosticFull),
				Items: []protocol.Diagnostic{},
			},
		}, nil
	}

	return protocol.RelatedFullDocumentDiagnosticReport{
		FullDocumentDiagnosticReport: protocol.FullDocumentDiagnosticReport{
			Kind:  string(protocol.DiagnosticFull),
			Items: computeDiagnostics(content, s.celEnv),
		},
	}, nil
}

// computeDiagnostics parses and type-checks a CEL file, returning LSP diagnostics.
func computeDiagnostics(content string, celEnv *cel.Env) []protocol.Diagnostic {
	if strings.TrimSpace(content) == "" {
		return []protocol.Diagnostic{}
	}

	// Parse phase.
	parsed, parseIssues := celEnv.Parse(content)
	if parseIssues.Err() != nil {
		return issuesToDiagnostics(content, parseIssues, protocol.SeverityError)
	}

	// Check (type-check) phase.
	_, checkIssues := celEnv.Check(parsed)
	if checkIssues.Err() != nil {
		return issuesToDiagnostics(content, checkIssues, protocol.SeverityWarning)
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
			Source:   serverName,
			Message:  cleanMessage(e.Message),
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
