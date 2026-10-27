package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
)

// restartableServer, with a directory after it, makes the test binary
// the MCP server serveRestartable serves of that directory.
const restartableServer = "dax-test-restartable-mcp"

// serveRestartable serves, on standard input and output, the tools
// named in dir/tools, each of which notes its call in dir/calls and
// says where it runs: its directory, process and parent process. While
// dir/refuse exists it exits at once, a server that cannot start. The
// tools are read at start, so a server started again after dir/tools
// changed lists the new ones.
func serveRestartable(dir string) int {
	if _, err := os.Stat(filepath.Join(dir, "refuse")); err == nil {
		return 3
	}
	names, err := os.ReadFile(filepath.Join(dir, "tools"))
	if err != nil {
		return 1
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "restartable", Version: "1"}, nil)
	for _, name := range strings.Fields(string(names)) {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: "Say where the server runs."},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
				f, err := os.OpenFile(filepath.Join(dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					return nil, struct{}{}, err
				}
				_, err = f.WriteString(name + "\n")
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					return nil, struct{}{}, err
				}
				wd, _ := os.Getwd()
				text := fmt.Sprintf("dir=[%s] pid=[%d] ppid=[%d]", wd, os.Getpid(), os.Getppid())
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, struct{}{}, nil
			})
	}
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		return 1
	}
	return 0
}

// restartable is a directory for serveRestartable listing tools, and
// the command line that starts the server over it.
func restartable(t *testing.T, tools ...string) (dir, command string) {
	t.Helper()
	dir = t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "tools"), []byte(strings.Join(tools, "\n")), 0o644))
	return dir, "'" + os.Args[0] + "' " + restartableServer + " '" + dir + "'"
}

var ppidRE = regexp.MustCompile(`ppid=\[(\d+)\]`)

// whereFrom is a restartable tool's output as its process and parent.
func whereFrom(t *testing.T, out string) (pid, ppid int) {
	t.Helper()
	_, pid = parseWhere(t, out)
	m := ppidRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no parent in %q", out)
	}
	ppid, _ = strconv.Atoi(m[1])
	return pid, ppid
}

// exited waits up to 15 seconds for pid to have exited, reaped or not
// (a server the command transport started is reaped when the client
// closes it), and then a moment for whoever reads its pipes to see
// them close.
func exited(t *testing.T, pid int) {
	t.Helper()
	for range 300 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			if _, after, ok := strings.Cut(string(stat), ") "); ok && strings.HasPrefix(after, "Z") {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
}

// offering is a scripted model that keeps the names of the function
// tools its last request offered.
type offering struct {
	scripted
	mu   sync.Mutex
	last []string
}

func (m *offering) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	var names []string
	for _, t := range req.Tools {
		if f, ok := t.(*openresponses.FunctionTool); ok {
			names = append(names, f.Name)
		}
	}
	m.mu.Lock()
	m.last = names
	m.mu.Unlock()
	return m.scripted.CreateStream(ctx, req, sink)
}

func (m *offering) offered() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.last)
}

// callOnce has the model call name once, in a run of its own, and
// returns the call's output.
func callOnce(ctx context.Context, t *testing.T, s *Session, model *offering, name string) string {
	t.Helper()
	model.calls = [][2]string{{name, `{}`}}
	if _, err := promptOn(ctx, s, "call "+name, nil); err != nil {
		t.Fatal(err)
	}
	outs := outputs(s)
	if len(outs) == 0 {
		t.Fatal("no output")
	}
	return outs[len(outs)-1]
}

// callOutside calls name outside any run, as the kit offers it, and
// returns its output, or its error as the model would read it.
func callOutside(ctx context.Context, t *testing.T, s *Session, name string) string {
	t.Helper()
	tl, ok := s.Kit.LookupTool(name)
	if !ok {
		t.Fatalf("no tool %s in %v", name, toolNames(s))
	}
	res, err := tl.Execute(ctx, agenttool.Call{ID: "outside", Args: json.RawMessage(`{}`)})
	if err != nil {
		return agenttool.ErrorResult(err).Output.Text
	}
	return res.Output.Text
}

