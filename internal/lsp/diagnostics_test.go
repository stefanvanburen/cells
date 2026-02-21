package lsp_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nalgeon/be"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// setupDiagServer initializes the LSP server, opens testFilePath, and returns
// a client connection and the document URI.
func setupDiagServer(t *testing.T, testFilePath string) (*jsonrpc2.Conn, protocol.DocumentURI) {
	t.Helper()
	ctx := t.Context()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	go func() {
		_ = lsp.ServeStream(ctx, serverConn)
	}()

	noop := jsonrpc2.HandlerFunc(func(_ context.Context, _ *jsonrpc2.Conn, _ *jsonrpc2.Request) (any, error) {
		return nil, nil
	})
	clientRPC := jsonrpc2.NewConn(ctx, clientConn, noop)
	t.Cleanup(func() {
		_ = clientRPC.Close()
	})

	testURI := protocol.URIFromPath(testFilePath)

	var initResult protocol.InitializeResult
	err := clientRPC.Call(ctx, "initialize", protocol.InitializeParams{}, &initResult)
	be.Err(t, err, nil)

	err = clientRPC.Notify(ctx, "initialized", protocol.InitializedParams{})
	be.Err(t, err, nil)

	content, err := os.ReadFile(testFilePath)
	be.Err(t, err, nil)

	err = clientRPC.Notify(ctx, "textDocument/didOpen", protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        testURI,
			LanguageID: "cel",
			Version:    1,
			Text:       string(content),
		},
	})
	be.Err(t, err, nil)

	return clientRPC, testURI
}

// pullDiagnostics sends a textDocument/diagnostic request and returns the items.
func pullDiagnostics(t *testing.T, conn *jsonrpc2.Conn, uri protocol.DocumentURI) []protocol.Diagnostic {
	t.Helper()
	var result protocol.RelatedFullDocumentDiagnosticReport
	err := conn.Call(t.Context(), "textDocument/diagnostic", protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	}, &result)
	be.Err(t, err, nil)
	be.Equal(t, result.Kind, string(protocol.DiagnosticFull))
	return result.Items
}

func diagMessages(diags []protocol.Diagnostic) []string {
	msgs := make([]string, len(diags))
	for i, d := range diags {
		msgs[i] = d.Message
	}
	return msgs
}

func containsSubstring(strs []string, sub string) bool {
	for _, s := range strs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// openDiagFile resolves a testdata/diagnostics/ path, opens it, and returns
// what's needed for pull-diagnostic assertions.
func openDiagFile(t *testing.T, name string) (*jsonrpc2.Conn, protocol.DocumentURI) {
	t.Helper()
	testPath, err := filepath.Abs(filepath.Join("testdata", "diagnostics", name))
	be.Err(t, err, nil)
	return setupDiagServer(t, testPath)
}

// --- Parse error tests ---

func TestDiagnosticsParseErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		file        string
		wantContain string
	}{
		{"unexpected EOF", "unexpected_eof.cel", "mismatched input"},
		{"unclosed paren", "unclosed_paren.cel", "missing ')'"},
		{"unclosed bracket", "unclosed_bracket.cel", "mismatched input"},
		{"unclosed brace", "unclosed_brace.cel", "mismatched input"},
		{"unterminated string", "unterminated_string.cel", "token recognition error"},
		{"unexpected token", "unexpected_token.cel", "extraneous input"},
		{"trailing dot", "trailing_dot.cel", "no viable alternative"},
		{"double dot", "double_dot.cel", "no viable alternative"},
		{"unexpected comma", "unexpected_comma.cel", "mismatched input ','"},
		{"missing operator", "missing_operator.cel", "extraneous input"},
		{"empty function arg", "empty_function_arg.cel", "extraneous input ','"},
		{"bad escape in string", "bad_escape.cel", "token recognition error"},
		{"bad hex in bytes", "bad_hex_bytes.cel", "token recognition error"},
		{"has with non-field", "has_non_field.cel", "invalid argument to has()"},
		{"multiline parse error", "multiline_parse_error.cel", "mismatched input"},
		{"unclosed paren multiline", "unclosed_paren_multiline.cel", "missing ')'"},
		{"comment only", "comment_only.cel", "mismatched input"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, uri := openDiagFile(t, tt.file)
			diags := pullDiagnostics(t, conn, uri)

			if len(diags) == 0 {
				t.Fatalf("expected parse errors for %s, got none", tt.file)
			}
			for _, d := range diags {
				be.Equal(t, d.Severity, protocol.SeverityError)
				be.Equal(t, d.Source, "cells")
			}
			if !containsSubstring(diagMessages(diags), tt.wantContain) {
				t.Fatalf("expected diagnostic containing %q in %s, got %v",
					tt.wantContain, tt.file, diagMessages(diags))
			}
		})
	}
}

