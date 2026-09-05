package lsp_test

import (
	"os"
	"sort"
	"strings"
	"testing"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	lspuri "go.lsp.dev/uri"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/ok"
)

// setupCompletionServer initializes an LSP server and opens celFile for completion testing.
// Returns the client connection, document URI, and file content.
func setupCompletionServer(t *testing.T, celFile string) (jsonrpc2.Conn, lspuri.URI, string) {
	t.Helper()

	testPath := getAbsPath(t, celFile)
	clientConn, testURI := setupLSPServer(t, testPath)

	content, err := os.ReadFile(testPath)
	ok.MustNoError(t, err)

	return clientConn, testURI, string(content)
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
func requestCompletion(t *testing.T, conn jsonrpc2.Conn, uri lspuri.URI, pos protocol.Position, triggerKind protocol.CompletionTriggerKind, triggerChar string) *protocol.CompletionList {
	t.Helper()
	var result protocol.CompletionList
	_, err := conn.Call(t.Context(), "textDocument/completion", protocol.CompletionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     pos,
		Context: protocol.CompletionContext{
			TriggerKind:      triggerKind,
			TriggerCharacter: &triggerChar,
		},
	}, &result)
	ok.MustNoError(t, err)
	return &result
}

// requestDotCompletion sends a dot-triggered completion request.
func requestDotCompletion(t *testing.T, celFile string) *protocol.CompletionList {
	t.Helper()
	conn, uri, content := setupCompletionServer(t, celFile)
	return requestCompletion(t, conn, uri, dotPosition(content), protocol.CompletionTriggerKindTriggerCharacter, ".")
}

// requestInvokedCompletion sends an invoked completion request at the start of the file.
func requestInvokedCompletion(t *testing.T, celFile string) *protocol.CompletionList {
	t.Helper()
	conn, uri, _ := setupCompletionServer(t, celFile)
	return requestCompletion(t, conn, uri, protocol.Position{Line: 0, Character: 0}, protocol.CompletionTriggerKindInvoked, "")
}

// requestInvokedAtEnd sends an invoked completion request at the end of the content.
func requestInvokedAtEnd(t *testing.T, celFile string) *protocol.CompletionList {
	t.Helper()
	conn, uri, content := setupCompletionServer(t, celFile)
	return requestCompletion(t, conn, uri, endOfContent(content), protocol.CompletionTriggerKindInvoked, "")
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

			if !ok.Equal(t, len(got), len(want)) {
				return
			}
			for i := range got {
				ok.Equal(t, got[i], want[i])
			}

			for _, item := range result.Items {
				ok.Equal(t, item.Kind, protocol.CompletionItemKindMethod)
			}
		})
	}
}

func TestCompletionDotUnknownReceiver(t *testing.T) {
	t.Parallel()
	result := requestDotCompletion(t, "testdata/completion/unknown_receiver.cel")

	for _, want := range []string{"contains", "startsWith", "getFullYear", "size"} {
		ok.True(t, containsLabel(result.Items, want))
	}
}

// TestCompletionInvokedAtDot verifies that manually-invoked completion at a
// dot position returns member completions, not globals.
func TestCompletionInvokedAtDot(t *testing.T) {
	t.Parallel()
	conn, uri, content := setupCompletionServer(t, "testdata/completion/string_receiver.cel")
	result := requestCompletion(t, conn, uri, dotPosition(content), protocol.CompletionTriggerKindInvoked, "")

	got := completionLabels(result.Items)
	want := []string{"contains", "endsWith", "matches", "size", "startsWith"}

	if !ok.Equal(t, len(got), len(want)) {
		return
	}
	for i := range got {
		ok.Equal(t, got[i], want[i])
	}

	for _, absent := range []string{"timestamp", "int", "duration", "dyn", "true"} {
		ok.True(t, !containsLabel(result.Items, absent))
	}
}

// --- Invoked completion ---

