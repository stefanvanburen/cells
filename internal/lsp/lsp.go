// Package lsp implements a language server for CEL (Common Expression Language).
//
// The main entry-point is the Serve() function, which creates a new LSP server
// communicating over stdin/stdout.
package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/stefanvanburen/cells/internal/jsonrpc2"
	"github.com/stefanvanburen/cells/internal/lsp/protocol"
)

const serverName = "cells"

// Serve starts the LSP server, communicating over stdin/stdout.
// It blocks until the connection is closed.
func Serve() error {
	return ServeStream(context.Background(), stdinout{})
}

// stdinout wraps stdin/stdout into a ReadWriteCloser.
type stdinout struct{}

func (stdinout) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdinout) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdinout) Close() error                { return os.Stdout.Close() }

// ServeStream starts the LSP server over the given stream.
// Exposed for testing.
func ServeStream(ctx context.Context, rwc io.ReadWriteCloser) error {
	s, err := newServer()
	if err != nil {
		return err
	}

	conn := jsonrpc2.NewConn(ctx, rwc, jsonrpc2.HandlerFunc(s.handle))
	<-conn.DisconnectNotify()
	return nil
}

// server holds all of the LSP server's mutable state.
type server struct {
	mu     sync.Mutex
	files  map[protocol.DocumentURI]*file
	celEnv *cel.Env
}

func newServer() (*server, error) {
	celEnv, err := cel.NewEnv(cel.EnableMacroCallTracking())
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}
	return &server{
		files:  make(map[protocol.DocumentURI]*file),
		celEnv: celEnv,
	}, nil
}

func (s *server) handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (any, error) {
	switch req.Method {
	case "initialize":
		return s.initialize(req)
	case "initialized":
		return nil, nil
	case "shutdown":
		return nil, nil
	case "exit":
		return nil, conn.Close()
	case "textDocument/didOpen":
		return nil, s.didOpen(ctx, conn, req)
	case "textDocument/didChange":
		return nil, s.didChange(ctx, conn, req)
	case "textDocument/didClose":
		return nil, s.didClose(req)
	case "textDocument/hover":
		return s.hover(req)
	case "textDocument/diagnostic":
		return s.diagnosticFull(req)
	case "textDocument/semanticTokens/full":
		return s.semanticTokensFull(req)
	default:
		return nil, &jsonrpc2.Error{
			Code:    jsonrpc2.CodeMethodNotFound,
			Message: fmt.Sprintf("method not supported: %s", req.Method),
		}
	}
}

func (s *server) initialize(req *jsonrpc2.Request) (any, error) {
	return protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: protocol.TextDocumentSyncOptions{
				OpenClose: true,
				Change:    protocol.Full,
			},
			HoverProvider: &protocol.Or_ServerCapabilities_hoverProvider{Value: true},
			SemanticTokensProvider: protocol.SemanticTokensOptions{
				Legend: protocol.SemanticTokensLegend{
					TokenTypes:     semanticTypeLegend,
					TokenModifiers: semanticModifierLegend,
				},
				Full: &protocol.Or_SemanticTokensOptions_full{Value: true},
			},
		},
		ServerInfo: &protocol.ServerInfo{
			Name: serverName,
		},
	}, nil
}

func (s *server) didOpen(_ context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) error {
	var params protocol.DidOpenTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return err
	}

	s.mu.Lock()
	f := &file{
		uri:     params.TextDocument.URI,
		version: params.TextDocument.Version,
		content: params.TextDocument.Text,
	}
	s.files[params.TextDocument.URI] = f
	uri, version, content := f.uri, f.version, f.content
	s.mu.Unlock()

	publishDiagnostics(conn, uri, version, content, s.celEnv)
	return nil
}

func (s *server) didChange(_ context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) error {
	var params protocol.DidChangeTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.TextDocumentIdentifier.URI]
	if f == nil {
		s.mu.Unlock()
		return fmt.Errorf("received update for file that was not open: %q", params.TextDocument.TextDocumentIdentifier.URI)
	}
	f.version = params.TextDocument.Version

	// We use full sync mode, so extract full text from content changes.
	if len(params.ContentChanges) > 0 {
		change := params.ContentChanges[0]
		switch v := change.Value.(type) {
		case protocol.TextDocumentContentChangeWholeDocument:
			f.content = v.Text
		case *protocol.TextDocumentContentChangeWholeDocument:
			f.content = v.Text
		case protocol.TextDocumentContentChangePartial:
			f.content = v.Text
		case *protocol.TextDocumentContentChangePartial:
			f.content = v.Text
		}
	}
	uri, version, content := f.uri, f.version, f.content
	s.mu.Unlock()

	publishDiagnostics(conn, uri, version, content, s.celEnv)
	return nil
}

func (s *server) didClose(req *jsonrpc2.Request) error {
	var params protocol.DidCloseTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.files, params.TextDocument.URI)
	return nil
}

func (s *server) semanticTokensFull(req *jsonrpc2.Request) (any, error) {
	var params protocol.SemanticTokensParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	if f == nil {
		return nil, nil
	}
	return computeSemanticTokens(f, s.celEnv)
}