// --- Type-check error tests ---

func TestDiagnosticsTypeCheckErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		file        string
		wantContain string
		wantNot     string // substring that should NOT appear (for message cleaning)
		wantCount   int    // expected diagnostic count (0 = just check > 0)
	}{
		// Undeclared references
		{"undeclared variable", "undeclared_variable.cel", "undeclared reference to 'x'", "", 1},
		{"multiple undeclared", "multiple_undeclared.cel", "undeclared reference", "", 2},
		{"three undeclared", "three_undeclared.cel", "undeclared reference", "", 3},
		{"undeclared with select", "undeclared_select.cel", "undeclared reference to 'x'", "", 0},

		// Arithmetic type mismatches (cleaned operator names)
		{"int + string", "int_plus_string.cel", "found no matching overload for '+' applied to '(int, string)'", "'_+_'", 1},
		{"bool + int", "bool_plus_int.cel", "found no matching overload for '+'", "'_+_'", 0},
		{"string - string", "string_minus_string.cel", "found no matching overload for '-'", "'_-_'", 0},
		{"int / string", "int_divide_string.cel", "found no matching overload for '/'", "'_/_'", 0},
		{"int % string", "int_modulo_string.cel", "found no matching overload for '%'", "'_%_'", 0},
		{"string + int", "string_plus_int.cel", "found no matching overload for '+' applied to '(string, int)'", "'_+_'", 0},

		// Comparison type mismatches
		{"int > string", "int_gt_string.cel", "found no matching overload for '>'", "'_>_'", 0},
		{"int == string", "int_eq_string.cel", "found no matching overload for '=='", "'_==_'", 0},
		{"int != string", "int_neq_string.cel", "found no matching overload for '!='", "'_!=_'", 0},

		// Logical operator type errors
		{"int && bool", "int_and_bool.cel", "expected type 'bool' but found 'int'", "", 0},
		{"int || bool", "int_or_bool.cel", "expected type 'bool' but found 'int'", "", 0},
		{"!int", "negate_int.cel", "found no matching overload for '!' applied to '(int)'", "'!_'", 0},

		// Unary minus
		{"-string", "unary_minus_string.cel", "found no matching overload for '-' applied to '(string)'", "'-_'", 0},

		// Ternary — conditional has no display name in cel-go, so it stays as-is
		{"ternary branch mismatch", "ternary_mismatch.cel", "no matching overload", "", 0},

		// Field selection
		{"field select on string", "field_select_string.cel", "does not support field selection", "", 0},

		// Method wrong arg type
		{"startsWith int arg", "startswith_int_arg.cel", "found no matching overload for 'startsWith'", "", 0},
		{"contains int arg", "contains_int_arg.cel", "found no matching overload for 'contains'", "", 0},

		// Function wrong arg type
		{"size of int", "size_of_int.cel", "found no matching overload for 'size'", "", 0},
		{"int from bool", "int_from_bool.cel", "found no matching overload for 'int'", "", 0},

		// Function wrong number of args
		{"int with two args", "int_two_args.cel", "found no matching overload for 'int'", "", 0},

		// Membership operator
		{"in on non-container", "in_non_container.cel", "found no matching overload for 'in'", "'@in'", 0},

		// Index operator — index has no display name, stays as-is
		{"index on non-indexable", "index_non_indexable.cel", "no matching overload", "", 0},
		{"bool index on list", "bool_index_list.cel", "no matching overload", "", 0},
		{"bool index on map", "bool_index_map.cel", "no matching overload", "", 0},
		{"index on string", "index_string.cel", "no matching overload", "", 0},

		// Macro body type errors
		{"all non-bool body", "all_non_bool.cel", "expected type 'bool' but found 'int'", "", 0},
		{"exists non-bool body", "exists_non_bool.cel", "expected type 'bool' but found 'int'", "", 0},
		{"filter non-bool body", "filter_non_bool.cel", "no matching overload", "", 0},
		{"exists_one non-bool body", "exists_one_non_bool.cel", "no matching overload", "", 0},
		{"map body type error", "map_body_type_error.cel", "found no matching overload for '+'", "'_+_'", 0},

		// Nested errors
		{"nested list type error", "nested_list_type_error.cel", "found no matching overload for '+'", "'_+_'", 0},
		{"nested map type error", "nested_map_type_error.cel", "found no matching overload for '+'", "'_+_'", 0},

		// Chained method error
		{"chained method error", "chained_method_error.cel", "found no matching overload for 'startsWith'", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, uri := openDiagFile(t, tt.file)
			diags := pullDiagnostics(t, conn, uri)

			if len(diags) == 0 {
				t.Fatalf("expected type-check errors for %s, got none", tt.file)
			}
			for _, d := range diags {
				be.Equal(t, d.Severity, protocol.SeverityWarning)
				be.Equal(t, d.Source, "cells")
			}
			if tt.wantCount > 0 {
				be.Equal(t, len(diags), tt.wantCount)
			}
			msgs := diagMessages(diags)
			if !containsSubstring(msgs, tt.wantContain) {
				t.Fatalf("expected diagnostic containing %q in %s, got %v",
					tt.wantContain, tt.file, msgs)
			}
			if tt.wantNot != "" {
				for _, msg := range msgs {
					if strings.Contains(msg, tt.wantNot) {
						t.Fatalf("did not expect %q in message %q", tt.wantNot, msg)
					}
				}
			}
		})
	}
}

