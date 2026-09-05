package lsp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// writeDescriptorSet writes an encoded FileDescriptorSet describing a message
// type that is deliberately not linked into this binary, so that resolving it
// can only have come from the file.
//
//	package cells.test;
//	message Request {
//	  string method = 1;
//	  int64 retries = 2;
//	  Nested nested = 3;
//	  repeated string tags = 4;
//	}
//	message Nested { string name = 1; }
func writeDescriptorSet(t *testing.T) string {
	t.Helper()

	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	proto3 := "proto3"

	set := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{{
			Name:    new("cells/test/request.proto"),
			Package: new("cells.test"),
			Syntax:  &proto3,
			MessageType: []*descriptorpb.DescriptorProto{
				{
					Name: new("Request"),
					Field: []*descriptorpb.FieldDescriptorProto{
						{Name: new("method"), Number: proto.Int32(1), Type: &str, Label: &optional, JsonName: new("method")},
						{Name: new("retries"), Number: proto.Int32(2), Type: &i64, Label: &optional, JsonName: new("retries")},
						{Name: new("nested"), Number: proto.Int32(3), Type: &msg, Label: &optional, TypeName: new(".cells.test.Nested"), JsonName: new("nested")},
						{Name: new("tags"), Number: proto.Int32(4), Type: &str, Label: &repeated, JsonName: new("tags")},
					},
				},
				{
					Name: new("Nested"),
					Field: []*descriptorpb.FieldDescriptorProto{
						{Name: new("name"), Number: proto.Int32(1), Type: &str, Label: &optional, JsonName: new("name")},
					},
				},
			},
		}},
	}

	data, err := proto.Marshal(set)
	ok.MustNoError(t, err)
	path := filepath.Join(t.TempDir(), "descriptors.binpb")
	ok.MustNoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func TestDescriptorSetDeclaresMessageTypes(t *testing.T) {
	t.Parallel()

	opts := loadOptions(t, `
name: test
variables:
  - name: request
    type_name: "cells.test.Request"
`)
	opts.DescriptorSets = []string{writeDescriptorSet(t)}

	tests := []struct {
		name string
		expr string
		want string // "" means the expression checks cleanly
	}{
		{"scalar_field", `request.method == "POST"`, ""},
		{"numeric_field", "request.retries < 3", ""},
		{"nested_message", `request.nested.name == "x"`, ""},
		{"repeated_field", `request.tags.exists(t, t == "x")`, ""},
		{"unknown_field", "request.nosuchfield == 1", "undefined field 'nosuchfield'"},
		{"unknown_nested_field", "request.nested.nosuchfield == 1", "undefined field 'nosuchfield'"},
		{"wrong_type", "request.method + 1", "no matching overload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			diags, err := lsp.Check(tt.expr, opts)
			ok.MustNoError(t, err)
			if tt.want == "" {
				ok.Equal(t, len(diags), 0, ok.Sprintf("diagnostics: %v", diags))
				return
			}
			if !ok.Equal(t, len(diags), 1, ok.Sprintf("diagnostics: %v", diags)) {
				return
			}
			ok.True(t, strings.Contains(diags[0].Message, tt.want), ok.Sprintf("message: %q", diags[0].Message))
		})
	}
}

// Without the descriptor set the type the configuration names does not exist,
// which is a failure to build the environment rather than a diagnostic.
func TestDescriptorSetRequiredForItsTypes(t *testing.T) {
	t.Parallel()

	opts := loadOptions(t, `
name: test
variables:
  - name: request
    type_name: "cells.test.Request"
`)

	_, err := lsp.Check(`request.method == "POST"`, opts)
	if !ok.True(t, err != nil) {
		return
	}
	ok.True(t, strings.Contains(err.Error(), "cells.test.Request"), ok.Sprintf("error: %v", err))
}

// A message type can also be used as the context variable, which declares each
// of its fields as a top-level name.
func TestDescriptorSetContextVariable(t *testing.T) {
	t.Parallel()

	opts := loadOptions(t, `
name: test
context_variable:
  type_name: "cells.test.Request"
`)
	opts.DescriptorSets = []string{writeDescriptorSet(t)}

	diags, err := lsp.Check(`method == "POST" && retries < 3`, opts)
	ok.MustNoError(t, err)
	ok.Equal(t, len(diags), 0, ok.Sprintf("diagnostics: %v", diags))

	diags, err = lsp.Check("nosuchfield == 1", opts)
	ok.MustNoError(t, err)
	if ok.Equal(t, len(diags), 1, ok.Sprintf("diagnostics: %v", diags)) {
		ok.True(t, strings.Contains(diags[0].Message, "undeclared reference"), ok.Sprintf("message: %q", diags[0].Message))
	}
}

func TestDescriptorSetErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing_file", func(t *testing.T) {
		t.Parallel()

		opts := lsp.Options{DescriptorSets: []string{filepath.Join(t.TempDir(), "absent.binpb")}}
		_, err := lsp.Check("1 + 1", opts)
		ok.True(t, err != nil)
	})

	t.Run("not_a_descriptor_set", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "junk.binpb")
		ok.MustNoError(t, os.WriteFile(path, []byte("this is not a descriptor set at all"), 0o600))

		opts := lsp.Options{DescriptorSets: []string{path}}
		_, err := lsp.Check("1 + 1", opts)
		if !ok.True(t, err != nil) {
			return
		}
		ok.True(t, strings.Contains(err.Error(), path), ok.Sprintf("error: %v", err))
	})
}

