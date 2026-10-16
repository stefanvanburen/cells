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
			name:       "macro variable",
			file:       "testdata/completion/macro_variable_receiver.cel",
			wantLabels: []string{"contains", "endsWith", "matches", "size", "startsWith"},
		},
		{
			name:       "nested macro variable",
			file:       "testdata/completion/nested_macro_variable_receiver.cel",
			wantLabels: []string{"contains", "endsWith", "matches", "size", "startsWith"},
		},
		{
			name:       "macro variable in conditional",
			file:       "testdata/completion/conditional_receiver.cel",
			wantLabels: []string{"contains", "endsWith", "matches", "size", "startsWith"},
		},
		{
			name:       "parenthesized",
			file:       "testdata/completion/parenthesized_receiver.cel",
			wantLabels: []string{"contains", "endsWith", "matches", "size", "startsWith"},
		},
		// The source around the macro could not type-check with the macro
		// closed, since && takes no list.
		{
			name:       "macro variable in an operand",
			file:       "testdata/completion/macro_in_operand_receiver.cel",
			wantLabels: []string{"contains", "endsWith", "matches", "size", "startsWith"},
		},
		// The source around the receiver could not type-check, though the
		// receiver can.
		{
			name:       "operand of a mismatched operator",
			file:       "testdata/completion/operand_of_mismatched_operator_receiver.cel",
			wantLabels: []string{"contains", "endsWith", "matches", "size", "startsWith"},
		},
		{
			name:       "negated",
			file:       "testdata/completion/negated_receiver.cel",
			wantLabels: []string{"getHours", "getMilliseconds", "getMinutes", "getSeconds"},
		},
		{
			name:       "macro result",
			file:       "testdata/completion/macro_result_receiver.cel",
			wantLabels: []string{"size"},
		},
		{
			name:       "right operand",
			file:       "testdata/completion/operand_receiver.cel",
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

func TestCompletionAfterToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		file        string
		wantPresent []string
		wantAbsent  []string
		// wantNone is set where nothing cells offers can come next.
		wantNone bool
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
		{
			name:        "string plus in macro",
			file:        "testdata/completion/after_plus_in_macro.cel",
			wantPresent: []string{"string", "s"},
			wantAbsent:  []string{"int", "size", "timestamp", "true"},
		},
		{
			name:        "string plus in macro in an operand",
			file:        "testdata/completion/after_plus_in_macro_operand.cel",
			wantPresent: []string{"string", "s"},
			wantAbsent:  []string{"int", "size", "timestamp", "true"},
		},
		{
			// + binds tighter than <, so its left operand is 2.
			name:        "plus in comparison",
			file:        "testdata/completion/after_plus_in_comparison.cel",
			wantPresent: []string{"int", "size"},
			wantAbsent:  []string{"string", "timestamp", "true"},
		},
		{
			// && binds looser than ==, so its left operand is the comparison.
			name:        "and after comparison",
			file:        "testdata/completion/after_and_comparison.cel",
			wantPresent: []string{"bool", "true"},
			wantAbsent:  []string{"int", "size", "string"},
		},
		{
			name:        "in",
			file:        "testdata/completion/after_in.cel",
			wantPresent: []string{"size", "true"},
		},
		{
			name:        "name being typed",
			file:        "testdata/completion/partial_name.cel",
			wantPresent: []string{"timestamp"},
		},
		{
			name:        "method being typed",
			file:        "testdata/completion/partial_method.cel",
			wantPresent: []string{"contains", "startsWith"},
			wantAbsent:  []string{"timestamp", "int", "getFullYear"},
		},
		// A macro's variables complete within it, and only there.
		{
			name:        "loop variable being typed",
			file:        "testdata/completion/loop_var_being_typed.cel",
			wantPresent: []string{"x", "size"},
		},
		{
			name:        "in macro body",
			file:        "testdata/completion/in_macro_body.cel",
			wantPresent: []string{"x"},
		},
		{
			name:        "in nested macro body",
			file:        "testdata/completion/in_nested_macro_body.cel",
			wantPresent: []string{"row", "x"},
		},
		{
			name:        "in conditional",
			file:        "testdata/completion/in_conditional.cel",
			wantPresent: []string{"x"},
		},
		{
			name:        "after macro",
			file:        "testdata/completion/after_macro.cel",
			wantPresent: []string{"y"},
			wantAbsent:  []string{"x"},
		},
		// Nothing completes inside a literal or a comment.
		{name: "in string", file: "testdata/completion/in_string.cel", wantNone: true},
		{name: "in string argument", file: "testdata/completion/in_string_argument.cel", wantNone: true},
		{name: "in bytes", file: "testdata/completion/in_bytes.cel", wantNone: true},
		{name: "in comment", file: "testdata/completion/in_comment.cel", wantNone: true},
		// After a complete operand only an operator can come.
		{name: "string", file: "testdata/completion/after_string.cel", wantNone: true},
		{name: "call", file: "testdata/completion/after_call.cel", wantNone: true},
		{name: "list", file: "testdata/completion/after_list.cel", wantNone: true},
		{name: "name and space", file: "testdata/completion/after_name.cel", wantNone: true},
		{name: "keyword and space", file: "testdata/completion/after_keyword.cel", wantNone: true},
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
			if tt.wantNone {
				ok.Equal(t, completionLabels(result.Items), []string{})
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
		// wantAfterLabel and wantLabelDesc are the labelDetails' detail,
		// which the client shows directly after the label, and description,
		// which it shows apart. Both empty means the item has no labelDetails.
		wantAfterLabel string
		wantLabelDesc  string
	}{
		{
			name:           "global function int",
			file:           "testdata/completion/invoked.cel",
			label:          "int",
			wantKind:       protocol.CompletionItemKindFunction,
			wantSnippet:    true,
			wantInsertText: "int($1)",
			wantDoc:        true,
			wantAfterLabel: "(int) -> int, +5 more",
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
			wantAfterLabel: "(string) -> bool",
			wantLabelDesc:  "string",
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
			wantAfterLabel: "() -> int, +1 more",
			wantLabelDesc:  "google.protobuf.Timestamp",
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
			name:          "keyword true",
			file:          "testdata/completion/invoked.cel",
			label:         "true",
			wantKind:      protocol.CompletionItemKindKeyword,
			wantLabelDesc: "bool",
		},
		{
			name:          "keyword false",
			file:          "testdata/completion/invoked.cel",
			label:         "false",
			wantKind:      protocol.CompletionItemKindKeyword,
			wantLabelDesc: "bool",
		},
		{
			name:          "keyword null",
			file:          "testdata/completion/invoked.cel",
			label:         "null",
			wantKind:      protocol.CompletionItemKindKeyword,
			wantLabelDesc: "null_type",
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
			wantAfterLabel: "() -> int",
			wantLabelDesc:  "string",
		},
		{
			name:           "size as global",
			file:           "testdata/completion/invoked.cel",
			label:          "size",
			wantKind:       protocol.CompletionItemKindFunction,
			wantSnippet:    true,
			wantInsertText: "size($1)",
			wantDoc:        true,
			wantAfterLabel: "(bytes) -> int, +3 more",
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
			wantAfterLabel: "(string) -> bool",
			wantLabelDesc:  "string",
		},
		{
			name:           "type conversion duration",
			file:           "testdata/completion/invoked.cel",
			label:          "duration",
			wantKind:       protocol.CompletionItemKindFunction,
			wantSnippet:    true,
			wantInsertText: "duration($1)",
			wantDoc:        true,
			wantAfterLabel: "(google.protobuf.Duration) -> google.protobuf.Duration, +1 more",
		},
		{
			name:           "type conversion timestamp",
			file:           "testdata/completion/invoked.cel",
			label:          "timestamp",
			wantKind:       protocol.CompletionItemKindFunction,
			wantSnippet:    true,
			wantInsertText: "timestamp($1)",
			wantDoc:        true,
			wantAfterLabel: "(google.protobuf.Timestamp) -> google.protobuf.Timestamp, +2 more",
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

			var afterLabel, labelDesc string
			if ld := item.LabelDetails; ld != nil {
				if ld.Detail != nil {
					afterLabel = *ld.Detail
				}
				if ld.Description != nil {
					labelDesc = *ld.Description
				}
			}
			ok.Equal(t, afterLabel, tt.wantAfterLabel)
			ok.Equal(t, labelDesc, tt.wantLabelDesc)
			ok.Equal(t, item.LabelDetails == nil, tt.wantAfterLabel == "" && tt.wantLabelDesc == "")
			// The labelDetails take the place of the detail.
			_, hasDetail := item.Detail.Get()
			ok.Equal(t, hasDetail, item.LabelDetails == nil)
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
	ok.True(t, result.Capabilities.CompletionProvider.CompletionItem != nil)
	ok.Equal(t, *result.Capabilities.CompletionProvider.CompletionItem.LabelDetailsSupport, true)
}

// A client that does not say it shows labelDetails gets none.
func TestCompletionLabelDetailsUnsupported(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	clientRPC := newLSPClient(t, protocol.UnimplementedClient{}, lsp.Options{})
	var result protocol.InitializeResult
	_, err := clientRPC.Call(ctx, "initialize", protocol.InitializeParams{}, &result)
	ok.MustNoError(t, err)
	ok.MustNoError(t, clientRPC.Notify(ctx, "initialized", protocol.InitializedParams{}))

	testURI := lspuri.URI("file:///test.cel")
	ok.MustNoError(t, clientRPC.Notify(ctx, "textDocument/didOpen", protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: testURI, LanguageID: "cel", Version: 1, Text: "true",
		},
	}))

	list := requestCompletion(t, clientRPC, testURI, protocol.Position{}, protocol.CompletionTriggerKindInvoked, "")
	ok.True(t, len(list.Items) > 0)
	for _, item := range list.Items {
		ok.True(t, item.LabelDetails == nil)
		_, hasDetail := item.Detail.Get()
		ok.True(t, hasDetail)
	}
}