// --- Clean (no diagnostic) tests ---

func TestDiagnosticsClean(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file string
	}{
		{"empty", "empty.cel"},
		{"whitespace", "whitespace.cel"},
		{"integer arithmetic", "int_arithmetic.cel"},
		{"integer comparison", "int_comparison.cel"},
		{"bool logic", "bool_logic.cel"},
		{"bool negation", "bool_negation.cel"},
		{"string concatenation", "string_concat.cel"},
		{"string size", "string_size.cel"},
		{"string startsWith", "string_startswith.cel"},
		{"string endsWith", "string_endswith.cel"},
		{"string contains", "string_contains.cel"},
		{"string matches", "string_matches.cel"},
		{"function size", "function_size.cel"},
		{"list literal", "list_literal.cel"},
		{"map literal", "map_literal.cel"},
		{"list index", "list_index.cel"},
		{"map index", "map_index.cel"},
		{"list membership", "list_membership.cel"},
		{"map membership", "map_membership.cel"},
		{"ternary", "ternary.cel"},
		{"int conversion", "int_conversion.cel"},
		{"uint conversion", "uint_conversion.cel"},
		{"double conversion", "double_conversion.cel"},
		{"string conversion", "string_conversion.cel"},
		{"bool conversion", "bool_conversion.cel"},
		{"bytes conversion", "bytes_conversion.cel"},
		{"type function", "type_function.cel"},
		{"duration", "duration.cel"},
		{"timestamp", "timestamp.cel"},
		{"nested arithmetic", "nested_arithmetic.cel"},
		{"unary minus", "unary_minus.cel"},
		{"all macro", "all_macro.cel"},
		{"exists macro", "exists_macro.cel"},
		{"exists_one macro", "exists_one_macro.cel"},
		{"map macro", "map_macro.cel"},
		{"filter macro", "filter_macro.cel"},
		{"has macro", "has_macro.cel"},
		{"nested macros", "nested_macros.cel"},
		{"empty list", "empty_list.cel"},
		{"empty map", "empty_map.cel"},
		{"null literal", "null_literal.cel"},
		{"bytes literal", "bytes_literal.cel"},
		{"multiline valid", "multiline_valid.cel"},
		{"double literal", "double_literal.cel"},
		{"uint literal", "uint_literal.cel"},
		{"mixed list types", "mixed_list.cel"},
		{"mixed map values", "mixed_map_values.cel"},
		{"clean", "clean.cel"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, uri := openDiagFile(t, tt.file)
			diags := pullDiagnostics(t, conn, uri)

			if len(diags) > 0 {
				t.Fatalf("expected no diagnostics for %s, got %d: %v",
					tt.file, len(diags), diagMessages(diags))
			}
		})
	}
}

