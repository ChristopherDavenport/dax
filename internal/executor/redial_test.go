package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpserver"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/tool"
)

// currentOf is r's current connection, failing the test without one.
func currentOf(t *testing.T, r *Remote) *conn {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		t.Fatal("no current connection")
	}
	return r.cur
}

// sessionOf is the session of r's current connection.
func sessionOf(r *Remote) *sdk.ClientSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur.c.Session()
}

// waitLost waits up to 10 seconds for r's current connection to be
// known lost, as a request finds it before it is sent.
func waitLost(t *testing.T, r *Remote) {
	t.Helper()
	cn := currentOf(t, r)
	select {
	case <-cn.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the connection is not known lost")
	}
}

// counter is an extension whose tools count their runs: "count" at
// once, "slow" once it has started and until its call ends, and "said"
// says what the executor serving it is told to say. Its definitions
// and claims are what desc and readOnly say.
type counter struct {
	runs, slow atomic.Int32
	started    chan struct{}
}

func newCounter() *counter { return &counter{started: make(chan struct{}, 16)} }

func (c *counter) ext(desc string, readOnly ...string) extension.Extension {
	return extension.Extension{
		Name: "acme",
		Tools: func(extension.ToolEnv) []agenttool.Tool {
			return []agenttool.Tool{
				agenttool.NewFunc("count", desc, json.RawMessage(`{"type":"object"}`), func(context.Context, agenttool.Call) (agenttool.Result, error) {
					return agenttool.Text("count " + string(rune('0'+c.runs.Add(1)))), nil
				}),
				agenttool.NewFunc("slow", "Wait.", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, _ agenttool.Call) (agenttool.Result, error) {
					c.slow.Add(1)
					c.started <- struct{}{}
					<-ctx.Done()
					return agenttool.Result{}, ctx.Err()
				}),
				plain("status", agenttool.WithReplay(func(context.Context, json.RawMessage) agenttool.Replay { return agenttool.ReplaySafe })),
			}
		},
		ReadOnly: readOnly,
	}
}

// serverOver is NewServer of exts over a local workspace at dir, as d.
func serverOver(t *testing.T, dir string, d func(root string) workspace.Descriptor, exts ...extension.Extension) *Server {
	t.Helper()
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	srv, err := NewServer(ServeOptions{Name: "dax", Version: "test", Descriptor: d(ws.Root())}, fixed(exts...), extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// pipeBox is an executor its Dialer reaches as a command's would: each
// dial is a new connection over pipes to a server, srvs[n] for the nth
// and the last for every one after, and cut severs one as a process
// killed would, with no word to either side.
type pipeBox struct {
	t *testing.T

	mu    sync.Mutex
	srvs  []connector
	dials int
	links []*pipeLink
	gate  chan struct{} // when set, a dial waits for it to close
	fail  error         // when set, a dial fails with it
}

type pipeLink struct {
	ss    *sdk.ServerSession
	ends  []io.Closer
	cutOf sync.Once
}

func (b *pipeBox) dial(ctx context.Context) (sdk.Transport, error) {
	b.mu.Lock()
	srv := b.srvs[min(b.dials, len(b.srvs)-1)]
	b.dials++
	gate, fail := b.gate, b.fail
	b.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail != nil {
		return nil, fail
	}
	cr, sw := io.Pipe() // the server's output, the client's input
	sr, cw := io.Pipe() // the client's output, the server's input
	ss, err := srv.Connect(ctx, &sdk.IOTransport{Reader: sr, Writer: sw}, nil)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.links = append(b.links, &pipeLink{ss: ss, ends: []io.Closer{cr, sw, sr, cw}})
	b.mu.Unlock()
	b.t.Cleanup(func() { ss.Close() })
	return &sdk.IOTransport{Reader: cr, Writer: cw}, nil
}

// cut severs the ith connection.
func (b *pipeBox) cut(i int) {
	b.mu.Lock()
	l := b.links[i]
	b.mu.Unlock()
	l.cut()
}

// cutSession severs the connection ss serves.
func (b *pipeBox) cutSession(ss *sdk.ServerSession) {
	b.mu.Lock()
	var l *pipeLink
	for _, x := range b.links {
		if x.ss == ss {
			l = x
		}
	}
	b.mu.Unlock()
	if l != nil {
		l.cut()
	}
}

func (l *pipeLink) cut() {
	l.cutOf.Do(func() {
		for _, e := range l.ends {
			e.Close()
		}
	})
}

func (b *pipeBox) dialed() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dials
}

// warned collects a Remote's warnings.
type warned struct {
	mu    sync.Mutex
	lines []string
}

func (w *warned) warn(line string) {
	w.mu.Lock()
	w.lines = append(w.lines, line)
	w.mu.Unlock()
}

func (w *warned) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.lines, "\n")
}

