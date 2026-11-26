package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool/mcpserver"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// eventsStdioServer, with a directory after it, makes the test binary an
// MCP server over stdio with one emit-only event type, "deploy", and two
// tools: emit, which emits a deploy event with the id it is given, and
// pid. Each start appends its process id to dir/starts. It is what a
// `command` server with an events block is, over a real pipe.
const eventsStdioServer = "dax-test-events-stdio-mcp"

type stdioEmitArgs struct {
	ID string `json:"id"`
}

func serveEventsStdio(dir string) int {
	f, err := os.OpenFile(filepath.Join(dir, "starts"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 1
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Close()
	srv := mcp.NewServer(&mcp.Implementation{Name: "ci", Version: "1"}, nil)
	ev, err := mcpserver.AddEvents(srv, mcpserver.EventsOptions{DefaultNextPoll: 10 * time.Millisecond})
	if err != nil {
		return 1
	}
	defer ev.Close()
	et, err := ev.Register(mcpserver.EventDef{
		Name: "deploy", Description: "a deploy finished", EmitOnly: true,
		InputSchema:   json.RawMessage(`{"type":"object"}`),
		PayloadSchema: json.RawMessage(`{"type":"object"}`),
		Match:         func(mcpserver.EventContext, mcpserver.Event, json.RawMessage) bool { return true },
	})
	if err != nil {
		return 1
	}
	text := func(s string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "emit", Description: "emit a deploy event"},
		func(_ context.Context, _ *mcp.CallToolRequest, in stdioEmitArgs) (*mcp.CallToolResult, struct{}, error) {
			err := et.Emit(mcpserver.Event{ID: in.ID, Data: json.RawMessage(`{"env":"prod"}`)})
			return text("ok"), struct{}{}, err
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "pid", Description: "the server's process id"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			return text(fmt.Sprintf("pid=[%d]", os.Getpid())), struct{}{}, nil
		})
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		return 1
	}
	return 0
}

// stdioEvents is a session whose `ci` server is a stdio command with a
// deploy events block, on this machine (in: host).
type stdioEvents struct {
	dir string
	o   Options
}

func newStdioEvents(t *testing.T, model openresponses.Streamer) *stdioEvents {
	t.Helper()
	dir := t.TempDir()
	o := options(t, model)
	o.Events = true
	o.eventsFloor = 5 * time.Millisecond
	o.MCP = []MCPServer{{
		Name: "ci", Host: true,
		Command: "'" + os.Args[0] + "' " + eventsStdioServer + " '" + dir + "'",
		Events:  &MCPEvents{Types: []MCPEventType{{Name: "deploy"}}},
	}}
	return &stdioEvents{dir: dir, o: o}
}

// starts are the process ids the server has run as, oldest first.
func (e *stdioEvents) starts() []int {
	b, _ := os.ReadFile(filepath.Join(e.dir, "starts"))
	var out []int
	for _, f := range strings.Fields(string(b)) {
		n, _ := strconv.Atoi(f)
		out = append(out, n)
	}
	return out
}

// call calls a tool of the session's ci server and returns its text.
func callCI(t *testing.T, s *Session, tool string, args map[string]any) string {
	t.Helper()
	var text string
	waitCond(t, "the call to "+tool, func() bool {
		servers := s.Kit.MCPServers()
		if len(servers) == 0 {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := servers[0].Remote.Session().CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil || res.IsError {
			return false
		}
		text = res.Content[0].(*mcp.TextContent).Text
		return true
	})
	return text
}

var stdioPidRE = regexp.MustCompile(`pid=\[(\d+)\]`)

// savedCursor is the cursor of the ci deploy subscription in the cursors
// file, or "".
func savedCursor(o Options) string {
	b, err := os.ReadFile(filepath.Join(o.UserDir, mcpEventsFile))
	if err != nil {
		return ""
	}
	var d cursorsFileData
	if json.Unmarshal(b, &d) != nil {
		return ""
	}
	for k, v := range d.Cursors {
		if strings.HasPrefix(k, "ci|deploy|") {
			return v
		}
	}
	return ""
}

// stdioProcessGone says pid no longer runs: it was reaped, or is a zombie.
func stdioProcessGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	_, after, ok := strings.Cut(string(stat), ") ")
	return ok && strings.HasPrefix(after, "Z")
}

// userMessages counts the user messages of the transcript holding text.
func userMessages(s *Session, text string) int {
	n := 0
	for _, it := range s.State().Transcript {
		if m, ok := it.(*openresponses.Message); ok && m.Role == openresponses.RoleUser && strings.Contains(m.Text(), text) {
			n++
		}
	}
	return n
}

