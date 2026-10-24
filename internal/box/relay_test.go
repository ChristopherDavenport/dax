package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "io.example/executor"

// echoChild stands in for the executor in the box: it records what it
// is sent and answers each line with the line itself, so what comes
// back is exactly what went through.
type echoChild struct {
	inR  *io.PipeReader
	inW  *io.PipeWriter
	outR *io.PipeReader
	outW *io.PipeWriter

	mu   sync.Mutex
	got  bytes.Buffer
	done chan struct{}
	err  error
}

func newEchoChild(err error) *echoChild {
	c := &echoChild{done: make(chan struct{}), err: err}
	c.inR, c.inW = io.Pipe()
	c.outR, c.outW = io.Pipe()
	go func() {
		defer close(c.done)
		buf := make([]byte, 4096)
		for {
			n, rerr := c.inR.Read(buf)
			if n > 0 {
				c.mu.Lock()
				c.got.Write(buf[:n])
				c.mu.Unlock()
				c.outW.Write(buf[:n])
			}
			if rerr != nil {
				c.outW.Close()
				return
			}
		}
	}()
	return c
}

func (c *echoChild) Stdin() io.WriteCloser { return c.inW }
func (c *echoChild) Stdout() io.Reader     { return c.outR }
func (c *echoChild) Wait() error           { <-c.done; return c.err }
func (c *echoChild) sent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got.String()
}

// discover is a stateless client's first message, its capabilities in
// _meta.
func discover(id string, box any) string {
	exp := map[string]any{testKey: map[string]any{"maxReadBytes": 1024}}
	if box != nil {
		exp[testKey].(map[string]any)["box"] = box
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": "server/discover",
		"params": map[string]any{"_meta": map[string]any{clientCapabilitiesMeta: map[string]any{"experimental": exp}}}})
	return string(b) + "\n"
}

func initialize(id string, box any) string {
	exp := map[string]any{testKey: map[string]any{"maxReadBytes": 1024}}
	if box != nil {
		exp[testKey].(map[string]any)["box"] = box
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"experimental": exp}}})
	return string(b) + "\n"
}

func TestRelay(t *testing.T) {
	// What the session sends after initialize: a request, a response to
	// the server's own request, a notification, ids of every shape, and
	// a line longer than one read.
	rest := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":"a-7","method":"tools/list"}` + "\n" +
		`{"jsonrpc":"2.0","id":0,"result":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"dax/process.start","params":{"args":["x"]}}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"arguments":{"s":"` + strings.Repeat("x", 100<<10) + `"}}}` + "\n"
	for _, tc := range []struct {
		name    string
		first   string
		want    Request
		start   error
		exit    error
		err     string
		refused string // in the error answer, when start refuses
	}{
		{name: "no box asked: the default", first: initialize("1", nil)},
		{name: "a profile and tightenings", first: initialize(`"init"`, map[string]any{"profile": "go", "tighten": map[string]any{"memory": "1g", "internet": "off"}}), want: Request{Profile: "go", Tighten: Tighten{Memory: "1g", Internet: "off"}}},
		{name: "a refused box", first: initialize("1", map[string]any{"profile": "big"}), start: errors.New(`profile "big": this box host has go`), err: `profile "big"`, refused: `profile \"big\": this box host has go`},
		{name: "the executor's exit", first: initialize("1", nil), exit: errors.New("exit status 3"), err: "exit status 3"},
		{name: "server/discover first", first: discover("1", map[string]any{"profile": "go"}), want: Request{Profile: "go"}},
		{name: "a refused discover, and the initialize after it", first: discover("1", map[string]any{"profile": "big"}), start: errors.New(`profile "big"`), err: `profile "big"`, refused: `"id":"a-7","error"`},
		{name: "not initialize first", first: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n", err: "not initialize", refused: "not initialize"},
		{name: "not JSON", first: "hello\n", err: "not JSON-RPC"},
		{name: "a bad box request", first: initialize("1", "go"), err: "box request", refused: "box request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := newEchoChild(tc.exit)
			var asked *Request
			start := func(_ context.Context, r Request) (Child, error) {
				asked = &r
				if tc.start != nil {
					return nil, tc.start
				}
				return child, nil
			}
			var out bytes.Buffer
			in := strings.NewReader(tc.first + rest)
			done := make(chan error, 1)
			go func() { done <- Relay(context.Background(), in, &out, testKey, start) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Relay did not return once the session's input ended")
			}
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("Relay error = %v, want one naming %q", err, tc.err)
				}
				if tc.refused != "" {
					if !strings.Contains(out.String(), `"error"`) || !strings.Contains(out.String(), tc.refused) {
						t.Fatalf("the session was told %q, want an error naming %q", out.String(), tc.refused)
					}
					if child.sent() != "" {
						t.Errorf("the executor was sent %q after a refusal", child.sent())
					}
					// The session's response (id 0) is never answered.
					if strings.Contains(out.String(), `"id":0,`) {
						t.Errorf("a response was answered: %q", out.String())
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Relay: %v", err)
			}
			if asked == nil || !equalRequest(*asked, tc.want) {
				t.Fatalf("start asked for %+v, want %+v", asked, tc.want)
			}
			// Byte for byte, both ways: the first line as it came, and the
			// rest in order.
			if got, want := child.sent(), tc.first+rest; got != want {
				t.Fatalf("the executor was sent %d bytes, want %d (the session's own)", len(got), len(want))
			}
			if got, want := out.String(), tc.first+rest; got != want {
				t.Fatalf("the session was sent %d bytes, want %d (the executor's own)", len(got), len(want))
			}
		})
	}
}

func equalRequest(a, b Request) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func TestRelayFirstLine(t *testing.T) {
	for _, tc := range []struct {
		name, in, err string
	}{
		{"empty", "", "EOF"},
		{"no newline", initialize("1", nil)[:20], "without a newline"},
		{"too long", `{"x":"` + strings.Repeat("y", maxFirstLine+1) + "\"}\n", "longer than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := false
			err := Relay(context.Background(), strings.NewReader(tc.in), io.Discard, testKey, func(context.Context, Request) (Child, error) {
				started = true
				return nil, nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("Relay error = %v, want one naming %q", err, tc.err)
			}
			if started {
				t.Error("a box was made before the session said which")
			}
		})
	}
}
