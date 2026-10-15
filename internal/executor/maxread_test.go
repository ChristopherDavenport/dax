package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/tool"
)

// bigFile is the size of big.txt, which every read limit in these
// tests cuts short of.
const bigFile = 20000

// limitServer is NewServer with dax-coding at each session's read
// limit under ceiling, over a fresh directory holding big.txt.
func limitServer(t *testing.T, ceiling int64, exts ...extension.Extension) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	line := strings.Repeat("x", 99) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat(line, bigFile/len(line))), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	srv, err := NewServer(ServeOptions{Name: "dax", Version: "test", Descriptor: testDescriptor(ws.Root()), MaxReadBytes: ceiling},
		withCoding(exts...), extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, dir
}

// connectAsking is Connect to srv in memory, asking for asked.
func connectAsking(t *testing.T, srv *Server, asked int64) (*Remote, *warned) {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	var w warned
	r, err := Connect(ctx, ct, ConnectOptions{Name: "dax", Version: "test", Warn: w.warn, MaxReadBytes: asked})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, &w
}

// readBig is read's output of big.txt through r.
func readBig(t *testing.T, r *Remote) string {
	t.Helper()
	res, err := r.Call(context.Background(), Call{Tool: "read", Call: agenttool.Call{ID: "r", Args: json.RawMessage(`{"path":"big.txt"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	return tool.Text(res)
}

// cutAt is what read says when it stops at limit bytes of big.txt.
func cutAt(limit int64) string {
	return fmt.Sprintf("stopped after the first %d of %d bytes", limit, bigFile)
}

// A session's tools read what it asked for, up to the executor's
// ceiling, which is dax's default when the executor's flag is unset;
// the capability says what applies, and the session is warned when it
// is less than it asked, with the flag that would allow more.
func TestASessionReadsWhatItAskedUpToTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ceiling, asked int64
		want           int64
		warns          string // "" for none
	}{
		{"below the ceiling", 8192, 4096, 4096, ""},
		{"at the ceiling", 8192, 8192, 8192, ""},
		{"above the ceiling", 8192, 16384, 8192, "pass `dax execute -max-read-bytes 16384` where it runs"},
		{"the default under an unset flag", 0, 0, tool.DefaultMaxRead, ""},
		{"above the default under an unset flag", 0, 3 << 20, tool.DefaultMaxRead, fmt.Sprintf("caps it at %d bytes", tool.DefaultMaxRead)},
		{"the default under a lower flag", 4096, 0, 4096, "max_read_bytes is the default"},
		{"the default under a higher flag", 8 << 20, 0, tool.DefaultMaxRead, ""},
		{"below the default under an unset flag", 0, 4096, 4096, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := limitServer(t, tc.ceiling)
			r, w := connectAsking(t, srv, tc.asked)
			if got := r.first.maxRead; got != tc.want {
				t.Errorf("the capability says %d, want %d", got, tc.want)
			}
			out := readBig(t, r)
			if tc.want < bigFile && !strings.Contains(out, cutAt(tc.want)) {
				t.Errorf("read is not cut at %d: %q", tc.want, out[max(0, len(out)-200):])
			}
			if tc.want >= bigFile && strings.Contains(out, "stopped after") {
				t.Errorf("read is cut: %q", out[max(0, len(out)-200):])
			}
			switch got := w.String(); {
			case tc.warns == "" && got != "":
				t.Errorf("warned %q", got)
			case tc.warns != "" && (!strings.Contains(got, tc.warns) || !strings.Contains(got, "dax test")):
				t.Errorf("warned %q, want it to say %q", got, tc.warns)
			}
		})
	}
}

// A client older than the read limit asks nothing, and its session's
// tools read what the executor's flag says, as every session's did
// before: its ceiling, dax's default when unset.
func TestAnOlderClientGetsTheFlagsValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ceiling int64
		want    int64
	}{
		{"a flag", 4096, 4096},
		{"a flag above the default", 8 << 20, 8 << 20},
		{"no flag", 0, tool.DefaultMaxRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := limitServer(t, tc.ceiling)
			ctx := context.Background()
			st, ct := sdk.NewInMemoryTransports()
			ss, err := srv.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ss.Close() })
			// mcpclient as dax before the setting connected: no
			// capability of dax's own.
			c, err := mcpclient.Connect(ctx, ct, mcpclient.WithClaims(), mcpclient.WithNotificationGrace(0))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			raw, _ := json.Marshal(c.Session().InitializeResult().Capabilities.Experimental[CapabilityKey])
			var cp capability
			if err := json.Unmarshal(raw, &cp); err != nil || cp.MaxReadBytes != tc.want {
				t.Errorf("capability says %d (%v), want %d", cp.MaxReadBytes, err, tc.want)
			}
			res, err := remoteTool(t, c, "read").Execute(ctx, agenttool.Call{Args: json.RawMessage(`{"path":"big.txt"}`)})
			if err != nil {
				t.Fatal(err)
			}
			if out := tool.Text(res); tc.want < bigFile != strings.Contains(out, cutAt(tc.want)) {
				t.Errorf("read: %q", out[max(0, len(out)-200):])
			}
		})
	}
}

// An executor older than the read limit says none: a session that set
// one is told that it does not reach the executor's tools, and one that
// left the default is told nothing.
func TestAnOlderExecutorIsWarnedOfWhenTheSessionSetALimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		asked int64
		warns bool
	}{
		{"a limit", 4096, true},
		{"the default", 0, false},
		{"the default, set", tool.DefaultMaxRead, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := toolEnv(t)
			srv, err := NewServer(ServeOptions{Name: "dax", Version: "test"}, withCoding(), env)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { srv.Close() })
			x, c, err := srv.build(srv.ceiling)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { x.Close() })
			// What dax execute said before the field.
			c.MaxReadBytes = 0
			old := fake(t, map[string]any{CapabilityKey: c}, false, x.inner...)
			ctx := context.Background()
			st, ct := sdk.NewInMemoryTransports()
			ss, err := old.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ss.Close() })
			var w warned
			r, err := Connect(ctx, ct, ConnectOptions{Name: "dax", Version: "test", Warn: w.warn, MaxReadBytes: tc.asked})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.Close() })
			got := w.String()
			if warns := strings.Contains(got, "older than the setting") && strings.Contains(got, "fake 1"); warns != tc.warns || (!tc.warns && got != "") {
				t.Errorf("warned %q, want a warning: %v", got, tc.warns)
			}
		})
	}
}

// Two sessions over HTTP at once, asking for different limits, each
// read what they asked.
func TestConcurrentHTTPSessionsReadTheirOwnLimits(t *testing.T) {
	srv, _ := limitServer(t, 0)
	s := executorServer{srv: srv}
	url := s.serveHTTP(t).URL
	limits := []int64{4096, 12288}
	var wg sync.WaitGroup
	for _, limit := range limits {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, err := Connect(ctx, HTTPTransport(url), ConnectOptions{Name: "dax", Version: "test", MaxReadBytes: limit})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		if r.first.maxRead != limit {
			t.Errorf("session asking %d: the capability says %d", limit, r.first.maxRead)
		}
		wg.Go(func() {
			for range 5 {
				res, err := r.Call(ctx, Call{Tool: "read", Call: agenttool.Call{Args: json.RawMessage(`{"path":"big.txt"}`)}})
				if err != nil {
					t.Error(err)
					return
				}
				if out := tool.Text(res); !strings.Contains(out, cutAt(limit)) {
					t.Errorf("session asking %d read %q", limit, out[max(0, len(out)-200):])
					return
				}
			}
		})
	}
	wg.Wait()
	if n := len(srv.Sessions()); n != len(limits) {
		t.Errorf("%d sessions, want %d", n, len(limits))
	}
}

// A redial asks for the same limit and gets the same, so the new
// connection is the same executor; one whose ceiling changed under it
// is not, and is refused.
func TestARedialKeepsTheReadLimit(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second int64 // the executors' ceilings
		refused       string
	}{
		{"the same ceiling", 8192, 8192, ""},
		{"a ceiling above what was asked", 8192, 1 << 20, ""},
		{"a ceiling lowered under it", 8192, 2048, "its tools read up to 2048 bytes of a file, not 4096"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			first, dir := limitServer(t, tc.first)
			ws, err := workspace.NewLocal(dir, tool.DefaultEnv(nil))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ws.Close() })
			second, err := NewServer(ServeOptions{Name: "dax", Version: "test", Descriptor: testDescriptor(ws.Root()), MaxReadBytes: tc.second},
				withCoding(), extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { second.Close() })
			b := &pipeBox{t: t, srvs: []connector{first, second}}
			var w warned
			r, err := Dial(ctx, b.dial, ConnectOptions{Name: "dax", Version: "test", Warn: w.warn, MaxReadBytes: 4096})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.Close() })
			if !strings.Contains(readBig(t, r), cutAt(4096)) {
				t.Fatal("the first connection's read is not cut at 4096")
			}
			b.cut(0)
			waitLost(t, r)
			res, err := r.Call(ctx, Call{Tool: "read", Call: agenttool.Call{Args: json.RawMessage(`{"path":"big.txt"}`)}})
			if tc.refused != "" {
				if !errors.Is(err, ErrChanged) || !strings.Contains(err.Error(), tc.refused) {
					t.Errorf("err = %v, want it refused: %s", err, tc.refused)
				}
				return
			}
			if err != nil {
				t.Fatalf("after the redial: %v", err)
			}
			if out := tool.Text(res); !strings.Contains(out, cutAt(4096)) {
				t.Errorf("after the redial read %q", out[max(0, len(out)-200):])
			}
			if got := currentOf(t, r); got == nil || b.dialed() != 2 {
				t.Errorf("%d dials, want 2", b.dialed())
			}
			var limits []int64
			for _, ss := range second.Sessions() {
				second.mu.Lock()
				limits = append(limits, second.sessions[ss].maxRead)
				second.mu.Unlock()
			}
			if len(limits) != 1 || limits[0] != 4096 {
				t.Errorf("the new connection's session reads %v, want [4096]", limits)
			}
		})
	}
}

// Each session's tools are built for it and closed when it ends, so
// that many sessions leave none behind; Close closes the tools of the
// sessions still open.
func TestASessionsToolsAreClosedWithIt(t *testing.T) {
	var built, closed atomic.Int32
	closing := extension.Extension{Name: "acme", Tools: func(extension.ToolEnv) []agenttool.Tool {
		built.Add(1)
		return []agenttool.Tool{plain("held", agenttool.WithCloser(func() error { closed.Add(1); return nil }))}
	}}
	srv, _ := limitServer(t, 0, closing)
	const sessions = 25
	for i := range sessions {
		ctx := context.Background()
		st, ct := sdk.NewInMemoryTransports()
		ss, err := srv.Connect(ctx, st, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := Connect(ctx, ct, ConnectOptions{Name: "dax", Version: "test", MaxReadBytes: int64(4096 * (i + 1))})
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		ss.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for closed.Load() != built.Load() || len(srv.Sessions()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("built %d, closed %d, %d sessions open", built.Load(), closed.Load(), len(srv.Sessions()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	// NewServer's check, and one build per session.
	if n := built.Load(); n != sessions+1 {
		t.Errorf("built %d times, want %d", n, sessions+1)
	}

	connectAsking(t, srv, 0)
	before := closed.Load()
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if closed.Load() != before+1 {
		t.Errorf("Close closed %d tools, want the open session's 1", closed.Load()-before)
	}
	st, _ := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); !errors.Is(err, errServerClosed) {
		t.Errorf("a session after Close: %v", err)
	}
}

// Every session's tools are built over the one tool.Files, whose lock a
// write takes: two sessions' edits of one file, at once, are each
// applied, none lost to the other's.
func TestSessionsShareTheWriteLock(t *testing.T) {
	var mu sync.Mutex
	var files []*tool.Files
	watching := extension.Extension{Name: "acme", Tools: func(e extension.ToolEnv) []agenttool.Tool {
		mu.Lock()
		files = append(files, e.Files)
		mu.Unlock()
		return nil
	}}
	srv, dir := limitServer(t, 0, watching)
	const tokens = 40
	var b strings.Builder
	for i := range tokens {
		fmt.Fprintf(&b, "<t%02d>\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	s := executorServer{srv: srv}
	url := s.serveHTTP(t).URL
	var wg sync.WaitGroup
	for side := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		r, err := Connect(ctx, HTTPTransport(url), ConnectOptions{Name: "dax", Version: "test", MaxReadBytes: int64(4096 * (side + 1))})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		wg.Go(func() {
			for i := side; i < tokens; i += 2 {
				args, _ := json.Marshal(map[string]string{"path": "shared.txt", "old_string": fmt.Sprintf("<t%02d>", i), "new_string": fmt.Sprintf("<done%02d>", i)})
				if _, err := r.Call(ctx, Call{Tool: "edit", Call: agenttool.Call{Args: args}}); err != nil {
					t.Errorf("edit %d: %v", i, err)
				}
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(dir, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range tokens {
		if !strings.Contains(string(data), fmt.Sprintf("<done%02d>", i)) {
			t.Errorf("edit %d was lost:\n%s", i, data)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(files) != 3 { // NewServer's check and the two sessions'
		t.Fatalf("%d builds, want 3", len(files))
	}
	for _, f := range files[1:] {
		if f != files[0] {
			t.Error("a session's tools are built over a tool.Files of their own")
		}
	}
}

func TestReadLimit(t *testing.T) {
	for _, tc := range []struct {
		ceiling, asked int64
		ok             bool
		want           int64
	}{
		{4096, 0, false, 4096},
		{4096, 1024, true, 1024},
		{4096, 8192, true, 4096},
		{4096, 0, true, 4096},
		{tool.DefaultMaxRead, 0, true, tool.DefaultMaxRead},
		{8 << 20, 0, true, tool.DefaultMaxRead},
		{8 << 20, -5, true, tool.DefaultMaxRead},
		{8 << 20, 0, false, 8 << 20},
	} {
		if got := readLimit(tc.ceiling, tc.asked, tc.ok); got != tc.want {
			t.Errorf("readLimit(%d, %d, %v) = %d, want %d", tc.ceiling, tc.asked, tc.ok, got, tc.want)
		}
	}
}

// A client capability that is not the capability fails the session's
// initialize, saying so.
func TestAMalformedClientCapabilityIsRefused(t *testing.T) {
	srv, _ := limitServer(t, 0)
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	_, err = mcpclient.Connect(ctx, ct, mcpclient.WithClientOptions(sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{
		Experimental: map[string]any{CapabilityKey: map[string]any{"maxReadBytes": "lots"}},
	}}))
	if err == nil || !strings.Contains(err.Error(), CapabilityKey) {
		t.Errorf("err = %v", err)
	}
}