// --- Position tests ---

func TestDiagnosticsParseErrorPositions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		file     string
		wantLine uint32
		wantCol  uint32
	}{
		{"error at end of line", "unexpected_eof.cel", 0, 3},
		{"error at start", "unexpected_comma.cel", 0, 0},
		{"error on third line", "multiline_parse_error.cel", 2, 0},
		{"unclosed paren at end", "unclosed_paren.cel", 0, 6},
		{"unclosed paren multiline", "unclosed_paren_multiline.cel", 3, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, uri := openDiagFile(t, tt.file)
			diags := pullDiagnostics(t, conn, uri)

			be.True(t, len(diags) > 0)
			first := diags[0]
			be.Equal(t, first.Range.Start.Line, tt.wantLine)
			be.Equal(t, first.Range.Start.Character, tt.wantCol)
			be.Equal(t, first.Severity, protocol.SeverityError)
		})
	}
}

func TestDiagnosticsTypeCheckPositions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		file     string
		wantLine uint32
		wantCol  uint32
	}{
		{"undeclared at start", "undeclared_variable.cel", 0, 0},
		{"operator mismatch", "int_plus_string.cel", 0, 2},
		{"multiple undeclared first", "multiple_undeclared.cel", 0, 0},
		{"method arg error", "contains_int_arg.cel", 0, 16},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, uri := openDiagFile(t, tt.file)
			diags := pullDiagnostics(t, conn, uri)

			be.True(t, len(diags) > 0)
			first := diags[0]
			be.Equal(t, first.Range.Start.Line, tt.wantLine)
			be.Equal(t, first.Range.Start.Character, tt.wantCol)
			be.Equal(t, first.Severity, protocol.SeverityWarning)
		})
	}
}

// --- Range tests ---

func TestDiagnosticsRangeEndsAtEndOfLine(t *testing.T) {
	t.Parallel()

	conn, uri := openDiagFile(t, "int_plus_string.cel")
	diags := pullDiagnostics(t, conn, uri)

	be.True(t, len(diags) > 0)
	d := diags[0]
	be.Equal(t, d.Range.Start.Line, uint32(0))
	be.Equal(t, d.Range.End.Line, uint32(0))
	be.Equal(t, d.Range.End.Character, uint32(11))
}

func TestDiagnosticsRangeSameLine(t *testing.T) {
	t.Parallel()

	conn, uri := openDiagFile(t, "multiline_parse_error.cel")
	diags := pullDiagnostics(t, conn, uri)

	be.True(t, len(diags) > 0)
	d := diags[0]
	be.Equal(t, d.Range.Start.Line, d.Range.End.Line)
}

// --- Multiple diagnostics tests ---

func TestDiagnosticsMultipleUndeclared(t *testing.T) {
	t.Parallel()

	conn, uri := openDiagFile(t, "three_undeclared.cel")
	diags := pullDiagnostics(t, conn, uri)

	be.Equal(t, len(diags), 3)

	messages := diagMessages(diags)
	if !containsSubstring(messages, "'x'") {
		t.Fatalf("expected 'x' in %v", messages)
	}
	if !containsSubstring(messages, "'y'") {
		t.Fatalf("expected 'y' in %v", messages)
	}
	if !containsSubstring(messages, "'z'") {
		t.Fatalf("expected 'z' in %v", messages)
	}

	be.Equal(t, diags[0].Range.Start.Character, uint32(0)) // x
	be.Equal(t, diags[1].Range.Start.Character, uint32(4)) // y
	be.Equal(t, diags[2].Range.Start.Character, uint32(8)) // z
}

func TestDiagnosticsMultipleParseErrors(t *testing.T) {
	t.Parallel()

	conn, uri := openDiagFile(t, "unterminated_string.cel")
	diags := pullDiagnostics(t, conn, uri)

	be.True(t, len(diags) >= 2)
	for _, d := range diags {
		be.Equal(t, d.Severity, protocol.SeverityError)
	}
}

// --- On-change lifecycle tests ---

func TestDiagnosticsOnChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		startFile    string
		startSev     protocol.DiagnosticSeverity // 0 means expect no diagnostics
		changeText   string
		changeSev    protocol.DiagnosticSeverity // 0 means expect no diagnostics
		changeMsg    string                      // substring to check in changed diagnostics (if any)
	}{
		{
			name:       "clean to parse error to clean",
			startFile:  "clean.cel",
			startSev:   0,
			changeText: "1 +",
			changeSev:  protocol.SeverityError,
		},
		{
			name:       "clean to type error",
			startFile:  "clean.cel",
			startSev:   0,
			changeText: `1 + "hello"`,
			changeSev:  protocol.SeverityWarning,
			changeMsg:  "'+'",
		},
		{
			name:       "parse error to type error",
			startFile:  "unexpected_eof.cel",
			startSev:   protocol.SeverityError,
			changeText: `1 + "hello"`,
			changeSev:  protocol.SeverityWarning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, uri := openDiagFile(t, tt.startFile)
			diags := pullDiagnostics(t, conn, uri)

			if tt.startSev == 0 {
				be.Equal(t, len(diags), 0)
			} else {
				be.True(t, len(diags) > 0)
				be.Equal(t, diags[0].Severity, tt.startSev)
			}

			err := conn.Notify(t.Context(), "textDocument/didChange", protocol.DidChangeTextDocumentParams{
				TextDocument: protocol.VersionedTextDocumentIdentifier{
					TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri},
					Version:                2,
				},
				ContentChanges: []protocol.TextDocumentContentChangeEvent{
					{Value: protocol.TextDocumentContentChangeWholeDocument{Text: tt.changeText}},
				},
			})
			be.Err(t, err, nil)

			diags = pullDiagnostics(t, conn, uri)
			if tt.changeSev == 0 {
				be.Equal(t, len(diags), 0)
			} else {
				be.True(t, len(diags) > 0)
				be.Equal(t, diags[0].Severity, tt.changeSev)
				if tt.changeMsg != "" {
					be.True(t, strings.Contains(diags[0].Message, tt.changeMsg))
				}
			}
		})
	}
}

// --- Pull for unknown file ---

func TestDiagnosticsPullUnknownFile(t *testing.T) {
	t.Parallel()

	conn, _ := openDiagFile(t, "clean.cel")
	be.Equal(t, len(pullDiagnostics(t, conn, "file:///nonexistent.cel")), 0)
}

// --- Push diagnostics tests ---

// diagnosticCollector captures publishDiagnostics notifications from the server.
type diagnosticCollector struct {
	mu          sync.Mutex
	diagnostics []protocol.PublishDiagnosticsParams
	ch          chan struct{}
}

func newDiagnosticCollector() *diagnosticCollector {
	return &diagnosticCollector{ch: make(chan struct{}, 100)}
}

func (dc *diagnosticCollector) handler(_ context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (any, error) {
	if req.Method == "textDocument/publishDiagnostics" && req.Params != nil {
		var params protocol.PublishDiagnosticsParams
		if err := json.Unmarshal(*req.Params, &params); err == nil {
			dc.mu.Lock()
			dc.diagnostics = append(dc.diagnostics, params)
			dc.mu.Unlock()
			select {
			case dc.ch <- struct{}{}:
			default:
			}
		}
	}
	return nil, nil
}

func (dc *diagnosticCollector) waitForDiagnostics(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		dc.mu.Lock()
		got := len(dc.diagnostics)
		dc.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-dc.ch:
		case <-deadline:
			t.Fatalf("timed out waiting for %d diagnostics notifications, got %d", n, got)
		}
	}
}

func (dc *diagnosticCollector) latest() protocol.PublishDiagnosticsParams {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	return dc.diagnostics[len(dc.diagnostics)-1]
}

