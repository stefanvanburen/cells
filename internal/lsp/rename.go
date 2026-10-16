package lsp

import (
	"context"
	"fmt"
	"maps"
	"unicode"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"go.lsp.dev/protocol"
	lspuri "go.lsp.dev/uri"
)

// identifierKind represents the type of identifier being renamed.
type identifierKind int

const (
	identifierKindUnknown  identifierKind = iota
	identifierKindTopLevel                // e.g., "x" in "x + y"
	identifierKindLoopVar                 // e.g., "x" in ".map(x, x * 2)"
	identifierKindFunction                // e.g., "size" in "size('hello')"
)

// scope represents the lexical scope in which an identifier appears.
type scope interface {
	isScope()
}

type topLevelScope struct{}

func (topLevelScope) isScope() {}

// loopVarScope represents a variable scoped to a comprehension.
type loopVarScope struct {
	comprehensionID int64 // ID of the comprehension expression
}

func (loopVarScope) isScope() {}

func (s *server) Rename(ctx context.Context, params *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	f, docEnv := s.document(ctx, params.TextDocument.URI)

	if f == nil || docEnv == nil || f.content == "" {
		return nil, nil
	}

	return computeRename(f, docEnv.celEnv, *params)
}

func (s *server) PrepareRename(ctx context.Context, params *protocol.PrepareRenameParams) (protocol.PrepareRenameResult, error) {
	f, docEnv := s.document(ctx, params.TextDocument.URI)

	if f == nil || docEnv == nil || f.content == "" {
		return nil, nil
	}

	return computePrepareRename(f, docEnv.celEnv, params.Position)
}

func computeRename(f *file, celEnv *cel.Env, params protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	if err := validateNewName(params.NewName); err != nil {
		return nil, err
	}

	info, ranges := f.identifierOccurrencesAt(celEnv, params.Position)
	if info == nil || len(ranges) == 0 {
		return nil, nil
	}

	edits := make([]protocol.TextEdit, 0, len(ranges))
	for _, r := range ranges {
		edits = append(edits, protocol.TextEdit{Range: r, NewText: params.NewName})
	}
	if changesReferents(f, celEnv, edits) {
		return nil, fmt.Errorf("renaming %q to %q would change what a name refers to", info.name, params.NewName)
	}
	return &protocol.WorkspaceEdit{
		Changes: map[lspuri.URI][]protocol.TextEdit{
			params.TextDocument.URI: edits,
		},
	}, nil
}

// changesReferents reports whether applying edits, a rename, to f would change
// what any name in it refers to: a name the renamed one would now be bound by,
// or one it would now bind. Renaming leaves the expression's shape and so its
// IDs as they were, so every identifier must resolve to the same binding after
// as before.
func changesReferents(f *file, celEnv *cel.Env, edits []protocol.TextEdit) bool {
	before := f.ast(celEnv)
	renamed, iss := celEnv.Parse(applyTextEdits(f.content, edits))
	if before == nil || iss.Err() != nil {
		return true
	}
	return !maps.Equal(nameBindings(before.Expr()), nameBindings(renamed.NativeRep().Expr()))
}

func computePrepareRename(f *file, celEnv *cel.Env, pos protocol.Position) (protocol.PrepareRenameResult, error) {
	info, ranges := f.identifierOccurrencesAt(celEnv, pos)
	// Functions are declared by the CEL environment, not by this file, so
	// there is nothing here to rename.
	if info == nil || info.kind == identifierKindFunction {
		return nil, nil
	}

	// The occurrence under the cursor, out of those a rename would edit. A
	// loop variable's info names its macro, which spans no source of its own.
	for _, r := range ranges {
		if r.Start.Line == pos.Line && r.Start.Character <= pos.Character && pos.Character <= r.End.Character {
			return &r, nil
		}
	}
	return nil, nil
}

type identifierInfo struct {
	name   string
	exprID int64
	kind   identifierKind
}

