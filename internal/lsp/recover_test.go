package lsp

// This test is in package lsp rather than lsp_test because no request is
// known to make the server panic; it serves a handler that does.

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"go.lsp.dev/jsonrpc2"
	"go.vanburen.xyz/ok"
)

// lockedBuffer is a strings.Builder safe to write from the connection's
// goroutine and read from the test's.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestRecoverPanics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	serverPipe, clientPipe := net.Pipe()
	t.Cleanup(func() {
		_ = serverPipe.Close()
		_ = clientPipe.Close()
	})

	var stderr lockedBuffer
	handler := func(ctx context.Context, req *jsonrpc2.Request) (any, error) {
		if strings.HasPrefix(req.Method(), "panic") {
			panic("boom")
		}
		return "pong", nil
	}
	server := jsonrpc2.NewConn(jsonrpc2.NewStream(serverPipe))
	server.Go(ctx, recoverPanics(&stderr, handler))

	client := jsonrpc2.NewConn(jsonrpc2.NewStream(clientPipe))
	client.Go(ctx, jsonrpc2.MethodNotFoundHandler)
	t.Cleanup(func() { _ = client.Close() })

	// A call that panics is answered with an internal error.
	_, err := client.Call(ctx, "panicCall", nil, nil)
	var rpcErr *jsonrpc2.Error
	if ok.True(t, errors.As(err, &rpcErr)) {
		ok.Equal(t, rpcErr.Code, jsonrpc2.InternalError)
		ok.Equal(t, rpcErr.Message, "panicCall panicked: boom")
	}

	// A notification that panics is dropped.
	ok.MustNoError(t, client.Notify(ctx, "panicNotify", nil))

	// The connection survives both.
	var got string
	_, err = client.Call(ctx, "ping", nil, &got)
	ok.MustNoError(t, err)
	ok.Equal(t, got, "pong")

	out := stderr.String()
	ok.True(t, strings.Contains(out, "panicCall panicked: boom\n"))
	ok.True(t, strings.Contains(out, "panicNotify panicked: boom\n"))
}