func TestDiagnosticsPushOnOpen(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	go func() {
		_ = lsp.ServeStream(ctx, serverConn)
	}()

	dc := newDiagnosticCollector()
	clientRPC := jsonrpc2.NewConn(ctx, clientConn, jsonrpc2.HandlerFunc(dc.handler))
	t.Cleanup(func() {
		_ = clientRPC.Close()
	})

	var initResult protocol.InitializeResult
	err := clientRPC.Call(ctx, "initialize", protocol.InitializeParams{}, &initResult)
	be.Err(t, err, nil)
	err = clientRPC.Notify(ctx, "initialized", protocol.InitializedParams{})
	be.Err(t, err, nil)

	testURI := protocol.DocumentURI("file:///test.cel")

	err = clientRPC.Notify(ctx, "textDocument/didOpen", protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        testURI,
			LanguageID: "cel",
			Version:    1,
			Text:       "1 +",
		},
	})
	be.Err(t, err, nil)

	dc.waitForDiagnostics(t, 1)
	params := dc.latest()
	be.Equal(t, string(params.URI), string(testURI))
	be.True(t, len(params.Diagnostics) > 0)
	be.Equal(t, params.Diagnostics[0].Severity, protocol.SeverityError)
}

func TestDiagnosticsPushOnChange(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	go func() {
		_ = lsp.ServeStream(ctx, serverConn)
	}()

	dc := newDiagnosticCollector()
	clientRPC := jsonrpc2.NewConn(ctx, clientConn, jsonrpc2.HandlerFunc(dc.handler))
	t.Cleanup(func() {
		_ = clientRPC.Close()
	})

	var initResult protocol.InitializeResult
	err := clientRPC.Call(ctx, "initialize", protocol.InitializeParams{}, &initResult)
	be.Err(t, err, nil)
	err = clientRPC.Notify(ctx, "initialized", protocol.InitializedParams{})
	be.Err(t, err, nil)

	testURI := protocol.DocumentURI("file:///test.cel")

	// Open with valid content.
	err = clientRPC.Notify(ctx, "textDocument/didOpen", protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        testURI,
			LanguageID: "cel",
			Version:    1,
			Text:       "1 + 2",
		},
	})
	be.Err(t, err, nil)

	dc.waitForDiagnostics(t, 1)
	be.Equal(t, len(dc.latest().Diagnostics), 0)

	// Change to invalid content.
	err = clientRPC.Notify(ctx, "textDocument/didChange", protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: testURI},
			Version:                2,
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			{Value: protocol.TextDocumentContentChangeWholeDocument{Text: "1 +"}},
		},
	})
	be.Err(t, err, nil)

	dc.waitForDiagnostics(t, 2)
	params := dc.latest()
	be.True(t, len(params.Diagnostics) > 0)
	be.Equal(t, params.Diagnostics[0].Severity, protocol.SeverityError)

	// Change back to valid.
	err = clientRPC.Notify(ctx, "textDocument/didChange", protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: testURI},
			Version:                3,
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			{Value: protocol.TextDocumentContentChangeWholeDocument{Text: "1 + 2"}},
		},
	})
	be.Err(t, err, nil)

	dc.waitForDiagnostics(t, 3)
	be.Equal(t, len(dc.latest().Diagnostics), 0)
}

// --- Comprehensive file test ---

func TestDiagnosticsComprehensive(t *testing.T) {
	t.Parallel()

	testPath, err := filepath.Abs("testdata/hover/comprehensive.cel")
	be.Err(t, err, nil)

	conn, uri := setupDiagServer(t, testPath)
	diags := pullDiagnostics(t, conn, uri)

	be.True(t, len(diags) > 0)
	for _, d := range diags {
		be.Equal(t, d.Severity, protocol.SeverityWarning)
		be.Equal(t, d.Source, "cells")
	}
}

// --- Server capabilities test ---

func TestDiagnosticsCapabilities(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	go func() {
		_ = lsp.ServeStream(ctx, serverConn)
	}()

	noop := jsonrpc2.HandlerFunc(func(_ context.Context, _ *jsonrpc2.Conn, _ *jsonrpc2.Request) (any, error) {
		return nil, nil
	})
	clientRPC := jsonrpc2.NewConn(ctx, clientConn, noop)
	t.Cleanup(func() {
		_ = clientRPC.Close()
	})

	var result protocol.InitializeResult
	err := clientRPC.Call(ctx, "initialize", protocol.InitializeParams{}, &result)
	be.Err(t, err, nil)

	// Push diagnostics only — DiagnosticProvider is not advertised to avoid
	// clients using both push and pull simultaneously.
	be.True(t, result.Capabilities.DiagnosticProvider == nil)
}
