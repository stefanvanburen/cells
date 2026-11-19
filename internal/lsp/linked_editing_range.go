package lsp

import (
	"context"

	"cel.dev/cel-go/cel"
	"go.lsp.dev/protocol"
)

// identifierPattern is the pattern a CEL identifier matches.
const identifierPattern = `[_a-zA-Z][_a-zA-Z0-9]*`

// LinkedEditingRange returns every occurrence of the name at the position when
// a macro in the file binds it, so that editing one edits them all: the x in
// [1, 2].map(x, x * 2), or the v in cel.bind(v, 1, v).
//
// A name declared anywhere else is left alone. Its declaration is not in the
// file, so editing every use of it would only leave them all undeclared.
func (s *server) LinkedEditingRange(ctx context.Context, params *protocol.LinkedEditingRangeParams) (*protocol.LinkedEditingRanges, error) {
	f, docEnv := s.document(ctx, params.TextDocument.URI)
	if f == nil || docEnv == nil || f.content == "" {
		return nil, nil
	}
	return computeLinkedEditingRanges(f, docEnv.celEnv, params.Position), nil
}

func computeLinkedEditingRanges(f *file, celEnv *cel.Env, pos protocol.Position) *protocol.LinkedEditingRanges {
	nativeAST, info := f.identifierAt(celEnv, pos)
	if info == nil {
		return nil
	}
	ranges := macroBoundOccurrences(f, nativeAST, info)
	if len(ranges) == 0 {
		return nil
	}
	return &protocol.LinkedEditingRanges{Ranges: ranges, WordPattern: new(identifierPattern)}
}
