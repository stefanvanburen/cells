package lsp

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/ast"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
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

func (s *server) rename(req *jsonrpc2.Request) (any, error) {
	var params protocol.RenameParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	if f == nil || f.content == "" {
		return nil, nil
	}

	return computeRename(f, s.celEnv, params)
}

func (s *server) prepareRename(req *jsonrpc2.Request) (any, error) {
	var params struct {
		TextDocument protocol.TextDocumentIdentifier `json:"textDocument"`
		Position     protocol.Position               `json:"position"`
	}
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	if f == nil || f.content == "" {
		return nil, nil
	}

	return computePrepareRename(f, s.celEnv, params.Position)
}

func computeRename(f *file, celEnv *cel.Env, params protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	// Validate the new name first
	if err := validateNewName(params.NewName); err != nil {
		return nil, err
	}

	parsed, issues := celEnv.Parse(f.content)
	if issues.Err() != nil {
		return nil, nil
	}

	nativeAST := parsed.NativeRep()
	sourceInfo := nativeAST.SourceInfo()

	// Convert LSP position to byte offset
	targetOffset := lineColToByteOffset(f.content, params.Position.Line, params.Position.Character)
	if targetOffset < 0 || targetOffset >= len(f.content) {
		// Debug: position out of range
		return nil, nil
	}

	// Find the identifier at the cursor position
	identInfo := findIdentifierAtPosition(nativeAST.Expr(), sourceInfo, f.content, targetOffset)
	if identInfo == nil {
		// Debug: no identifier found at position
		// For now, try a fallback: look for any identifier that matches the character at the position
		if targetOffset < len(f.content) {
			// Try to find a word boundary
			start := targetOffset
			for start > 0 && isIdentifierChar(rune(f.content[start-1])) {
				start--
			}
			end := targetOffset
			for end < len(f.content) && isIdentifierChar(rune(f.content[end])) {
				end++
			}
			if start < end {
				possibleName := f.content[start:end]
				// Try to find this name in the AST
				identInfo = findIdentifierByName(nativeAST.Expr(), sourceInfo, f.content, possibleName, targetOffset)
			}
		}
		if identInfo == nil {
			return nil, nil
		}
	}

	// Determine the scope of this identifier
	s := determineIdentifierScope(identInfo.exprID, identInfo.name, nativeAST.Expr(), sourceInfo, f.content)

	// Find all occurrences of this identifier within its scope
	textEdits := findAllOccurrences(nativeAST.Expr(), sourceInfo, f.content, s, identInfo.name, params.NewName)

	if len(textEdits) == 0 {
		return nil, nil
	}

	// Build WorkspaceEdit
	uri := params.TextDocument.URI
	return &protocol.WorkspaceEdit{
		Changes: map[protocol.DocumentURI][]protocol.TextEdit{
			uri: textEdits,
		},
	}, nil
}

func computePrepareRename(f *file, celEnv *cel.Env, pos protocol.Position) (any, error) {
	parsed, issues := celEnv.Parse(f.content)
	if issues.Err() != nil {
		return nil, nil
	}

	nativeAST := parsed.NativeRep()
	sourceInfo := nativeAST.SourceInfo()

	// Convert LSP position to byte offset
	targetOffset := lineColToByteOffset(f.content, pos.Line, pos.Character)
	if targetOffset < 0 || targetOffset >= len(f.content) {
		return nil, nil
	}

	// Find the identifier at the cursor position
	identInfo := findIdentifierAtPosition(nativeAST.Expr(), sourceInfo, f.content, targetOffset)
	if identInfo == nil {
		return nil, nil
	}

	// Can't rename function calls
	if identInfo.kind == identifierKindFunction {
		return nil, nil
	}

	// Return the range of the identifier
	offsetRange, hasOffset := sourceInfo.GetOffsetRange(identInfo.exprID)
	if !hasOffset {
		return nil, nil
	}

	byteStart, byteEnd := celOffsetRangeToByteRange(f.content, offsetRange)
	startLine, startCol := byteOffsetToLineCol(f.content, byteStart)
	endLine, endCol := byteOffsetToLineCol(f.content, byteEnd)

	return &protocol.Range{
		Start: protocol.Position{Line: startLine, Character: startCol},
		End:   protocol.Position{Line: endLine, Character: endCol},
	}, nil
}

