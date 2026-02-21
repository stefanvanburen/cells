package lsp_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nalgeon/be"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

// setupCompletionServer initializes an LSP server and opens celFile for completion testing.
// Returns the client connection, document URI, and file content.
func setupCompletionServer(t *testing.T, celFile string) (*jsonrpc2.Conn, protocol.DocumentURI, string) {
	t.Helper()
	ctx := t.Context()

	testPath, err := filepath.Abs(celFile)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}

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

	testURI := protocol.URIFromPath(testPath)

	var initResult protocol.InitializeResult
	err = clientRPC.Call(ctx, "initialize", protocol.InitializeParams{}, &initResult)
	be.Err(t, err, nil)

	err = clientRPC.Notify(ctx, "initialized", protocol.InitializedParams{})
	be.Err(t, err, nil)

	content, err := os.ReadFile(testPath)
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

	return clientRPC, testURI, string(content)
}

// dotPosition returns the position right after the last dot in the content.
func dotPosition(content string) protocol.Position {
	pos := protocol.Position{}
	line := uint32(0)
	col := uint32(0)
	for _, ch := range content {
		if ch == '.' {
			pos = protocol.Position{Line: line, Character: col + 1}
		}
		if ch == '\n' {
			line++
			col = 0
		} else {
			col++
		}
	}
	return pos
}

// endOfContent returns the position at the end of the last non-empty line.
func endOfContent(content string) protocol.Position {
	content = strings.TrimRight(content, "\n")
	line := uint32(0)
	col := uint32(0)
	for _, ch := range content {
		if ch == '\n' {
			line++
			col = 0
		} else {
			col++
		}
	}
	return protocol.Position{Line: line, Character: col}
}

// requestCompletion sends a completion request at the given position.
func requestCompletion(t *testing.T, conn *jsonrpc2.Conn, uri protocol.DocumentURI, pos protocol.Position, triggerKind protocol.CompletionTriggerKind, triggerChar string) *protocol.CompletionList {
	t.Helper()
	var result protocol.CompletionList
	err := conn.Call(t.Context(), "textDocument/completion", protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			Position:     pos,
		},
		Context: protocol.CompletionContext{
			TriggerKind:      triggerKind,
			TriggerCharacter: triggerChar,
		},
	}, &result)
	if err != nil {
		t.Fatalf("textDocument/completion call failed: %v", err)
	}
	return &result
}

// requestDotCompletion sends a dot-triggered completion request.
func requestDotCompletion(t *testing.T, celFile string) *protocol.CompletionList {
	t.Helper()
	conn, uri, content := setupCompletionServer(t, celFile)
	return requestCompletion(t, conn, uri, dotPosition(content), protocol.TriggerCharacter, ".")
}

// requestInvokedCompletion sends an invoked completion request at the start of the file.
func requestInvokedCompletion(t *testing.T, celFile string) *protocol.CompletionList {
	t.Helper()
	conn, uri, _ := setupCompletionServer(t, celFile)
	return requestCompletion(t, conn, uri, protocol.Position{Line: 0, Character: 0}, protocol.Invoked, "")
}

// requestInvokedAtEnd sends an invoked completion request at the end of the content.
func requestInvokedAtEnd(t *testing.T, celFile string) *protocol.CompletionList {
	t.Helper()
	conn, uri, content := setupCompletionServer(t, celFile)
	return requestCompletion(t, conn, uri, endOfContent(content), protocol.Invoked, "")
}

// completionLabels returns the sorted labels from a completion list.
func completionLabels(items []protocol.CompletionItem) []string {
	labels := make([]string, len(items))
	for i, item := range items {
		labels[i] = item.Label
	}
	sort.Strings(labels)
	return labels
}

// findCompletionItem searches for a completion item by label.
func findCompletionItem(items []protocol.CompletionItem, label string) *protocol.CompletionItem {
	for i := range items {
		if items[i].Label == label {
			return &items[i]
		}
	}
	return nil
}

func containsLabel(items []protocol.CompletionItem, label string) bool {
	return findCompletionItem(items, label) != nil
}

