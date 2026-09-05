package lsp

import (
	"context"

	"cel.dev/cel-go/cel"
	"go.lsp.dev/protocol"
)

func (s *server) References(_ context.Context, params *protocol.ReferenceParams) ([]protocol.Location, error) {
	f, docEnv := s.document(params.TextDocument.URI)

	if f == nil || docEnv == nil || f.content == "" {
		return nil, nil
	}

	return computeReferences(f, docEnv.celEnv, *params)
}

func computeReferences(f *file, celEnv *cel.Env, params protocol.ReferenceParams) ([]protocol.Location, error) {
	info, ranges := f.identifierOccurrencesAt(celEnv, params.Position)
	// A function has no references to report; only its declaration, which
	// lives in the CEL environment rather than in this file.
	if info == nil || info.kind == identifierKindFunction {
		return nil, nil
	}

	locations := make([]protocol.Location, 0, len(ranges))
	for _, r := range ranges {
		locations = append(locations, protocol.Location{URI: params.TextDocument.URI, Range: r})
	}
	return locations, nil
}
