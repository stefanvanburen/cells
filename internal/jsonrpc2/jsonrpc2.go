// Package jsonrpc2 implements a JSON-RPC 2.0 client/server over an LSP
// (Content-Length framed) byte stream.
//
// It is a minimal replacement for github.com/sourcegraph/jsonrpc2, tailored
// for language-server use: only the Content-Length framing ("VS Code codec")
// is supported.
package jsonrpc2

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Error codes defined by JSON-RPC 2.0 spec.
// ---------------------------------------------------------------------------

const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Error is a JSON-RPC 2.0 response error.
type Error struct {
	Code    int64            `json:"code"`
	Message string           `json:"message"`
	Data    *json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("jsonrpc2: code %d message: %s", e.Code, e.Message)
}

// ErrClosed indicates that the connection is closed.
var ErrClosed = errors.New("jsonrpc2: connection is closed")

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// ID is a JSON-RPC 2.0 request ID (number or string).
type ID struct {
	Num      uint64
	Str      string
	IsString bool
}

func (id ID) String() string {
	if id.IsString {
		return strconv.Quote(id.Str)
	}
	return strconv.FormatUint(id.Num, 10)
}

func (id ID) MarshalJSON() ([]byte, error) {
	if id.IsString {
		return json.Marshal(id.Str)
	}
	return json.Marshal(id.Num)
}

func (id *ID) UnmarshalJSON(data []byte) error {
	var n uint64
	if err := json.Unmarshal(data, &n); err == nil {
		*id = ID{Num: n}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	*id = ID{Str: s, IsString: true}
	return nil
}

// Request is an incoming JSON-RPC 2.0 request or notification.
type Request struct {
	Method string           `json:"method"`
	Params *json.RawMessage `json:"params,omitempty"`
	ID     ID               `json:"id"`
	Notif  bool             `json:"-"` // true if this is a notification (no id)
}

// wireRequest is used for JSON marshaling (adds jsonrpc field).
type wireRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	Method  string           `json:"method"`
	Params  *json.RawMessage `json:"params,omitempty"`
	ID      *ID              `json:"id,omitempty"`
}

func (r *Request) UnmarshalJSON(data []byte) error {
	// Use a map to detect presence/absence of "id".
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if m, ok := raw["method"]; ok {
		if err := json.Unmarshal(m, &r.Method); err != nil {
			return err
		}
	}
	if p, ok := raw["params"]; ok {
		r.Params = &p
	}
	if idRaw, ok := raw["id"]; ok {
		if err := json.Unmarshal(idRaw, &r.ID); err != nil {
			return err
		}
		r.Notif = false
	} else {
		r.Notif = true
	}
	return nil
}

// response is an outgoing JSON-RPC 2.0 response.
type response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      ID               `json:"id"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
}

// incomingResponse is the wire format for a response we receive.
type incomingResponse struct {
	ID     ID               `json:"id"`
	Result *json.RawMessage `json:"result,omitempty"`
	Error  *Error           `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// Handler handles incoming JSON-RPC requests.
type Handler interface {
	Handle(ctx context.Context, conn *Conn, req *Request)
}

// HandlerFunc adapts a function to the Handler interface. The function returns
// (result, error); the Conn automatically sends the appropriate response.
type HandlerFunc func(ctx context.Context, conn *Conn, req *Request) (any, error)

func (f HandlerFunc) Handle(ctx context.Context, conn *Conn, req *Request) {
	result, err := f(ctx, conn, req)
	if req.Notif {
		return // notifications don't get responses
	}
	if err != nil {
		var rpcErr *Error
		if !errors.As(err, &rpcErr) {
			rpcErr = &Error{Code: CodeInternalError, Message: err.Error()}
		}
		_ = conn.sendResponse(&response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
		return
	}
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		_ = conn.sendResponse(&response{
			JSONRPC: "2.0", ID: req.ID,
			Error: &Error{Code: CodeInternalError, Message: marshalErr.Error()},
		})
		return
	}
	rm := json.RawMessage(raw)
	_ = conn.sendResponse(&response{JSONRPC: "2.0", ID: req.ID, Result: &rm})
}

