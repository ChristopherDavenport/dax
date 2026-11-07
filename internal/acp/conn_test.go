package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// wire is a Conn served over pipes: the test writes the editor's lines
// to in and reads dax's from out.
type wire struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Scanner
	conn *Conn
	done chan error
}

func serve(t *testing.T, max int, h Handler) *wire {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := NewConn(outW)
	c.max = max
	w := &wire{t: t, in: inW, out: bufio.NewScanner(outR), conn: c, done: make(chan error, 1)}
	w.out.Buffer(make([]byte, 1<<20), 1<<20)
	go func() {
		err := c.Serve(context.Background(), inR, h)
		outW.Close()
		w.done <- err
	}()
	t.Cleanup(func() { inW.Close() })
	return w
}

func (w *wire) send(line string) {
	w.t.Helper()
	if _, err := io.WriteString(w.in, line+"\n"); err != nil {
		w.t.Fatal(err)
	}
}

// read is dax's next message.
func (w *wire) read() message {
	w.t.Helper()
	got := make(chan message, 1)
	go func() {
		var m message
		if w.out.Scan() {
			json.Unmarshal(w.out.Bytes(), &m)
		}
		got <- m
	}()
	select {
	case m := <-got:
		return m
	case <-time.After(10 * time.Second):
		w.t.Fatal("dax wrote nothing")
	}
	return message{}
}

func echo(_ context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "echo":
		return params, nil
	case "refuse":
		return nil, Errorf(CodeInvalidParams, "no")
	case "fail":
		return nil, errors.New("broke")
	}
	return nil, Errorf(CodeMethodNotFound, "dax does not take %s", method)
}

// A line past the size cap is refused with a JSON-RPC error and the
// connection goes on; so is a line that is not JSON-RPC 2.0. A
// handler's *Error is sent as it is, any other error as an internal
// one, and an unknown method gets the handler's MethodNotFound.
func TestALineIsAnsweredWhateverItHolds(t *testing.T) {
	w := serve(t, 64, echo)
	w.send(`{"jsonrpc":"2.0","id":1,"method":"echo","params":"` + strings.Repeat("x", 200) + `"}`)
	if m := w.read(); m.Error == nil || m.Error.Code != CodeInvalidRequest || string(m.ID) != "null" {
		t.Fatalf("an oversized line: %+v", m)
	}
	for _, tc := range []struct {
		line string
		code int
		id   string
	}{
		{`not json`, CodeParse, "null"},
		{`{"jsonrpc":"1.0","id":2,"method":"echo"}`, CodeParse, "null"},
		{`{"jsonrpc":"2.0","id":3,"method":"refuse"}`, CodeInvalidParams, "3"},
		{`{"jsonrpc":"2.0","id":4,"method":"fail"}`, CodeInternal, "4"},
		{`{"jsonrpc":"2.0","id":5,"method":"nope"}`, CodeMethodNotFound, "5"},
	} {
		w.send(tc.line)
		if m := w.read(); m.Error == nil || m.Error.Code != tc.code || string(m.ID) != tc.id {
			t.Errorf("%s: %+v, want code %d id %s", tc.line, m, tc.code, tc.id)
		}
	}
	w.send(`{"jsonrpc":"2.0","id":6,"method":"echo","params":"ok"}`)
	if m := w.read(); m.Error != nil || string(m.Result) != `"ok"` || string(m.ID) != "6" {
		t.Errorf("after the refusals: %+v", m)
	}
}

// Each message is handled on its own: a notification reaches a handler
// while a request's runs (session/cancel during session/prompt), and
// dax's own request to the editor gets the editor's answer.
func TestMessagesAreHandledConcurrently(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	var w *wire
	w = serve(t, 0, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		switch method {
		case "prompt":
			var asked string
			if err := w.conn.Request(ctx, "ask", "q", &asked); err != nil {
				return nil, err
			}
			<-release
			return asked, nil
		case "cancel":
			once.Do(func() { close(release) })
		}
		return nil, nil
	})
	w.send(`{"jsonrpc":"2.0","id":1,"method":"prompt"}`)
	req := w.read()
	if req.Method != "ask" || len(req.ID) == 0 {
		t.Fatalf("dax's request: %+v", req)
	}
	w.send(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":"yes"}`)
	w.send(`{"jsonrpc":"2.0","method":"cancel"}`)
	if m := w.read(); string(m.ID) != "1" || string(m.Result) != `"yes"` {
		t.Errorf("the prompt's answer: %+v", m)
	}
}

// When the editor's input ends, a running handler's context is
// cancelled, so a prompt nobody watches stops, and Serve returns once
// it has; a request dax was waiting on fails as closed.
func TestTheEndOfInputCancelsRunningHandlers(t *testing.T) {
	stopped := make(chan error, 2)
	var w *wire
	w = serve(t, 0, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		switch method {
		case "prompt":
			<-ctx.Done()
			stopped <- ctx.Err()
		case "ask":
			stopped <- w.conn.Request(context.Background(), "q", nil, nil)
		}
		return nil, nil
	})
	w.send(`{"jsonrpc":"2.0","id":1,"method":"prompt"}`)
	w.send(`{"jsonrpc":"2.0","id":2,"method":"ask"}`)
	if m := w.read(); m.Method != "q" {
		t.Fatalf("dax's request: %+v", m)
	}
	// What dax writes after is read and dropped, as a pipe the editor
	// still holds takes it.
	go func() {
		for w.out.Scan() {
		}
	}()
	w.in.Close()
	select {
	case err := <-w.done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return at the end of input: a running prompt holds it")
	}
	got := []error{<-stopped, <-stopped}
	if !(errors.Is(got[0], context.Canceled) || errors.Is(got[1], context.Canceled)) || !(errors.Is(got[0], ErrClosed) || errors.Is(got[1], ErrClosed)) {
		t.Errorf("the handlers saw %v, want one cancelled, one closed", got)
	}
}
