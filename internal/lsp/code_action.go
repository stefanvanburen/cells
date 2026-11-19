package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// CodeAction offers a quick fix for each name cel-go suggests in place of an
// undeclared reference in the requested range.
//
// The diagnostics are computed afresh rather than taken from the request: the
// ones a client sends back may predate its latest edit, and a fix applied at a
// stale range would corrupt the document.
func (s *server) CodeAction(ctx context.Context, params *protocol.CodeActionParams) ([]protocol.CommandOrCodeAction, error) {
	if only := params.Context.Only; len(only) > 0 && !slices.Contains(only, protocol.CodeActionKindQuickFix) {
		return nil, nil
	}
	f, _ := s.document(ctx, params.TextDocument.URI)
	if f == nil {
		return nil, nil
	}
	return computeCodeActions(params.TextDocument.URI, s.diagnosticsFor(f), params.Range), nil
}

func computeCodeActions(docURI uri.URI, diagnostics []protocol.Diagnostic, r protocol.Range) []protocol.CommandOrCodeAction {
	var actions []protocol.CommandOrCodeAction
	for _, d := range diagnostics {
		var data diagnosticData
		if len(d.Data) == 0 || !rangesOverlap(d.Range, r) || json.Unmarshal(d.Data, &data) != nil {
			continue
		}
		for _, name := range data.Suggestions {
			actions = append(actions, &protocol.CodeAction{
				Title:       fmt.Sprintf("Change to '%s'", name),
				Kind:        new(protocol.CodeActionKindQuickFix),
				Diagnostics: []protocol.Diagnostic{d},
				// Of two suggestions, neither is clearly the one meant.
				IsPreferred: new(len(data.Suggestions) == 1),
				Edit: &protocol.WorkspaceEdit{
					Changes: map[uri.URI][]protocol.TextEdit{
						docURI: {{Range: d.Range, NewText: name}},
					},
				},
			})
		}
	}
	return actions
}

// rangesOverlap reports whether a and b share a position, counting their
// ends: a cursor just after a name still touches it.
func rangesOverlap(a, b protocol.Range) bool {
	return !positionBefore(a.End, b.Start) && !positionBefore(b.End, a.Start)
}

func positionBefore(p, q protocol.Position) bool {
	return p.Line < q.Line || (p.Line == q.Line && p.Character < q.Character)
}
