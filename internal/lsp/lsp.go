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
	"runtime/debug"
	"sync"
	"sync/atomic"

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
//
// opts describe the environment to start with. A client that sends the
// corresponding key in its initialize request's initializationOptions
// overrides that part of it for the lifetime of the connection.
func Serve(opts Options) error {
	return ServeStream(context.Background(), stdinout{}, opts)
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
func ServeStream(ctx context.Context, rwc io.ReadWriteCloser, opts Options) error {
	s, err := newServer(opts)
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

	mu    sync.Mutex
	files map[uri.URI]*file

	// envs holds the CEL environment for each configuration in use. It is
	// written by Initialize and mutated as documents are seen, and is
	// deliberately not guarded by mu — neither it nor its readers take the
	// lock. What makes that safe is ServeStream's synchronous dispatch: every
	// handler runs on one goroutine in wire order, and the LSP spec puts
	// initialize before any other request. Introducing an async handler would
	// invalidate this.
	envs *envCache

	// startOpts are the options the server was started with, which
	// initializationOptions overrides key by key.
	startOpts Options

	shutdown atomic.Bool // set by Shutdown; read by Exit to pick its process exit code
}

func newServer(opts Options) (*server, error) {
	s := &server{
		files:     make(map[uri.URI]*file),
		startOpts: opts,
	}
	return s, s.setEnv(opts)
}

// setEnv installs the environment the server builds from, discarding anything
// it had cached against the previous one. An opts.ConfigPath applies to every
// document; without one, each document uses the nearest configuration above it.
func (s *server) setEnv(opts Options) error {
	envs := newEnvCache(opts)
	// Build the named environment now so that a configuration or extension the
	// server cannot use is reported from initialize, where a client will show
	// it, rather than from the first request that happens to need it.
	if _, err := envs.forPath(opts.ConfigPath); err != nil {
		return fmt.Errorf("failed to create CEL environment: %w", err)
	}
	s.envs = envs
	return nil
}

// document returns the open document at docURI along with the CEL environment
// that governs it. It returns a nil file when there is no such document, and a
// nil environment when the configuration covering it does not load — features
// other than diagnostics have nothing useful to say in that case.
func (s *server) document(docURI uri.URI) (*file, *environment) {
	s.mu.Lock()
	f := s.files[docURI]
	s.mu.Unlock()

	if f == nil {
		return nil, nil
	}
	docEnv, err := s.envs.forDocument(docURI)
	if err != nil {
		return f, nil
	}
	return f, docEnv
}

// clientInitializationOptions is the shape cells reads out of the LSP
// initialize request's initializationOptions. Unrecognized keys are ignored.
type clientInitializationOptions struct {
	// Extensions names CEL extension libraries to enable (see
	// extensionFactories). When present, even as an empty list, it replaces
	// whatever default extensions the server was started with.
	Extensions []string `json:"extensions"`

	// Config is the path to a CEL environment configuration file, declaring
	// the variables and types expressions may refer to. When present it
	// replaces whatever configuration the server was started with; an empty
	// string clears it.
	Config *string `json:"config"`
}

func (s *server) Initialize(_ context.Context, params *protocol.InitializeParams) (*protocol.InitializeResult, error) {
	if params != nil && len(params.InitializationOptions) > 0 {
		if err := s.applyInitializationOptions(params.InitializationOptions); err != nil {
			return nil, fmt.Errorf("invalid initializationOptions: %w", err)
		}
	}
	return &protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: &protocol.TextDocumentSyncOptions{
				OpenClose: new(true),
				Change:    new(protocol.TextDocumentSyncKindFull),
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

// applyInitializationOptions replaces the server's environment with the one
// the client asked for. Each key the client sends overrides the corresponding
// value the server was started with; keys it omits are left alone.
func (s *server) applyInitializationOptions(raw json.RawMessage) error {
	var clientOpts clientInitializationOptions
	if err := json.Unmarshal(raw, &clientOpts); err != nil {
		return err
	}
	if clientOpts.Extensions == nil && clientOpts.Config == nil {
		return nil
	}

	opts := s.startOpts
	if clientOpts.Extensions != nil {
		opts.Extensions = clientOpts.Extensions
	}
	if clientOpts.Config != nil {
		opts.ConfigPath = *clientOpts.Config
	}
	return s.setEnv(opts)
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
	f := &file{
		uri:     params.TextDocument.URI,
		version: params.TextDocument.Version,
		content: params.TextDocument.Text,
	}

	s.mu.Lock()
	s.files[f.uri] = f
	s.mu.Unlock()

	s.publishDiagnostics(ctx, f)
	return nil
}

func (s *server) DidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	s.mu.Lock()
	prev := s.files[params.TextDocument.URI]
	s.mu.Unlock()

	if prev == nil {
		return fmt.Errorf("received update for file that was not open: %q", params.TextDocument.URI)
	}

	// A file is never written to in place: replacing it leaves any handler
	// that already took the old value with a consistent document, and drops
	// the parse cached against the old content.
	f := &file{
		uri:     prev.uri,
		version: params.TextDocument.Version,
		content: prev.content,
	}
	// We use full sync mode, so extract full text from content changes.
	if len(params.ContentChanges) > 0 {
		switch c := params.ContentChanges[0].(type) {
		case *protocol.TextDocumentContentChangeWholeDocument:
			f.content = c.Text
		case *protocol.TextDocumentContentChangePartial:
			f.content = c.Text
		}
	}

	s.mu.Lock()
	s.files[f.uri] = f
	s.mu.Unlock()

	s.publishDiagnostics(ctx, f)
	return nil
}

func (s *server) DidClose(_ context.Context, params *protocol.DidCloseTextDocumentParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.files, params.TextDocument.URI)
	return nil
}
