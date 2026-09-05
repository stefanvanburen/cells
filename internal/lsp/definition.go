package lsp

import (
	"cmp"
	"context"
	"os"
	"slices"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"go.lsp.dev/protocol"
	lspuri "go.lsp.dev/uri"
	"go.yaml.in/yaml/v3"
)

func (s *server) Definition(_ context.Context, params *protocol.DefinitionParams) (protocol.DefinitionResult, error) {
	f, docEnv := s.document(params.TextDocument.URI)
	if f == nil || docEnv == nil || f.content == "" {
		return nil, nil
	}

	location, found := computeDefinition(f, docEnv.celEnv, docEnv.configPath, params.Position)
	if !found {
		return nil, nil
	}
	return protocol.LocationSlice{location}, nil
}

// computeDefinition resolves the name at pos to where it was declared.
//
// A CEL expression has no other file to point into on its own: nothing imports
// anything, and every function the standard library and the extensions supply
// is defined in Go. What is left is what a configuration declared, so a name
// resolves to its entry in that file.
func computeDefinition(f *file, celEnv *cel.Env, configPath string, pos protocol.Position) (protocol.Location, bool) {
	nativeAST, info := f.identifierAt(celEnv, pos)
	if info == nil {
		return protocol.Location{}, false
	}

	// A name a macro binds was declared in this same file, where the macro
	// binds it.
	if location, isBound := macroBindingLocation(f, nativeAST, info); isBound {
		return location, true
	}
	if configPath == "" {
		return protocol.Location{}, false
	}

	// Only names the configuration could have declared are worth looking for.
	// A function is declared under "functions", anything else under
	// "variables"; a name cells cannot find in the environment at all was
	// never declared and has no definition to point at.
	section := configVariables
	if info.kind == identifierKindFunction {
		section = configFunctions
	} else if !declaresVariable(celEnv, info.name) {
		return protocol.Location{}, false
	}

	return findConfigDeclaration(configPath, section, info.name)
}

// macroBindingLocation returns where a macro binds the name, when it is one a
// macro binds: the x in [1, 2].map(x, x * 2), or the v in cel.bind(v, 1, v).
//
// The declaration is the first of the name's occurrences within the macro that
// binds it, since a macro names what it binds before any use of it.
func macroBindingLocation(f *file, nativeAST *celast.AST, info *identifierInfo) (protocol.Location, bool) {
	sourceInfo := nativeAST.SourceInfo()
	s := determineIdentifierScope(info.exprID, info.name, nativeAST.Expr(), sourceInfo)
	if _, bound := s.(loopVarScope); !bound {
		return protocol.Location{}, false
	}

	ranges := identifierOccurrences(nativeAST.Expr(), sourceInfo, f.content, s, info.name)
	if len(ranges) == 0 {
		return protocol.Location{}, false
	}
	declaration := slices.MinFunc(ranges, func(a, b protocol.Range) int {
		return cmp.Or(
			cmp.Compare(a.Start.Line, b.Start.Line),
			cmp.Compare(a.Start.Character, b.Start.Character),
		)
	})
	return protocol.Location{URI: f.uri, Range: declaration}, true
}

// declaresVariable reports whether the environment declares a variable of this
// name, which rules out a comprehension variable or an undeclared reference.
func declaresVariable(celEnv *cel.Env, name string) bool {
	for _, variable := range celEnv.Variables() {
		if variable.Name() == name {
			return true
		}
	}
	return false
}

// The top-level configuration keys whose entries declare a name.
const (
	configVariables = "variables"
	configFunctions = "functions"
)

// findConfigDeclaration returns the location of the entry named name under the
// given top-level key of the configuration file, which is where that name was
// declared.
//
// The configuration is re-parsed as a YAML node tree rather than read from the
// env.Config cells already holds, because unmarshalling into that discards the
// positions this needs.
func findConfigDeclaration(configPath, section, name string) (protocol.Location, bool) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return protocol.Location{}, false
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return protocol.Location{}, false
	}

	entries := mappingValue(documentRoot(&document), section)
	if entries == nil || entries.Kind != yaml.SequenceNode {
		return protocol.Location{}, false
	}
	for _, entry := range entries.Content {
		declared := mappingValue(entry, "name")
		if declared == nil || declared.Value != name {
			continue
		}
		return yamlNodeLocation(configPath, declared), true
	}
	return protocol.Location{}, false
}

// documentRoot unwraps a document node to the value it holds.
func documentRoot(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return node.Content[0]
	}
	return node
}

// mappingValue returns the value a mapping holds under key, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	// A mapping's content alternates key, value.
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// yamlNodeLocation converts a YAML node's position to an LSP location. yaml.v3
// reports 1-indexed lines and columns, in runes; LSP wants them 0-indexed, and
// its columns are UTF-16 code units.
func yamlNodeLocation(path string, node *yaml.Node) protocol.Location {
	line := uint32(max(node.Line-1, 0))
	start := uint32(max(node.Column-1, 0))
	end := start + uint32(len([]rune(node.Value)))
	return protocol.Location{
		URI: lspuri.File(path),
		Range: protocol.Range{
			Start: protocol.Position{Line: line, Character: start},
			End:   protocol.Position{Line: line, Character: end},
		},
	}
}
