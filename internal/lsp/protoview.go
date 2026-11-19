package lsp

import (
	"context"
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/parser/gen"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// protoScheme is the URI scheme of the .proto files cells renders from its
// descriptor sets. A descriptor set holds no source, so cells writes some, and
// a client that supports workspace/textDocumentContent asks cells for it.
const protoScheme = "cells-proto"

// protoURI returns the URI of the rendered file at path, a descriptor's file
// name such as acme/v1/user.proto.
func protoURI(path string) uri.URI {
	// Components this simple always form a URI.
	u, _ := uri.From(uri.Components{Scheme: protoScheme, Path: "/" + path})
	return u
}

// protoPath returns the file name a protoURI refers to.
func protoPath(docURI uri.URI) (string, bool) {
	c := docURI.Components()
	if c.Scheme != protoScheme {
		return "", false
	}
	return strings.TrimPrefix(c.Path, "/"), true
}

// newProtoFiles resolves the files of every descriptor set, or returns nil if
// they do not resolve together. A file named in more than one set is taken
// from the first, and an import none of them holds is left unresolved.
func newProtoFiles(sets []*descriptorpb.FileDescriptorSet) *protoregistry.Files {
	merged := &descriptorpb.FileDescriptorSet{}
	seen := make(map[string]bool)
	for _, set := range sets {
		for _, file := range set.GetFile() {
			if !seen[file.GetName()] {
				seen[file.GetName()] = true
				merged.File = append(merged.File, file)
			}
		}
	}
	files, err := protodesc.FileOptions{AllowUnresolvable: true}.NewFiles(merged)
	if err != nil {
		return nil
	}
	return files
}

// TextDocumentContent renders the .proto file a protoURI names, from the
// descriptor sets cells was given.
func (s *server) TextDocumentContent(_ context.Context, params *protocol.TextDocumentContentParams) (*protocol.TextDocumentContentResult, error) {
	path, ok := protoPath(params.URI)
	if !ok {
		return nil, jsonrpc2.Errorf(jsonrpc2.InvalidParams, "%s is not a document cells provides", params.URI)
	}
	file := s.envs.protoFile(path)
	if file == nil {
		return nil, jsonrpc2.Errorf(jsonrpc2.InvalidParams, "no descriptor set describes %s", path)
	}
	text, _ := renderProtoFile(file)
	return &protocol.TextDocumentContentResult{Text: text}, nil
}

// protoFile returns the file at path that the descriptor sets describe, or
// nil. Every environment is built from the same descriptor sets, so any one
// of them will do.
func (c *envCache) protoFile(path string) protoreflect.FileDescriptor {
	for _, e := range c.byPath {
		if e.protoFiles == nil {
			continue
		}
		if file, err := e.protoFiles.FindFileByPath(path); err == nil {
			return file
		}
		return nil
	}
	e, err := c.forPath(c.opts.ConfigPath)
	if err != nil || e.protoFiles == nil {
		return nil
	}
	file, err := e.protoFiles.FindFileByPath(path)
	if err != nil {
		return nil
	}
	return file
}

// protoDefinition returns where the message or field at pos is declared in
// the rendered .proto file that declares it: the field of a selection such as
// request.method, or the message or a field named in a construction such as
// acme.v1.User{name: "a"}.
func protoDefinition(f *file, docEnv *environment, pos protocol.Position) (protocol.Location, bool) {
	if docEnv.protoFiles == nil {
		return protocol.Location{}, false
	}
	name, ok := protoNameAt(f, docEnv.celEnv, pos)
	if !ok {
		return protocol.Location{}, false
	}
	d, err := docEnv.protoFiles.FindDescriptorByName(name)
	if err != nil {
		return protocol.Location{}, false
	}
	_, ranges := renderProtoFile(d.ParentFile())
	r, ok := ranges[name]
	if !ok {
		return protocol.Location{}, false
	}
	return protocol.Location{URI: protoURI(d.ParentFile().Path()), Range: r}, true
}

// protoNameAt returns the full name of the message or field named at pos.
// It needs the expression to type-check, since only the check knows which
// message an operand or a construction is.
func protoNameAt(f *file, celEnv *cel.Env, pos protocol.Position) (protoreflect.FullName, bool) {
	offset, ok := f.offsetAt(pos)
	if !ok {
		return "", false
	}
	checked, _ := f.check(celEnv)
	nativeAST := f.ast(celEnv)
	if checked == nil || nativeAST == nil {
		return "", false
	}
	typed := checked.NativeRep()
	tokens := newSourceTokens(f.content, nativeAST.SourceInfo())
	at := func(i int) bool {
		return i >= 0 && tokens.tokens[i].start <= offset && offset <= tokens.tokens[i].end
	}
	messageOf := func(id int64) (string, bool) {
		t := typed.GetType(id)
		if t == nil || t.Kind() != types.StructKind {
			return "", false
		}
		return t.TypeName(), true
	}

	var found protoreflect.FullName
	celast.PreOrderVisit(nativeAST.Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		if found != "" {
			return
		}
		switch e.Kind() {
		case celast.SelectKind:
			sel := e.AsSelect()
			message, ok := messageOf(sel.Operand().ID())
			// A selection is recorded at its dot; the field name follows.
			if i := tokens.step(tokens.anchor(e.ID()), 1); ok && tokens.isName(i) && tokens.text(i) == sel.FieldName() && at(i) {
				found = protoreflect.FullName(message + "." + sel.FieldName())
			}

		case celast.StructKind:
			message, ok := messageOf(e.ID())
			if !ok {
				return
			}
			// A construction is recorded at its brace, after the message's
			// name, which may be qualified.
			for i := tokens.step(tokens.anchor(e.ID()), -1); tokens.isName(i); i = tokens.step(i, -2) {
				if at(i) {
					found = protoreflect.FullName(message)
					return
				}
				if tokens.kind(tokens.step(i, -1)) != gen.CELLexerDOT {
					break
				}
			}
			for _, field := range e.AsStruct().Fields() {
				name := field.AsStructField().Name()
				// A field is recorded at its colon, after its name.
				if i := tokens.step(tokens.anchor(field.ID()), -1); tokens.isName(i) && tokens.text(i) == name && at(i) {
					found = protoreflect.FullName(message + "." + name)
					return
				}
			}
		}
	}))
	return found, found != ""
}