// --- Dot completion: type-aware member filtering ---

func TestCompletionDot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		file       string
		wantLabels []string
	}{
		{
			name:       "string",
			file:       "testdata/completion/string_receiver.cel",
			wantLabels: []string{"contains", "endsWith", "matches", "size", "startsWith"},
		},
		{
			name:       "list",
			file:       "testdata/completion/list_receiver.cel",
			wantLabels: []string{"size"},
		},
		{
			name:       "map",
			file:       "testdata/completion/map_receiver.cel",
			wantLabels: []string{"size"},
		},
		{
			name:       "bytes",
			file:       "testdata/completion/bytes_receiver.cel",
			wantLabels: []string{"size"},
		},
		{
			name: "duration",
			file: "testdata/completion/duration_receiver.cel",
			wantLabels: []string{
				"getHours", "getMilliseconds", "getMinutes", "getSeconds",
			},
		},
		{
			name: "timestamp",
			file: "testdata/completion/timestamp_receiver.cel",
			wantLabels: []string{
				"getDate", "getDayOfMonth", "getDayOfWeek", "getDayOfYear",
				"getFullYear", "getHours", "getMilliseconds", "getMinutes",
				"getMonth", "getSeconds",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := requestDotCompletion(t, tt.file)

			got := completionLabels(result.Items)
			want := make([]string, len(tt.wantLabels))
			copy(want, tt.wantLabels)
			sort.Strings(want)

			be.Equal(t, len(got), len(want))
			for i := range got {
				be.Equal(t, got[i], want[i])
			}

			for _, item := range result.Items {
				be.Equal(t, item.Kind, protocol.MethodCompletion)
			}
		})
	}
}

func TestCompletionDotUnknownReceiver(t *testing.T) {
	t.Parallel()
	result := requestDotCompletion(t, "testdata/completion/unknown_receiver.cel")

	labels := completionLabels(result.Items)
	for _, want := range []string{"contains", "startsWith", "getFullYear", "size"} {
		if !containsLabel(result.Items, want) {
			t.Errorf("expected %q in unknown-receiver fallback, got %v", want, labels)
		}
	}
}

// TestCompletionInvokedAtDot verifies that manually-invoked completion at a
// dot position returns member completions, not globals.
func TestCompletionInvokedAtDot(t *testing.T) {
	t.Parallel()
	conn, uri, content := setupCompletionServer(t, "testdata/completion/string_receiver.cel")
	result := requestCompletion(t, conn, uri, dotPosition(content), protocol.Invoked, "")

	got := completionLabels(result.Items)
	want := []string{"contains", "endsWith", "matches", "size", "startsWith"}

	be.Equal(t, len(got), len(want))
	for i := range got {
		be.Equal(t, got[i], want[i])
	}

	for _, absent := range []string{"timestamp", "int", "duration", "dyn", "true"} {
		if containsLabel(result.Items, absent) {
			t.Errorf("did not expect %q in invoked-at-dot completions", absent)
		}
	}
}

// --- Invoked completion ---

func TestCompletionInvoked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		label string
		kind  protocol.CompletionItemKind
	}{
		{"int", protocol.FunctionCompletion},
		{"uint", protocol.FunctionCompletion},
		{"double", protocol.FunctionCompletion},
		{"string", protocol.FunctionCompletion},
		{"bool", protocol.FunctionCompletion},
		{"bytes", protocol.FunctionCompletion},
		{"duration", protocol.FunctionCompletion},
		{"timestamp", protocol.FunctionCompletion},
		{"type", protocol.FunctionCompletion},
		{"dyn", protocol.FunctionCompletion},
		{"size", protocol.FunctionCompletion},
		{"matches", protocol.FunctionCompletion},
		{"has", protocol.FunctionCompletion},
		{"all", protocol.FunctionCompletion},
		{"exists", protocol.FunctionCompletion},
		{"exists_one", protocol.FunctionCompletion},
		{"map", protocol.FunctionCompletion},
		{"filter", protocol.FunctionCompletion},
		{"true", protocol.KeywordCompletion},
		{"false", protocol.KeywordCompletion},
		{"null", protocol.KeywordCompletion},
	}

	result := requestInvokedCompletion(t, "testdata/completion/invoked.cel")

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			item := findCompletionItem(result.Items, tt.label)
			if item == nil {
				t.Fatalf("expected %q in invoked completions", tt.label)
			}
			be.Equal(t, item.Kind, tt.kind)
		})
	}

	for _, absent := range []string{"contains", "startsWith", "endsWith", "getFullYear", "getMonth", "getHours"} {
		if containsLabel(result.Items, absent) {
			t.Errorf("did not expect member-only %q in invoked completions", absent)
		}
	}
}

