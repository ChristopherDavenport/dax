package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace"
	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace/wire"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveHTTP serves s over Handler on a loopback test server.
func (s executorServer) serveHTTP(t *testing.T) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	ts := httptest.NewServer(Handler(s.srv, done))
	t.Cleanup(func() {
		close(done)
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts
}

// dialHTTP is a client of the executor at url, closed at the test's
// end.
func dialHTTP(t *testing.T, url string, keepAlive time.Duration) *Remote {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Connect(ctx, HTTPTransport(url), ConnectOptions{Name: "dax", Version: "test", KeepAlive: keepAlive})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// Over HTTP the executor is the same executor: the client checks its
// capability and descriptor, a call's facts are read and the call runs
// there, its files are read through it, and a process started there
// echoes.
func TestTheExecutorServesOverHTTP(t *testing.T) {
	s := newServer(t)
	os.WriteFile(filepath.Join(s.dir, "notes.txt"), []byte("notes\n"), 0o644)
	r := dialHTTP(t, s.serveHTTP(t).URL, KeepAlive)
	ctx := context.Background()

	if got, want := r.Descriptor(), testDescriptor(s.ws.Root()); got != want {
		t.Errorf("descriptor = %+v, want %+v", got, want)
	}
	read := Call{Tool: "read", Call: agenttool.Call{ID: "r", Args: json.RawMessage(`{"path":"notes.txt"}`)}}
	f, err := r.Facts(ctx, read)
	if err != nil {
		t.Errorf("facts of a read: %+v, %v", f, err)
	}
	res, err := r.Call(ctx, read)
	if err != nil || !strings.Contains(res.Output.String(), "notes") {
		t.Errorf("read: %q, %v", res.Output.String(), err)
	}
	if b, err := fs.ReadFile(r.FS(), "notes.txt"); err != nil || string(b) != "notes\n" {
		t.Errorf("the file through the executor: %q, %v", b, err)
	}
	p := start(t, r, workspace.Command{Args: []string{"cat"}})
	if _, err := p.Stdin().Write([]byte("over http\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(p.Stdout()).ReadString('\n'); err != nil || line != "over http\n" {
		t.Errorf("cat: %q, %v", line, err)
	}
}

// Each client over HTTP is a session of its own: another client's
// process id is refused, and a client's close ends its processes and
// no one else's.
func TestEachHTTPClientIsASessionOfItsOwn(t *testing.T) {
	s := newServer(t)
	url := s.serveHTTP(t).URL
	a := dialHTTP(t, url, 0)
	b := dialHTTP(t, url, 0)
	pa := start(t, a, sleeper)
	id := idOf(t, pa)
	pidA := pidOf(t, pa)
	pidB := pidOf(t, start(t, b, sleeper))
	_, err := sdk.CallCustomMethod[*mcpworkspace.IDParams, *mcpworkspace.ExitResult](context.Background(), sessionOf(b), wire.MethodClose, &mcpworkspace.IDParams{ProcessID: wire.ProcessID{ID: id}})
	if err == nil || !strings.Contains(err.Error(), mcpworkspace.ErrNoProcess.Error()) {
		t.Errorf("b closing a's process: %v", err)
	}
	a.Close()
	gone(t, pidA)
	if err := syscall.Kill(pidB, 0); err != nil {
		t.Errorf("b's process after a closed: %v", err)
	}
}

// A session that sends nothing for SessionIdle is closed and its
// processes end; a client's pings keep it open.
func TestAnIdleHTTPSessionIsClosed(t *testing.T) {
	idle := SessionIdle
	SessionIdle = 300 * time.Millisecond
	t.Cleanup(func() { SessionIdle = idle })
	s := newServer(t)
	url := s.serveHTTP(t).URL
	quiet := dialHTTP(t, url, 0)
	kept := dialHTTP(t, url, 50*time.Millisecond)
	pidQuiet := pidOf(t, start(t, quiet, sleeper))
	pidKept := pidOf(t, start(t, kept, sleeper))
	gone(t, pidQuiet)
	time.Sleep(3 * SessionIdle)
	if err := syscall.Kill(pidKept, 0); err != nil {
		t.Errorf("the pinging client's process: %v", err)
	}
}

// A browser's ways in are refused: a request that reached a loopback
// address naming another host (DNS rebinding), and a cross-origin
// request from a page.
func TestTheHTTPExecutorRefusesABrowser(t *testing.T) {
	s := newServer(t)
	ts := s.serveHTTP(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
	for _, tc := range []struct {
		name   string
		header map[string]string
	}{
		{"another host", map[string]string{"Host": "evil.example"}},
		{"another origin", map[string]string{"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"}},
	} {
		req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range tc.header {
			req.Header.Set(k, v)
		}
		if h := tc.header["Host"]; h != "" {
			req.Host = h
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", tc.name, resp.StatusCode)
		}
	}
}
