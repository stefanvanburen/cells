// Package lsp implements a language server for CEL (Common Expression Language).
//
// The main entry-point is the Serve() function, which creates a new LSP server
// communicating over stdin/stdout.
package lsp

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/google/cel-go/cel"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

const serverName = "cells"

func getVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	return info.Main.Version
}

// Serve starts the LSP server, communicating over stdin/stdout.
// It blocks until the connection is closed.
func Serve() error {
	return ServeStream(context.Background(), stdinout{})
}

// stdinout wraps stdin/stdout into a ReadWriteCloser.
type stdinout struct{}

func (stdinout) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdinout) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

// Close closes both directions of the wrapped stream. Closing only stdout
// would leave a concurrent blocked os.Stdin.Read() (e.g. the jsonrpc2 read
// loop, during connection teardown after a broken pipe) unable to ever
// return.
func (stdinout) Close() error {
	err := os.Stdin.Close()
	if writeErr := os.Stdout.Close(); err == nil {
		err = writeErr
	}
	return err
}

// ServeStream starts the LSP server over the given stream.
// Exposed for testing.
func ServeStream(ctx context.Context, rwc io.ReadWriteCloser) error {
	s, err := newServer()
	if err != nil {
		return err
	}

	stream := jsonrpc2.NewStream(rwc)
	conn := jsonrpc2.NewConn(stream, jsonrpc2.WithCodec(lspCodec{}))
	ctx = protocol.WithClient(ctx, protocol.ClientDispatcher(conn))

	// Dispatch synchronously (unlike protocol.NewServer's default, which wraps
	// every handler in jsonrpc2.AsyncHandler): cells relies on messages being
	// fully processed in wire order, e.g. a didOpen notification must finish
	// registering the file before a following hover/completion request reads
	// it back. cells never makes a server-initiated call awaiting a response
	// (only fire-and-forget notifications like publishDiagnostics), so
	// synchronous dispatch carries no deadlock risk.
	//
	// protocol.CancelHandler is deliberately omitted: it derives a
	// context.WithCancel from the per-request context, which assumes the
	// request has been async-released (and so cloned) per jsonrpc2's pooling
	// contract; under synchronous dispatch the request is recycled as soon as
	// the handler returns, racing that derivation. cells' requests complete
	// near-instantly, so $/cancelRequest support isn't worth the tradeoff.
	conn.Go(ctx, protocol.ServerHandler(s, jsonrpc2.MethodNotFoundHandler))
	<-conn.Done()
	return nil
}

// lspCodec mirrors go.lsp.dev/protocol's own (unexported) codec: it marshals
// and unmarshals LSP payloads through protocol.Marshal/Unmarshal so
// sealed-interface union values round-trip correctly, with a passthrough for
// already-encoded jsonrpc2.RawMessage values per the jsonrpc2.Codec contract.
type lspCodec struct{}

var _ jsonrpc2.Codec = lspCodec{}

func (lspCodec) Marshal(v any) ([]byte, error) {
	switch m := v.(type) {
	case jsonrpc2.RawMessage:
		if m == nil {
			return []byte("null"), nil
		}
		return m, nil
	case *jsonrpc2.RawMessage:
		if m == nil || *m == nil {
			return []byte("null"), nil
		}
		return *m, nil
	}
	return protocol.Marshal(v)
}

func (lspCodec) Unmarshal(data []byte, v any) error {
	if p, ok := v.(*jsonrpc2.RawMessage); ok {
		b := make(jsonrpc2.RawMessage, len(data))
		copy(b, data)
		*p = b
		return nil
	}
	return protocol.Unmarshal(data, v)
}

// server holds all of the LSP server's mutable state.
type server struct {
	protocol.UnimplementedServer

	mu     sync.Mutex
	files  map[uri.URI]*file
	celEnv *cel.Env

	shutdown atomic.Bool // set by Shutdown; read by Exit to pick its process exit code
}