// renderProtoFile writes file as .proto source, with the comments its source
// info kept, and returns it along with the range of the name in each
// declaration of a message, field, enum, or enum value. Services and
// extensions, which an expression cannot refer to, and options are left out.
func renderProtoFile(file protoreflect.FileDescriptor) (string, map[protoreflect.FullName]protocol.Range) {
	w := &protoWriter{file: file, ranges: make(map[protoreflect.FullName]protocol.Range)}
	w.line(0, "// Rendered by cells from a descriptor set.")
	if file.Syntax() == protoreflect.Editions {
		edition := protodesc.ToFileDescriptorProto(file).GetEdition()
		w.line(0, fmt.Sprintf("edition = %q;", strings.TrimPrefix(edition.String(), "EDITION_")))
	} else {
		w.line(0, fmt.Sprintf("syntax = %q;", file.Syntax()))
	}
	if file.Package() != "" {
		w.line(0, "")
		w.line(0, fmt.Sprintf("package %s;", file.Package()))
	}
	if imports := file.Imports(); imports.Len() > 0 {
		w.line(0, "")
		for i := range imports.Len() {
			w.line(0, fmt.Sprintf("import %q;", imports.Get(i).Path()))
		}
	}
	for i := range file.Enums().Len() {
		w.line(0, "")
		w.enum(0, file.Enums().Get(i))
	}
	for i := range file.Messages().Len() {
		w.line(0, "")
		w.message(0, file.Messages().Get(i))
	}
	return w.text.String(), w.ranges
}

