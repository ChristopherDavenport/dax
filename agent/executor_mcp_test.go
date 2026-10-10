package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/tool"
)

var whereRE = regexp.MustCompile(`dir=\[([^\]]*)\] pid=\[(\d+)\]`)

func parseWhere(t *testing.T, text string) (string, int) {
	t.Helper()
	m := whereRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("where said %q", text)
	}
	pid, _ := strconv.Atoi(m[2])
	return m[1], pid
}

// inDir reports whether got is dir, by name or with its links
// resolved.
func inDir(got, dir string) bool {
	real, _ := filepath.EvalSymlinks(dir)
	return got == dir || got == real
}

// processGone waits up to 15 seconds for pid to have exited and been
// reaped.
func processGone(t *testing.T, pid int) {
	t.Helper()
	for range 300 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("process %d is still running", pid)
}

// With an executor, the MCP servers run in it: the configured one and
// one /mcp add starts, at the executor's root and with the environment
// its commands get. Their tools are mcp__<name>__<tool>, make no facts
// claim (so the executor is sent no facts request for them), ask by
// default under the session's policy, run, and are recorded here; their
// standard error comes back cleaned; RemoveMCP and the session's end
// stop them.
func TestMCPServersRunInTheExecutor(t *testing.T) {
	var got syncBuffer
	defer CaptureWarnings(&got)()
	// The box's commands get this environment, scrubbed, as `dax
	// execute`'s get its own.
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	t.Setenv("DAX_TEST_MCP_NOISE", "1")
	t.Setenv("OPENAI_API_KEY", "sk-leak")
	t.Setenv("PLAIN", "the box")
	ctx := context.Background()
	box := newRemoteBox(t)
	ex, _ := box.dial(t)
	model := &scripted{calls: [][2]string{{"mcp__cfg__where", `{}`}}}
	o := remoteOptions(t, model, ex)
	o.MCP = []MCPServer{{Name: "cfg", Command: os.Args[0]}}
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
	for _, name := range []string{"mcp__cfg__where", "mcp__cfg__leak"} {
		if !slices.Contains(toolNames(s), name) {
			t.Fatalf("no %s in %v", name, toolNames(s))
		}
		if tl, _ := s.Kit.LookupTool(name); agenttool.IsFactual(tl) {
			t.Errorf("%s makes a facts claim", name)
		}
	}
	if got := leak(t, s, "mcp__cfg__leak"); got != "key=[] token=[] plain=[the box]" {
		t.Errorf("environment: %s", got)
	}

	before := box.factsRequests()
	var asked []string
	if _, err := promptOn(ctx, s, "where are you", func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Name)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{"mcp__cfg__where"}) {
		t.Errorf("asked about %v, want the MCP tool, which asks by default", asked)
	}
	if n := box.factsRequests() - before; n != 0 {
		t.Errorf("%d facts requests for an MCP tool's call", n)
	}
	outs := outputs(s)
	if len(outs) != 1 {
		t.Fatalf("outputs %q", outs)
	}
	dir, cfgPID := parseWhere(t, outs[0])
	if !inDir(dir, box.dir) {
		t.Errorf("the configured server ran in %s, not the executor's %s", dir, box.dir)
	}
	if _, calls := box.sent(); len(calls) != 0 {
		t.Errorf("the executor was sent tool calls %v", calls)
	}

	label, err := s.AddMCP(ctx, "added", os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	// Its tools are offered from the next run.
	model.calls = [][2]string{{"mcp__added__where", `{}`}}
	if _, err := promptOn(ctx, s, "and the added one", func(*openresponses.FunctionCall, string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	outs = outputs(s)
	dir, addedPID := parseWhere(t, outs[len(outs)-1])
	if !inDir(dir, box.dir) {
		t.Errorf("the added server ran in %s, not the executor's %s", dir, box.dir)
	}
	if err := s.RemoveMCP(label); err != nil {
		t.Fatal(err)
	}
	processGone(t, addedPID)

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got.String(), "shout") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if out := got.String(); !strings.Contains(out, "start [2K]0;pwnedshout") {
		t.Errorf("stderr = %q", out)
	} else {
		for _, r := range out {
			if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || r == 0x202e) {
				t.Errorf("control character %U in %q", r, out)
			}
		}
	}

	data := projected(t, o, s)
	closed = true
	if !strings.Contains(string(data), "mcp__cfg__where") || !strings.Contains(string(data), "pid=["+strconv.Itoa(cfgPID)+"]") {
		t.Errorf("the record lacks the MCP call and its output")
	}
	processGone(t, cfgPID)
}

// oldBox is an executor whose workspace cannot start a process, as
// dax execute before the process methods: its capability has no start.
func oldBox(t *testing.T) *Executor {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir(), tool.DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	srv, closeTools, err := executor.NewServer(executor.ServeOptions{Name: "dax", Version: "old", Descriptor: workspace.Descriptor{Kind: workspace.KindContainer, Ref: "old", Root: "/work"}},
		[]extension.Extension{coding.New(0), acme()}, extension.ToolEnv{Workspace: struct{ workspace.Workspace }{ws}, Files: tool.NewFiles(ws)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTools() })
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	ex, err := connectExecutor(ctx, ct, "dax", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })
	return ex
}

// An executor that cannot start a process fails a session with an MCP
// server, and /mcp add, saying to update it; the server never starts on
// this machine in its place. A session without one runs as before.
func TestAnExecutorThatCannotStartFailsItsMCPServers(t *testing.T) {
	ctx := context.Background()
	sentinel := filepath.Join(t.TempDir(), "ran-here")
	o := remoteOptions(t, &scripted{}, oldBox(t))
	o.MCP = []MCPServer{{Name: "s", Command: "touch " + sentinel}}
	if s, err := New(ctx, o); err == nil {
		s.Close()
		t.Error("a session with an MCP server started on an executor that cannot start it")
	} else if !strings.Contains(err.Error(), "update dax execute where it runs") {
		t.Errorf("err = %v, want it to say to update the executor", err)
	}

	o = remoteOptions(t, &scripted{}, oldBox(t))
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AddMCP(ctx, "s", "touch "+sentinel); err == nil || !strings.Contains(err.Error(), "update dax execute where it runs") {
		t.Errorf("AddMCP: %v", err)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the MCP server's command ran on this machine: %v", err)
	}
}
