package agent

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses/echo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMain lets the test binary be the MCP server the tests start: two
// tools, leak, that says what its environment holds, and where, its
// directory and process id; or, with
// executorRoot set, `dax execute` over that directory.
func TestMain(m *testing.M) {
	if dir := os.Getenv(executorRoot); dir != "" {
		if err := serveExecutor(dir); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("DAX_TEST_MCP_SERVER") == "1" {
		if os.Getenv("DAX_TEST_MCP_NOISE") == "1" {
			os.Stderr.WriteString("start \x1b[2K\x1b]0;pwned\x07\r\u202eshout\n")
		}
		srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
		mcp.AddTool(srv, &mcp.Tool{Name: "leak", Description: "report the environment"},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
				text := "key=[" + os.Getenv("OPENAI_API_KEY") + "] token=[" + os.Getenv("MY_TOKEN") + "] plain=[" + os.Getenv("PLAIN") + "]"
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, struct{}{}, nil
			})
		mcp.AddTool(srv, &mcp.Tool{Name: "where", Description: "report where the server runs"},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
				wd, _ := os.Getwd()
				text := "dir=[" + wd + "] pid=[" + strconv.Itoa(os.Getpid()) + "]"
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, struct{}{}, nil
			})
		if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func toolNames(s *Session) []string {
	var names []string
	for _, tl := range s.Tools() {
		names = append(names, tl.Name)
	}
	return names
}

func leak(t *testing.T, s *Session, name string) string {
	t.Helper()
	tl, ok := s.Kit.LookupTool(name)
	if !ok {
		t.Fatalf("no tool %s in %v", name, toolNames(s))
	}
	res, err := tl.Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(res.Output.Text), "{}"))
}

// #14 of the review: /mcp add skipped the mcp__ prefix.
func TestMCPToolsAreNamedMcpServerTool(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	t.Setenv("OPENAI_API_KEY", "sk-leak")
	t.Setenv("MY_TOKEN", "tok-leak")
	t.Setenv("PLAIN", "visible")
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	o.MCP = []MCPServer{{Name: "cfg", Command: os.Args[0]}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !slices.Contains(toolNames(s), "mcp__cfg__leak") {
		t.Fatalf("tools: %v", toolNames(s))
	}
	// The server's environment has no credentials, and keeps the rest.
	if got := leak(t, s, "mcp__cfg__leak"); got != "key=[] token=[] plain=[visible]" {
		t.Errorf("environment: %s", got)
	}

	// /mcp add goes through the same naming.
	if _, err := s.AddMCP(ctx, "added", os.Args[0]); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(toolNames(s), "mcp__added__leak") {
		t.Errorf("tools after /mcp add: %v", toolNames(s))
	}

	// pass_env gives a named variable and no other.
	o2 := options(t, &echo.Adapter{})
	o2.MCP = []MCPServer{{Name: "p", Command: os.Args[0]}}
	o2.PassEnv = []string{"MY_TOKEN"}
	s2, err := New(ctx, o2)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := leak(t, s2, "mcp__p__leak"); got != "key=[] token=[tok-leak] plain=[visible]" {
		t.Errorf("pass_env: %s", got)
	}

	// The provider's key variable is removed whatever its name.
	o3 := options(t, &echo.Adapter{})
	o3.MCP = []MCPServer{{Name: "k", Command: os.Args[0]}}
	o3.KeyEnv = "PLAIN"
	s3, err := New(ctx, o3)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if got := leak(t, s3, "mcp__k__leak"); got != "key=[] token=[] plain=[]" {
		t.Errorf("key variable: %s", got)
	}
}

func TestMCPNamesAreChecked(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"a__b", "__a", "a__", "a b", "", "x/y", "é", "read.x", "-a", "a*"} {
		if _, err := s.AddMCP(ctx, name, "true"); err == nil {
			t.Errorf("AddMCP(%q) should be refused", name)
		}
		o.MCP = []MCPServer{{Name: name, Command: "true"}}
		if s2, err := New(ctx, o); err == nil {
			s2.Close()
			t.Errorf("a configured server named %q should be refused", name)
		}
	}
}

