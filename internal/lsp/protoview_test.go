package lsp_test

import (
	"path/filepath"
	"testing"

	"github.com/bufbuild/protocompile"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const requestProtoURI = uri.URI("cells-proto:/cells/test/request.proto")

// newProtoContentClient starts a server whose environment declares request, a
// cells.test.Request from writeDescriptorSet, for a client that supports
// workspace/textDocumentContent if supported is set.
func newProtoContentClient(t *testing.T, supported bool) jsonrpc2.Conn {
	t.Helper()

	opts := loadOptions(t, `
name: test
variables:
  - name: request
    type_name: "cells.test.Request"
`)
	opts.DescriptorSets = []string{writeDescriptorSet(t)}
	conn := newLSPClient(t, protocol.UnimplementedClient{}, opts)
	params := protocol.InitializeParams{}
	if supported {
		params.Capabilities.Workspace = &protocol.WorkspaceClientCapabilities{
			TextDocumentContent: &protocol.TextDocumentContentClientCapabilities{},
		}
	}
	var result protocol.InitializeResult
	_, err := conn.Call(t.Context(), "initialize", params, &result)
	ok.MustNoError(t, err)
	ok.MustNoError(t, conn.Notify(t.Context(), "initialized", protocol.InitializedParams{}))
	return conn
}

func TestProtoContent(t *testing.T) {
	t.Parallel()

	conn := newProtoContentClient(t, true)
	var result protocol.TextDocumentContentResult
	_, err := conn.Call(t.Context(), "workspace/textDocumentContent", protocol.TextDocumentContentParams{URI: requestProtoURI}, &result)
	ok.MustNoError(t, err)
	ok.Equal(t, result.Text, `// Rendered by cells from a descriptor set.
syntax = "proto3";

package cells.test;

message Request {
  // The HTTP method, uppercased.
  //
  // Only POST and GET reach policy evaluation.
  string method = 1;
  int64 retries = 2;
  cells.test.Nested nested = 3;
  repeated string tags = 4;
  cells.test.Request.Inner inner = 5;
  message Inner {
    // A label on the inner message.
    string label = 1;
  }
}

message Nested {
  // The name of the thing.
  string name = 1;
}
`)

	_, err = conn.Call(t.Context(), "workspace/textDocumentContent", protocol.TextDocumentContentParams{URI: "cells-proto:/cells/test/missing.proto"}, &result)
	ok.ErrorContains(t, err, "no descriptor set describes cells/test/missing.proto")
}

// The rendering of a file written the way cells renders one is the file
// itself.
func TestProtoContentRoundTrip(t *testing.T) {
	t.Parallel()

	const source = `// Rendered by cells from a descriptor set.
syntax = "proto3";

package cells.roundtrip;

import "google/protobuf/timestamp.proto";

// The state of an account.
enum Status {
  STATUS_UNSPECIFIED = 0;
  // Able to sign in.
  STATUS_ACTIVE = 1;
}

// An account.
message Account {
  string id = 1;
  map<string, int64> counts = 2;
  // How the account signs in.
  oneof login {
    string password_hash = 3;
    string sso_provider = 4;
  }
  optional string nickname = 5;
  cells.roundtrip.Status status = 6;
  repeated cells.roundtrip.Account.Device devices = 7;
  google.protobuf.Timestamp created = 8;
  enum Kind {
    KIND_UNSPECIFIED = 0;
  }
  message Device {
    string name = 1;
  }
}
`
	compiler := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: protocompile.SourceAccessorFromMap(map[string]string{"account.proto": source}),
		}),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	files, err := compiler.Compile(t.Context(), "account.proto")
	ok.MustNoError(t, err)
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto),
		protodesc.ToFileDescriptorProto(files[0]),
	}}

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{DescriptorSets: []string{marshalDescriptorSet(t, set)}})
	initializeServer(t, conn, "")
	var result protocol.TextDocumentContentResult
	_, err = conn.Call(t.Context(), "workspace/textDocumentContent", protocol.TextDocumentContentParams{URI: "cells-proto:/account.proto"}, &result)
	ok.MustNoError(t, err)
	ok.Equal(t, result.Text, source)
}

func TestProtoDefinition(t *testing.T) {
	t.Parallel()

	at := func(line, startCol, endCol uint32) protocol.Range {
		return protocol.Range{
			Start: protocol.Position{Line: line, Character: startCol},
			End:   protocol.Position{Line: line, Character: endCol},
		}
	}
	tests := []struct {
		name    string
		content string
		cursor  uint32
		// unsupported leaves the client without workspace/textDocumentContent.
		unsupported bool
		// want is the range in the rendered request.proto, or nil for no
		// location there.
		want *protocol.Range
	}{
		{name: "selected field", content: `request.method == "POST"`, cursor: 9, want: new(at(9, 9, 15))},
		{name: "field of a selected message", content: `request.nested.name == "x"`, cursor: 16, want: new(at(22, 9, 13))},
		{name: "field of a nested message", content: `request.inner.label == "x"`, cursor: 15, want: new(at(16, 11, 16))},
		{name: "constructed message", content: `cells.test.Nested{name: "x"}.name == "x"`, cursor: 12, want: new(at(20, 8, 14))},
		{name: "package of a constructed message", content: `cells.test.Nested{name: "x"}.name == "x"`, cursor: 1, want: new(at(20, 8, 14))},
		{name: "field of a constructed message", content: `cells.test.Nested{name: "x"}.name == "x"`, cursor: 19, want: new(at(22, 9, 13))},
		{name: "variable", content: `request.method == "POST"`, cursor: 2},
		{name: "client without support", content: `request.method == "POST"`, cursor: 9, unsupported: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn := newProtoContentClient(t, !tt.unsupported)
			celPath := filepath.Join(t.TempDir(), "a.cel")
			openDocument(t, conn, celPath, tt.content)
			var locations []protocol.Location
			_, err := conn.Call(t.Context(), "textDocument/definition", protocol.DefinitionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(celPath)},
				Position:     protocol.Position{Character: tt.cursor},
			}, &locations)
			ok.MustNoError(t, err)

			var got *protocol.Range
			for _, location := range locations {
				if location.URI == requestProtoURI {
					got = &location.Range
				}
			}
			ok.Equal(t, got, tt.want)
		})
	}
}

func TestProtoContentCapability(t *testing.T) {
	t.Parallel()

	conn := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	var result protocol.InitializeResult
	_, err := conn.Call(t.Context(), "initialize", protocol.InitializeParams{}, &result)
	ok.MustNoError(t, err)
	// The two forms the capability takes have the same JSON when there is no
	// registration ID, so a client cannot tell which was sent.
	content, isOptions := result.Capabilities.Workspace.TextDocumentContent.(*protocol.TextDocumentContentRegistrationOptions)
	if !isOptions {
		t.Fatalf("textDocumentContent: %#v", result.Capabilities.Workspace.TextDocumentContent)
	}
	ok.Equal(t, content.Schemes, []string{"cells-proto"})
}