// Declared names are what hover and completion had nothing to say about
// before a configuration existed.
func TestDeclarationsDriveHoverAndCompletion(t *testing.T) {
	t.Parallel()

	opts := loadOptions(t, `
name: test
variables:
  - name: request
    type_name: "cells.test.Request"
    description: The request being authorized.
`)
	opts.DescriptorSets = []string{writeDescriptorSet(t)}

	t.Run("hover_on_variable", func(t *testing.T) {
		t.Parallel()

		got, err := lsp.Hover(`request.method == "POST"`, 1, 1, opts)
		ok.MustNoError(t, err)
		ok.True(t, strings.Contains(got, "cells.test.Request"), ok.Sprintf("hover: %q", got))
		// The configuration's own description comes along.
		ok.True(t, strings.Contains(got, "The request being authorized."), ok.Sprintf("hover: %q", got))
	})

	t.Run("hover_on_field", func(t *testing.T) {
		t.Parallel()

		// Column 9 is the "m" of method.
		got, err := lsp.Hover(`request.method == "POST"`, 1, 9, opts)
		ok.MustNoError(t, err)
		ok.True(t, strings.Contains(got, "`method`"), ok.Sprintf("hover: %q", got))
		ok.True(t, strings.Contains(got, "string"), ok.Sprintf("hover: %q", got))
	})

	t.Run("hover_on_nested_field", func(t *testing.T) {
		t.Parallel()

		// Column 16 is the "n" of name in request.nested.name.
		got, err := lsp.Hover(`request.nested.name == "x"`, 1, 16, opts)
		ok.MustNoError(t, err)
		ok.True(t, strings.Contains(got, "`name`"), ok.Sprintf("hover: %q", got))
		ok.True(t, strings.Contains(got, "string"), ok.Sprintf("hover: %q", got))
	})

	t.Run("no_hover_without_declarations", func(t *testing.T) {
		t.Parallel()

		// The same expression against plain CEL: nothing is declared, so
		// there is nothing to say about the name.
		got, err := lsp.Hover(`request.method == "POST"`, 1, 1, lsp.Options{})
		ok.MustNoError(t, err)
		ok.Equal(t, got, "")
	})
}

// Completion offers the declared names, which is the other thing that was
// impossible before there was a configuration.
func TestDeclarationsDriveCompletionItems(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	descriptors := writeDescriptorSet(t)
	ok.MustNoError(t, os.WriteFile(filepath.Join(dir, lsp.ConfigFileName), []byte(`
name: test
variables:
  - name: request
    type_name: "cells.test.Request"
  - name: retries
    type: "int"
`), 0o600))

	conn := newLSPClient(t, protocol.UnimplementedClient{},
		lsp.Options{DescriptorSets: []string{descriptors}})
	initializeServer(t, conn, "")

	t.Run("variables", func(t *testing.T) {
		celPath := filepath.Join(dir, "vars.cel")
		openDocument(t, conn, celPath, "")

		items := completionAt(t, conn, celPath, protocol.Position{Line: 0, Character: 0})
		ok.True(t, hasLabel(items, "request"), ok.Sprintf("labels: %v", labelsOf(items)))
		ok.True(t, hasLabel(items, "retries"), ok.Sprintf("labels: %v", labelsOf(items)))
	})

	t.Run("message_fields_after_dot", func(t *testing.T) {
		celPath := filepath.Join(dir, "fields.cel")
		openDocument(t, conn, celPath, "request.")

		items := completionAt(t, conn, celPath, protocol.Position{Line: 0, Character: 8})
		// A message has fields and no member functions of its own, so its
		// fields are the whole of what can follow the dot.
		ok.DeepEqual(t, labelsOf(items), []string{"method", "nested", "retries", "tags"})
	})

	t.Run("no_fields_on_a_scalar", func(t *testing.T) {
		celPath := filepath.Join(dir, "scalar.cel")
		openDocument(t, conn, celPath, "retries.")

		// An int has no fields of its own, so a message's field names must not
		// leak into what follows its dot.
		items := completionAt(t, conn, celPath, protocol.Position{Line: 0, Character: 8})
		ok.True(t, !hasLabel(items, "method"), ok.Sprintf("labels: %v", labelsOf(items)))
		ok.True(t, !hasLabel(items, "tags"), ok.Sprintf("labels: %v", labelsOf(items)))
	})
}

// openDocument opens a document at path holding content.
func openDocument(t *testing.T, conn jsonrpc2.Conn, path, content string) {
	t.Helper()
	err := conn.Notify(t.Context(), "textDocument/didOpen", protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: uri.File(path), LanguageID: "cel", Version: 1, Text: content,
		},
	})
	ok.MustNoError(t, err)
}

// completionAt returns the completion items offered at a position.
func completionAt(t *testing.T, conn jsonrpc2.Conn, path string, pos protocol.Position) []protocol.CompletionItem {
	t.Helper()
	var result protocol.CompletionList
	_, err := conn.Call(t.Context(), "textDocument/completion", protocol.CompletionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
		Position:     pos,
	}, &result)
	ok.MustNoError(t, err)
	return result.Items
}

func hasLabel(items []protocol.CompletionItem, label string) bool {
	for _, item := range items {
		if item.Label == label {
			return true
		}
	}
	return false
}

func labelsOf(items []protocol.CompletionItem) []string {
	labels := make([]string, 0, len(items))
	for _, item := range items {
		labels = append(labels, item.Label)
	}
	return labels
}
