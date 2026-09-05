package lsp

import (
	"context"
	"fmt"
	"strings"
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
	comprehensionID int64  // ID of the comprehension expression
	macroName       string // "map", "filter", "all", "exists", "exists_one"
}

func (loopVarScope) isScope() {}

func (s *server) Rename(_ context.Context, params *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	f, docEnv := s.document(params.TextDocument.URI)

	if f == nil || docEnv == nil || f.content == "" {
		return nil, nil
	}

	return computeRename(f, docEnv.celEnv, *params)
}

func (s *server) PrepareRename(_ context.Context, params *protocol.PrepareRenameParams) (protocol.PrepareRenameResult, error) {
	f, docEnv := s.document(params.TextDocument.URI)

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
	return &protocol.WorkspaceEdit{
		Changes: map[lspuri.URI][]protocol.TextEdit{
			params.TextDocument.URI: edits,
		},
	}, nil
}

func computePrepareRename(f *file, celEnv *cel.Env, pos protocol.Position) (protocol.PrepareRenameResult, error) {
	nativeAST, info := f.identifierAt(celEnv, pos)
	// Functions are declared by the CEL environment, not by this file, so
	// there is nothing here to rename.
	if info == nil || info.kind == identifierKindFunction {
		return nil, nil
	}

	r, ok := rangeOfExpr(f.content, nativeAST.SourceInfo(), info.exprID)
	if !ok {
		return nil, nil
	}
	return &r, nil
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

		case ast.CallKind:
			call := e.AsCall()
			funcName := call.FunctionName()

			// A macro still in call form declares its loop variable as the
			// first argument, e.g. the "x" in list.map(x, x * 2).
			if !isCELMacroFunction(funcName) || len(call.Args()) == 0 {
				return
			}
			decl := call.Args()[0]
			if decl.Kind() != ast.IdentKind {
				return
			}
			loopVar := decl.AsIdent()
			if start, end, found := findWord(fileContent, loopVar, byteStart); found && end <= byteStop {
				if targetOffset >= start && targetOffset < end {
					// Record the call's ID rather than the argument's: the
					// scope of a loop variable is the macro it belongs to.
					add(loopVar, e.ID(), identifierKindTopLevel)
				}
			}

		case ast.ComprehensionKind:
			// An expanded macro keeps the name it binds but not where it was
			// written. The call the macro was written as holds the
			// declaration, with its own range; failing that, the name has to
			// be found in the source.
			if name, declID, ok := macroDeclaration(sourceInfo, e.ID()); ok {
				start, end, hasRange := exprByteRange(fileContent, sourceInfo, declID)
				if hasRange && targetOffset >= start && targetOffset < end {
					add(name, e.ID(), identifierKindTopLevel)
				}
				return
			}
			loopVar := e.AsComprehension().IterVar()
			if start, end, found := findWord(fileContent, loopVar, byteStart); found {
				if targetOffset >= start && targetOffset < end {
					add(loopVar, e.ID(), identifierKindTopLevel)
				}
			}
		}
	}))

	if len(candidates) == 0 {
		return nil
	}
	best := candidates[len(candidates)-1]

	// An identifier that is never selected from (no "name." anywhere) may be a
	// loop variable whose comprehension the walk above didn't attribute it to.
	// Re-point it at that comprehension so its scope comes out right.
	if best.kind == identifierKindTopLevel && !strings.Contains(fileContent, "."+best.name) {
		ast.PreOrderVisit(expr, ast.NewExprVisitor(func(e ast.Expr) {
			if !comprehensionBinds(sourceInfo, e, best.name) {
				return
			}
			offsetRange, hasOffset := sourceInfo.GetOffsetRange(e.ID())
			if !hasOffset {
				return
			}
			byteStart, byteStop := celOffsetRangeToByteRange(fileContent, offsetRange)
			// An expanded macro can report an empty range; treat the offset as
			// inside it as long as it comes after where the macro starts.
			if byteStart == byteStop {
				if targetOffset > byteStart {
					best.exprID = e.ID()
				}
				return
			}
			if targetOffset >= byteStart && targetOffset < byteStop {
				best.exprID = e.ID()
			}
		}))
	}

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

// determineIdentifierScope figures out whether an identifier is top-level or a loop variable.
func determineIdentifierScope(exprID int64, identName string, expr ast.Expr, sourceInfo *ast.SourceInfo) scope {
	if s := findLoopVarScope(exprID, identName, expr, sourceInfo); s != nil {
		return *s
	}
	return topLevelScope{}
}

// findLoopVarScope reports the comprehension that binds identName as its loop
// variable, when the identifier at exprID is that variable or one of its uses.
// It returns nil when identName is not a loop variable.
func findLoopVarScope(exprID int64, identName string, expr ast.Expr, sourceInfo *ast.SourceInfo) *loopVarScope {
	var result *loopVarScope
	ast.PreOrderVisit(expr, ast.NewExprVisitor(func(e ast.Expr) {
		if result != nil {
			return
		}
		switch e.Kind() {
		case ast.ComprehensionKind:
			// An expanded macro holds the name it binds as a name, not an
			// expression, so the identifier was attributed to the
			// comprehension itself.
			if exprID == e.ID() && comprehensionBinds(sourceInfo, e, identName) {
				result = &loopVarScope{comprehensionID: e.ID(), macroName: macroName(sourceInfo, e.ID())}
			}

		case ast.CallKind:
			call := e.AsCall()
			funcName := call.FunctionName()
			if !isCELComprehensionMacro(funcName) || len(call.Args()) < 2 {
				return
			}
			decl := call.Args()[0]
			if decl.Kind() != ast.IdentKind || decl.AsIdent() != identName {
				return
			}
			if decl.ID() == exprID || isUsedInComprehension(decl, call) {
				result = &loopVarScope{comprehensionID: e.ID(), macroName: funcName}
			}
		}
	}))
	return result
}

// isUsedInComprehension checks if the loop variable appears within the comprehension call.
func isUsedInComprehension(loopVarExpr ast.Expr, call ast.CallExpr) bool {
	loopVarName := loopVarExpr.AsIdent()

	// Check arguments 1+ (the actual expressions using the loop var)
	for i := 1; i < len(call.Args()); i++ {
		if containsIdentifier(call.Args()[i], loopVarName, loopVarExpr.ID()) {
			return true
		}
	}

	return false
}

// containsIdentifier reports whether the subtree rooted at expr holds the
// identifier identName under the expression ID targetID.
func containsIdentifier(expr ast.Expr, identName string, targetID int64) bool {
	e := findExprByID(expr, targetID)
	return e != nil && e.Kind() == ast.IdentKind && e.AsIdent() == identName
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