// --- Operator-context completion ---

func TestCompletionAfterOperator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		file        string
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name: "int plus",
			file: "testdata/completion/after_plus_int.cel",
			// 1 + : expected right type = int
			wantPresent: []string{"int", "size"},
			wantAbsent:  []string{"string", "timestamp", "duration", "bool", "bytes", "true", "false", "null"},
		},
		{
			name: "string plus",
			file: "testdata/completion/after_plus_string.cel",
			// "hi" + : expected right type = string
			wantPresent: []string{"string"},
			wantAbsent:  []string{"int", "size", "timestamp", "bool", "true", "false", "null"},
		},
		{
			name: "bool and",
			file: "testdata/completion/after_and.cel",
			// true && : expected right type = bool
			wantPresent: []string{"bool", "true", "false"},
			wantAbsent:  []string{"int", "size", "string", "timestamp", "duration", "null"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := requestInvokedAtEnd(t, tt.file)

			labels := completionLabels(result.Items)
			for _, want := range tt.wantPresent {
				if !containsLabel(result.Items, want) {
					t.Errorf("expected %q in completions, got %v", want, labels)
				}
			}
			for _, absent := range tt.wantAbsent {
				if containsLabel(result.Items, absent) {
					t.Errorf("did not expect %q in completions after operator", absent)
				}
			}
		})
	}
}

// --- Item property details ---