// The events loop runs over a stdio command server: an event it emits
// reaches the session as the untrusted, framed user message under the
// mcp-event trigger, and the cursor is saved to the cursors file.
func TestEventsOverAStdioCommandServer(t *testing.T) {
	e := newStdioEvents(t, &gated{})
	s, err := New(t.Context(), e.o)
	if err != nil {
		t.Fatal(err)
	}
	waitCond(t, "the first poll", func() bool { return cursorSaved(e.o) })
	first := savedCursor(e.o)
	callCI(t, s, "emit", map[string]any{"id": "st-1"})
	waitCond(t, "st-1", func() bool { return userMessages(s, "id st-1") == 1 && idle(s) })
	waitCond(t, "the cursor to move past the event", func() bool { return savedCursor(e.o) != first })
	if fi, _ := os.Stat(filepath.Join(e.o.UserDir, mcpEventsFile)); fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("the cursor file is mode %04o", fi.Mode().Perm())
	}
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range recordedUserMessages(t, e.o, id, "st-1") {
		found = true
		if !strings.Contains(m.text, eventOpen) || !strings.Contains(m.text, "untrusted data") || !strings.Contains(m.text, `"env":"prod"`) {
			t.Errorf("the event's text = %q", m.text)
		}
		if m.kind != EventTrigger || m.source != "ci" || m.ref != "st-1" || !m.untrusted {
			t.Errorf("recorded as %+v", m)
		}
	}
	if !found {
		t.Error("the event is not on the record")
	}
}

