package lsp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
