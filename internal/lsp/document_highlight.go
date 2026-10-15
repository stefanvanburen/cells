package lsp

import (
	"context"

	"cel.dev/cel-go/cel"
	"go.lsp.dev/protocol"
)

func (s *server) DocumentHighlight(ctx context.Context, params *protocol.DocumentHighlightParams) ([]protocol.DocumentHighlight, error) {
	f, docEnv := s.document(ctx, params.TextDocument.URI)

	if f == nil || docEnv == nil || f.content == "" {
		return nil, nil
	}

	return computeDocumentHighlight(f, docEnv.celEnv, *params)
}

// computeDocumentHighlight highlights every occurrence of the name at the
// position. A name a macro binds is written where the macro binds it and read
// everywhere else. Any other name is declared outside the expression, so every
// occurrence of it is a read.
func computeDocumentHighlight(f *file, celEnv *cel.Env, params protocol.DocumentHighlightParams) ([]protocol.DocumentHighlight, error) {
	nativeAST, info := f.identifierAt(celEnv, params.Position)
	if info == nil {
		return nil, nil
	}
	sourceInfo := nativeAST.SourceInfo()
	s := determineIdentifierScope(info.exprID, info.name, nativeAST.Expr(), sourceInfo)
	ranges := identifierOccurrences(nativeAST.Expr(), sourceInfo, f.content, s, info.name)
	if len(ranges) == 0 {
		return nil, nil
	}

	_, bound := s.(loopVarScope)
	declaration := firstRange(ranges)
	highlights := make([]protocol.DocumentHighlight, 0, len(ranges))
	for _, r := range ranges {
		kind := protocol.DocumentHighlightKindRead
		if bound && r == declaration {
			kind = protocol.DocumentHighlightKindWrite
		}
		highlights = append(highlights, protocol.DocumentHighlight{Range: r, Kind: kind})
	}
	return highlights, nil
}