// dialBox is Dial of b, with its warnings.
func dialBox(t *testing.T, b *pipeBox) (*Remote, *warned) {
	t.Helper()
	b.t = t
	var w warned
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Dial(ctx, b.dial, ConnectOptions{Name: "dax", Version: "test", Warn: w.warn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, &w
}

// testBox is a pipeBox over one server of dax-coding and counter's
// tools over a fresh directory with notes.txt in it.
func testBox(t *testing.T) (*pipeBox, *counter, string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes\n"), 0o644)
	c := newCounter()
	return &pipeBox{srvs: []connector{serverOver(t, dir, testDescriptor, coding.New(0), c.ext("Count."))}}, c, dir
}

var (
	countCall = Call{Tool: "count", Call: agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)}}
	readCall  = Call{Tool: "read", Call: agenttool.Call{ID: "r", Args: json.RawMessage(`{"path":"notes.txt"}`)}}
)

// When the connection is lost (the process that served it killed), the
// next request of every kind dials again before it is sent and goes on
// over the new connection, and the user is told.
func TestALostConnectionIsDialedAgainBeforeTheNextRequest(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		do   func(r *Remote) error
	}{
		{"call", func(r *Remote) error {
			res, err := r.Call(ctx, countCall)
			if err == nil && !strings.Contains(res.Output.String(), "count") {
				err = errors.New("output " + res.Output.String())
			}
			return err
		}},
		{"facts", func(r *Remote) error {
			f, err := r.Facts(ctx, readCall)
			if err == nil && !strings.Contains(string(f.Rewrite), "dax_stamp") {
				err = errors.New("no stamp")
			}
			return err
		}},
		{"batch", func(r *Remote) error {
			_, errs, err := r.BatchFacts(ctx, []Call{readCall})
			if err == nil {
				err = errs[0]
			}
			return err
		}},
		{"replay", func(r *Remote) error {
			if got := r.Replay(ctx, Call{Tool: "status", Call: agenttool.Call{Args: json.RawMessage(`{}`)}}); got != agenttool.ReplaySafe {
				return errors.New("replay " + got.String())
			}
			return nil
		}},
		{"files", func(r *Remote) error {
			_, err := fs.ReadFile(r.FS(), "notes.txt")
			return err
		}},
		{"start", func(r *Remote) error {
			p, err := r.Start(ctx, workspace.Command{Args: []string{"true"}})
			if err != nil {
				return err
			}
			_, err = p.Wait()
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, _ := testBox(t)
			r, w := dialBox(t, b)
			if err := tc.do(r); err != nil {
				t.Fatal(err)
			}
			b.cut(0)
			waitLost(t, r)
			if err := tc.do(r); err != nil {
				t.Errorf("after the cut: %v", err)
			}
			if n := b.dialed(); n != 2 {
				t.Errorf("%d dials, want 2", n)
			}
			if got := w.String(); !strings.Contains(got, "executor connection lost; reconnected to dax test") || !strings.Contains(got, "container box") {
				t.Errorf("warned %q", got)
			}
		})
	}
}