func newServer() (*server, error) {
	celEnv, err := newCELEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}
	return &server{
		files:  make(map[uri.URI]*file),
		celEnv: celEnv,
	}, nil
}

//go:fix inline
func ptrTo[T any](v T) *T { return new(v) }

func (s *server) Initialize(context.Context, *protocol.InitializeParams) (*protocol.InitializeResult, error) {
	return &protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: &protocol.TextDocumentSyncOptions{
				OpenClose: new(true),
				Change:    ptrTo(protocol.TextDocumentSyncKindFull),
			},
			HoverProvider:              protocol.Boolean(true),
			DocumentFormattingProvider: protocol.Boolean(true),
			CompletionProvider: &protocol.CompletionOptions{
				TriggerCharacters: []string{"."},
			},
			SemanticTokensProvider: &protocol.SemanticTokensOptions{
				Legend: protocol.SemanticTokensLegend{
					TokenTypes:     semanticTypeLegend,
					TokenModifiers: semanticModifierLegend,
				},
				Full: protocol.Boolean(true),
			},
			SignatureHelpProvider: &protocol.SignatureHelpOptions{
				TriggerCharacters: []string{"(", ","},
			},
			RenameProvider:            protocol.Boolean(true),
			ReferencesProvider:        protocol.Boolean(true),
			DocumentHighlightProvider: protocol.Boolean(true),
			InlayHintProvider:         protocol.Boolean(true),
		},
		ServerInfo: protocol.ServerInfo{
			Name:    serverName,
			Version: protocol.NewOptional(getVersion()),
		},
	}, nil
}

func (s *server) Initialized(context.Context, *protocol.InitializedParams) error {
	return nil
}

func (s *server) Shutdown(context.Context) error {
	s.shutdown.Store(true)
	return nil
}

// Exit terminates the process per the LSP spec: exit code 0 if shutdown was
// received first, 1 otherwise.
//
// This was tried the "graceful" way first (close the connection and let
// ServeStream's <-conn.Done() return on its own, matching gopls and
// vscode-languageserver-node), but that relies on closing the connection
// interrupting the read loop's in-progress blocked read on stdin, and a
// goroutine dump confirmed that doesn't happen here: the read loop was still
// parked in syscall.Read on stdin seconds after close. A direct os.Exit is
// the reliable option for this transport.
func (s *server) Exit(context.Context) error {
	if s.shutdown.Load() {
		os.Exit(0)
	}
	os.Exit(1)
	return nil
}

func (s *server) DidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) error {
	s.mu.Lock()
	f := &file{
		uri:     params.TextDocument.URI,
		version: params.TextDocument.Version,
		content: params.TextDocument.Text,
	}
	s.files[params.TextDocument.URI] = f
	docURI, version, content := f.uri, f.version, f.content
	s.mu.Unlock()

	publishDiagnostics(ctx, docURI, version, content, s.celEnv)
	return nil
}

func (s *server) DidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	s.mu.Lock()
	f := s.files[params.TextDocument.URI]
	if f == nil {
		s.mu.Unlock()
		return fmt.Errorf("received update for file that was not open: %q", params.TextDocument.URI)
	}
	f.version = params.TextDocument.Version

	// We use full sync mode, so extract full text from content changes.
	if len(params.ContentChanges) > 0 {
		switch c := params.ContentChanges[0].(type) {
		case *protocol.TextDocumentContentChangeWholeDocument:
			f.content = c.Text
		case *protocol.TextDocumentContentChangePartial:
			f.content = c.Text
		}
	}
	docURI, version, content := f.uri, f.version, f.content
	s.mu.Unlock()

	publishDiagnostics(ctx, docURI, version, content, s.celEnv)
	return nil
}

func (s *server) DidClose(_ context.Context, params *protocol.DidCloseTextDocumentParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.files, params.TextDocument.URI)
	return nil
}