func TestCompletionInvoked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		label string
		kind  protocol.CompletionItemKind
	}{
		{"int", protocol.CompletionItemKindFunction},
		{"uint", protocol.CompletionItemKindFunction},
		{"double", protocol.CompletionItemKindFunction},
		{"string", protocol.CompletionItemKindFunction},
		{"bool", protocol.CompletionItemKindFunction},
		{"bytes", protocol.CompletionItemKindFunction},
		{"duration", protocol.CompletionItemKindFunction},
		{"timestamp", protocol.CompletionItemKindFunction},
		{"type", protocol.CompletionItemKindFunction},
		{"dyn", protocol.CompletionItemKindFunction},
		{"size", protocol.CompletionItemKindFunction},
		{"matches", protocol.CompletionItemKindFunction},
		{"has", protocol.CompletionItemKindFunction},
		{"all", protocol.CompletionItemKindFunction},
		{"exists", protocol.CompletionItemKindFunction},
		{"exists_one", protocol.CompletionItemKindFunction},
		{"map", protocol.CompletionItemKindFunction},
		{"filter", protocol.CompletionItemKindFunction},
		{"true", protocol.CompletionItemKindKeyword},
		{"false", protocol.CompletionItemKindKeyword},
		{"null", protocol.CompletionItemKindKeyword},
	}

	result := requestInvokedCompletion(t, "testdata/completion/invoked.cel")

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			item := findCompletionItem(result.Items, tt.label)
			ok.True(t, item != nil)
			ok.Equal(t, item.Kind, tt.kind)
		})
	}

	for _, absent := range []string{"contains", "startsWith", "endsWith", "getFullYear", "getMonth", "getHours"} {
		ok.True(t, !containsLabel(result.Items, absent))
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

			for _, want := range tt.wantPresent {
				ok.True(t, containsLabel(result.Items, want))
			}
			for _, absent := range tt.wantAbsent {
				ok.True(t, !containsLabel(result.Items, absent))
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
			wantKind:       protocol.CompletionItemKindFunction,
			wantSnippet:    true,
			wantInsertText: "int($1)",
			wantDoc:        true,
		},
		{
			name:           "member method contains",
			file:           "testdata/completion/string_receiver.cel",
			triggerDot:     true,
			label:          "contains",
			wantKind:       protocol.CompletionItemKindMethod,
			wantSnippet:    true,
			wantInsertText: "contains($1)",
			wantDoc:        true,
		},
		{
			name:           "member method getFullYear",
			file:           "testdata/completion/timestamp_receiver.cel",
			triggerDot:     true,
			label:          "getFullYear",
			wantKind:       protocol.CompletionItemKindMethod,
			wantSnippet:    true,
			wantInsertText: "getFullYear($1)",
			wantDoc:        true,
		},
		{
			name:           "macro exists",
			file:           "testdata/completion/invoked.cel",
			label:          "exists",
			wantKind:       protocol.CompletionItemKindFunction,
			wantSnippet:    true,
			wantInsertText: "exists($1)",
		},
		{
			name:     "keyword true",
			file:     "testdata/completion/invoked.cel",
			label:    "true",
			wantKind: protocol.CompletionItemKindKeyword,
		},
		{
			name:     "keyword false",
			file:     "testdata/completion/invoked.cel",
			label:    "false",
			wantKind: protocol.CompletionItemKindKeyword,
		},
		{
			name:     "keyword null",
			file:     "testdata/completion/invoked.cel",
			label:    "null",
			wantKind: protocol.CompletionItemKindKeyword,
		},
		{
			name:           "size as member on string",
			file:           "testdata/completion/string_receiver.cel",
			triggerDot:     true,
			label:          "size",
			wantKind:       protocol.CompletionItemKindMethod,
			wantSnippet:    true,
			wantInsertText: "size($1)",
			wantDoc:        true,
		},
		{
			name:           "size as global",
			file:           "testdata/completion/invoked.cel",
			label:          "size",
			wantKind:       protocol.CompletionItemKindFunction,
			wantSnippet:    true,
			wantInsertText: "size($1)",
			wantDoc:        true,
		},
		{
			name:           "matches as member on string",
			file:           "testdata/completion/string_receiver.cel",
			triggerDot:     true,
			label:          "matches",
			wantKind:       protocol.CompletionItemKindMethod,
			wantSnippet:    true,
			wantInsertText: "matches($1)",
			wantDoc:        true,
		},
		{
			name:           "type conversion duration",
			file:           "testdata/completion/invoked.cel",
			label:          "duration",
			wantKind:       protocol.CompletionItemKindFunction,
			wantSnippet:    true,
			wantInsertText: "duration($1)",
			wantDoc:        true,
		},
		{
			name:           "type conversion timestamp",
			file:           "testdata/completion/invoked.cel",
			label:          "timestamp",
			wantKind:       protocol.CompletionItemKindFunction,
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
			ok.True(t, item != nil)

			ok.Equal(t, item.Kind, tt.wantKind)
			insertText, _ := item.InsertText.Get()
			ok.Equal(t, insertText, tt.wantInsertText)

			if tt.wantSnippet {
				ok.Equal(t, item.InsertTextFormat, protocol.InsertTextFormatSnippet)
			} else {
				ok.Equal(t, item.InsertTextFormat, protocol.InsertTextFormat(0))
			}

			if tt.wantDoc {
				ok.True(t, item.Documentation != nil)
			} else {
				ok.True(t, item.Documentation == nil)
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
	ok.Equal(t, count, 1)
}

// --- No operators ---

func TestCompletionNoOperators(t *testing.T) {
	t.Parallel()

	operators := []string{"_+_", "_-_", "_*_", "_/_", "_%_", "_&&_", "_||_", "_==_", "_!=_", "_>_", "_>=_", "_<_", "_<=_", "@in", "_[_]", "!_", "-_"}

	t.Run("invoked", func(t *testing.T) {
		t.Parallel()
		result := requestInvokedCompletion(t, "testdata/completion/invoked.cel")
		for _, op := range operators {
			ok.True(t, !containsLabel(result.Items, op))
		}
	})

	t.Run("dot", func(t *testing.T) {
		t.Parallel()
		result := requestDotCompletion(t, "testdata/completion/string_receiver.cel")
		for _, op := range operators {
			ok.True(t, !containsLabel(result.Items, op))
		}
	})
}

// --- Capabilities ---

func TestCompletionCapabilities(t *testing.T) {
	t.Parallel()

	clientRPC := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})

	var result protocol.InitializeResult
	_, err := clientRPC.Call(t.Context(), "initialize", protocol.InitializeParams{}, &result)
	ok.MustNoError(t, err)

	ok.True(t, result.Capabilities.CompletionProvider != nil)
	ok.True(t, len(result.Capabilities.CompletionProvider.TriggerCharacters) > 0)
	ok.Equal(t, result.Capabilities.CompletionProvider.TriggerCharacters[0], ".")
}