// Over HTTP, a restarted server, which has forgotten the session, is
// connected to again: a request right after the restart that has no
// effect is read again on the new connection, and a call once the
// client knows its session gone runs on the new one.
func TestARestartedHTTPExecutorIsConnectedAgain(t *testing.T) {
	s := newServer(t)
	os.WriteFile(filepath.Join(s.dir, "notes.txt"), []byte("notes\n"), 0o644)
	var h atomic.Pointer[http.Handler]
	done := make(chan struct{})
	first := Handler(s.srv, done)
	h.Store(&first)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { (*h.Load()).ServeHTTP(w, req) }))
	var dials atomic.Int32
	dial := func(context.Context) (sdk.Transport, error) {
		dials.Add(1)
		return HTTPTransport(ts.URL), nil
	}
	var w warned
	ctx := context.Background()
	r, err := Dial(ctx, dial, ConnectOptions{Name: "dax", Version: "test", Warn: w.warn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.Close()
		close(done)
		ts.CloseClientConnections()
		ts.Close()
	})
	restart := func() {
		next := Handler(s.srv, done)
		h.Store(&next)
		for _, ss := range s.srv.Sessions() {
			ss.Close()
		}
	}
	if _, err := r.Call(ctx, readCall); err != nil {
		t.Fatal(err)
	}

	restart()
	if f, err := r.Facts(ctx, readCall); err != nil || !strings.Contains(string(f.Rewrite), "dax_stamp") {
		t.Errorf("a reading right after the restart: %+v, %v", f, err)
	}
	if n := dials.Load(); n != 2 {
		t.Errorf("%d dials, want 2", n)
	}

	restart()
	waitLost(t, r)
	res, err := r.Call(ctx, readCall)
	if err != nil || !strings.Contains(res.Output.String(), "notes") {
		t.Errorf("a call after the second restart: %q, %v", res.Output.String(), err)
	}
	if n := dials.Load(); n != 3 {
		t.Errorf("%d dials, want 3", n)
	}
	if got := w.String(); strings.Count(got, "reconnected") != 2 {
		t.Errorf("warned %q", got)
	}
}

// A call in flight when the connection is lost returns an error that
// says it may have run, and is not sent again: the executor ran it
// once. The next call reconnects.
func TestACallCutInFlightIsNotSentAgain(t *testing.T) {
	ctx := context.Background()
	b, c, _ := testBox(t)
	r, _ := dialBox(t, b)
	errc := make(chan error, 1)
	go func() {
		_, err := r.Call(ctx, Call{Tool: "slow", Call: agenttool.Call{ID: "s", Args: json.RawMessage(`{}`)}})
		errc <- err
	}()
	<-c.started
	b.cut(0)
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "may or may not have run") {
			t.Errorf("the cut call: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cut call did not return")
	}
	if _, err := r.Call(ctx, countCall); err != nil {
		t.Errorf("the next call: %v", err)
	}
	if n := c.slow.Load(); n != 1 {
		t.Errorf("slow ran %d times, want 1", n)
	}
	if n := b.dialed(); n != 2 {
		t.Errorf("%d dials, want 2", n)
	}
}

// A reading of facts cut in flight, which has no effect, is read again
// once on a new connection; one cut again is an error, not a third
// try.
func TestAFactsReadingCutInFlightIsReadAgainOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cuts      int32 // the readings cut, first first
		ok        bool
		readings  int32
		wantDials int
	}{
		{"cut once", 1, true, 2, 2},
		{"cut twice", 2, false, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, c, dir := testBox(t)
			srv := serverOver(t, dir, testDescriptor, coding.New(0), c.ext("Count."))
			var readings atomic.Int32
			srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
				return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
					if method == mcpserver.FactsMethod {
						if readings.Add(1) <= tc.cuts {
							b.cutSession(req.GetSession().(*sdk.ServerSession))
							<-ctx.Done()
							return nil, ctx.Err()
						}
					}
					return next(ctx, method, req)
				}
			})
			b.srvs = []connector{srv}
			r, _ := dialBox(t, b)
			f, err := r.Facts(context.Background(), readCall)
			if ok := err == nil && strings.Contains(string(f.Rewrite), "dax_stamp"); ok != tc.ok {
				t.Errorf("facts %+v, %v", f, err)
			}
			if n := readings.Load(); n != tc.readings {
				t.Errorf("%d readings, want %d", n, tc.readings)
			}
			if n := b.dialed(); n != tc.wantDials {
				t.Errorf("%d dials, want %d", n, tc.wantDials)
			}
		})
	}
}

