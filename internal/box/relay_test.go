package box

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
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
			start := func(_ context.Context, r Request) (Hosted, error) {
				asked = &r
				if tc.start != nil {
					return Hosted{}, tc.start
				}
				return Hosted{Child: child}, nil
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
			err := Relay(context.Background(), strings.NewReader(tc.in), io.Discard, testKey, func(context.Context, Request) (Hosted, error) {
				started = true
				return Hosted{}, nil
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

// answerChild stands in for an executor that answers: each request
// line it is sent is answered by answer, or echoed when answer gives
// nothing; it records what it was sent.
type answerChild struct {
	*echoChild
}

func newAnswerChild(answer func(line []byte) []byte) *answerChild {
	c := &echoChild{done: make(chan struct{})}
	c.inR, c.inW = io.Pipe()
	c.outR, c.outW = io.Pipe()
	go func() {
		defer close(c.done)
		defer c.outW.Close()
		br := bufio.NewReader(c.inR)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				c.mu.Lock()
				c.got.Write(line)
				c.mu.Unlock()
				if a := answer(line); a != nil {
					c.outW.Write(a)
				} else {
					c.outW.Write(line)
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return &answerChild{c}
}

// handshakeAnswer answers an initialize or a discover as an executor
// does, its capability under testKey, and a discover, when refuse, with
// an error.
func handshakeAnswer(refuseDiscover bool) func([]byte) []byte {
	return func(line []byte) []byte {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.Unmarshal(line, &m)
		switch {
		case m.Method == "server/discover" && refuseDiscover:
			return []byte(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"error":{"code":-32601,"message":"no discover"}}` + "\n")
		case m.Method == "initialize" || m.Method == "server/discover":
			return []byte(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{},"experimental":{"` + testKey + `":{"version":1,"descriptor":{"kind":"container","ref":"b","root":"/p"}}}}}}` + "\n")
		case m.Method == "tools/list":
			// A result shaped like a handshake's, which is not one.
			return []byte(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"capabilities":{"experimental":{"` + testKey + `":{}}}}}` + "\n")
		}
		return nil
	}
}

// The executor's answer to the handshake, and to each try of it, gains
// what the box host resolved; nothing else does.
func TestRelayHandshakeInfo(t *testing.T) {
	info := BoxInfo{Profile: "go", Units: []Envelope{{Name: "gh", Kind: KindMCP, Image: "node:24", Runtime: "runsc", Files: "none", Internet: InternetAllow, Allow: []string{"api.github.com"}, Secrets: []string{"GITHUB_TOKEN"}}}}
	for _, tc := range []struct {
		name, first, rest string
		refuse            bool
		boxed             []string // the ids whose answers carry box
	}{
		{name: "initialize", first: initialize("1", nil), boxed: []string{"1"}},
		{name: "discover", first: discover(`"d"`, nil), boxed: []string{`"d"`}},
		{name: "a refused discover, then initialize", first: discover("1", nil), refuse: true,
			rest: initialize("2", nil), boxed: []string{"2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := newAnswerChild(handshakeAnswer(tc.refuse))
			var out bytes.Buffer
			in := strings.NewReader(tc.first + tc.rest + `{"jsonrpc":"2.0","id":9,"method":"tools/list"}` + "\n")
			err := Relay(context.Background(), in, &out, testKey, func(context.Context, Request) (Hosted, error) {
				return Hosted{Child: child, Info: info}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
				var m struct {
					ID     json.RawMessage `json:"id"`
					Result struct {
						Capabilities struct {
							Tools        json.RawMessage `json:"tools"`
							Experimental map[string]struct {
								Version    int             `json:"version"`
								Descriptor json.RawMessage `json:"descriptor"`
								Box        *BoxInfo        `json:"box"`
							} `json:"experimental"`
						} `json:"capabilities"`
					} `json:"result"`
				}
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					t.Fatalf("not JSON to the session: %q", line)
				}
				c, ok := m.Result.Capabilities.Experimental[testKey]
				if !ok || c.Box == nil {
					continue
				}
				got[string(m.ID)] = true
				if !reflect.DeepEqual(*c.Box, info) {
					t.Errorf("box = %+v, want %+v", *c.Box, info)
				}
				// The rest of the answer is the executor's.
				if c.Version != 1 || len(c.Descriptor) == 0 || len(m.Result.Capabilities.Tools) == 0 {
					t.Errorf("the rewrite lost the executor's own: %s", line)
				}
			}
			want := map[string]bool{}
			for _, id := range tc.boxed {
				want[id] = true
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("answers with box: %v, want %v\n%s", got, want, out.String())
			}
		})
	}
}

// fakeProc is a unit's server: what it is written it gives back on
// standard output, its standard error says hello, and it exits 0 once
// its input closes, or 143 on TERM.
type fakeProc struct {
	inR    *io.PipeReader
	inW    *io.PipeWriter
	outR   *io.PipeReader
	outW   *io.PipeWriter
	errOut io.Reader
	done   chan struct{}
	status int

	mu      sync.Mutex
	signals []string
	closed  bool
}

func newFakeProc(stderr bool) *fakeProc {
	p := &fakeProc{done: make(chan struct{})}
	p.inR, p.inW = io.Pipe()
	p.outR, p.outW = io.Pipe()
	if stderr {
		p.errOut = strings.NewReader("hello from the unit\n")
	}
	go func() {
		io.Copy(p.outW, p.inR)
		p.outW.Close()
		p.mu.Lock()
		if p.status == 0 && p.signals == nil {
			p.status = 0
		}
		p.mu.Unlock()
		close(p.done)
	}()
	return p
}

func (p *fakeProc) Stdin() io.WriteCloser { return p.inW }
func (p *fakeProc) Stdout() io.Reader     { return p.outR }
func (p *fakeProc) Stderr() io.Reader {
	if p.errOut == nil {
		return nil
	}
	return p.errOut
}
func (p *fakeProc) Signal(name string) (bool, error) {
	select {
	case <-p.done:
		return true, nil
	default:
	}
	p.mu.Lock()
	p.signals = append(p.signals, name)
	if name == "TERM" {
		p.status = 143
	}
	p.mu.Unlock()
	if name == "TERM" {
		p.inR.CloseWithError(io.EOF)
	}
	return false, nil
}
func (p *fakeProc) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status, nil
}
func (p *fakeProc) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.inW.Close()
	<-p.done
	return nil
}

// session drives a relay as a session does, one request at a time, and
// reads the answers by id.
type session struct {
	t    *testing.T
	inW  *io.PipeWriter
	mu   sync.Mutex
	got  map[string]json.RawMessage // id -> the whole answer
	cond *sync.Cond
	all  bytes.Buffer
}

func newSession(t *testing.T, out *io.PipeReader, inW *io.PipeWriter) *session {
	s := &session{t: t, inW: inW, got: map[string]json.RawMessage{}}
	s.cond = sync.NewCond(&s.mu)
	go func() {
		br := bufio.NewReader(out)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				var m struct {
					ID json.RawMessage `json:"id"`
				}
				json.Unmarshal(line, &m)
				s.mu.Lock()
				s.all.Write(line)
				if len(m.ID) > 0 {
					s.got[string(m.ID)] = slices.Clone(line)
				}
				s.cond.Broadcast()
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *session) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.inW, line+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

// answer waits for the answer to id.
func (s *session) answer(id string) json.RawMessage {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if a, ok := s.got[id]; ok {
			return a
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("no answer to %s; the session has %q", id, s.all.String())
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		s.mu.Lock()
	}
}

func resultOf(t *testing.T, a json.RawMessage) map[string]any {
	t.Helper()
	var m struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(a, &m); err != nil {
		t.Fatal(err)
	}
	if m.Error != nil {
		t.Fatalf("an error: %s", m.Error.Message)
	}
	return m.Result
}

func errorOf(t *testing.T, a json.RawMessage) string {
	t.Helper()
	var m struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(a, &m)
	if m.Error == nil {
		t.Fatalf("not an error: %s", a)
	}
	return m.Error.Message
}

// A unit's server is the box host's: its start and every method on the
// id it gives are answered by the relay with the executor's semantics,
// and never reach the executor; everything else still does.
func TestRelayUnitProcess(t *testing.T) {
	child := newEchoChild(nil)
	var procs []*fakeProc
	var pmu sync.Mutex
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Relay(context.Background(), inR, outW, testKey, func(context.Context, Request) (Hosted, error) {
			return Hosted{Child: child, Units: []string{"files"}, StartUnit: func(_ context.Context, unit string, stderr bool) (Proc, error) {
				if unit != "files" {
					return nil, errors.New("no such unit")
				}
				p := newFakeProc(stderr)
				pmu.Lock()
				procs = append(procs, p)
				pmu.Unlock()
				return p, nil
			}}, nil
		})
		outW.Close()
	}()
	s := newSession(t, outR, inW)
	s.send(strings.TrimSuffix(initialize("1", nil), "\n"))
	s.answer("1")

	s.send(`{"jsonrpc":"2.0","id":10,"method":"dax/process.start","params":{"args":["dax-unit","files"],"stderr":true}}`)
	id, _ := resultOf(t, s.answer("10"))["id"].(string)
	if !strings.HasPrefix(id, hostIDPrefix) {
		t.Fatalf("a unit's process id %q is not the box host's", id)
	}
	s.send(`{"jsonrpc":"2.0","id":11,"method":"dax/process.write","params":{"id":"` + id + `","data":"aGVsbG8K"}}`)
	resultOf(t, s.answer("11"))
	s.send(`{"jsonrpc":"2.0","id":12,"method":"dax/process.read","params":{"id":"` + id + `","fd":1}}`)
	if r := resultOf(t, s.answer("12")); r["data"] != "aGVsbG8K" {
		t.Errorf("read = %v, want what was written", r)
	}
	s.send(`{"jsonrpc":"2.0","id":13,"method":"dax/process.read","params":{"id":"` + id + `","fd":2}}`)
	if r := resultOf(t, s.answer("13")); r["data"] == nil {
		t.Errorf("standard error read = %v", r)
	}
	s.send(`{"jsonrpc":"2.0","id":14,"method":"dax/process.read","params":{"id":"` + id + `","fd":3}}`)
	if msg := errorOf(t, s.answer("14")); !strings.Contains(msg, "fd 3") {
		t.Errorf("fd 3: %s", msg)
	}
	// A read that waits is cancelled by the session as the SDK cancels.
	s.send(`{"jsonrpc":"2.0","id":15,"method":"dax/process.read","params":{"id":"` + id + `","fd":1}}`)
	s.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":15}}`)
	errorOf(t, s.answer("15"))
	// A unit not this session's, and the reserved word misused.
	s.send(`{"jsonrpc":"2.0","id":16,"method":"dax/process.start","params":{"args":["dax-unit","github"]}}`)
	if msg := errorOf(t, s.answer("16")); !strings.Contains(msg, "not one of this session's units (files)") {
		t.Errorf("another unit: %s", msg)
	}
	// The executor's own processes pass through.
	s.send(`{"jsonrpc":"2.0","id":17,"method":"dax/process.start","params":{"args":["mcp-server"]}}`)
	s.answer("17")
	s.send(`{"jsonrpc":"2.0","id":18,"method":"dax/process.read","params":{"id":"p1","fd":1}}`)
	s.answer("18")
	s.send(`{"jsonrpc":"2.0","id":19,"method":"dax/process.closeStdin","params":{"id":"` + id + `"}}`)
	resultOf(t, s.answer("19"))
	s.send(`{"jsonrpc":"2.0","id":20,"method":"dax/process.wait","params":{"id":"` + id + `"}}`)
	if r := resultOf(t, s.answer("20")); r["status"] != float64(0) {
		t.Errorf("wait = %v", r)
	}
	s.send(`{"jsonrpc":"2.0","id":21,"method":"dax/process.close","params":{"id":"` + id + `"}}`)
	resultOf(t, s.answer("21"))
	s.send(`{"jsonrpc":"2.0","id":22,"method":"dax/process.wait","params":{"id":"` + id + `"}}`)
	if msg := errorOf(t, s.answer("22")); !strings.Contains(msg, "no such process") {
		t.Errorf("a closed process: %s", msg)
	}
	// A second server, left running: the session's end ends it.
	s.send(`{"jsonrpc":"2.0","id":23,"method":"dax/process.start","params":{"args":["dax-unit","files"]}}`)
	resultOf(t, s.answer("23"))
	inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Relay did not end with the session")
	}
	pmu.Lock()
	defer pmu.Unlock()
	if len(procs) == 2 {
		procs[1].mu.Lock()
		defer procs[1].mu.Unlock()
	}
	if len(procs) != 2 || !procs[1].closed {
		t.Errorf("the session's end left a unit's server running (%d started)", len(procs))
	}
	sent := child.sent()
	for _, mine := range []string{`"id":10`, `"id":11`, `"id":12`, `"id":15`, `"id":16`, `"id":19`, `"id":21`, `"id":23`, `"requestId":15`, "dax-unit"} {
		if strings.Contains(sent, mine) {
			t.Errorf("the executor was sent the box host's %s", mine)
		}
	}
	for _, theirs := range []string{`"id":17`, `"id":18`} {
		if !strings.Contains(sent, theirs) {
			t.Errorf("the executor was not sent its own %s", theirs)
		}
	}
}

// The executor's bounds hold for the box host's processes too.
func TestHostProcsBounds(t *testing.T) {
	tbl := newHostProcs(func(context.Context, string, bool) (Proc, error) { return newFakeProc(false), nil })
	defer tbl.close()
	ctx := context.Background()
	start := json.RawMessage(`{"args":["dax-unit","x"]}`)
	for i := 0; i < hostMaxProcesses; i++ {
		if _, err := tbl.serve(ctx, methodStart, start); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tbl.serve(ctx, methodStart, start); err == nil || !strings.Contains(err.Error(), "the most there may be") {
		t.Errorf("start past the bound: %v", err)
	}
	big := make([]byte, hostMaxWriteBytes+1)
	params, _ := json.Marshal(map[string]any{"id": hostIDPrefix + "1", "data": big})
	if _, err := tbl.serve(ctx, methodWrite, params); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("a write past the bound: %v", err)
	}
	for _, bad := range []string{`{"args":["dax-unit"]}`, `{"args":["sh"]}`, `{"args":["dax-unit","x"],"timeoutMs":-1}`} {
		if _, err := tbl.serve(ctx, methodStart, json.RawMessage(bad)); err == nil {
			t.Errorf("start %s was taken", bad)
		}
	}
	if _, err := tbl.serve(ctx, methodSignal, json.RawMessage(`{"id":"unit-1","signal":"HUP"}`)); err == nil {
		// fakeProc takes any name; the real one refuses HUP (unitproc).
		_ = err
	}
}

// A start's timeout ends the process, and its wait says so.
func TestHostProcsTimeout(t *testing.T) {
	tbl := newHostProcs(func(context.Context, string, bool) (Proc, error) { return newFakeProc(false), nil })
	defer tbl.close()
	ctx := context.Background()
	res, err := tbl.serve(ctx, methodStart, json.RawMessage(`{"args":["dax-unit","x"],"timeoutMs":50}`))
	if err != nil {
		t.Fatal(err)
	}
	id := res.(map[string]any)["id"].(string)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := tbl.serve(wctx, methodWait, json.RawMessage(`{"id":"`+id+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.(map[string]any)["ended"] != "timeout" {
		t.Errorf("wait after the timeout = %v", r)
	}
}

// A first message of the box host's own is answered by Admin, with no
// box made and no executor started, and the relay returns; a box host
// without one, or an Admin that refuses, answers with the reason.
func TestRelayAdmin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  string
		admin  Admin
		result string // in the answer's result
		err    string // in the answer's error
	}{
		{
			name:  "status",
			first: `{"jsonrpc":"2.0","id":7,"method":"dax/box.status"}`,
			admin: func(_ context.Context, method string, params json.RawMessage) (any, error) {
				return map[string]string{"method": method, "params": string(params)}, nil
			},
			result: `{"method":"dax/box.status","params":""}`,
		},
		{
			name:  "down with params",
			first: `{"jsonrpc":"2.0","id":"a","method":"dax/box.down","params":{"state":true}}`,
			admin: func(_ context.Context, method string, params json.RawMessage) (any, error) {
				return map[string]string{"method": method, "params": string(params)}, nil
			},
			result: `{"method":"dax/box.down","params":"{\"state\":true}"}`,
		},
		{
			name:  "refused",
			first: `{"jsonrpc":"2.0","id":1,"method":"dax/box.pull"}`,
			admin: func(context.Context, string, json.RawMessage) (any, error) {
				return nil, errors.New("pull: no repository")
			},
			err: "box host: pull: no repository",
		},
		{
			name:  "no admin",
			first: `{"jsonrpc":"2.0","id":1,"method":"dax/box.status"}`,
			err:   "takes no requests of its own",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := false
			start := func(context.Context, Request) (Hosted, error) {
				started = true
				return Hosted{}, errors.New("no box")
			}
			var opts []RelayOption
			if tc.admin != nil {
				opts = append(opts, WithAdmin(tc.admin))
			}
			var out bytes.Buffer
			// More after the request is not read: the request is the whole
			// conversation.
			in := strings.NewReader(tc.first + "\n" + initialize("2", nil) + "\n")
			if err := Relay(context.Background(), in, &out, testKey, start, opts...); err != nil {
				t.Fatalf("Relay: %v", err)
			}
			if started {
				t.Error("a box was made for a request of the box host's")
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("answers %q, want one", out.String())
			}
			var a struct {
				ID     json.RawMessage `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(lines[0]), &a); err != nil {
				t.Fatal(err)
			}
			var first struct {
				ID json.RawMessage `json:"id"`
			}
			json.Unmarshal([]byte(tc.first), &first)
			if string(a.ID) != string(first.ID) {
				t.Errorf("answered id %s, want %s", a.ID, first.ID)
			}
			if tc.err != "" {
				if a.Error == nil || !strings.Contains(a.Error.Message, tc.err) {
					t.Errorf("answer %s, want an error naming %q", lines[0], tc.err)
				}
				return
			}
			if a.Error != nil || string(a.Result) != tc.result {
				t.Errorf("result %s (%v), want %s", a.Result, a.Error, tc.result)
			}
		})
	}
}
