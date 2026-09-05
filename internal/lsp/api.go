package lsp

import (
	"slices"

	"cel.dev/cel-go/cel"
	"go.lsp.dev/protocol"
)

func newCELEnv(extraOpts ...cel.EnvOption) (*cel.Env, error) {
	opts := []cel.EnvOption{
		cel.EnableMacroCallTracking(),
		// Validate the arguments to duration(), timestamp(), and matches()
		// when they are literals. cel-go runs these during Check, so bad
		// literals surface as diagnostics rather than as runtime errors the
		// author only finds later.
		//
		// This is deliberately not cel.ExtendedValidations(), which also
		// bundles ValidateHomogeneousAggregateLiterals(). Heterogeneous
		// literals like [1, 'two'] are valid CEL — they type as list(dyn) —
		// so flagging them would be a false positive.
		cel.ASTValidators(
			cel.ValidateDurationLiterals(),
			cel.ValidateTimestampLiterals(),
			cel.ValidateRegexLiterals(),
		),
	}
	return cel.NewEnv(append(opts, extraOpts...)...)
}

// newCELEnvForExtensions builds a CEL environment with the named extension
// libraries enabled (see extensionFactories for valid names).
func newCELEnvForExtensions(names []string) (*cel.Env, error) {
	opts, err := resolveExtensions(names)
	if err != nil {
		return nil, err
	}
	return newCELEnv(opts...)
}

// ExtensionNames returns the names of the CEL extension libraries that can be
// passed to Serve, ServeStream, Format, Check, Hover, References, and Rename,
// sorted alphabetically.
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
// extensions names CEL extension libraries to enable (see extensionFactories for valid names).
func Format(content string, extensions ...string) (string, error) {
	env, err := newCELEnvForExtensions(extensions)
	if err != nil {
		return "", err
	}
	return formatCEL(content, env)
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

// Check parses and type-checks the given CEL content, returning any diagnostics.
func Check(content string, extensions ...string) ([]CheckDiagnostic, error) {
	env, err := newCELEnvForExtensions(extensions)
	if err != nil {
		return nil, err
	}
	diags := computeDiagnostics(&file{uri: cliURI, content: content}, env)
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
func Hover(content string, line, col int, extensions ...string) (string, error) {
	env, err := newCELEnvForExtensions(extensions)
	if err != nil {
		return "", err
	}
	f := &file{uri: cliURI, content: content}
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
func References(content string, line, col int, extensions ...string) ([]Reference, error) {
	env, err := newCELEnvForExtensions(extensions)
	if err != nil {
		return nil, err
	}
	f := &file{uri: cliURI, content: content}
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
func Rename(content string, line, col int, newName string, extensions ...string) (string, error) {
	env, err := newCELEnvForExtensions(extensions)
	if err != nil {
		return "", err
	}
	f := &file{uri: cliURI, content: content}
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
