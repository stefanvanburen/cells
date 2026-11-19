package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.yaml.in/yaml/v3"
)

// CodeAction offers quick fixes for the undeclared references in the requested
// range: the names cel-go suggests in their place, and enabling the extension
// that declares them in the configuration that governs the document.
//
// The diagnostics are computed afresh rather than taken from the request: the
// ones a client sends back may predate its latest edit, and a fix applied at a
// stale range would corrupt the document.
func (s *server) CodeAction(ctx context.Context, params *protocol.CodeActionParams) ([]protocol.CommandOrCodeAction, error) {
	if only := params.Context.Only; len(only) > 0 && !slices.Contains(only, protocol.CodeActionKindQuickFix) {
		return nil, nil
	}
	f, docEnv := s.document(ctx, params.TextDocument.URI)
	if f == nil {
		return nil, nil
	}
	configPath := ""
	if docEnv != nil {
		configPath = docEnv.configPath
	}
	return computeCodeActions(params.TextDocument.URI, configPath, s.diagnosticsFor(f), params.Range), nil
}

func computeCodeActions(docURI uri.URI, configPath string, diagnostics []protocol.Diagnostic, r protocol.Range) []protocol.CommandOrCodeAction {
	var actions []protocol.CommandOrCodeAction
	for _, d := range diagnostics {
		var data diagnosticData
		if len(d.Data) == 0 || !rangesOverlap(d.Range, r) || json.Unmarshal(d.Data, &data) != nil {
			continue
		}
		if len(data.Extensions) > 0 {
			actions = append(actions, enableExtensionActions(docURI, configPath, d, data)...)
			continue
		}
		for _, name := range data.Suggestions {
			actions = append(actions, quickFix(
				fmt.Sprintf("Change to '%s'", name),
				d,
				// Of two suggestions, neither is clearly the one meant.
				len(data.Suggestions) == 1,
				map[uri.URI][]protocol.TextEdit{docURI: {{Range: d.Range, NewText: name}}},
			))
		}
	}
	return actions
}

// enableExtensionActions offers to enable each extension that declares the
// name d reports, renaming the reference to the name cel-go suggests when
// there is one. Without a configuration there is nowhere to enable one.
func enableExtensionActions(docURI uri.URI, configPath string, d protocol.Diagnostic, data diagnosticData) []protocol.CommandOrCodeAction {
	if configPath == "" {
		return nil
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var actions []protocol.CommandOrCodeAction
	for _, ext := range data.Extensions {
		edit, ok := enableExtensionEdit(string(content), ext)
		if !ok {
			continue
		}
		changes := map[uri.URI][]protocol.TextEdit{uri.File(configPath): {edit}}
		title := fmt.Sprintf("Enable the `%s` extension", ext)
		if len(data.Suggestions) > 0 {
			changes[docURI] = []protocol.TextEdit{{Range: d.Range, NewText: data.Suggestions[0]}}
			title = fmt.Sprintf("Change to '%s' and enable the `%s` extension", data.Suggestions[0], ext)
		}
		actions = append(actions, quickFix(title, d, len(data.Extensions) == 1, changes))
	}
	return actions
}

// enableExtensionEdit returns the edit that adds ext to the extensions a
// configuration, content, enables. It reports false when the configuration
// already names ext, so enabling it again would change nothing, or lists its
// extensions in a form cells does not edit: flow style, or an empty value.
//
// The entry goes before the first one listed, with the same indentation, or
// in a new extensions list at the end of the file when there is none.
func enableExtensionEdit(content, ext string) (protocol.TextEdit, bool) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(content), &document); err != nil {
		return protocol.TextEdit{}, false
	}
	root := documentRoot(&document)
	if root.Kind != 0 && root.Kind != yaml.MappingNode {
		return protocol.TextEdit{}, false
	}
	entries := mappingValue(root, "extensions")
	if entries == nil {
		end := len(content)
		text := "extensions:\n  - name: " + ext + "\n"
		if end > 0 && !strings.HasSuffix(content, "\n") {
			text = "\n" + text
		}
		pos := lspRange(content, end, end)
		return protocol.TextEdit{Range: pos, NewText: text}, true
	}
	if entries.Kind != yaml.SequenceNode || entries.Style&yaml.FlowStyle != 0 || len(entries.Content) == 0 {
		return protocol.TextEdit{}, false
	}
	for _, entry := range entries.Content {
		if name := mappingValue(entry, "name"); name != nil && name.Value == ext {
			return protocol.TextEdit{}, false
		}
	}

	// The first entry's line up to its first key is the indentation and dash
	// every entry starts with.
	first := entries.Content[0]
	lineStart := runeLineColToByteOffset(content, first.Line-1, 0)
	keyStart := runeLineColToByteOffset(content, first.Line-1, first.Column-1)
	if lineStart < 0 || keyStart < lineStart {
		return protocol.TextEdit{}, false
	}
	prefix := content[lineStart:keyStart]
	if strings.TrimSpace(prefix) != "-" {
		return protocol.TextEdit{}, false
	}
	return protocol.TextEdit{
		Range:   lspRange(content, lineStart, lineStart),
		NewText: prefix + "name: " + ext + "\n",
	}, true
}

// quickFix returns a quick fix for d that applies changes.
func quickFix(title string, d protocol.Diagnostic, preferred bool, changes map[uri.URI][]protocol.TextEdit) *protocol.CodeAction {
	return &protocol.CodeAction{
		Title:       title,
		Kind:        new(protocol.CodeActionKindQuickFix),
		Diagnostics: []protocol.Diagnostic{d},
		IsPreferred: new(preferred),
		Edit:        &protocol.WorkspaceEdit{Changes: changes},
	}
}

// rangesOverlap reports whether a and b share a position, counting their
// ends: a cursor just after a name still touches it.
func rangesOverlap(a, b protocol.Range) bool {
	return !positionBefore(a.End, b.Start) && !positionBefore(b.End, a.Start)
}

func positionBefore(p, q protocol.Position) bool {
	return p.Line < q.Line || (p.Line == q.Line && p.Character < q.Character)
}
