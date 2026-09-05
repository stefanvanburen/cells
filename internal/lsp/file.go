package lsp

import (
	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// file tracks a single open document.
//
// A file's content is immutable: DidChange installs a replacement rather than
// writing to the value already in the map, so a handler that took a *file out
// of the map holds a consistent snapshot, and the parse below stays valid for
// as long as the value does.
type file struct {
	uri     uri.URI
	version int32
	content string

	// The parse and type-check of content, computed on first use. Every
	// feature needs the same AST, and there is no point in redoing that work
	// once per request against an unchanged document.
	//
	// celEnv is fixed for as long as a file value lives — the server settles
	// on one during initialize, and the CLI builds a file per invocation — so
	// these are not keyed by environment.
	parsed      *cel.Ast
	parseIssues *cel.Issues
	parseDone   bool
	checked     *cel.Ast
	checkIssues *cel.Issues
	checkDone   bool
}

// parse parses the file's content, returning the AST — nil if the content does
// not parse — and the issues the parser reported.
func (f *file) parse(celEnv *cel.Env) (*cel.Ast, *cel.Issues) {
	if !f.parseDone {
		f.parseDone = true
		parsed, issues := celEnv.Parse(f.content)
		f.parseIssues = issues
		if issues.Err() == nil {
			f.parsed = parsed
		}
	}
	return f.parsed, f.parseIssues
}

// check type-checks the file, returning the checked AST — nil if the content
// does not parse or does not type-check — and the issues the checker reported,
// which are nil when the content never got as far as being checked.
func (f *file) check(celEnv *cel.Env) (*cel.Ast, *cel.Issues) {
	if !f.checkDone {
		f.checkDone = true
		if parsed, _ := f.parse(celEnv); parsed != nil {
			checked, issues := celEnv.Check(parsed)
			f.checkIssues = issues
			if issues.Err() == nil {
				f.checked = checked
			}
		}
	}
	return f.checked, f.checkIssues
}

// ast returns the file's parsed AST in cel-go's native representation, or nil
// if the content does not parse.
func (f *file) ast(celEnv *cel.Env) *celast.AST {
	parsed, _ := f.parse(celEnv)
	if parsed == nil {
		return nil
	}
	return parsed.NativeRep()
}

// offsetAt converts an LSP position to a byte offset into the file, reporting
// false if the position does not fall inside the content.
func (f *file) offsetAt(pos protocol.Position) (int, bool) {
	offset := lineColToByteOffset(f.content, pos.Line, pos.Character)
	if offset < 0 || offset >= len(f.content) {
		return -1, false
	}
	return offset, true
}

// identifierAt returns the identifier-like element at pos along with the
// file's AST, or nil if the file does not parse or there is no identifier
// there. References, document highlights and renames all start here.
func (f *file) identifierAt(celEnv *cel.Env, pos protocol.Position) (*celast.AST, *identifierInfo) {
	nativeAST := f.ast(celEnv)
	if nativeAST == nil {
		return nil, nil
	}
	offset, ok := f.offsetAt(pos)
	if !ok {
		return nil, nil
	}

	sourceInfo := nativeAST.SourceInfo()
	if info := findIdentifierAtPosition(nativeAST.Expr(), sourceInfo, f.content, offset); info != nil {
		return nativeAST, info
	}

	// Nothing in the AST covers the offset. A loop variable's declaration is
	// the usual reason: macro expansion can leave it without a source range.
	// Take the word under the cursor and look for it in the AST by name.
	start, end := offset, offset
	for start > 0 && isIdentifierChar(rune(f.content[start-1])) {
		start--
	}
	for end < len(f.content) && isIdentifierChar(rune(f.content[end])) {
		end++
	}
	if start == end {
		return nil, nil
	}
	info := findIdentifierByName(nativeAST.Expr(), sourceInfo, f.content, f.content[start:end], offset)
	if info == nil {
		return nil, nil
	}
	return nativeAST, info
}

// identifierOccurrencesAt returns the ranges of every occurrence of the
// identifier at pos, within the scope that identifier belongs to.
func (f *file) identifierOccurrencesAt(celEnv *cel.Env, pos protocol.Position) (*identifierInfo, []protocol.Range) {
	nativeAST, info := f.identifierAt(celEnv, pos)
	if info == nil {
		return nil, nil
	}
	s := determineIdentifierScope(info.exprID, info.name, nativeAST.Expr())
	return info, identifierOccurrences(nativeAST.Expr(), nativeAST.SourceInfo(), f.content, s, info.name)
}