// callAfterEnd is callOnce, or with outside callOutside, after the
// server's session ended, sent again while the call was cut, sent as
// the session was ending: a call in flight is never sent again, and
// does not restart the server. mcpclient reads a closed connection as
// a cut at once, before it marks the session ended, so more than one
// call can be cut before the next restarts the server (#129); this
// gives up after cutGrace.
func callAfterEnd(ctx context.Context, t *testing.T, s *Session, model *offering, name string, outside bool) string {
	t.Helper()
	call := func() string {
		if outside {
			return callOutside(ctx, t, s, name)
		}
		return callOnce(ctx, t, s, model, name)
	}
	deadline := time.Now().Add(cutGrace)
	out := call()
	for strings.Contains(out, "may or may not have run") && time.Now().Before(deadline) {
		t.Logf("a call after the end was cut: %s", out)
		time.Sleep(20 * time.Millisecond)
		out = call()
	}
	return out
}

// cutGrace is how long callAfterEnd sends a call again that was cut.
const cutGrace = 10 * time.Second

// restartsIn is the restarts a session's record holds (mcpRestartNS),
// read from its projection.
func restartsIn(t *testing.T, data []byte) []mcpRestart {
	t.Helper()
	var out []mcpRestart
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `"`+mcpRestartNS+`"`) {
			continue
		}
		var e map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		var r mcpRestart
		if err := json.Unmarshal(e["data"], &r); err != nil {
			t.Fatalf("data: %v: %s", err, line)
		}
		out = append(out, r)
	}
	return out
}