func TestCompletionItemProperties(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		file           string
		triggerDot     bool
		label          string
		wantKind       protocol.CompletionItemKind
		wantSnippet    bool
		wantInsertText string
		wantDoc        bool
	}{
		{
			name:           "global function int",
			file:           "testdata/completion/invoked.cel",
			label:          "int",
			wantKind:       protocol.FunctionCompletion,
			wantSnippet:    true,
			wantInsertText: "int($1)",
			wantDoc:        true,
		},
		{
			name:           "member method contains",
			file:           "testdata/completion/string_receiver.cel",
			triggerDot:     true,
			label:          "contains",
			wantKind:       protocol.MethodCompletion,
			wantSnippet:    true,
			wantInsertText: "contains($1)",
			wantDoc:        true,
		},
		{
			name:           "member method getFullYear",
			file:           "testdata/completion/timestamp_receiver.cel",
			triggerDot:     true,
			label:          "getFullYear",
			wantKind:       protocol.MethodCompletion,
			wantSnippet:    true,
			wantInsertText: "getFullYear($1)",
			wantDoc:        true,
		},
		{
			name:           "macro exists",
			file:           "testdata/completion/invoked.cel",
			label:          "exists",
			wantKind:       protocol.FunctionCompletion,
			wantSnippet:    true,
			wantInsertText: "exists($1)",
		},
		{
			name:     "keyword true",
			file:     "testdata/completion/invoked.cel",
			label:    "true",
			wantKind: protocol.KeywordCompletion,
		},
		{
			name:     "keyword false",
			file:     "testdata/completion/invoked.cel",
			label:    "false",
			wantKind: protocol.KeywordCompletion,
		},
		{
			name:     "keyword null",
			file:     "testdata/completion/invoked.cel",
			label:    "null",
			wantKind: protocol.KeywordCompletion,
		},
		{
			name:           "size as member on string",
			file:           "testdata/completion/string_receiver.cel",
			triggerDot:     true,
			label:          "size",
			wantKind:       protocol.MethodCompletion,
			wantSnippet:    true,
			wantInsertText: "size($1)",
			wantDoc:        true,
		},
		{
			name:           "size as global",
			file:           "testdata/completion/invoked.cel",
			label:          "size",
			wantKind:       protocol.FunctionCompletion,
			wantSnippet:    true,
			wantInsertText: "size($1)",
			wantDoc:        true,
		},
		{
			name:           "matches as member on string",
			file:           "testdata/completion/string_receiver.cel",
			triggerDot:     true,
			label:          "matches",
			wantKind:       protocol.MethodCompletion,
			wantSnippet:    true,
			wantInsertText: "matches($1)",
			wantDoc:        true,
		},
		{
			name:           "type conversion duration",
			file:           "testdata/completion/invoked.cel",
			label:          "duration",
			wantKind:       protocol.FunctionCompletion,
			wantSnippet:    true,
			wantInsertText: "duration($1)",
			wantDoc:        true,
		},
		{
			name:           "type conversion timestamp",
			file:           "testdata/completion/invoked.cel",
			label:          "timestamp",
			wantKind:       protocol.FunctionCompletion,
			wantSnippet:    true,
			wantInsertText: "timestamp($1)",
			wantDoc:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var result *protocol.CompletionList
			if tt.triggerDot {
				result = requestDotCompletion(t, tt.file)
			} else {
				result = requestInvokedCompletion(t, tt.file)
			}

			item := findCompletionItem(result.Items, tt.label)
			if item == nil {
				t.Fatalf("expected %q in completions", tt.label)
			}

			be.Equal(t, item.Kind, tt.wantKind)
			be.Equal(t, item.InsertText, tt.wantInsertText)

			if tt.wantSnippet {
				if item.InsertTextFormat == nil {
					t.Fatal("expected snippet insert text format")
				}
				be.Equal(t, *item.InsertTextFormat, protocol.SnippetTextFormat)
			} else {
				if item.InsertTextFormat != nil {
					t.Fatalf("did not expect insert text format for %q", tt.label)
				}
			}

			if tt.wantDoc {
				if item.Documentation == nil {
					t.Fatalf("expected documentation for %q", tt.label)
				}
			} else {
				if item.Documentation != nil {
					t.Fatalf("did not expect documentation for %q", tt.label)
				}
			}
		})
	}
}

// --- No duplicate macros ---

func TestCompletionNoDuplicateMacros(t *testing.T) {
	t.Parallel()

	result := requestInvokedCompletion(t, "testdata/completion/invoked.cel")

	count := 0
	for _, item := range result.Items {
		if item.Label == "map" {
			count++
		}
	}
	be.Equal(t, count, 1)
}

// --- No operators ---

func TestCompletionNoOperators(t *testing.T) {
	t.Parallel()

	operators := []string{"_+_", "_-_", "_*_", "_/_", "_%_", "_&&_", "_||_", "_==_", "_!=_", "_>_", "_>=_", "_<_", "_<=_", "@in", "_[_]", "!_", "-_"}

	t.Run("invoked", func(t *testing.T) {
		t.Parallel()
		result := requestInvokedCompletion(t, "testdata/completion/invoked.cel")
		for _, op := range operators {
			if containsLabel(result.Items, op) {
				t.Errorf("did not expect operator %q in invoked completions", op)
			}
		}
	})

	t.Run("dot", func(t *testing.T) {
		t.Parallel()
		result := requestDotCompletion(t, "testdata/completion/string_receiver.cel")
		for _, op := range operators {
			if containsLabel(result.Items, op) {
				t.Errorf("did not expect operator %q in dot completions", op)
			}
		}
	})
}

// --- Capabilities ---

func TestCompletionCapabilities(t *testing.T) {
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

	be.True(t, result.Capabilities.CompletionProvider != nil)
	be.True(t, len(result.Capabilities.CompletionProvider.TriggerCharacters) > 0)
	be.Equal(t, result.Capabilities.CompletionProvider.TriggerCharacters[0], ".")
}
