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

	startLine, startCol := byteOffsetToLineCol(f.content, int(offsetRange.Start))
	endLine, endCol := byteOffsetToLineCol(f.content, int(offsetRange.Stop))

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
		if !hasOffset {
			return
		}

		// Check if targetOffset falls within this expression's range
		if targetOffset >= int(offsetRange.Start) && targetOffset < int(offsetRange.Stop) {
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
				byteStart, byteEnd := celOffsetRangeToByteRange(fileContent, offsetRange)
				funcNameStart := strings.Index(fileContent[byteStart:byteEnd], funcName)
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
	return candidates[len(candidates)-1]
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

		// Check if this is a comprehension and the target identifier is its loop var
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
		comp := findComprehensionByID(expr, sc.comprehensionID)
		if comp != nil {
			collectIdentifiersInComprehension(comp, sourceInfo, fileContent, oldName, newName, &edits)
		}

	case topLevelScope:
		// Search entire expression
		collectAllIdentifiersInExpr(expr, sourceInfo, fileContent, oldName, newName, &edits)
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
			startLine, startCol := byteOffsetToLineCol(fileContent, int(offsetRange.Start))
			endLine, endCol := byteOffsetToLineCol(fileContent, int(offsetRange.Stop))

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
		collectAllIdentifiersInExpr(comp.Args()[i], sourceInfo, fileContent, identName, newName, edits)
	}
}

// collectAllIdentifiersInExpr recursively collects all occurrences of identName.
func collectAllIdentifiersInExpr(expr ast.Expr, sourceInfo *ast.SourceInfo, fileContent string, identName string, newName string, edits *[]protocol.TextEdit) {
	if expr == nil {
		return
	}

	if expr.Kind() == ast.IdentKind && expr.AsIdent() == identName {
		offsetRange, hasOffset := sourceInfo.GetOffsetRange(expr.ID())
		if hasOffset {
			startLine, startCol := byteOffsetToLineCol(fileContent, int(offsetRange.Start))
			endLine, endCol := byteOffsetToLineCol(fileContent, int(offsetRange.Stop))

			*edits = append(*edits, protocol.TextEdit{
				Range: protocol.Range{
					Start: protocol.Position{Line: startLine, Character: startCol},
					End:   protocol.Position{Line: endLine, Character: endCol},
				},
				NewText: newName,
			})
		}
	}

	switch expr.Kind() {
	case ast.CallKind:
		call := expr.AsCall()
		for _, arg := range call.Args() {
			collectAllIdentifiersInExpr(arg, sourceInfo, fileContent, identName, newName, edits)
		}
		if call.IsMemberFunction() {
			collectAllIdentifiersInExpr(call.Target(), sourceInfo, fileContent, identName, newName, edits)
		}

	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			collectAllIdentifiersInExpr(elem, sourceInfo, fileContent, identName, newName, edits)
		}

	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			mapEntry := entry.AsMapEntry()
			collectAllIdentifiersInExpr(mapEntry.Key(), sourceInfo, fileContent, identName, newName, edits)
			collectAllIdentifiersInExpr(mapEntry.Value(), sourceInfo, fileContent, identName, newName, edits)
		}

	case ast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			collectAllIdentifiersInExpr(field.AsStructField().Value(), sourceInfo, fileContent, identName, newName, edits)
		}

	case ast.SelectKind:
		sel := expr.AsSelect()
		if sel.Operand() != nil {
			collectAllIdentifiersInExpr(sel.Operand(), sourceInfo, fileContent, identName, newName, edits)
		}

	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		collectAllIdentifiersInExpr(comp.IterRange(), sourceInfo, fileContent, identName, newName, edits)
		collectAllIdentifiersInExpr(comp.AccuInit(), sourceInfo, fileContent, identName, newName, edits)
		collectAllIdentifiersInExpr(comp.LoopCondition(), sourceInfo, fileContent, identName, newName, edits)
		collectAllIdentifiersInExpr(comp.LoopStep(), sourceInfo, fileContent, identName, newName, edits)
		collectAllIdentifiersInExpr(comp.Result(), sourceInfo, fileContent, identName, newName, edits)
	}
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
			if hasOffset && targetOffset >= int(offsetRange.Start) && targetOffset < int(offsetRange.Stop) {
				result = &identifierInfo{
					name:   identName,
					exprID: e.ID(),
					kind:   identifierKindTopLevel,
				}
				return
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
