package agent

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/remote"

	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// serveDir serves dir as `dax workspace` does, a workspace.Local behind
// one bearer token whose principal may read, write and run, with env as
// the workspace's environment; it returns the base URL and the token.
func serveDir(t *testing.T, dir string, env []string) (url, token string) {
	t.Helper()
	local, err := workspace.NewLocal(dir, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	token = "the-served-token"
	h, err := remote.NewHandler(local, remote.WithAuthenticator(remote.BearerTokens(map[string]remote.Principal{
		token: {Name: "dax", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeWrite, remote.ScopeExec}},
	})))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { h.Close() })
	return srv.URL, token
}

// A session whose workspace is served over HTTP (-workspace) runs its
// tools, their checks and the policy here, and acts there: the model is
// told the served root, a write, a read and a bash call land in the
// served directory, the project's AGENTS.md and skills are the served
// one's and not this machine's, an MCP stdio server starts there, through the
// workspace's Start, with its environment, and ends with the session.
// The record names the workspace as remote at the URL, never the
// token, and -list finds it by the URL without dialing.
func TestASessionOverAServedWorkspace(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	t.Setenv("PLAIN", "this machine")
	ctx := context.Background()
	servedDir := t.TempDir()
	write(t, filepath.Join(servedDir, "README.md"), "# served\n")
	write(t, filepath.Join(servedDir, "AGENTS.md"), "Served instructions.\n")
	write(t, filepath.Join(servedDir, ".dax", "skills", "deploy", "SKILL.md"), "---\nname: deploy\ndescription: How to deploy the served project.\n---\nRun make deploy.\n")
	url, token := serveDir(t, servedDir, append(tool.DefaultEnv(nil), "DAX_TEST_MCP_SERVER=1", "PLAIN=the workspace"))
	ws, err := remote.Dial(ctx, url+"/", remote.WithBearerToken(token))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if _, ok := ws.(workspace.Starter); !ok {
		t.Fatal("a principal with exec over a Local is no Starter")
	}

	model := &scripted{calls: [][2]string{
		{"write", `{"path":"made.txt","content":"hi\n"}`},
		{"read", `{"path":"` + servedDir + `/README.md"}`},
		{"bash", `{"command":"pwd > where.txt && cat made.txt"}`},
	}}
	o := options(t, model)
	o.Workspace = ws
	o.MCP = []MCPServer{{Name: "ws", Command: os.Args[0]}}
	o.Policy = &policy.Settings{Builtin: true, Fallback: "allow"}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	instr := s.ag.Config().Instructions
	for _, want := range []string{"Current working directory: " + servedDir, "Served instructions.", "How to deploy the served project."} {
		if !strings.Contains(instr, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
	if strings.Contains(instr, "Run go test before saying done.") {
		t.Error("the prompt carries this machine's AGENTS.md")
	}
	if _, err := promptOn(ctx, s, "go", nil); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(servedDir, "made.txt")); err != nil || string(b) != "hi\n" {
		t.Errorf("the write did not land in the served directory: %q %v", b, err)
	}
	out := outputs(s)
	if len(out) != 3 || !strings.Contains(out[1], "# served") || !strings.Contains(out[2], "hi") {
		t.Errorf("outputs %q", out)
	}
	if b, err := os.ReadFile(filepath.Join(servedDir, "where.txt")); err != nil || strings.TrimSpace(string(b)) != servedDir {
		t.Errorf("bash ran in %q, %v; want %s", b, err, servedDir)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "made.txt")); err == nil {
		t.Error("the write landed in this machine's directory")
	}

	// The MCP server runs in the served workspace, with its environment.
	if got := leak(t, s, "mcp__ws__leak"); got != "key=[] token=[] plain=[the workspace]" {
		t.Errorf("the server's environment: %s", got)
	}
	where := leak(t, s, "mcp__ws__where")
	m := regexp.MustCompile(`dir=\[(.*)\] pid=\[(\d+)\]`).FindStringSubmatch(where)
	if m == nil || m[1] != servedDir {
		t.Fatalf("the server runs at %q, want %s", where, servedDir)
	}
	pid, _ := strconv.Atoi(m[2])

	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the MCP server outlived its session")
		}
	}

	path, err := Project(ctx, o.Root, id, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"cwd":"` + servedDir + `"`, `"kind":"remote"`, `"ref":"` + url + `"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the record lacks %s", want)
		}
	}
	if strings.Contains(string(data), token) {
		t.Error("the record holds the token")
	}

	// -list finds the session by the URL, however it is written, and
	// not by this machine's directory or another URL.
	if got := remoteRef(url + "/"); got != ws.Descriptor().Ref {
		t.Errorf("remoteRef = %q, the client's ref %q", got, ws.Descriptor().Ref)
	}
	for _, u := range []string{url, url + "/", " " + url + "/ "} {
		d, sums, err := ListWorkspaceURL(ctx, o.Root, u)
		if err != nil || d != ws.Descriptor() || len(sums) != 1 || sums[0].Header.ID != id {
			t.Errorf("ListWorkspaceURL(%q) = %+v, %d sessions, %v", u, d, len(sums), err)
		}
	}
	if d, sums, err := ListWorkspaceURL(ctx, o.Root, "http://127.0.0.1:1"); err != nil || d != (workspace.Descriptor{}) || len(sums) != 0 {
		t.Errorf("another URL lists %+v %d, %v", d, len(sums), err)
	}
	if sums, err := List(ctx, o.Root, servedDir); err != nil || len(sums) != 0 {
		t.Errorf("this machine's directory of the same path lists %d, %v", len(sums), err)
	}
}

