package lsp

import (
	"slices"

	"cel.dev/cel-go/cel"
	"go.lsp.dev/protocol"
)

func newCELEnv() (*cel.Env, error) {
	return cel.NewEnv(cel.EnableMacroCallTracking())
}

// Format formats the given CEL content.
// Returns the original content unchanged if formatting is not possible (e.g., parse errors).
func Format(content string) (string, error) {
	env, err := newCELEnv()
	if err != nil {
		return "", err
	}
	return formatCEL(content, env)
}

// CheckDiagnostic represents a single diagnostic from parsing or type-checking.
type CheckDiagnostic struct {
	Line     int    // 1-indexed
	Col      int    // 1-indexed, UTF-8 bytes
	EndLine  int    // 1-indexed
	EndCol   int    // 1-indexed, UTF-8 bytes
	Severity string // "error" or "warning"
	Message  string
}

// Check parses and type-checks the given CEL content, returning any diagnostics.
func Check(content string) ([]CheckDiagnostic, error) {
	env, err := newCELEnv()
	if err != nil {
		return nil, err
	}
	diags := computeDiagnostics(content, env)
	result := make([]CheckDiagnostic, 0, len(diags))
	for _, d := range diags {
		sev := "error"
		if d.Severity == protocol.DiagnosticSeverityWarning {
			sev = "warning"
		}
		message, _ := d.Message.(protocol.String)
		result = append(result, CheckDiagnostic{
			Line:     int(d.Range.Start.Line) + 1,
			Col:      int(d.Range.Start.Character) + 1,
			EndLine:  int(d.Range.End.Line) + 1,
			EndCol:   int(d.Range.End.Character) + 1,
			Severity: sev,
			Message:  string(message),
		})
	}
	return result, nil
}

// Hover returns hover documentation for the element at the given 1-indexed line:col position.
// Returns an empty string if no hover info is available.
func Hover(content string, line, col int) (string, error) {
	env, err := newCELEnv()
	if err != nil {
		return "", err
	}
	f := &file{uri: "file:///cli", content: content}
	result, err := computeHover(f, env, protocol.Position{
		Line:      uint32(line - 1),
		Character: uint32(col - 1),
	})
	if err != nil || result == nil {
		return "", err
	}
	markup, _ := result.Contents.(*protocol.MarkupContent)
	if markup == nil {
		return "", nil
	}
	return markup.Value, nil
}

// Reference is a source location (1-indexed, UTF-8 byte columns).
type Reference struct {
	Line    int
	Col     int
	EndLine int
	EndCol  int
}

// References returns all references to the identifier at the given 1-indexed line:col position.
func References(content string, line, col int) ([]Reference, error) {
	env, err := newCELEnv()
	if err != nil {
		return nil, err
	}
	f := &file{uri: "file:///cli", content: content}
	locs, err := computeReferences(f, env, protocol.ReferenceParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: f.uri},
		Position: protocol.Position{
			Line:      uint32(line - 1),
			Character: uint32(col - 1),
		},
	})
	if err != nil {
		return nil, err
	}
	refs := make([]Reference, 0, len(locs))
	for _, loc := range locs {
		refs = append(refs, Reference{
			Line:    int(loc.Range.Start.Line) + 1,
			Col:     int(loc.Range.Start.Character) + 1,
			EndLine: int(loc.Range.End.Line) + 1,
			EndCol:  int(loc.Range.End.Character) + 1,
		})
	}
	return refs, nil
}

// Rename renames the identifier at the given 1-indexed line:col to newName.
// Returns the updated content, or the original content if nothing was renamed.
func Rename(content string, line, col int, newName string) (string, error) {
	env, err := newCELEnv()
	if err != nil {
		return "", err
	}
	f := &file{uri: "file:///cli", content: content}
	edit, err := computeRename(f, env, protocol.RenameParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: f.uri},
		Position: protocol.Position{
			Line:      uint32(line - 1),
			Character: uint32(col - 1),
		},
		NewName: newName,
	})
	if err != nil {
		return "", err
	}
	if edit == nil {
		return content, nil
	}
	if edits, ok := edit.Changes[f.uri]; ok {
		return applyTextEdits(content, edits), nil
	}
	return content, nil
}

// applyTextEdits applies a set of LSP text edits to content and returns the updated content.
// Edits must be non-overlapping.
func applyTextEdits(content string, edits []protocol.TextEdit) string {
	type byteEdit struct {
		start, end int
		newText    string
	}

	byteEdits := make([]byteEdit, 0, len(edits))
	for _, e := range edits {
		start := lineColToByteOffset(content, e.Range.Start.Line, e.Range.Start.Character)
		end := lineColToByteOffset(content, e.Range.End.Line, e.Range.End.Character)
		if start < 0 || end < 0 {
			continue
		}
		byteEdits = append(byteEdits, byteEdit{start, end, e.NewText})
	}

	// Sort in reverse order so earlier offsets are not invalidated as we apply edits.
	slices.SortFunc(byteEdits, func(a, b byteEdit) int {
		return b.start - a.start
	})

	for _, e := range byteEdits {
		content = content[:e.start] + e.newText + content[e.end:]
	}
	return content
}