type identifierInfo struct {
	name   string
	exprID int64
	kind   identifierKind
}

// findIdentifierAtPosition walks the AST to find an identifier at the given byte offset.
func findIdentifierAtPosition(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, targetOffset int) *identifierInfo {
	var candidates []*identifierInfo

	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		if e == nil {
			return
		}

		offsetRange, hasOffset := sourceInfo.GetOffsetRange(e.ID())
		var byteStart, byteStop int
		var isInRange bool

		if hasOffset {
			// Convert rune offsets to byte offsets for comparison
			byteStart, byteStop = celOffsetRangeToByteRange(fileContent, offsetRange)

			// Check if targetOffset falls within this expression's range
			// For comprehensions with empty offset ranges, we need to check differently
			isInRange = targetOffset >= byteStart && targetOffset < byteStop
			if e.Kind() == ast.ComprehensionKind && byteStart == byteStop {
				// Comprehensions expanded from macros may have empty offset ranges
				// In this case, we can't rely on the range check, so we'll skip to special handling
				isInRange = true
			}
		} else {
			// No offset range - for comprehensions, we still want to recurse into children
			if e.Kind() == ast.ComprehensionKind {
				isInRange = true // Process comprehensions even without offset ranges
			} else {
				return // Skip non-comprehension expressions without offset ranges
			}
		}

		if isInRange && hasOffset {
			// For identifiers, add to candidates
			if e.Kind() == ast.IdentKind {
				identName := e.AsIdent()
				candidates = append(candidates, &identifierInfo{
					name:   identName,
					exprID: e.ID(),
					kind:   identifierKindTopLevel, // Will be refined by determineIdentifierScope
				})
			}

			// For calls, check if we're on the function name itself
			if e.Kind() == ast.CallKind {
				call := e.AsCall()
				funcName := call.FunctionName()
				// Try to find the function name in the source
				funcNameStart := strings.Index(fileContent[byteStart:byteStop], funcName)
				if funcNameStart >= 0 {
					funcNameStart += byteStart
					funcNameEnd := funcNameStart + len(funcName)
					if targetOffset >= funcNameStart && targetOffset < funcNameEnd {
						candidates = append(candidates, &identifierInfo{
							name:   funcName,
							exprID: e.ID(),
							kind:   identifierKindFunction,
						})
					}
				}

				// Special handling for comprehension macros (map, filter, all, exists, etc.)
				// The first argument is the loop variable declaration
				if isCELMacroFunction(funcName) && len(call.Args()) > 0 {
					firstArg := call.Args()[0]
					if firstArg.Kind() == ast.IdentKind {
						loopVarName := firstArg.AsIdent()
						// Try to find the loop variable in the source within the call's range
						loopVarIdx := strings.Index(fileContent[byteStart:byteStop], loopVarName)
						if loopVarIdx >= 0 {
							loopVarStart := byteStart + loopVarIdx
							loopVarEnd := loopVarStart + len(loopVarName)
							// Make sure we found the actual loop variable, not some other occurrence
							if targetOffset >= loopVarStart && targetOffset < loopVarEnd {
								// Make sure it's a word boundary (not part of a larger identifier)
								if (loopVarStart == 0 || !isIdentifierChar(rune(fileContent[loopVarStart-1]))) &&
									(loopVarEnd >= len(fileContent) || !isIdentifierChar(rune(fileContent[loopVarEnd]))) {
									candidates = append(candidates, &identifierInfo{
										name:   loopVarName,
										exprID: e.ID(),                 // Use the call's ID, not the first arg's ID
										kind:   identifierKindTopLevel, // Will be refined to loop var by determineIdentifierScope
									})
								}
							}
						}
					}
				}
			}

			// For comprehensions (which are expanded macros like map, filter, all, exists)
			// The loop variable is accessed differently
			if e.Kind() == ast.ComprehensionKind {
				comp := e.AsComprehension()
				// The loop variable name is stored in the comprehension
				loopVarName := comp.IterVar()
				// Try to find the loop variable in the source
				loopVarIdx := strings.Index(fileContent[byteStart:], loopVarName)
				if loopVarIdx >= 0 {
					loopVarStart := byteStart + loopVarIdx
					loopVarEnd := loopVarStart + len(loopVarName)
					// Make sure it's a word boundary
					if (loopVarStart == 0 || !isIdentifierChar(rune(fileContent[loopVarStart-1]))) &&
						(loopVarEnd >= len(fileContent) || !isIdentifierChar(rune(fileContent[loopVarEnd]))) {
						if targetOffset >= loopVarStart && targetOffset < loopVarEnd {
							// Get the ID of the iterator variable
							candidates = append(candidates, &identifierInfo{
								name:   loopVarName,
								exprID: e.ID(),
								kind:   identifierKindTopLevel, // Will be refined to loop var by determineIdentifierScope
							})
						}
					}
				}
			}
		}

		// Recurse into child expressions
		switch e.Kind() {
		case ast.CallKind:
			call := e.AsCall()
			for _, arg := range call.Args() {
				walk(arg)
			}
			if call.IsMemberFunction() {
				walk(call.Target())
			}

		case ast.ListKind:
			for _, elem := range e.AsList().Elements() {
				walk(elem)
			}

		case ast.MapKind:
			for _, entry := range e.AsMap().Entries() {
				mapEntry := entry.AsMapEntry()
				walk(mapEntry.Key())
				walk(mapEntry.Value())
			}

		case ast.StructKind:
			for _, field := range e.AsStruct().Fields() {
				walk(field.AsStructField().Value())
			}

		case ast.SelectKind:
			sel := e.AsSelect()
			if sel.Operand() != nil {
				walk(sel.Operand())
			}

		case ast.ComprehensionKind:
			comp := e.AsComprehension()
			walk(comp.IterRange())
			walk(comp.AccuInit())
			walk(comp.LoopCondition())
			walk(comp.LoopStep())
			walk(comp.Result())
		}
	}

	walk(expr)

	// Return the most specific (innermost) candidate
	if len(candidates) == 0 {
		return nil
	}

	bestCandidate := candidates[len(candidates)-1]

	// If we found an Ident that looks like it might be a loop variable inside a comprehension,
	// check if any comprehension has this as its loop variable, and use that comprehension's ID.
	if bestCandidate.kind == identifierKindTopLevel && !strings.Contains(fileContent, "."+bestCandidate.name) {
		// Try to find if this identifier is a loop variable in any comprehension
		var checkWalk func(ast.Expr)
		checkWalk = func(e ast.Expr) {
			if e == nil {
				return
			}

			if e.Kind() == ast.ComprehensionKind {
				comp := e.AsComprehension()
				if comp.IterVar() == bestCandidate.name {
					// Check if targetOffset is within or after this comprehension
					if offsetRange, hasOffset := sourceInfo.GetOffsetRange(e.ID()); hasOffset {
						byteStart, byteStop := celOffsetRangeToByteRange(fileContent, offsetRange)
						// For empty comprehensions, use a larger range
						if byteStart == byteStop {
							// Try to find the comprehension in the source
							// It should be somewhere around the target offset
							if targetOffset > byteStart && targetOffset < byteStart+1000 { // arbitrary large range
								bestCandidate.exprID = e.ID()
								return
							}
						} else if targetOffset >= byteStart && targetOffset < byteStop {
							bestCandidate.exprID = e.ID()
							return
						}
					}
				}
			}

			// Recurse
			switch e.Kind() {
			case ast.CallKind:
				call := e.AsCall()
				for _, arg := range call.Args() {
					checkWalk(arg)
				}
				if call.IsMemberFunction() {
					checkWalk(call.Target())
				}
			case ast.ListKind:
				for _, elem := range e.AsList().Elements() {
					checkWalk(elem)
				}
			case ast.MapKind:
				for _, entry := range e.AsMap().Entries() {
					mapEntry := entry.AsMapEntry()
					checkWalk(mapEntry.Key())
					checkWalk(mapEntry.Value())
				}
			case ast.StructKind:
				for _, field := range e.AsStruct().Fields() {
					checkWalk(field.AsStructField().Value())
				}
			case ast.SelectKind:
				sel := e.AsSelect()
				if sel.Operand() != nil {
					checkWalk(sel.Operand())
				}
			case ast.ComprehensionKind:
				comp := e.AsComprehension()
				checkWalk(comp.IterRange())
				checkWalk(comp.AccuInit())
				checkWalk(comp.LoopCondition())
				checkWalk(comp.LoopStep())
				checkWalk(comp.Result())
			}
		}
		checkWalk(expr)
	}

	return bestCandidate
}