// protoWriter accumulates rendered .proto source, a line at a time.
type protoWriter struct {
	file   protoreflect.FileDescriptor
	text   strings.Builder
	lines  uint32
	ranges map[protoreflect.FullName]protocol.Range
}

// line writes s on a line of its own, indented by depth levels.
func (w *protoWriter) line(depth int, s string) {
	if s != "" {
		w.text.WriteString(strings.Repeat("  ", depth))
	}
	w.text.WriteString(s)
	w.text.WriteString("\n")
	w.lines++
}

// declaration writes the comments on d, then a line holding before, d's name,
// and after, recording where the name is.
func (w *protoWriter) declaration(depth int, d protoreflect.Descriptor, before, after string) {
	if comments := w.file.SourceLocations().ByDescriptor(d).LeadingComments; comments != "" {
		for comment := range strings.SplitSeq(strings.TrimSuffix(comments, "\n"), "\n") {
			w.line(depth, "//"+comment)
		}
	}
	start := uint32(2*depth + len(before))
	w.ranges[d.FullName()] = protocol.Range{
		Start: protocol.Position{Line: w.lines, Character: start},
		End:   protocol.Position{Line: w.lines, Character: start + uint32(len(d.Name()))},
	}
	w.line(depth, before+string(d.Name())+after)
}

func (w *protoWriter) message(depth int, m protoreflect.MessageDescriptor) {
	w.declaration(depth, m, "message ", " {")
	fields := m.Fields()
	written := make(map[protoreflect.FullName]bool)
	for i := range fields.Len() {
		field := fields.Get(i)
		oneof := field.ContainingOneof()
		if oneof == nil || oneof.IsSynthetic() {
			w.field(depth+1, field)
			continue
		}
		if written[oneof.FullName()] {
			continue
		}
		written[oneof.FullName()] = true
		w.declaration(depth+1, oneof, "oneof ", " {")
		for j := range oneof.Fields().Len() {
			w.field(depth+2, oneof.Fields().Get(j))
		}
		w.line(depth+1, "}")
	}
	for i := range m.Enums().Len() {
		w.enum(depth+1, m.Enums().Get(i))
	}
	for i := range m.Messages().Len() {
		if nested := m.Messages().Get(i); !nested.IsMapEntry() {
			w.message(depth+1, nested)
		}
	}
	w.line(depth, "}")
}

func (w *protoWriter) field(depth int, field protoreflect.FieldDescriptor) {
	var label string
	switch {
	case field.IsMap():
	case field.Cardinality() == protoreflect.Repeated:
		label = "repeated "
	case w.file.Syntax() == protoreflect.Proto2:
		label = field.Cardinality().String() + " "
	case w.file.Syntax() == protoreflect.Proto3 && field.HasOptionalKeyword():
		label = "optional "
	}
	w.declaration(depth, field, label+protoFieldType(field)+" ", fmt.Sprintf(" = %d;", field.Number()))
}

func (w *protoWriter) enum(depth int, e protoreflect.EnumDescriptor) {
	w.declaration(depth, e, "enum ", " {")
	for i := range e.Values().Len() {
		value := e.Values().Get(i)
		w.declaration(depth+1, value, "", fmt.Sprintf(" = %d;", value.Number()))
	}
	w.line(depth, "}")
}

// protoFieldType returns how a field's type is written in a .proto file,
// naming a message or enum in full.
func protoFieldType(field protoreflect.FieldDescriptor) string {
	if field.IsMap() {
		return fmt.Sprintf("map<%s, %s>", protoFieldType(field.MapKey()), protoFieldType(field.MapValue()))
	}
	switch field.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return string(field.Message().FullName())
	case protoreflect.EnumKind:
		return string(field.Enum().FullName())
	default:
		return field.Kind().String()
	}
}