// findIdentifierAtPosition finds the identifier-like element whose source
// range contains targetOffset: a plain identifier, the name of a function
// being called, or a macro's loop variable.
func findIdentifierAtPosition(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, targetOffset int) *identifierInfo {
	// Candidates accumulate outermost-first, because a pre-order walk reaches
	// a node before its children and only nodes containing targetOffset are
	// recorded. The last one is therefore the innermost.
	var candidates []*identifierInfo
	add := func(name string, exprID int64, kind identifierKind) {
		candidates = append(candidates, &identifierInfo{name: name, exprID: exprID, kind: kind})
	}

	ast.PreOrderVisit(expr, ast.NewExprVisitor(func(e ast.Expr) {
		offsetRange, hasOffset := sourceInfo.GetOffsetRange(e.ID())
		if !hasOffset {
			return
		}
		byteStart, byteStop := celOffsetRangeToByteRange(fileContent, offsetRange)

		// The parser records a call's location as its opening parenthesis, so
		// the name being called lies before the range rather than inside it
		// and has to be tested separately. Operators are spelled by their
		// symbol, which the range does cover.
		if e.Kind() == ast.CallKind {
			funcName := e.AsCall().FunctionName()
			if _, isOperator := celOperatorSymbol(funcName); !isOperator {
				if start, end, found := callNameRange(fileContent, funcName, byteStart); found &&
					targetOffset >= start && targetOffset < end {
					add(funcName, e.ID(), identifierKindFunction)
				}
			}
		}

		// A macro that the parser expanded into a comprehension can be left
		// with an empty range. Its loop variable is still findable in the
		// source, so don't rule the node out on the range alone.
		emptyComprehension := e.Kind() == ast.ComprehensionKind && byteStart == byteStop
		if !emptyComprehension && (targetOffset < byteStart || targetOffset >= byteStop) {
			return
		}

		switch e.Kind() {
		case ast.IdentKind:
			// The kind is refined by determineIdentifierScope, which is what
			// decides whether this is a loop variable.
			add(e.AsIdent(), e.ID(), identifierKindTopLevel)

		case ast.ComprehensionKind:
			// An expanded macro keeps the names it binds but not where they
			// were written. The call the macro was written as holds the
			// declarations, with ranges of their own.
			for _, decl := range macroDeclarations(sourceInfo, e) {
				start, end, hasRange := exprByteRange(fileContent, sourceInfo, decl.ID())
				if hasRange && targetOffset >= start && targetOffset < end {
					add(decl.AsIdent(), e.ID(), identifierKindTopLevel)
				}
			}
		}
	}))

	if len(candidates) == 0 {
		return nil
	}
	best := candidates[len(candidates)-1]

	return best
}

// callNameRange returns the byte range of the name of a called function, which
// the parser does not record: it sits before the parenthesis that a call's
// location points at, with whitespace allowed between the two.
func callNameRange(content string, funcName string, parenOffset int) (start, end int, found bool) {
	end = min(parenOffset, len(content))
	for end > 0 && isSpaceByte(content[end-1]) {
		end--
	}
	start = end - len(funcName)
	if start < 0 || content[start:end] != funcName {
		return 0, 0, false
	}
	// The name must not be the tail of a longer one.
	if start > 0 && isIdentifierChar(rune(content[start-1])) {
		return 0, 0, false
	}
	return start, end, true
}

// isSpaceByte reports whether b is one of the whitespace bytes CEL allows
// between tokens. Every one of them is ASCII, so a byte is enough.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// determineIdentifierScope reports what the identifier at exprID refers to: a
// name the comprehension at exprID declares, a name a comprehension binds, or
// a name the environment declares.
func determineIdentifierScope(exprID int64, expr ast.Expr) scope {
	if e := findExprByID(expr, exprID); e != nil && e.Kind() == ast.ComprehensionKind {
		return loopVarScope{comprehensionID: exprID}
	}
	if id, bound := nameBindings(expr)[exprID]; bound {
		return loopVarScope{comprehensionID: id}
	}
	return topLevelScope{}
}

// validateNewName checks if the new name is a valid CEL identifier.
func validateNewName(newName string) error {
	if newName == "" {
		return fmt.Errorf("new name cannot be empty")
	}

	// Check valid identifier syntax: starts with letter or underscore, contains only alphanumeric + underscore
	if !isValidIdentifier(newName) {
		return fmt.Errorf("invalid identifier: %q", newName)
	}

	// Check not a CEL keyword
	if isCELKeyword(newName) {
		return fmt.Errorf("cannot rename to CEL keyword: %q", newName)
	}

	return nil
}

// isValidIdentifier checks if a string is a valid CEL identifier.
func isValidIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}

	// Must start with letter or underscore
	if !unicode.IsLetter(rune(s[0])) && s[0] != '_' {
		return false
	}

	// Rest must be alphanumeric or underscore
	for _, ch := range s[1:] {
		if !unicode.IsLetter(ch) && !unicode.IsDigit(ch) && ch != '_' {
			return false
		}
	}

	return true
}

// isIdentifierChar checks if a rune can be part of an identifier.
func isIdentifierChar(ch rune) bool {
	return unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_'
}

// findIdentifierByName finds the identifier named identName whose source
// range contains targetOffset.
func findIdentifierByName(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, targetOffset int) *identifierInfo {
	var result *identifierInfo
	ast.PreOrderVisit(expr, ast.NewExprVisitor(func(e ast.Expr) {
		if result != nil || e.Kind() != ast.IdentKind || e.AsIdent() != identName {
			return
		}
		offsetRange, hasOffset := sourceInfo.GetOffsetRange(e.ID())
		if !hasOffset {
			return
		}
		byteStart, byteEnd := celOffsetRangeToByteRange(fileContent, offsetRange)
		if targetOffset >= byteStart && targetOffset < byteEnd {
			result = &identifierInfo{name: identName, exprID: e.ID(), kind: identifierKindTopLevel}
		}
	}))
	return result
}