// determineIdentifierScope figures out whether an identifier is top-level or a loop variable.
func determineIdentifierScope(exprID int64, identName string, expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string) scope {
	// Check if this identifier is a loop variable in a comprehension
	loopVarScope := findLoopVarScope(exprID, identName, expr, sourceInfo, fileContent)
	if loopVarScope != nil {
		return *loopVarScope
	}

	// Otherwise it's top-level
	return topLevelScope{}
}

// findLoopVarScope checks if the identifier at exprID is a loop variable in a comprehension.
func findLoopVarScope(exprID int64, identName string, expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string) *loopVarScope {
	var result *loopVarScope

	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		if e == nil || result != nil {
			return
		}

		// Check if this is a comprehension (expanded macro) and the target identifier is its loop var
		if e.Kind() == ast.ComprehensionKind {
			comp := e.AsComprehension()
			loopVarName := comp.IterVar()
			if loopVarName == identName {
				// For expanded macros, the exprID will be the comprehension's ID
				// since the loop variable doesn't have a separate ID
				if exprID == e.ID() {
					// Infer the macro name from the result expression
					// This is a best-effort approach since we don't have direct access to the macro name
					result = &loopVarScope{
						comprehensionID: e.ID(),
						macroName:       "unknown", // Will be refined if needed
					}
					return
				}
			}
		}

		// Check if this is a comprehension macro CallExpr and the target identifier is its loop var
		if e.Kind() == ast.CallKind {
			call := e.AsCall()
			funcName := call.FunctionName()

			// These are the comprehension macros with loop variables
			isComprehension := funcName == "map" || funcName == "filter" || funcName == "all" ||
				funcName == "exists" || funcName == "exists_one"

			if isComprehension && len(call.Args()) >= 2 {
				// The first argument is the loop variable (as an ident)
				loopVarArg := call.Args()[0]
				if loopVarArg.Kind() == ast.IdentKind && loopVarArg.AsIdent() == identName {
					// Check if exprID refers to this loop variable or its uses
					if loopVarArg.ID() == exprID || isUsedInComprehension(loopVarArg, call) {
						result = &loopVarScope{
							comprehensionID: e.ID(),
							macroName:       funcName,
						}
						return
					}
				}
			}
		}

		// Recurse
		switch e.Kind() {
		case ast.CallKind:
			call := e.AsCall()
			for _, arg := range call.Args() {
				walk(arg)
			}
			if call.IsMemberFunction() {
				walk(call.Target())
			}

		case ast.ListKind:
			for _, elem := range e.AsList().Elements() {
				walk(elem)
			}

		case ast.MapKind:
			for _, entry := range e.AsMap().Entries() {
				mapEntry := entry.AsMapEntry()
				walk(mapEntry.Key())
				walk(mapEntry.Value())
			}

		case ast.StructKind:
			for _, field := range e.AsStruct().Fields() {
				walk(field.AsStructField().Value())
			}

		case ast.SelectKind:
			sel := e.AsSelect()
			if sel.Operand() != nil {
				walk(sel.Operand())
			}

		case ast.ComprehensionKind:
			comp := e.AsComprehension()
			walk(comp.IterRange())
			walk(comp.AccuInit())
			walk(comp.LoopCondition())
			walk(comp.LoopStep())
			walk(comp.Result())
		}
	}

	walk(expr)
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

