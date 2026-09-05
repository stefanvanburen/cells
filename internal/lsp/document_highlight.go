package lsp

import (
	"context"

	"cel.dev/cel-go/cel"
	"go.lsp.dev/protocol"
)

func (s *server) DocumentHighlight(_ context.Context, params *protocol.DocumentHighlightParams) ([]protocol.DocumentHighlight, error) {
	f, docEnv := s.document(params.TextDocument.URI)

	if f == nil || f.content == "" {
		return nil, nil
	}

	return computeDocumentHighlight(f, docEnv.celEnv, *params)
}

func computeDocumentHighlight(f *file, celEnv *cel.Env, params protocol.DocumentHighlightParams) ([]protocol.DocumentHighlight, error) {
	info, ranges := f.identifierOccurrencesAt(celEnv, params.Position)
	if info == nil {
		return nil, nil
	}

	highlights := make([]protocol.DocumentHighlight, 0, len(ranges))
	for _, r := range ranges {
		highlights = append(highlights, protocol.DocumentHighlight{Range: r})
	}
	return highlights, nil
}
