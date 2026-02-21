package lsp

import "github.com/stefanvanburen/cells/internal/lsp/protocol"

// file tracks a single open document.
type file struct {
	uri     protocol.DocumentURI
	version int32
	content string
}