// containsIdentifier recursively checks if an expression contains the given identifier.
func containsIdentifier(expr ast.Expr, identName string, targetID int64) bool {
	if expr == nil {
		return false
	}

	if expr.Kind() == ast.IdentKind && expr.AsIdent() == identName {
		return expr.ID() == targetID
	}

	switch expr.Kind() {
	case ast.CallKind:
		call := expr.AsCall()
		for _, arg := range call.Args() {
			if containsIdentifier(arg, identName, targetID) {
				return true
			}
		}
		if call.IsMemberFunction() {
			return containsIdentifier(call.Target(), identName, targetID)
		}

	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			if containsIdentifier(elem, identName, targetID) {
				return true
			}
		}

	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			if containsIdentifier(mapEntry.Key(), identName, targetID) ||
				containsIdentifier(mapEntry.Value(), identName, targetID) {
				return true
			}
		}

	case ast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			if containsIdentifier(field.AsStructField().Value(), identName, targetID) {
				return true
			}
		}

	case ast.SelectKind:
		sel := expr.AsSelect()
		if sel.Operand() != nil {
			return containsIdentifier(sel.Operand(), identName, targetID)
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		return containsIdentifier(comp.IterRange(), identName, targetID) ||
			containsIdentifier(comp.AccuInit(), identName, targetID) ||
			containsIdentifier(comp.LoopCondition(), identName, targetID) ||
			containsIdentifier(comp.LoopStep(), identName, targetID) ||
			containsIdentifier(comp.Result(), identName, targetID)
	}

	return false
}

