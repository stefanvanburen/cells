package lsp

import (
	"encoding/json"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

func (s *server) formatting(req *jsonrpc2.Request) (any, error) {
	var params protocol.DocumentFormattingParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	if f == nil {
		return nil, nil
	}

	formatted, err := formatCEL(f.content, s.celEnv)
	if err != nil {
		// If formatting fails (e.g., parse error), return no edits.
		return nil, nil
	}

	if formatted == f.content {
		return nil, nil
	}

	// Replace the entire document.
	lines := strings.Count(f.content, "\n")
	lastNewline := strings.LastIndex(f.content, "\n")
	lastLineLen := len(f.content) - lastNewline - 1
	if lastNewline == -1 {
		lastLineLen = len(f.content)
	}

	return []protocol.TextEdit{{
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: uint32(lines), Character: uint32(lastLineLen)},
		},
		NewText: formatted,
	}}, nil
}

// formatCEL formats a CEL expression, preserving comments.
//
// cel-go's parser discards comments, so we extract leading comment lines and
// trailing comment lines from the source, format the expression portion via
// cel.AstToString, and reattach the comments.
//
// If any comments exist within the expression body (interleaved comments that
// we can't safely reposition), formatting is skipped to avoid losing them.
func formatCEL(content string, celEnv *cel.Env) (string, error) {
	leading, expr, trailing, ok := splitComments(content)
	if !ok {
		// Comments interleaved with expression — unsafe to format.
		return content, nil
	}

	ast, iss := celEnv.Parse(expr)
	if iss.Err() != nil {
		return "", iss.Err()
	}

	formatted, err := cel.AstToString(ast)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if leading != "" {
		b.WriteString(leading)
	}
	b.WriteString(formatted)
	if trailing != "" {
		b.WriteString(trailing)
	}

	// Preserve trailing newline if the original had one.
	result := b.String()
	if strings.HasSuffix(content, "\n") && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}

	return result, nil
}

// splitComments separates a CEL source into:
//   - leading: comment/blank lines before the expression (with trailing \n)
//   - expr: the expression body (no comments)
//   - trailing: inline comment + comment/blank lines after the expression
//   - ok: false if there are interleaved comments we can't safely handle
//
// The expression body must be contiguous non-comment lines. If any comment
// lines appear between expression lines, ok is false.
func splitComments(content string) (leading, expr, trailing string, ok bool) {
	lines := strings.Split(content, "\n")

	// Find leading comment/blank lines.
	leadEnd := 0
	for leadEnd < len(lines) {
		trimmed := strings.TrimSpace(lines[leadEnd])
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			leadEnd++
		} else {
			break
		}
	}

	// Find trailing comment/blank lines (scanning from the end).
	trailStart := len(lines)
	for trailStart > leadEnd {
		trimmed := strings.TrimSpace(lines[trailStart-1])
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			trailStart--
		} else {
			break
		}
	}

	// The expression body is lines[leadEnd:trailStart].
	exprLines := lines[leadEnd:trailStart]

	// Check for interleaved comments within the expression body.
	// We allow an inline comment on the LAST expression line only.
	for i, line := range exprLines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			// A pure comment line in the middle of the expression.
			return "", "", "", false
		}
		// Check for inline comments (// not inside a string).
		if i < len(exprLines)-1 {
			_, comment := stripInlineComment(line)
			if comment != "" {
				// Inline comment on a non-last expression line.
				return "", "", "", false
			}
		}
	}

	if leadEnd > 0 {
		leading = strings.Join(lines[:leadEnd], "\n") + "\n"
	}

	// Handle inline comment on the last expression line.
	if len(exprLines) > 0 {
		lastIdx := len(exprLines) - 1
		clean, comment := stripInlineComment(exprLines[lastIdx])
		exprLines[lastIdx] = clean
		if comment != "" {
			trailing = " " + comment
		}
	}

	expr = strings.Join(exprLines, "\n")

	// Append trailing comment lines.
	if trailStart < len(lines) {
		trailLines := lines[trailStart:]
		joined := strings.Join(trailLines, "\n")
		if strings.TrimSpace(joined) != "" {
			if trailing != "" {
				trailing += "\n" + joined
			} else {
				trailing = "\n" + joined
			}
		}
	}

	return leading, expr, trailing, true
}

// stripInlineComment splits a line into the code portion and any trailing
// // comment. It respects strings (including triple-quoted) so that // inside
// quotes isn't treated as a comment.
func stripInlineComment(line string) (code, comment string) {
	inDouble := false
	inSingle := false
	inTripleDouble := false
	inTripleSingle := false

	for i := 0; i < len(line); i++ {
		ch := line[i]

		// Handle escape sequences inside strings.
		if (inDouble || inSingle || inTripleDouble || inTripleSingle) && ch == '\\' && i+1 < len(line) {
			i++
			continue
		}

		// Triple-double-quote: """
		if !inSingle && !inTripleSingle && i+2 < len(line) && line[i] == '"' && line[i+1] == '"' && line[i+2] == '"' {
			if inTripleDouble {
				inTripleDouble = false
				i += 2
				continue
			}
			if !inDouble {
				inTripleDouble = true
				i += 2
				continue
			}
		}

		// Triple-single-quote: '''
		if !inDouble && !inTripleDouble && i+2 < len(line) && line[i] == '\'' && line[i+1] == '\'' && line[i+2] == '\'' {
			if inTripleSingle {
				inTripleSingle = false
				i += 2
				continue
			}
			if !inSingle {
				inTripleSingle = true
				i += 2
				continue
			}
		}

		// Regular double-quote (not inside triple-quote).
		if ch == '"' && !inSingle && !inTripleDouble && !inTripleSingle {
			inDouble = !inDouble
			continue
		}

		// Regular single-quote (not inside triple-quote).
		if ch == '\'' && !inDouble && !inTripleDouble && !inTripleSingle {
			inSingle = !inSingle
			continue
		}

		// Comment detection — only outside any string.
		if !inDouble && !inSingle && !inTripleDouble && !inTripleSingle {
			if ch == '/' && i+1 < len(line) && line[i+1] == '/' {
				return strings.TrimRight(line[:i], " \t"), strings.TrimSpace(line[i:])
			}
		}
	}
	return line, ""
}
