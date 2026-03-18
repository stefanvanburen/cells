package jsonrpc2

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// frame builds a Content-Length–framed message for use in tests.
func frame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

func TestReadFrame(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{
			name:  "normal message",
			input: frame(`{"jsonrpc":"2.0","method":"foo"}`),
			want:  `{"jsonrpc":"2.0","method":"foo"}`,
		},
		{
			name:  "zero content-length",
			input: "Content-Length: 0\r\n\r\n",
			want:  "",
		},
		{
			name:  "extra headers ignored",
			input: "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\nContent-Length: 2\r\n\r\n{}",
			want:  "{}",
		},
		{
			name:  "reads only first message",
			input: frame(`{"id":1}`) + frame(`{"id":2}`),
			want:  `{"id":1}`,
		},
		{
			name:    "missing content-length",
			input:   "Content-Type: application/vscode-jsonrpc\r\n\r\n",
			wantErr: "missing Content-Length header",
		},
		{
			name:    "bad content-length value",
			input:   "Content-Length: abc\r\n\r\n",
			wantErr: "bad Content-Length",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tt.input))
			got, err := readFrame(r)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIDJSON(t *testing.T) {
	tests := []struct {
		name string
		id   ID
		json string
	}{
		{"numeric", ID{Num: 42}, "42"},
		{"zero numeric", ID{Num: 0}, "0"},
		{"string", ID{Str: "abc", IsString: true}, `"abc"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.id)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tt.json {
				t.Fatalf("marshal: got %s, want %s", got, tt.json)
			}
			var id ID
			if err := json.Unmarshal(got, &id); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if id != tt.id {
				t.Fatalf("roundtrip: got %+v, want %+v", id, tt.id)
			}
		})
	}
}

func TestRequestUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantMethod string
		wantIDNum  uint64
		wantNotif  bool
	}{
		{
			name:       "request with numeric id and object params",
			input:      `{"jsonrpc":"2.0","id":1,"method":"textDocument/hover","params":{}}`,
			wantMethod: "textDocument/hover",
			wantIDNum:  1,
			wantNotif:  false,
		},
		{
			name:       "request with array params",
			input:      `{"jsonrpc":"2.0","id":2,"method":"subtract","params":[42,23]}`,
			wantMethod: "subtract",
			wantIDNum:  2,
			wantNotif:  false,
		},
		{
			name:       "notification (no id)",
			input:      `{"jsonrpc":"2.0","method":"initialized","params":{}}`,
			wantMethod: "initialized",
			wantNotif:  true,
		},
		{
			name:       "notification with no params",
			input:      `{"jsonrpc":"2.0","method":"exit"}`,
			wantMethod: "exit",
			wantNotif:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req Request
			if err := json.Unmarshal([]byte(tt.input), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if req.Method != tt.wantMethod {
				t.Errorf("method: got %q, want %q", req.Method, tt.wantMethod)
			}
			if req.Notif != tt.wantNotif {
				t.Errorf("notif: got %v, want %v", req.Notif, tt.wantNotif)
			}
			if !tt.wantNotif && req.ID.Num != tt.wantIDNum {
				t.Errorf("id: got %d, want %d", req.ID.Num, tt.wantIDNum)
			}
		})
	}
}

// newTestPair creates a connected client/server Conn pair over a net.Pipe.
func newTestPair(t *testing.T, serverHandler Handler) (client, server *Conn) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	ctx := context.Background()
	nullHandler := HandlerFunc(func(_ context.Context, _ *Conn, _ *Request) (any, error) {
		return nil, nil
	})
	server = NewConn(ctx, serverConn, serverHandler)
	client = NewConn(ctx, clientConn, nullHandler)
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return client, server
}

func TestConnCall(t *testing.T) {
	handler := HandlerFunc(func(_ context.Context, _ *Conn, req *Request) (any, error) {
		return map[string]string{"echo": req.Method}, nil
	})
	client, _ := newTestPair(t, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var result map[string]string
	if err := client.Call(ctx, "textDocument/hover", map[string]any{"x": 1}, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result["echo"] != "textDocument/hover" {
		t.Errorf("got %v, want echo=textDocument/hover", result)
	}
}

func TestConnNotify(t *testing.T) {
	received := make(chan string, 1)
	handler := HandlerFunc(func(_ context.Context, _ *Conn, req *Request) (any, error) {
		if req.Notif {
			received <- req.Method
		}
		return nil, nil
	})
	client, _ := newTestPair(t, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Notify(ctx, "initialized", map[string]any{}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	select {
	case method := <-received:
		if method != "initialized" {
			t.Errorf("got %q, want %q", method, "initialized")
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for notification")
	}
}

func TestConnErrorResponse(t *testing.T) {
	handler := HandlerFunc(func(_ context.Context, _ *Conn, _ *Request) (any, error) {
		return nil, &Error{Code: CodeMethodNotFound, Message: "method not found"}
	})
	client, _ := newTestPair(t, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Call(ctx, "unknown/method", nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if rpcErr.Code != CodeMethodNotFound {
		t.Errorf("code: got %d, want %d", rpcErr.Code, CodeMethodNotFound)
	}
}

func TestConnDisconnectNotify(t *testing.T) {
	client, server := newTestPair(t, HandlerFunc(func(_ context.Context, _ *Conn, _ *Request) (any, error) {
		return nil, nil
	}))

	server.Close()

	select {
	case <-client.DisconnectNotify():
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for client to observe disconnect")
	}
}

func TestConnCallErrClosed(t *testing.T) {
	testDone := make(chan struct{})
	t.Cleanup(func() { close(testDone) })

	received := make(chan struct{}, 1)
	handler := HandlerFunc(func(_ context.Context, _ *Conn, _ *Request) (any, error) {
		select {
		case received <- struct{}{}:
		default:
		}
		<-testDone // block without responding until test cleanup
		return nil, nil
	})
	client, server := newTestPair(t, handler)

	ctx := context.Background()
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Call(ctx, "foo", nil, nil)
	}()

	// Wait until the server has received the request, then drop the connection.
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server to receive request")
	}
	server.Close()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("expected ErrClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Call to return ErrClosed")
	}
}

func TestConnInternalErrorWrapping(t *testing.T) {
	// Per spec §5.1: if the handler returns a non-*Error, it must be wrapped
	// as a CodeInternalError response.
	handler := HandlerFunc(func(_ context.Context, _ *Conn, _ *Request) (any, error) {
		return nil, errors.New("something went wrong")
	})
	client, _ := newTestPair(t, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Call(ctx, "foo", nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if rpcErr.Code != CodeInternalError {
		t.Errorf("code: got %d, want %d (CodeInternalError)", rpcErr.Code, CodeInternalError)
	}
}

func TestConnErrorWithData(t *testing.T) {
	// Per spec §5.1: error objects may include a data field.
	data := json.RawMessage(`{"detail":"extra info"}`)
	handler := HandlerFunc(func(_ context.Context, _ *Conn, _ *Request) (any, error) {
		return nil, &Error{Code: CodeInvalidParams, Message: "invalid params", Data: &data}
	})
	client, _ := newTestPair(t, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Call(ctx, "foo", nil, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("code: got %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
	if rpcErr.Data == nil {
		t.Fatal("expected non-nil Data field")
	}
	if string(*rpcErr.Data) != `{"detail":"extra info"}` {
		t.Errorf("data: got %s, want %s", *rpcErr.Data, `{"detail":"extra info"}`)
	}
}

func TestConnNotifyNoResponse(t *testing.T) {
	// Notifications (req.Notif=true) must never elicit a response per spec §4.
	// We verify by sending a notification and then a call; the call response
	// must arrive (not be displaced by a spurious notification response).
	handler := HandlerFunc(func(_ context.Context, _ *Conn, req *Request) (any, error) {
		return map[string]string{"method": req.Method}, nil
	})
	client, _ := newTestPair(t, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Notify(ctx, "$/cancelRequest", map[string]any{"id": 1}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	var result map[string]string
	if err := client.Call(ctx, "textDocument/definition", nil, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result["method"] != "textDocument/definition" {
		t.Errorf("got %v, want method=textDocument/definition", result)
	}
}

func TestConnConcurrentCalls(t *testing.T) {
	// Multiple in-flight calls must be matched to their responses by ID, even
	// when responses arrive out of order.
	handler := HandlerFunc(func(_ context.Context, _ *Conn, req *Request) (any, error) {
		// Echo the method name so callers can verify they got the right response.
		return map[string]string{"method": req.Method}, nil
	})
	client, _ := newTestPair(t, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const n = 20
	type callResult struct {
		method string
		err    error
	}
	results := make(chan callResult, n)
	methods := make([]string, n)
	for i := range n {
		methods[i] = fmt.Sprintf("method/%d", i)
	}
	for _, method := range methods {
		go func() {
			var result map[string]string
			err := client.Call(ctx, method, nil, &result)
			results <- callResult{method: result["method"], err: err}
		}()
	}
	for range n {
		r := <-results
		if r.err != nil {
			t.Errorf("Call error: %v", r.err)
			continue
		}
		// Each response must match one of our methods; verify non-empty.
		if r.method == "" {
			t.Error("got empty method in response")
		}
	}
}

func TestConnCallContextCancel(t *testing.T) {
	testDone := make(chan struct{})
	t.Cleanup(func() { close(testDone) })

	received := make(chan struct{}, 1)
	handler := HandlerFunc(func(_ context.Context, _ *Conn, _ *Request) (any, error) {
		select {
		case received <- struct{}{}:
		default:
		}
		<-testDone
		return nil, nil
	})
	client, _ := newTestPair(t, handler)

	callCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Call(callCtx, "foo", nil, nil)
	}()

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server to receive request")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Call to return after cancel")
	}
}