// findAllOccurrences collects all text edits for renaming the identifier within its scope.
func findAllOccurrences(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, s scope, oldName string, newName string) []protocol.TextEdit {
	var edits []protocol.TextEdit

	switch sc := s.(type) {
	case loopVarScope:
		// Find the comprehension and only search within its scope
		// First try CallExpr
		comp := findComprehensionByID(expr, sc.comprehensionID)
		if comp != nil {
			collectIdentifiersInComprehension(comp, sourceInfo, fileContent, oldName, newName, &edits)
		} else {
			// Try ComprehensionKind
			compExpr := findComprehensionExprByID(expr, sc.comprehensionID)
			if compExpr != nil {
				edits = collectIdentifiersInComprehensionExpr(compExpr, sourceInfo, fileContent, oldName, newName)
			}
		}

	case topLevelScope:
		// Search entire expression
		edits = CollectIdentifierOccurrences(expr, sourceInfo, fileContent, oldName, newName)
	}

	return edits
}

// findComprehensionByID finds a comprehension call expression by its ID.
func findComprehensionByID(expr ast.Expr, targetID int64) ast.CallExpr {
	if expr == nil {
		return nil
	}

	if expr.Kind() == ast.CallKind && expr.ID() == targetID {
		return expr.AsCall()
	}

	switch expr.Kind() {
	case ast.CallKind:
		call := expr.AsCall()
		for _, arg := range call.Args() {
			if result := findComprehensionByID(arg, targetID); result != nil {
				return result
			}
		}
		if call.IsMemberFunction() {
			if result := findComprehensionByID(call.Target(), targetID); result != nil {
				return result
			}
		}

	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			if result := findComprehensionByID(elem, targetID); result != nil {
				return result
			}
		}

	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			if result := findComprehensionByID(mapEntry.Key(), targetID); result != nil {
				return result
			}
			if result := findComprehensionByID(mapEntry.Value(), targetID); result != nil {
				return result
			}
		}

	case ast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			if result := findComprehensionByID(field.AsStructField().Value(), targetID); result != nil {
				return result
			}
		}

	case ast.SelectKind:
		sel := expr.AsSelect()
		if sel.Operand() != nil {
			if result := findComprehensionByID(sel.Operand(), targetID); result != nil {
				return result
			}
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		// Check if this comprehension's ID matches the target
		if expr.ID() == targetID {
			// For expanded macros, the comprehension itself is the target.
			// Return nil here, and the caller will use findComprehensionExprByID instead.
			return nil
		}
		if result := findComprehensionByID(comp.IterRange(), targetID); result != nil {
			return result
		}
		if result := findComprehensionByID(comp.AccuInit(), targetID); result != nil {
			return result
		}
		if result := findComprehensionByID(comp.LoopCondition(), targetID); result != nil {
			return result
		}
		if result := findComprehensionByID(comp.LoopStep(), targetID); result != nil {
			return result
		}
		if result := findComprehensionByID(comp.Result(), targetID); result != nil {
			return result
		}
	}

	return nil
}

