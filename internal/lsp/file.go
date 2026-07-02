package lsp

import "go.lsp.dev/uri"

// file tracks a single open document.
type file struct {
	uri     uri.URI
	version int32
	content string
}