// A redial that reaches another executor (another workspace, other
// tools, a tool defined or claimed otherwise, or no executor at all)
// is refused, with a warning, and every request after fails without
// dialing again: the model was offered the first executor's tools, and
// the record names its workspace.
func TestAnotherExecutorIsRefusedOnRedial(t *testing.T) {
	for _, tc := range []struct {
		name  string
		other func(t *testing.T, dir string, c *counter) connector
		wants string
	}{
		{"another workspace", func(t *testing.T, dir string, c *counter) connector {
			return serverOver(t, dir, func(root string) workspace.Descriptor {
				return workspace.Descriptor{Kind: workspace.KindContainer, Ref: "other", Root: root}
			}, coding.New(0), c.ext("Count."))
		}, "container other"},
		{"other extensions", func(t *testing.T, dir string, c *counter) connector {
			return serverOver(t, dir, testDescriptor, c.ext("Count."))
		}, "it runs the extensions [acme]"},
		{"another tool", func(t *testing.T, dir string, c *counter) connector {
			more := c.ext("Count.")
			tools := more.Tools
			more.Tools = func(env extension.ToolEnv) []agenttool.Tool { return append(tools(env), plain("rm")) }
			return serverOver(t, dir, testDescriptor, coding.New(0), more)
		}, "it runs the tools"},
		{"a tool defined otherwise", func(t *testing.T, dir string, c *counter) connector {
			return serverOver(t, dir, testDescriptor, coding.New(0), c.ext("Count twice."))
		}, `its tool "count"`},
		{"a tool claimed otherwise", func(t *testing.T, dir string, c *counter) connector {
			return serverOver(t, dir, testDescriptor, coding.New(0), c.ext("Count.", "count"))
		}, `its tool "count"`},
		{"no executor", func(t *testing.T, dir string, c *counter) connector {
			return fake(t, nil, true, plain("count"))
		}, "not a dax executor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, c, dir := testBox(t)
			b.srvs = append(b.srvs, tc.other(t, dir, c))
			r, w := dialBox(t, b)
			b.cut(0)
			waitLost(t, r)
			for i := range 3 {
				_, err := r.Call(ctx, countCall)
				if !errors.Is(err, ErrChanged) || !strings.Contains(err.Error(), tc.wants) || !strings.Contains(err.Error(), "restart dax") {
					t.Errorf("call %d: %v", i, err)
				}
			}
			if _, err := r.Facts(ctx, readCall); !errors.Is(err, ErrChanged) {
				t.Errorf("facts: %v", err)
			}
			if _, err := fs.Stat(r.FS(), "notes.txt"); !errors.Is(err, ErrChanged) {
				t.Errorf("files: %v", err)
			}
			if n := b.dialed(); n != 2 {
				t.Errorf("%d dials, want 2", n)
			}
			if n := c.runs.Load(); n != 0 {
				t.Errorf("count ran %d times", n)
			}
			if got := w.String(); !strings.Contains(got, "not the one this session started with") || strings.Contains(got, "reconnected") {
				t.Errorf("warned %q", got)
			}
		})
	}
}