// findComprehensionExprByID walks the AST to find a ComprehensionKind node with the given ID.
func findComprehensionExprByID(expr ast.Expr, targetID int64) ast.Expr {
	if expr == nil {
		return nil
	}

	if expr.Kind() == ast.ComprehensionKind && expr.ID() == targetID {
		return expr
	}

	switch expr.Kind() {
	case ast.CallKind:
		call := expr.AsCall()
		for _, arg := range call.Args() {
			if result := findComprehensionExprByID(arg, targetID); result != nil {
				return result
			}
		}
		if call.IsMemberFunction() {
			if result := findComprehensionExprByID(call.Target(), targetID); result != nil {
				return result
			}
		}

	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			if result := findComprehensionExprByID(elem, targetID); result != nil {
				return result
			}
		}

	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			if result := findComprehensionExprByID(mapEntry.Key(), targetID); result != nil {
				return result
			}
			if result := findComprehensionExprByID(mapEntry.Value(), targetID); result != nil {
				return result
			}
		}

	case ast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			if result := findComprehensionExprByID(field.AsStructField().Value(), targetID); result != nil {
				return result
			}
		}

	case ast.SelectKind:
		sel := expr.AsSelect()
		if sel.Operand() != nil {
			if result := findComprehensionExprByID(sel.Operand(), targetID); result != nil {
				return result
			}
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		if result := findComprehensionExprByID(comp.IterRange(), targetID); result != nil {
			return result
		}
		if result := findComprehensionExprByID(comp.AccuInit(), targetID); result != nil {
			return result
		}
		if result := findComprehensionExprByID(comp.LoopCondition(), targetID); result != nil {
			return result
		}
		if result := findComprehensionExprByID(comp.LoopStep(), targetID); result != nil {
			return result
		}
		if result := findComprehensionExprByID(comp.Result(), targetID); result != nil {
			return result
		}
	}

	return nil
}

// collectIdentifiersInComprehension collects all occurrences of identName in a comprehension's expressions.
func collectIdentifiersInComprehension(comp ast.CallExpr, sourceInfo *ast.SourceInfo, fileContent string, identName string, newName string, edits *[]protocol.TextEdit) {
	if comp == nil || len(comp.Args()) < 2 {
		return
	}

	// The first argument is the loop variable declaration (e.g., "x" in ".map(x, ...)")
	// Collect it directly
	firstArg := comp.Args()[0]
	if firstArg.Kind() == ast.IdentKind && firstArg.AsIdent() == identName {
		offsetRange, hasOffset := sourceInfo.GetOffsetRange(firstArg.ID())
		if hasOffset {
			byteStart, byteEnd := celOffsetRangeToByteRange(fileContent, offsetRange)
			startLine, startCol := byteOffsetToLineCol(fileContent, byteStart)
			endLine, endCol := byteOffsetToLineCol(fileContent, byteEnd)

			*edits = append(*edits, protocol.TextEdit{
				Range: protocol.Range{
					Start: protocol.Position{Line: startLine, Character: startCol},
					End:   protocol.Position{Line: endLine, Character: endCol},
				},
				NewText: newName,
			})
		}
	}

	// The second argument onward are expressions that use the loop variable
	for i := 1; i < len(comp.Args()); i++ {
		collected := CollectIdentifierOccurrences(comp.Args()[i], sourceInfo, fileContent, identName, newName)
		*edits = append(*edits, collected...)
	}
}

