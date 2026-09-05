package lsp

import (
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/descriptorpb"
)

// Field numbers within the descriptor protos, which are what a
// SourceCodeInfo.Location path is written in. A path of [4, 0, 2, 1] is the
// second field of the first message in the file.
const (
	fileMessageTypeField  = 4 // FileDescriptorProto.message_type
	messageFieldField     = 2 // DescriptorProto.field
	messageNestedTypeFild = 3 // DescriptorProto.nested_type
)

// protoFieldDocs indexes the documentation comments in a FileDescriptorSet by
// fully-qualified field name, so that hovering a field can show what the
// .proto file said about it.
//
// The comments are only there when the set was built with source info, which
// buf build includes by default and protoc includes with --include_source_info.
// A set without them simply contributes nothing.
func protoFieldDocs(set *descriptorpb.FileDescriptorSet, into map[string]string) {
	for _, file := range set.GetFile() {
		// SourceCodeInfo is a flat list keyed by path, so index it once and
		// then walk the messages to learn which path names which field.
		comments := make(map[string]string)
		for _, location := range file.GetSourceCodeInfo().GetLocation() {
			if comment := locationComment(location); comment != "" {
				comments[pathKey(location.GetPath())] = comment
			}
		}
		if len(comments) == 0 {
			continue
		}
		for i, message := range file.GetMessageType() {
			indexMessageDocs(
				message,
				qualify(file.GetPackage(), message.GetName()),
				[]int32{fileMessageTypeField, int32(i)},
				comments,
				into,
			)
		}
	}
}

// indexMessageDocs records the comment on each of a message's fields, and
// recurses into the messages nested inside it.
func indexMessageDocs(
	message *descriptorpb.DescriptorProto,
	qualifiedName string,
	path []int32,
	comments map[string]string,
	into map[string]string,
) {
	for i, field := range message.GetField() {
		fieldPath := childPath(path, messageFieldField, int32(i))
		if comment, ok := comments[pathKey(fieldPath)]; ok {
			into[qualifiedName+"."+field.GetName()] = comment
		}
	}
	for i, nested := range message.GetNestedType() {
		indexMessageDocs(
			nested,
			qualifiedName+"."+nested.GetName(),
			childPath(path, messageNestedTypeFild, int32(i)),
			comments,
			into,
		)
	}
}

// childPath extends a location path by a field number and an index, without
// writing into the array the parent path points at.
func childPath(path []int32, field, index int32) []int32 {
	child := make([]int32, 0, len(path)+2)
	child = append(child, path...)
	return append(child, field, index)
}

// pathKey renders a location path as a map key.
func pathKey(path []int32) string {
	parts := make([]string, len(path))
	for i, n := range path {
		parts[i] = strconv.Itoa(int(n))
	}
	return strings.Join(parts, ".")
}

// qualify joins a protobuf package and a name, tolerating a file that declares
// no package.
func qualify(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

// locationComment returns the documentation written against a location: the
// comment above it, or the one beside it when there is nothing above.
//
// protoc records comments with their leading space and trailing newline
// intact, and a comment spanning several lines arrives as several lines each
// still carrying that space, so strip it rather than showing it as Markdown
// indentation.
func locationComment(location *descriptorpb.SourceCodeInfo_Location) string {
	comment := location.GetLeadingComments()
	if comment == "" {
		comment = location.GetTrailingComments()
	}
	lines := strings.Split(strings.TrimRight(comment, "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, " ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