// ---------------------------------------------------------------------------
// Conn
// ---------------------------------------------------------------------------

// Conn is a bidirectional JSON-RPC 2.0 connection.
type Conn struct {
	r    *bufio.Reader
	wc   io.WriteCloser
	h    Handler
	wmu  sync.Mutex // guards writes
	mu   sync.Mutex
	seq  uint64
	pend map[uint64]*pending
	done chan struct{}
	once sync.Once
}

type pending struct {
	ch chan *incomingResponse
}

// NewConn creates a new JSON-RPC connection over the given stream. It
// immediately starts reading messages in a background goroutine. The handler
// is called for each incoming request.
func NewConn(ctx context.Context, rwc io.ReadWriteCloser, h Handler) *Conn {
	c := &Conn{
		r:    bufio.NewReaderSize(rwc, 4096),
		wc:   rwc,
		h:    h,
		pend: make(map[uint64]*pending),
		done: make(chan struct{}),
	}
	go c.readLoop(ctx)
	return c
}

// Close closes the connection.
func (c *Conn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.wc.Close()
}

// DisconnectNotify returns a channel that is closed when the connection is
// closed (either by Close or by the remote end).
func (c *Conn) DisconnectNotify() <-chan struct{} {
	return c.done
}

// Call sends a request and waits for the response. result should be a pointer.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	id := c.seq
	c.seq++
	p := &pending{ch: make(chan *incomingResponse, 1)}
	c.pend[id] = p
	c.mu.Unlock()

	raw, err := json.Marshal(params)
	if err != nil {
		c.mu.Lock()
		delete(c.pend, id)
		c.mu.Unlock()
		return err
	}
	rm := json.RawMessage(raw)
	reqID := ID{Num: id}

	if err := c.writeMessage(&wireRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  &rm,
		ID:      &reqID,
	}); err != nil {
		c.mu.Lock()
		delete(c.pend, id)
		c.mu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pend, id)
		c.mu.Unlock()
		return ctx.Err()
	case resp := <-p.ch:
		if resp == nil {
			return ErrClosed
		}
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && resp.Result != nil {
			return json.Unmarshal(*resp.Result, result)
		}
		return nil
	}
}

// Notify sends a notification (no response expected).
func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	rm := json.RawMessage(raw)
	return c.writeMessage(&wireRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  &rm,
		// no ID → notification
	})
}

// ---------------------------------------------------------------------------
// Internal
// ---------------------------------------------------------------------------

func (c *Conn) sendResponse(resp *response) error {
	return c.writeMessage(resp)
}

func (c *Conn) writeMessage(v any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := io.WriteString(c.wc, header); err != nil {
		return err
	}
	_, err = c.wc.Write(data)
	return err
}

func (c *Conn) readLoop(ctx context.Context) {
	defer func() {
		c.once.Do(func() { close(c.done) })
		// Wake all pending calls.
		c.mu.Lock()
		for id, p := range c.pend {
			close(p.ch)
			delete(c.pend, id)
		}
		c.mu.Unlock()
	}()

	for {
		data, err := readFrame(c.r)
		if err != nil {
			return
		}

		// Determine if this is a request or response by checking for "method".
		var probe struct {
			Method *string          `json:"method"`
			ID     *json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			continue
		}

		if probe.Method != nil {
			// It's a request or notification.
			var req Request
			if err := json.Unmarshal(data, &req); err != nil {
				continue
			}
			c.h.Handle(ctx, c, &req)
		} else {
			// It's a response.
			var resp incomingResponse
			if err := json.Unmarshal(data, &resp); err != nil {
				continue
			}
			c.mu.Lock()
			p := c.pend[resp.ID.Num]
			delete(c.pend, resp.ID.Num)
			c.mu.Unlock()
			if p != nil {
				p.ch <- &resp
			}
		}
	}
}

// readFrame reads one Content-Length–framed message from r.
func readFrame(r *bufio.Reader) ([]byte, error) {
	var contentLength int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if after, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			n, err := strconv.Atoi(after)
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length: %w", err)
			}
			contentLength = n
		}
		// ignore other headers (Content-Type, etc.)
	}
	if contentLength == 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
