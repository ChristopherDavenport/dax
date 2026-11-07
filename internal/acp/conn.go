// Package acp is the Agent Client Protocol's wire as dax speaks it: JSON-RPC
// 2.0, one message per line, over the editor's pipe to the agent's
// standard input and output, and the protocol's messages (v1) that dax
// sends and takes. It holds no session: the front (dax's acp.go) answers
// the editor's requests through a Handler and calls back with Request
// and Notify. Standard library only.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// ProtocolVersion is the ACP version dax speaks.
const ProtocolVersion = 1

// JSON-RPC error codes.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

// Error is a JSON-RPC error, which a Handler returns to answer a
// request with one.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Errorf is an Error of code, its message formatted.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// message is any JSON-RPC message.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Handler answers the editor's requests and takes its notifications:
// result is marshalled as the request's result; an error that is an
// *Error is sent as it is, any other as an internal error. A
// notification's result is dropped. Each message is handled in a
// goroutine of its own, so a long session/prompt does not hold up the
// session/cancel the editor sends during it.
type Handler func(ctx context.Context, method string, params json.RawMessage) (result any, err error)

// Then is a Handler's result that runs After once Result is sent: the
// updates a session/new or session/load owes, which name a session the
// editor learns of from the result.
type Then struct {
	Result any
	After  func()
}

// Conn is one editor's connection.
type Conn struct {
	w  io.Writer
	wm sync.Mutex

	mu      sync.Mutex
	next    int
	waiting map[string]chan message
	closed  bool
}

// NewConn is a connection writing to w.
func NewConn(w io.Writer) *Conn {
	return &Conn{w: w, waiting: map[string]chan message{}}
}

// Serve reads messages from r until it ends or ctx does, handling each
// request and notification with h and routing each response to the
// Request waiting on it. It waits for the handlers it started.
func (c *Conn) Serve(ctx context.Context, r io.Reader, h Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer c.close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil || m.JSONRPC != "2.0" {
			_ = c.write(message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: Errorf(CodeParse, "not a JSON-RPC 2.0 message")})
			continue
		}
		switch {
		case m.Method == "" && len(m.ID) > 0:
			c.mu.Lock()
			ch := c.waiting[string(m.ID)]
			delete(c.waiting, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case m.Method != "":
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := h(ctx, m.Method, m.Params)
				if len(m.ID) == 0 {
					if t, ok := res.(Then); ok && t.After != nil {
						t.After()
					}
					return
				}
				out := message{JSONRPC: "2.0", ID: m.ID}
				var rpc *Error
				switch {
				case errors.As(err, &rpc):
					out.Error = rpc
				case err != nil:
					out.Error = Errorf(CodeInternal, "%s", err.Error())
				default:
					var after func()
					if t, ok := res.(Then); ok {
						res, after = t.Result, t.After
					}
					if after != nil {
						defer after()
					}
					b, merr := json.Marshal(res)
					if merr != nil {
						out.Error = Errorf(CodeInternal, "%s", merr.Error())
					} else {
						out.Result = b
					}
				}
				_ = c.write(out)
			}()
		}
	}
	return sc.Err()
}

// close fails every Request still waiting: the editor is gone.
func (c *Conn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id, ch := range c.waiting {
		close(ch)
		delete(c.waiting, id)
	}
}

// ErrClosed is a Request's error when the editor's input ended first.
var ErrClosed = errors.New("acp: the editor's connection is closed")

// Request asks the editor, method with params, and unmarshals its result
// into result, or returns its error.
func (c *Conn) Request(ctx context.Context, method string, params, result any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ch := make(chan message, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.next++
	id := strconv.Itoa(c.next)
	c.waiting[id] = ch
	c.mu.Unlock()
	if err := c.write(message{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: b}); err != nil {
		return err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if result != nil {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

// Notify sends the editor a notification.
func (c *Conn) Notify(method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(message{JSONRPC: "2.0", Method: method, Params: b})
}

func (c *Conn) write(m message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wm.Lock()
	defer c.wm.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}