// collectIdentifiersInComprehensionExpr collects all occurrences of identName in a ComprehensionKind.
func collectIdentifiersInComprehensionExpr(compExpr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, newName string) []protocol.TextEdit {
	if compExpr == nil || compExpr.Kind() != ast.ComprehensionKind {
		return nil
	}

	var edits []protocol.TextEdit
	comp := compExpr.AsComprehension()
	loopVarName := comp.IterVar()

	// Add edit for the loop variable declaration itself (if it matches)
	if loopVarName == identName {
		_, byteStop := celOffsetRangeToByteRange(fileContent, ast.OffsetRange{})
		iterOffset, hasOffset := sourceInfo.GetOffsetRange(comp.IterRange().ID())
		if hasOffset {
			_, byteStop = celOffsetRangeToByteRange(fileContent, iterOffset)
		}

		// Find the loop var declaration in the source after the IterRange
		for i := byteStop; i < len(fileContent)-len(loopVarName)+1; i++ {
			if fileContent[i:i+len(loopVarName)] == loopVarName {
				if (i == 0 || !isIdentifierChar(rune(fileContent[i-1]))) &&
					(i+len(loopVarName) >= len(fileContent) || !isIdentifierChar(rune(fileContent[i+len(loopVarName)]))) {
					byteStart := i
					byteEnd := i + len(loopVarName)
					startLine, startCol := byteOffsetToLineCol(fileContent, byteStart)
					endLine, endCol := byteOffsetToLineCol(fileContent, byteEnd)

					edits = append(edits, protocol.TextEdit{
						Range: protocol.Range{
							Start: protocol.Position{Line: startLine, Character: startCol},
							End:   protocol.Position{Line: endLine, Character: endCol},
						},
						NewText: newName,
					})
					break
				}
			}
		}
	}

	// Collect all occurrences in the comprehension's expressions
	collected := CollectIdentifierOccurrences(comp.IterRange(), sourceInfo, fileContent, identName, newName)
	edits = append(edits, collected...)
	collected = CollectIdentifierOccurrences(comp.AccuInit(), sourceInfo, fileContent, identName, newName)
	edits = append(edits, collected...)
	collected = CollectIdentifierOccurrences(comp.LoopCondition(), sourceInfo, fileContent, identName, newName)
	edits = append(edits, collected...)
	collected = CollectIdentifierOccurrences(comp.LoopStep(), sourceInfo, fileContent, identName, newName)
	edits = append(edits, collected...)
	collected = CollectIdentifierOccurrences(comp.Result(), sourceInfo, fileContent, identName, newName)
	edits = append(edits, collected...)

	return edits
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

// findIdentifierByName finds an identifier with the given name whose range contains the target offset.
func findIdentifierByName(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, targetOffset int) *identifierInfo {
	var result *identifierInfo

	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		if e == nil || result != nil {
			return
		}

		if e.Kind() == ast.IdentKind && e.AsIdent() == identName {
			offsetRange, hasOffset := sourceInfo.GetOffsetRange(e.ID())
			if hasOffset {
				byteStart, byteEnd := celOffsetRangeToByteRange(fileContent, offsetRange)
				if targetOffset >= byteStart && targetOffset < byteEnd {
					result = &identifierInfo{
						name:   identName,
						exprID: e.ID(),
						kind:   identifierKindTopLevel,
					}
					return
				}
			}
		}

		switch e.Kind() {
		case ast.CallKind:
			call := e.AsCall()
			for _, arg := range call.Args() {
				walk(arg)
			}
			if call.IsMemberFunction() {
				walk(call.Target())
			}

		case ast.ListKind:
			for _, elem := range e.AsList().Elements() {
				walk(elem)
			}

		case ast.MapKind:
			for _, entry := range e.AsMap().Entries() {
				mapEntry := entry.AsMapEntry()
				walk(mapEntry.Key())
				walk(mapEntry.Value())
			}

		case ast.StructKind:
			for _, field := range e.AsStruct().Fields() {
				walk(field.AsStructField().Value())
			}

		case ast.SelectKind:
			sel := e.AsSelect()
			if sel.Operand() != nil {
				walk(sel.Operand())
			}

		case ast.ComprehensionKind:
			comp := e.AsComprehension()
			walk(comp.IterRange())
			walk(comp.AccuInit())
			walk(comp.LoopCondition())
			walk(comp.LoopStep())
			walk(comp.Result())
		}
	}

	walk(expr)
	return result
}