// A token without exec dials, reads and writes, but is no Starter:
// Exec is refused, and an MCP server runs on this machine, as in any
// workspace that cannot start a process.
func TestAServedWorkspaceWithoutExec(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	t.Setenv("PLAIN", "this machine")
	ctx := context.Background()
	dir := t.TempDir()
	local, err := workspace.NewLocal(dir, []string{"DAX_TEST_MCP_SERVER=1", "PLAIN=the workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	h, err := remote.NewHandler(local, remote.WithAuthenticator(remote.BearerTokens(map[string]remote.Principal{
		"tok": {Name: "dax", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeWrite}},
	})))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer h.Close()
	ws, err := remote.Dial(ctx, srv.URL, remote.WithBearerToken("tok"))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if _, ok := ws.(workspace.Starter); ok {
		t.Fatal("a principal without exec is a Starter")
	}
	if _, err := ws.Exec(ctx, workspace.Command{Args: []string{"true"}}); err == nil {
		t.Error("Exec without exec ran")
	}
	o := options(t, &scripted{})
	o.Workspace = ws
	o.MCP = []MCPServer{{Name: "ws", Command: os.Args[0]}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := leak(t, s, "mcp__ws__leak"); got != "key=[] token=[] plain=[this machine]" {
		t.Errorf("the server's environment: %s", got)
	}
}

// A project's command servers start in its workspace or not at all: in
// a served workspace that cannot start a process, a trusted one fails
// the session, saying why, where your own still runs here; and a trust
// given while they ran there does not hold for running here.
func TestAProjectsServersDoNotMoveHere(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	ctx := context.Background()
	dir := t.TempDir()
	local, err := workspace.NewLocal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	h, err := remote.NewHandler(local, remote.WithAuthenticator(remote.BearerTokens(map[string]remote.Principal{
		"tok": {Name: "dax", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeWrite}},
	})))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer h.Close()
	ws, err := remote.Dial(ctx, srv.URL, remote.WithBearerToken("tok"))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	o := options(t, &scripted{})
	o.Workspace = ws
	o.MCPTrustFile = filepath.Join(t.TempDir(), "trust", "mcp-trust.json")
	o.TrustMCP = true
	o.MCP = []MCPServer{{Name: "mine", Command: os.Args[0]}, {Name: "theirs", Command: os.Args[0], Project: true}}
	if s, err := New(ctx, o); err == nil {
		s.Close()
		t.Fatal("a project's server started on this machine")
	} else if !strings.Contains(err.Error(), "mcp theirs: the project's server would run on this machine") || !strings.Contains(err.Error(), "cannot start a process") {
		t.Errorf("err = %v", err)
	}
	o.MCP = o.MCP[:1]
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(toolNames(s), "mcp__mine__leak") {
		t.Errorf("your own server did not start here: %v", toolNames(s))
	}
	s.Close()

	theirs := []MCPServer{{Name: "theirs", Command: "lint --serve", Project: true}}
	file := filepath.Join(t.TempDir(), "trust", "mcp-trust.json")
	d := ws.Descriptor()
	trusted(theirs, d, false, file, true)
	if got := trusted(theirs, d, false, file, false); len(got) != 1 {
		t.Fatalf("the trust given does not hold where it was given: %v", got)
	}
	if got := trusted(theirs, d, true, file, false); len(got) != 0 {
		t.Errorf("a trust given while the servers ran in the workspace holds for running them here: %v", got)
	}
	// A server at a URL is called from here either way.
	urls := []MCPServer{{Name: "docs", URL: "https://docs.example/mcp", Project: true}}
	if serverSetHash(urls, true) != serverSetHash(urls, false) {
		t.Error("a URL's placement changed with the workspace's")
	}
}