// R2-7 of the second review: an MCP server's stderr reached the
// terminal raw.
func TestAnMCPServersStderrIsCleaned(t *testing.T) {
	var got syncBuffer
	defer CaptureWarnings(&got)()
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	t.Setenv("DAX_TEST_MCP_NOISE", "1")
	o := options(t, &echo.Adapter{})
	o.MCP = []MCPServer{{Name: "noisy", Command: os.Args[0]}}
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got.String(), "shout") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	out := got.String()
	if !strings.Contains(out, "start [2K]0;pwnedshout") {
		t.Fatalf("stderr = %q", out)
	}
	for _, r := range out {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || r == 0x202e) {
			t.Errorf("control character %U in %q", r, out)
		}
	}
}

// starter is a workspace.Local that records the processes it starts.
type starter struct {
	*workspace.Local
	mu      sync.Mutex
	started []workspace.Command
	procs   []workspace.Process
}

func (s *starter) Start(ctx context.Context, c workspace.Command) (workspace.Process, error) {
	p, err := s.Local.Start(ctx, c)
	if err == nil {
		s.mu.Lock()
		s.started = append(s.started, c)
		s.procs = append(s.procs, p)
		s.mu.Unlock()
	}
	return p, err
}

// The plan's step 3: an MCP server runs in the workspace when the
// workspace can start a process, with the workspace's environment,
// and ends with the session.
func TestAnMCPServerStartsInTheWorkspace(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	t.Setenv("PLAIN", "this machine")
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	local, err := workspace.NewLocal(o.Dir, []string{"DAX_TEST_MCP_SERVER=1", "PLAIN=the workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	ws := &starter{Local: local}
	o.Workspace = ws
	o.MCP = []MCPServer{{Name: "ws", Command: os.Args[0]}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := leak(t, s, "mcp__ws__leak"); got != "key=[] token=[] plain=[the workspace]" {
		t.Errorf("environment: %s", got)
	}
	if _, err := s.AddMCP(ctx, "added", os.Args[0]); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(toolNames(s), "mcp__added__leak") {
		t.Errorf("tools after /mcp add: %v", toolNames(s))
	}
	ws.mu.Lock()
	started, procs := slices.Clone(ws.started), slices.Clone(ws.procs)
	ws.mu.Unlock()
	if len(started) != 2 || strings.Join(started[0].Args, "|") != os.Args[0] || started[0].Dir != "" || started[0].Stream == nil {
		t.Fatalf("started: %+v", started)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		done := make(chan struct{})
		go func() { p.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("an MCP server outlived its session")
		}
	}
}

// A workspace that cannot start a process leaves the server on this
// machine, as before, with the scrubbed environment.
func TestAnMCPServerRunsHereWhenTheWorkspaceCannotStartIt(t *testing.T) {
	ws := localWorkspace(t, t.TempDir())
	env := []string{"PATH=/bin"}
	tr, err := mcpServer(struct{ workspace.Workspace }{ws}, "server arg", env)
	if err != nil {
		t.Fatal(err)
	}
	ct, ok := tr.(*mcp.CommandTransport)
	if !ok {
		t.Fatalf("transport: %T", tr)
	}
	if strings.Join(ct.Command.Args, "|") != "server|arg" || strings.Join(ct.Command.Env, " ") != "PATH=/bin" {
		t.Errorf("command: %v %v", ct.Command.Args, ct.Command.Env)
	}
	if tr, err := mcpServer(ws, "server arg", env); err != nil {
		t.Fatal(err)
	} else if _, ok := tr.(*startTransport); !ok {
		t.Errorf("a workspace.Local's transport: %T", tr)
	}
	if _, err := mcpServer(ws, "  ", env); err == nil {
		t.Error("an empty command should be an error")
	}
}

// syncBuffer is a strings.Builder the copying goroutine and the test
// can share.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// An MCP server's command line is split as the executor's and the key
// command's are: quotes keep a space in a word, nothing is expanded.
func TestAnMCPServersCommandIsSplitAsAShellSplitsWords(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
		err  string
	}{
		{"server --root /work", []string{"server", "--root", "/work"}, ""},
		{`server --root "/my work" '$HOME' ~`, []string{"server", "--root", "/my work", "$HOME", "~"}, ""},
		{`server "--root`, nil, "a double quote is not closed"},
		{"   ", nil, "empty command"},
		{`'' server`, nil, "empty command"},
	} {
		got, err := splitCommand(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%q: %q, %v; want %q", tc.in, got, err, tc.err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%q: %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}
