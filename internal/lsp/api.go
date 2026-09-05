package lsp

import (
	"slices"

	"go.lsp.dev/protocol"
	lspuri "go.lsp.dev/uri"
)

// ExtensionNames returns the names of the CEL extension libraries that can be
// named in Options.Extensions, sorted alphabetically.
func ExtensionNames() []string {
	return sortedExtensionNames()
}

// ValidateExtensions reports an error naming the first extension in names
// that is not one of ExtensionNames.
func ValidateExtensions(names []string) error {
	_, err := resolveExtensions(names)
	return err
}

// Format formats the given CEL content.
// Returns the original content unchanged if formatting is not possible (e.g., parse errors).
func Format(content string, opts Options) (string, error) {
	docEnv, err := newEnvironment(opts)
	if err != nil {
		return "", err
	}
	return formatCEL(content, docEnv.celEnv)
}

// cliURI stands in for a document URI when these entry points are called from
// the command line, where there is no editor and no open document.
const cliURI = "file:///cli"

// CheckDiagnostic represents a single diagnostic from parsing or type-checking.
type CheckDiagnostic struct {
	Line     int    // 1-indexed
	Col      int    // 1-indexed, UTF-8 bytes
	EndLine  int    // 1-indexed
	EndCol   int    // 1-indexed, UTF-8 bytes
	Severity string // "error" or "warning"
	Message  string
}

// Check parses and type-checks the given CEL content, returning any
// diagnostics. A check-phase diagnostic is an error when opts declare the
// environment and a warning when they do not; see Options.declared.
func Check(content string, opts Options) ([]CheckDiagnostic, error) {
	docEnv, err := newEnvironment(opts)
	if err != nil {
		return nil, err
	}
	diags := computeDiagnostics(&file{uri: cliURI, content: content}, docEnv.celEnv, docEnv.checkSeverity)
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
func Hover(content string, line, col int, opts Options) (string, error) {
	docEnv, err := newEnvironment(opts)
	if err != nil {
		return "", err
	}
	f := &file{uri: cliURI, content: content}
	result, err := computeHover(f, docEnv, protocol.Position{
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
func References(content string, line, col int, opts Options) ([]Reference, error) {
	docEnv, err := newEnvironment(opts)
	if err != nil {
		return nil, err
	}
	f := &file{uri: cliURI, content: content}
	locs, err := computeReferences(f, docEnv.celEnv, protocol.ReferenceParams{
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

// Definition is where a name was declared: a location in a named file
// (1-indexed, UTF-8 byte columns).
type Definition struct {
	Path    string
	Line    int
	Col     int
	EndLine int
	EndCol  int
}

// FindDefinition returns where the name at the given 1-indexed line:col was
// declared: its entry in the configuration opts name, or, for a name a macro
// binds, where the macro binds it in path itself. It reports false when there
// is no such name, or nothing that declared it.
//
// Unlike the other entry points this one takes the file's path, because its
// result names a file.
func FindDefinition(path, content string, line, col int, opts Options) (Definition, bool, error) {
	docEnv, err := newEnvironment(opts)
	if err != nil {
		return Definition{}, false, err
	}
	f := &file{uri: lspuri.File(path), content: content}
	location, found := computeDefinition(f, docEnv.celEnv, docEnv.configPath, protocol.Position{
		Line:      uint32(line - 1),
		Character: uint32(col - 1),
	})
	if !found {
		return Definition{}, false, nil
	}
	return Definition{
		Path:    location.URI.FsPath(),
		Line:    int(location.Range.Start.Line) + 1,
		Col:     int(location.Range.Start.Character) + 1,
		EndLine: int(location.Range.End.Line) + 1,
		EndCol:  int(location.Range.End.Character) + 1,
	}, true, nil
}

// Rename renames the identifier at the given 1-indexed line:col to newName.
// Returns the updated content, or the original content if nothing was renamed.
func Rename(content string, line, col int, newName string, opts Options) (string, error) {
	docEnv, err := newEnvironment(opts)
	if err != nil {
		return "", err
	}
	f := &file{uri: cliURI, content: content}
	edit, err := computeRename(f, docEnv.celEnv, protocol.RenameParams{
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