// Requests that find the connection lost together share one redial,
// whether it succeeds or fails; after one failed, the next request
// dials again.
func TestConcurrentRequestsShareOneRedial(t *testing.T) {
	const n = 8
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		fail error
	}{
		{"it succeeds", nil},
		{"it fails", errors.New("no route to the box")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _, _ := testBox(t)
			r, _ := dialBox(t, b)
			b.cut(0)
			waitLost(t, r)
			gate := make(chan struct{})
			b.mu.Lock()
			b.gate, b.fail = gate, tc.fail
			b.mu.Unlock()
			errs := make(chan error, n)
			for i := range n {
				go func() {
					var err error
					if i%2 == 0 {
						_, err = r.Call(ctx, countCall)
					} else {
						_, err = r.Facts(ctx, readCall)
					}
					errs <- err
				}()
			}
			time.Sleep(100 * time.Millisecond)
			close(gate)
			for range n {
				err := <-errs
				if tc.fail == nil && err != nil {
					t.Errorf("a request: %v", err)
				}
				if tc.fail != nil && (!errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "next call will try again")) {
					t.Errorf("a request: %v", err)
				}
			}
			if got := b.dialed(); got != 2 {
				t.Errorf("%d dials, want 2", got)
			}
			if tc.fail != nil {
				b.mu.Lock()
				b.gate, b.fail = nil, nil
				b.mu.Unlock()
				if _, err := r.Call(ctx, countCall); err != nil {
					t.Errorf("the next call: %v", err)
				}
				if got := b.dialed(); got != 3 {
					t.Errorf("%d dials, want 3", got)
				}
			}
		})
	}
}

// Close stops a redial in flight, and nothing is dialed after it,
// whether the connection was lost before it or not.
func TestCloseStopsRedialing(t *testing.T) {
	ctx := context.Background()
	t.Run("after a drop", func(t *testing.T) {
		b, _, _ := testBox(t)
		r, _ := dialBox(t, b)
		b.cut(0)
		waitLost(t, r)
		r.Close()
		if _, err := r.Call(ctx, countCall); !errors.Is(err, errRemoteClosed) {
			t.Errorf("a call after Close: %v", err)
		}
		if n := b.dialed(); n != 1 {
			t.Errorf("%d dials, want 1", n)
		}
	})
	t.Run("during a redial", func(t *testing.T) {
		b, _, _ := testBox(t)
		r, _ := dialBox(t, b)
		b.cut(0)
		waitLost(t, r)
		b.mu.Lock()
		b.gate = make(chan struct{}) // never opened
		b.mu.Unlock()
		errc := make(chan error, 1)
		go func() {
			_, err := r.Call(ctx, countCall)
			errc <- err
		}()
		for b.dialed() < 2 {
			time.Sleep(5 * time.Millisecond)
		}
		closed := make(chan struct{})
		go func() { r.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			t.Fatal("Close waited on the redial")
		}
		if err := <-errc; err == nil {
			t.Error("the call waiting on the redial ran")
		}
		if _, err := r.Call(ctx, countCall); !errors.Is(err, errRemoteClosed) {
			t.Errorf("a call after Close: %v", err)
		}
		if n := b.dialed(); n != 2 {
			t.Errorf("%d dials, want 2", n)
		}
	})
}

// A process started over a connection that is lost ends with it: its
// reads and its wait fail, saying so, rather than wait, and it is not
// started again on the new connection.
func TestAProcessEndsWithItsConnection(t *testing.T) {
	b, _, _ := testBox(t)
	r, _ := dialBox(t, b)
	p := start(t, r, sleeper)
	pidOf(t, p)
	b.cut(0)
	done := make(chan error, 1)
	go func() {
		_, err := p.Stdout().Read(make([]byte, 16))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "the process ended with it") {
			t.Errorf("a read: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a read waited")
	}
	waited := make(chan error, 1)
	go func() {
		_, err := p.Wait()
		waited <- err
	}()
	select {
	case err := <-waited:
		if err == nil {
			t.Error("the wait gave no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait waited")
	}
	if _, err := p.Stdin().Write([]byte("x\n")); err == nil || !strings.Contains(err.Error(), "dax does not start it again") {
		t.Errorf("a write: %v", err)
	}
}
