package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool/mcpclient"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/tool"
)

// lockedBuffer is a stream a process's standard error is written to
// while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
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

// start is r.Start of args, failing the test on an error; the process
// is closed at the test's end.
func start(t *testing.T, r *Remote, cmd workspace.Command) workspace.Process {
	t.Helper()
	p, err := r.Start(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// sleeper is a process that prints its pid and then does nothing,
// whatever its input does, until it is ended.
var sleeper = workspace.Command{Args: []string{"sh", "-c", "echo $$; exec sleep 1000"}}

// pidOf reads the pid sleeper prints.
func pidOf(t *testing.T, p workspace.Process) int {
	t.Helper()
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// gone waits up to 10 seconds for pid to have exited and been reaped.
func gone(t *testing.T, pid int) {
	t.Helper()
	for range 200 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("process %d is still running", pid)
}

// A process started in the executor is a workspace.Process: what is
// written to its input comes back from cat, a write larger than one
// request included, the end of its input ends it, and its status is
// its own. It runs at the executor's root, with the directory and
// environment the start gave.
func TestAProcessInTheExecutorEchoes(t *testing.T) {
	s := newServer(t)
	r, _ := s.remote(t)
	p := start(t, r, workspace.Command{Args: []string{"cat"}})

	big := bytes.Repeat([]byte("0123456789abcdef"), (MaxWriteBytes+MaxWriteBytes/2)/16)
	var got bytes.Buffer
	read := make(chan error, 1)
	go func() {
		_, err := io.Copy(&got, p.Stdout())
		read <- err
	}()
	if n, err := p.Stdin().Write(big); err != nil || n != len(big) {
		t.Fatalf("write: %d, %v", n, err)
	}
	if err := p.Stdin().Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), big) {
		t.Errorf("read back %d bytes, want the %d written", got.Len(), len(big))
	}
	if status, err := p.Wait(); status != 0 || err != nil {
		t.Errorf("wait: %d, %v", status, err)
	}

	if err := os.Mkdir(filepath.Join(s.dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	q := start(t, r, workspace.Command{Args: []string{"sh", "-c", "pwd; echo $DAX_TEST; exit 3"}, Dir: "sub", Env: []string{"DAX_TEST=here"}})
	out, err := io.ReadAll(q.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(s.ws.Root(), "sub") + "\nhere\n"; string(out) != want {
		t.Errorf("output %q, want %q", out, want)
	}
	if status, err := q.Wait(); status != 3 || err != nil {
		t.Errorf("wait: %d, %v", status, err)
	}
	if _, err := r.Start(context.Background(), workspace.Command{Args: []string{"true"}, Dir: ".."}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("a directory outside the workspace: %v", err)
	}
}

// Standard error is read as fd 2 and written to the start's stream, the
// part no read took coming with the close; without a stream it is not
// kept.
func TestAProcessStandardErrorReachesTheStream(t *testing.T) {
	s := newServer(t)
	r, _ := s.remote(t)
	var stream lockedBuffer
	p := start(t, r, workspace.Command{Args: []string{"sh", "-c", "echo oops >&2; echo out"}, Stream: &stream})
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "out\n" {
		t.Errorf("stdout %q", out)
	}
	p.Wait()
	p.Close()
	if got := stream.String(); got != "oops\n" {
		t.Errorf("stderr %q, want oops", got)
	}

	q := start(t, r, workspace.Command{Args: []string{"sh", "-c", "echo oops >&2"}})
	q.Wait()
	cs := r.c.Session()
	if _, err := sdk.CallCustomMethod[*readParams, *readResult](context.Background(), cs, MethodRead, &readParams{ID: q.(*remoteProcess).id, FD: 2}); err == nil {
		t.Error("read fd 2 of a process started without a stream")
	}
}

// Close ends a process that ignores its input, and Signal reaches it;
// both leave Wait with how it ended.
func TestClosingAProcessKillsIt(t *testing.T) {
	s := newServer(t)
	r, _ := s.remote(t)
	p := start(t, r, sleeper)
	pid := pidOf(t, p)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	gone(t, pid)
	if status, _ := p.Wait(); status != -1 {
		t.Errorf("status %d, want -1 (a signal)", status)
	}

	q := start(t, r, sleeper)
	pid = pidOf(t, q)
	if err := q.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if status, err := q.Wait(); status != -1 || err != nil {
		t.Errorf("wait: %d, %v", status, err)
	}
	gone(t, pid)
	if err := q.Signal(syscall.SIGTERM); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("signal after exit: %v", err)
	}
	if err := q.Signal(syscall.SIGHUP); err == nil {
		t.Error("SIGHUP was sent")
	}
}

// The end of the start's context ends the process, and Wait says so.
func TestAProcessEndsWithItsContext(t *testing.T) {
	s := newServer(t)
	r, _ := s.remote(t)
	ctx, cancel := context.WithCancel(context.Background())
	p, err := r.Start(ctx, sleeper)
	if err != nil {
		t.Fatal(err)
	}
	pid := pidOf(t, p)
	cancel()
	if _, err := p.Wait(); !errors.Is(err, context.Canceled) {
		t.Errorf("wait: %v, want canceled", err)
	}
	gone(t, pid)
}

// A session's end, the connection closed, ends every process it
// started.
func TestASessionsEndKillsItsProcesses(t *testing.T) {
	s := newServer(t)
	r, _ := s.remote(t)
	pid := pidOf(t, start(t, r, sleeper))
	r.Close()
	gone(t, pid)
}

// The executor's Done ends every process and the reads waiting on
// them, so a graceful close of the session (Server.Run's at a signal),
// which waits for the requests in flight, returns.
func TestTheExecutorsDoneEndsItsProcesses(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	done := make(chan struct{})
	srv, closeTools, err := NewServer(ServeOptions{Name: "dax", Version: "test", Done: done}, []extension.Extension{coding.New(0)}, extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTools() })
	r, ss, err := connectTo(srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	p := start(t, r, sleeper)
	pid := pidOf(t, p)
	go io.Copy(io.Discard, p.Stdout()) // a read waiting for output
	time.Sleep(50 * time.Millisecond)
	close(done)
	closed := make(chan struct{})
	go func() {
		ss.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("the session's close waited on a read")
	}
	gone(t, pid)
	if _, err := r.Start(context.Background(), workspace.Command{Args: []string{"true"}}); err == nil {
		t.Error("a start after Done")
	}
}

// A session sees only its own processes: an id another session started
// is refused for every method, and the process goes on.
func TestAnotherSessionsProcessIsRefused(t *testing.T) {
	s := newServer(t)
	a, _ := s.remote(t)
	b, _ := s.remote(t)
	p := start(t, a, workspace.Command{Args: []string{"cat"}})
	id := p.(*remoteProcess).id
	ctx := context.Background()
	cs := b.c.Session()
	for name, call := range map[string]func() error{
		MethodWrite: func() error {
			_, err := sdk.CallCustomMethod[*writeParams, *emptyResult](ctx, cs, MethodWrite, &writeParams{ID: id, Data: []byte("x")})
			return err
		},
		MethodRead: func() error {
			_, err := sdk.CallCustomMethod[*readParams, *readResult](ctx, cs, MethodRead, &readParams{ID: id, FD: 1})
			return err
		},
		MethodCloseStdin: func() error {
			_, err := sdk.CallCustomMethod[*idParams, *emptyResult](ctx, cs, MethodCloseStdin, &idParams{ID: id})
			return err
		},
		MethodSignal: func() error {
			_, err := sdk.CallCustomMethod[*signalParams, *signalResult](ctx, cs, MethodSignal, &signalParams{ID: id, Signal: "KILL"})
			return err
		},
		MethodWait: func() error {
			_, err := sdk.CallCustomMethod[*idParams, *waitResult](ctx, cs, MethodWait, &idParams{ID: id})
			return err
		},
		MethodClose: func() error {
			_, err := sdk.CallCustomMethod[*idParams, *waitResult](ctx, cs, MethodClose, &idParams{ID: id})
			return err
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), errNoProcess.Error()) {
			t.Errorf("%s of another session's process: %v", name, err)
		}
	}
	if _, err := p.Stdin().Write([]byte("still\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil || line != "still\n" {
		t.Errorf("after the other session's calls: %q, %v", line, err)
	}
}

// A session has at most MaxProcesses open; a write is at most
// MaxWriteBytes and a read's reply at most MaxReadBytes, whatever it
// asks; and a start, a fd and a signal must be ones the methods know.
func TestTheProcessMethodsAreBounded(t *testing.T) {
	s := newServer(t)
	r, _ := s.remote(t)
	ctx := context.Background()
	var open []workspace.Process
	for range MaxProcesses {
		open = append(open, start(t, r, workspace.Command{Args: []string{"cat"}}))
	}
	if _, err := r.Start(ctx, workspace.Command{Args: []string{"cat"}}); err == nil || !strings.Contains(err.Error(), "32 processes") {
		t.Errorf("start %d: %v", MaxProcesses+1, err)
	}
	if err := open[0].Close(); err != nil {
		t.Fatal(err)
	}
	p := start(t, r, workspace.Command{Args: []string{"cat"}})
	for _, q := range open[1:] {
		q.Close()
	}

	cs := r.c.Session()
	id := p.(*remoteProcess).id
	if _, err := sdk.CallCustomMethod[*writeParams, *emptyResult](ctx, cs, MethodWrite, &writeParams{ID: id, Data: make([]byte, MaxWriteBytes+1)}); err == nil {
		t.Error("a write over MaxWriteBytes")
	}
	if _, err := p.Stdin().Write(bytes.Repeat([]byte("x"), 3*MaxReadBytes)); err != nil {
		t.Fatal(err)
	}
	res, err := sdk.CallCustomMethod[*readParams, *readResult](ctx, cs, MethodRead, &readParams{ID: id, FD: 1, Max: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Data) == 0 || len(res.Data) > MaxReadBytes {
		t.Errorf("a read asking 1 MiB gave %d bytes, want 1 to %d", len(res.Data), MaxReadBytes)
	}
	for name, call := range map[string]func() error{
		"fd 3": func() error {
			_, err := sdk.CallCustomMethod[*readParams, *readResult](ctx, cs, MethodRead, &readParams{ID: id, FD: 3})
			return err
		},
		"HUP": func() error {
			_, err := sdk.CallCustomMethod[*signalParams, *signalResult](ctx, cs, MethodSignal, &signalParams{ID: id, Signal: "HUP"})
			return err
		},
		"no command": func() error {
			_, err := sdk.CallCustomMethod[*startParams, *startResult](ctx, cs, MethodStart, &startParams{})
			return err
		},
		"an environment entry without =": func() error {
			_, err := sdk.CallCustomMethod[*startParams, *startResult](ctx, cs, MethodStart, &startParams{Args: []string{"true"}, Env: []string{"X"}})
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	if _, err := r.Start(ctx, workspace.Command{Args: []string{"cat"}, Stdin: []byte("x")}); err == nil {
		t.Error("a start with Command.Stdin")
	}
}

// notStarter is a workspace that cannot start a process.
type notStarter struct{ workspace.Workspace }

// An executor whose workspace cannot start a process says no start in
// its capability and answers no process method, and the session's
// Start is refused with what to do, as unsupported; nothing is started
// anywhere.
func TestAnExecutorWithoutStartIsRefused(t *testing.T) {
	dir := t.TempDir()
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	srv, closeTools, err := NewServer(ServeOptions{Name: "dax", Version: "old"}, []extension.Extension{coding.New(0)}, extension.ToolEnv{Workspace: notStarter{ws}, Files: tool.NewFiles(ws)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTools() })
	r, ss, err := connectTo(srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); ss.Close() })
	if r.start != nil {
		t.Errorf("start capability %+v", r.start)
	}
	sentinel := filepath.Join(dir, "started")
	_, err = r.Start(context.Background(), workspace.Command{Args: []string{"touch", sentinel}})
	if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "update dax execute where it runs") {
		t.Errorf("start: %v", err)
	}
	if _, err := sdk.CallCustomMethod[*startParams, *startResult](context.Background(), r.c.Session(), MethodStart, &startParams{Args: []string{"touch", sentinel}}); err == nil {
		t.Error("the executor took a start")
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sentinel ran: %v", err)
	}
}

// The process methods are not tools: an MCP client of the executor,
// another harness's, lists none and sees nothing that runs a process
// in the capability's tools; and nothing in the workspace, a project
// config naming an MCP server included, starts one.
func TestTheProcessMethodsAreNotTools(t *testing.T) {
	s := newServer(t)
	sentinel := filepath.Join(s.dir, "started")
	if err := os.MkdirAll(filepath.Join(s.dir, ".dax"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"mcp_servers": map[string]any{"x": map[string]any{"command": "touch " + sentinel}}})
	if err := os.WriteFile(filepath.Join(s.dir, ".dax", "config.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	other := s.connect(t, mcpclient.WithClientInfo("another-harness", "1"))
	for _, tl := range other.Tools() {
		if strings.Contains(tl.Name(), "process") || strings.HasPrefix(tl.Name(), "dax/") {
			t.Errorf("listed %q", tl.Name())
		}
	}
	r, _ := s.remote(t)
	tools, _ := r.Tools(context.Background())
	for _, tl := range tools {
		if strings.Contains(tl.Name(), "process") {
			t.Errorf("the capability names %q", tl.Name())
		}
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("something started the project's MCP server: %v", err)
	}
}
