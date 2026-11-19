package lsp

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"go.lsp.dev/protocol"
)

// unusedVariableDiagnostics returns a hint for each name a macro binds that is
// never used, such as the x in [1, 2].all(x, true) or the v in
// cel.bind(v, 1, 2). The hints are tagged unnecessary, which editors show by
// fading the name rather than underlining it.
//
// Some names are taken to be unused on purpose: one that starts with an
// underscore, and the first of the two a comprehension binds, as in
// m.all(k, v, v > 0), since binding the second is the only way to reach a
// map's values or a list's elements alongside their indices.
func unusedVariableDiagnostics(f *file, celEnv *cel.Env) []protocol.Diagnostic {
	nativeAST := f.ast(celEnv)
	if nativeAST == nil {
		return nil
	}
	sourceInfo := nativeAST.SourceInfo()
	var diagnostics []protocol.Diagnostic
	seen := make(map[int64]bool)
	celast.PostOrderVisit(nativeAST.Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		decls := macroDeclarations(sourceInfo, e)
		if len(decls) == 2 {
			decls = decls[1:]
		}
		for _, decl := range decls {
			name := decl.AsIdent()
			if seen[decl.ID()] || strings.HasPrefix(name, "_") {
				continue
			}
			seen[decl.ID()] = true
			r, ok := rangeOfExpr(f.content, sourceInfo, decl.ID())
			if !ok {
				continue
			}
			// The declaration is itself one of the occurrences.
			if _, occurrences := f.identifierOccurrencesAt(celEnv, r.Start); len(occurrences) != 1 {
				continue
			}
			diagnostics = append(diagnostics, protocol.Diagnostic{
				Range:    r,
				Severity: protocol.DiagnosticSeverityHint,
				Source:   protocol.NewOptional(serverName),
				Message:  protocol.String(fmt.Sprintf("'%s' is never used", name)),
				Tags:     protocol.NewDiagnosticTags(protocol.DiagnosticTagUnnecessary),
			})
		}
	}))
	slices.SortFunc(diagnostics, func(a, b protocol.Diagnostic) int {
		return cmp.Or(
			cmp.Compare(a.Range.Start.Line, b.Range.Start.Line),
			cmp.Compare(a.Range.Start.Character, b.Range.Start.Character),
		)
	})
	return diagnostics
}