// The server process is killed: the loop starts it again on its own, an
// old cursor of the dead process reads as a gap (a warning, not an error
// that stops the loop), and events of the new process arrive, the old
// not delivered again. A session of a later run, over a third process,
// resumes from the cursor file the same way.
func TestEventsResumeAfterAStdioServerIsKilled(t *testing.T) {
	var warned syncBuffer
	defer CaptureWarnings(&warned)()
	e := newStdioEvents(t, &gated{})
	s, err := New(t.Context(), e.o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitCond(t, "the first poll", func() bool { return cursorSaved(e.o) })
	callCI(t, s, "emit", map[string]any{"id": "k1"})
	waitCond(t, "k1", func() bool { return userMessages(s, "id k1") == 1 && idle(s) })
	waitCond(t, "k1's cursor", func() bool { return savedCursor(e.o) != "" })
	old := savedCursor(e.o)
	pids := e.starts()
	if len(pids) != 1 {
		t.Fatalf("starts = %v", pids)
	}

	if err := syscall.Kill(pids[0], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// The loop restarts the server by itself: a new process, and a poll
	// of it with the old cursor, which it answers as a gap.
	waitCond(t, "the server to start again", func() bool { return len(e.starts()) == 2 })
	waitCond(t, "the gap and a fresh cursor", func() bool {
		return savedCursor(e.o) != old && strings.Contains(warned.String(), "may have been lost")
	})
	callCI(t, s, "emit", map[string]any{"id": "k2"})
	waitCond(t, "k2 after the restart", func() bool { return userMessages(s, "id k2") == 1 && idle(s) })
	if n := userMessages(s, "id k1"); n != 1 {
		t.Errorf("k1 was delivered %d times", n)
	}
	if strings.Contains(warned.String(), "polling events of type deploy: ") && strings.Contains(warned.String(), "forbidden") {
		t.Errorf("warnings: %q", warned.String())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Another session over the same cursor file and a third process.
	s2, err := New(t.Context(), e.o)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	waitCond(t, "the third process", func() bool { return len(e.starts()) == 3 })
	waitCond(t, "the new session's poll", func() bool { return savedCursor(e.o) != "" })
	callCI(t, s2, "emit", map[string]any{"id": "k3"})
	waitCond(t, "k3", func() bool { return userMessages(s2, "id k3") == 1 && idle(s2) })
	if userMessages(s2, "id k1") != 0 || userMessages(s2, "id k2") != 0 {
		t.Error("an old event was delivered again")
	}
}

// Closing the session ends the server process and the loops: no process
// and no goroutine of the hub is left.
func TestEventsOverStdioEndWithTheSession(t *testing.T) {
	hubRunning := func() bool {
		buf := make([]byte, 1<<20)
		return strings.Contains(string(buf[:runtime.Stack(buf, true)]), "(*eventHub)")
	}
	e := newStdioEvents(t, &echo.Adapter{})
	s, err := New(t.Context(), e.o)
	if err != nil {
		t.Fatal(err)
	}
	waitCond(t, "the first poll", func() bool { return cursorSaved(e.o) })
	out := callCI(t, s, "pid", nil)
	m := stdioPidRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no pid in %q", out)
	}
	pid, _ := strconv.Atoi(m[1])
	if !hubRunning() {
		t.Fatal("no goroutine polls")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	waitCond(t, "the server process to end", func() bool { return stdioProcessGone(pid) })
	if hubRunning() {
		t.Error("a goroutine of the hub outlived the session")
	}
	if n := len(e.starts()); n != 1 {
		t.Errorf("the server was started %d times", n)
	}
}

// The events block of a stdio server is not used without Options.Events
// (-p): the server runs, nothing polls it and no event reaches the
// session or the cursors file.
func TestAStdioServersEventsBlockIsIgnoredWithoutTheOption(t *testing.T) {
	e := newStdioEvents(t, &echo.Adapter{})
	e.o.Events = false
	s, err := New(t.Context(), e.o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.events != nil {
		t.Error("a hub was started")
	}
	callCI(t, s, "emit", map[string]any{"id": "p1"})
	time.Sleep(100 * time.Millisecond)
	if cursorSaved(e.o) || userMessages(s, "p1") != 0 {
		t.Error("the server's events were taken")
	}
}

type recordedMessage struct {
	text, kind, source, ref string
	untrusted               bool
}

// recordedUserMessages are the recorded user inputs of session id whose
// text holds sub.
func recordedUserMessages(t *testing.T, o Options, id, sub string) []recordedMessage {
	t.Helper()
	var out []recordedMessage
	for _, e := range record(t, o, id) {
		it, ok := e.(*agentsession.ItemEntry)
		if !ok {
			continue
		}
		m, ok := it.Item.(*openresponses.Message)
		if !ok || m.Role != openresponses.RoleUser || !strings.Contains(m.Text(), sub) {
			continue
		}
		r := recordedMessage{text: m.Text()}
		if it.Source != nil {
			r.kind, r.source, r.ref = it.Source.Kind, it.Source.Source, it.Source.Ref
		}
		r.untrusted = it.Trust != nil && len(it.Trust.Reads) == 1 && it.Trust.Reads[0] == agentsession.TrustUntrusted
		out = append(out, r)
	}
	return out
}

// The same in a workspace: the server is started in the executor
// (mcpworkspace.CommandTransport), an event arrives, the server's
// process is killed, and the loop resumes with a gap and takes the next.
func TestEventsOverAStdioServerInAnExecutor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var warned syncBuffer
	defer CaptureWarnings(&warned)()
	t.Setenv(executorRoot, t.TempDir())
	ex, err := DialExecutor(ctx, []string{os.Args[0]}, ExecutorOptions{Name: "dax", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	e := newStdioEvents(t, &gated{})
	e.o.Executor = ex
	e.o.MCP[0].Host = false
	s, err := New(ctx, e.o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitCond(t, "the first poll", func() bool { return cursorSaved(e.o) })
	callCI(t, s, "emit", map[string]any{"id": "x1"})
	waitCond(t, "x1", func() bool { return userMessages(s, "id x1") == 1 && idle(s) })
	waitCond(t, "x1's cursor", func() bool { return savedCursor(e.o) != "" })
	old := savedCursor(e.o)
	pids := e.starts()
	if len(pids) != 1 {
		t.Fatalf("starts = %v", pids)
	}
	// It runs under the executor, not as a child of this process.
	if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pids[0]) + "/stat"); err == nil {
		_, after, _ := strings.Cut(string(stat), ") ")
		if f := strings.Fields(after); len(f) > 1 && f[1] == strconv.Itoa(os.Getpid()) {
			t.Error("the server is this process's child: it was not placed in the executor")
		}
	}
	if err := syscall.Kill(pids[0], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitCond(t, "the server to start again", func() bool { return len(e.starts()) == 2 })
	waitCond(t, "the gap", func() bool {
		return savedCursor(e.o) != old && strings.Contains(warned.String(), "may have been lost")
	})
	callCI(t, s, "emit", map[string]any{"id": "x2"})
	waitCond(t, "x2", func() bool { return userMessages(s, "id x2") == 1 && idle(s) })
	if userMessages(s, "id x1") != 1 {
		t.Error("x1 was delivered again")
	}
}