// calledIn is what dir/calls says the restartable servers over dir ran.
func calledIn(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// #87: an MCP server whose session ended is started again by the next
// call to one of its tools, in the workspace and on this machine alike.
// The restart is one of dax's warnings and an entry in the session's
// record. One whose tools changed says which, and a call decided on a
// tool it no longer lists is not sent; one that cannot be started
// warns, fails the call, and is tried again by the next.
func TestAnMCPServerThatEndsIsStartedAgain(t *testing.T) {
	ctx := context.Background()
	for _, where := range []struct {
		name string
		// workspace, when set, is the session's: one that cannot start
		// a process, whose servers run on this machine.
		workspace func(t *testing.T, o Options) workspace.Workspace
	}{
		{"in the workspace", nil},
		{"on this machine", func(t *testing.T, o Options) workspace.Workspace {
			local, err := workspace.NewLocal(o.Dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { local.Close() })
			return struct{ workspace.Workspace }{local}
		}},
	} {
		for _, tc := range []struct {
			name  string
			tools []string
			// between runs while the server is down; after is the call
			// made then, and check what it and the record say.
			between func(t *testing.T, dir string)
			after   string
			// outside makes the call after outside any run, so the
			// restart is recorded with none in flight.
			outside bool
			// added starts the server with /mcp add, not the config.
			added bool
			check func(t *testing.T, s *Session, model *offering, dir string, out string, pid int)
			// warned and recorded are what the warnings and the record
			// say, after check.
			warned   []string
			recorded []mcpRestart
		}{
			{
				name: "same tools", tools: []string{"where"}, after: "mcp__srv__where",
				check: func(t *testing.T, _ *Session, _ *offering, _ string, out string, pid int) {
					if again, _ := whereFrom(t, out); again == pid {
						t.Errorf("the call ran in the ended server %d", pid)
					}
				},
				warned:   []string{"dax: MCP server srv ended and was started again (restartable 1); whatever it held is gone, and its tools are the same\n"},
				recorded: []mcpRestart{{Server: "srv", Why: mcpSessionEnded, OK: true, Implementation: "restartable", Version: "1"}},
			},
			{
				name: "same tools, outside a run", tools: []string{"where"}, after: "mcp__srv__where", outside: true,
				check: func(t *testing.T, _ *Session, _ *offering, _ string, out string, pid int) {
					if again, _ := whereFrom(t, out); again == pid {
						t.Errorf("the call ran in the ended server %d", pid)
					}
				},
				warned:   []string{"dax: MCP server srv ended and was started again (restartable 1); whatever it held is gone, and its tools are the same\n"},
				recorded: []mcpRestart{{Server: "srv", Why: mcpSessionEnded, OK: true, Implementation: "restartable", Version: "1"}},
			},
			{
				name: "same tools, added by /mcp add", tools: []string{"where"}, after: "mcp__srv__where", added: true,
				check: func(t *testing.T, _ *Session, _ *offering, _ string, out string, pid int) {
					if again, _ := whereFrom(t, out); again == pid {
						t.Errorf("the call ran in the ended server %d", pid)
					}
				},
				warned:   []string{"dax: MCP server srv ended and was started again (restartable 1); whatever it held is gone, and its tools are the same\n"},
				recorded: []mcpRestart{{Server: "srv", Why: mcpSessionEnded, OK: true, Implementation: "restartable", Version: "1"}},
			},
			{
				name: "changed tools", tools: []string{"where", "gone"}, after: "mcp__srv__gone",
				between: func(t *testing.T, dir string) {
					must(t, os.WriteFile(filepath.Join(dir, "tools"), []byte("where\nnew\n"), 0o644))
				},
				check: func(t *testing.T, s *Session, model *offering, dir string, out string, _ int) {
					if !strings.HasPrefix(out, "Error: ") || !strings.Contains(out, "the tool changed when the server restarted") || !strings.Contains(out, "not sent") {
						t.Errorf("a call of a tool the server no longer lists: %q", out)
					}
					if slices.Contains(calledIn(t, dir), "gone") {
						t.Error("the call of the removed tool was sent")
					}
					// The model is offered the restarted server's tools
					// from the next request.
					callOnce(context.Background(), t, s, model, "mcp__srv__new")
					if names := model.offered(); slices.Contains(names, "mcp__srv__gone") || !slices.Contains(names, "mcp__srv__new") {
						t.Errorf("tools offered after the restart: %v", names)
					}
					if !slices.Contains(calledIn(t, dir), "new") {
						t.Error("the restarted server's new tool did not run")
					}
				},
				warned: []string{"dax: MCP server srv ended and was started again (restartable 1); whatever it held is gone, and its tools changed: added mcp__srv__new; removed mcp__srv__gone\n"},
				recorded: []mcpRestart{{Server: "srv", Why: mcpSessionEnded, OK: true, Implementation: "restartable", Version: "1",
					Changed: true, Added: []string{"mcp__srv__new"}, Removed: []string{"mcp__srv__gone"}}},
			},
			{
				name: "a server that cannot start", tools: []string{"where"}, after: "mcp__srv__where",
				between: func(t *testing.T, dir string) {
					must(t, os.WriteFile(filepath.Join(dir, "refuse"), nil, 0o644))
				},
				check: func(t *testing.T, s *Session, model *offering, dir string, out string, pid int) {
					if !strings.HasPrefix(out, "Error: ") || !strings.Contains(out, "could not be restarted; the next request tries again") {
						t.Errorf("a call to a server that cannot start: %q", out)
					}
					// The next call tries again, and now it starts.
					must(t, os.Remove(filepath.Join(dir, "refuse")))
					out = callOnce(context.Background(), t, s, model, "mcp__srv__where")
					if again, _ := whereFrom(t, out); again == pid {
						t.Errorf("the call ran in the ended server %d", pid)
					}
				},
				warned: []string{
					"dax: MCP server srv: mcp: the server's session ended and could not be restarted; the next request tries again: ",
					"dax: MCP server srv ended and was started again (restartable 1); whatever it held is gone, and its tools are the same\n",
				},
				recorded: []mcpRestart{
					{Server: "srv", Why: mcpSessionEnded},
					{Server: "srv", Why: mcpSessionEnded, OK: true, Implementation: "restartable", Version: "1"},
				},
			},
		} {
			t.Run(where.name+"/"+tc.name, func(t *testing.T) {
				var warned syncBuffer
				defer CaptureWarnings(&warned)()
				dir, command := restartable(t, tc.tools...)
				model := &offering{}
				o := options(t, model)
				if where.workspace != nil {
					o.Workspace = where.workspace(t, o)
				}
				if !tc.added {
					o.MCP = []MCPServer{{Name: "srv", Command: command}}
				}
				s, err := New(ctx, o)
				if err != nil {
					t.Fatal(err)
				}
				closed := false
				defer func() {
					if !closed {
						s.Close()
					}
				}()
				if tc.added {
					if _, err := s.AddMCP(ctx, "srv", command); err != nil {
						t.Fatal(err)
					}
				}
				pid, _ := whereFrom(t, callOnce(ctx, t, s, model, "mcp__srv__where"))
				if tc.between != nil {
					tc.between(t, dir)
				}
				must(t, syscall.Kill(pid, syscall.SIGKILL))
				exited(t, pid)
				out := callAfterEnd(ctx, t, s, model, tc.after, tc.outside)
				tc.check(t, s, model, dir, out, pid)

				got := warned.String()
				at := 0
				for _, w := range tc.warned {
					i := strings.Index(got[at:], w)
					if i < 0 {
						t.Fatalf("warnings %q lack %q after offset %d", got, w, at)
					}
					at += i + len(w)
				}
				recs := restartsIn(t, projected(t, o, s))
				closed = true
				for i := range recs {
					if i < len(tc.recorded) && !tc.recorded[i].OK {
						// The error is the client's; that there is one is
						// what the record must say.
						if recs[i].Error == "" {
							t.Errorf("restart %d records no error", i)
						}
						recs[i].Error = ""
					}
				}
				if fmt.Sprint(recs) != fmt.Sprint(tc.recorded) {
					t.Errorf("recorded restarts:\n%+v\nwant\n%+v", recs, tc.recorded)
				}
			})
		}
	}
}

// #87 with an executor: an MCP server started in the executor ends
// when the executor's connection is lost, and the next call to its tool
// starts it again in the executor dax reconnected to, which answers it.
func TestAnMCPServerInTheExecutorIsStartedAgainAfterAReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var warned syncBuffer
	defer CaptureWarnings(&warned)()
	box := t.TempDir()
	t.Setenv(executorRoot, box)
	ex, err := DialExecutor(ctx, []string{os.Args[0]}, ExecutorOptions{Name: "dax", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	_, command := restartable(t, "where")
	model := &offering{}
	o := options(t, model)
	o.Extensions, o.Executor = []extension.Extension{coding.New(0)}, ex
	o.MCP = []MCPServer{{Name: "srv", Command: command}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			s.Close()
		}
	}()
	out := callOnce(ctx, t, s, model, "mcp__srv__where")
	if dir, _ := parseWhere(t, out); !inDir(dir, box) {
		t.Fatalf("the server ran in %s, not the executor's %s", dir, box)
	}
	pid, executorPID := whereFrom(t, out)
	must(t, syscall.Kill(executorPID, syscall.SIGKILL))
	exited(t, executorPID)

	out = callAfterEnd(ctx, t, s, model, "mcp__srv__where", false)
	if strings.HasPrefix(out, "Error: ") {
		t.Fatalf("the call after the reconnect: %s", out)
	}
	if dir, _ := parseWhere(t, out); !inDir(dir, box) {
		t.Errorf("the restarted server ran in %s, not the executor's %s", dir, box)
	}
	again, againExecutor := whereFrom(t, out)
	if again == pid || againExecutor == executorPID {
		t.Errorf("the call ran in server %d under executor %d, the ones that ended", again, againExecutor)
	}
	got := warned.String()
	for _, w := range []string{
		"dax: executor connection lost; reconnected to dax child",
		"dax: MCP server srv ended and was started again (restartable 1); whatever it held is gone, and its tools are the same\n",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("warnings %q lack %q", got, w)
		}
	}
	recs := restartsIn(t, projected(t, o, s))
	closed = true
	if len(recs) != 1 || !recs[0].OK || recs[0].Server != "srv" {
		t.Errorf("recorded restarts: %+v", recs)
	}
}
