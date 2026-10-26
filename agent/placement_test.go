package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// whereServer is an MCP server over streamable HTTP whose "where" tool
// says it was reached, and which records each request's Authorization.
func whereServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "where", Description: "report where the server runs"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "at a URL"}}}, struct{}{}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	var mu sync.Mutex
	var auth []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(auth)
	}
}

// A server at a URL is called from this machine, with the token its
// variable holds on every request, and that variable is one every
// child's environment loses.
func TestAnMCPServerAtAURL(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	t.Setenv("REMOTE_MCP_TOKEN", "tok-remote")
	ts, auth := whereServer(t)
	ctx := context.Background()
	o := options(t, &scripted{})
	o.MCP = []MCPServer{
		{Name: "remote", URL: ts.URL + "/mcp", TokenEnv: "REMOTE_MCP_TOKEN"},
		{Name: "local", Command: os.Args[0]},
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := leak(t, s, "mcp__remote__where"); got != "at a URL" {
		t.Errorf("where = %q", got)
	}
	got := auth()
	if len(got) == 0 || slices.ContainsFunc(got, func(a string) bool { return a != "Bearer tok-remote" }) {
		t.Errorf("Authorization on the server's requests: %q", got)
	}
	if !slices.Contains(o.secrets(), "REMOTE_MCP_TOKEN") {
		t.Errorf("secrets %v leave the token's variable in children's environments", o.secrets())
	}

	// A token variable that is not set fails the session before
	// anything is sent.
	t.Setenv("REMOTE_MCP_TOKEN", "")
	before := len(auth())
	o2 := options(t, &scripted{})
	o2.MCP = []MCPServer{{Name: "remote", URL: ts.URL + "/mcp", TokenEnv: "REMOTE_MCP_TOKEN"}}
	if s2, err := New(ctx, o2); err == nil || !strings.Contains(err.Error(), "REMOTE_MCP_TOKEN is not set") {
		if s2 != nil {
			s2.Close()
		}
		t.Errorf("an unset token variable: %v", err)
	}
	if len(auth()) != before {
		t.Error("a request was sent without its token")
	}
}

// The guard sends the token to the server's origin and nothing to any
// other: a request elsewhere is refused, and so is a redirect off it.
func TestTheURLGuardHoldsTheTokenToItsOrigin(t *testing.T) {
	var got []string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = append(got, r.URL.Host+" "+r.Header.Get("Authorization"))
		return &http.Response{StatusCode: 200, Body: http.NoBody, Request: r}, nil
	})
	g := &originGuard{origin: "https://mcp.example:443", token: "tok", rt: rt}
	ok, _ := http.NewRequest("GET", "https://MCP.example/mcp", nil)
	if _, err := g.RoundTrip(ok); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"https://evil.example/mcp", "http://mcp.example/mcp", "https://mcp.example:8443/mcp"} {
		req, _ := http.NewRequest("GET", other, nil)
		if _, err := g.RoundTrip(req); err == nil {
			t.Errorf("%s was sent", other)
		}
		if err := g.redirect(req, []*http.Request{ok}); err == nil {
			t.Errorf("a redirect to %s was followed", other)
		}
	}
	if !slices.Equal(got, []string{"MCP.example Bearer tok"}) {
		t.Errorf("sent %q", got)
	}
	if ok.Header.Get("Authorization") != "" {
		t.Error("the caller's request was changed")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A server placed on the host runs on this machine even with an
// executor, where every other command's server runs in the executor.
func TestAHostServerRunsHereBesideAnExecutor(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	ctx := context.Background()
	box := newRemoteBox(t)
	ex, _ := box.dial(t)
	o := remoteOptions(t, &scripted{}, ex)
	o.MCP = []MCPServer{
		{Name: "here", Command: os.Args[0], Host: true},
		{Name: "there", Command: os.Args[0]},
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if dir, _ := parseWhere(t, leak(t, s, "mcp__here__where")); !inDir(dir, wd) || inDir(dir, box.dir) {
		t.Errorf("the host's server ran in %s, want this process's %s", dir, wd)
	}
	if dir, _ := parseWhere(t, leak(t, s, "mcp__there__where")); !inDir(dir, box.dir) {
		t.Errorf("the workspace's server ran in %s, want the executor's %s", dir, box.dir)
	}
}

// The servers a workspace offers of its own are connected with the
// session, named after themselves, and a configured server with one's
// name is refused, at open and on /mcp add.
func TestTheWorkspacesOwnServers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	local, err := workspace.NewLocal(dir, append(os.Environ(), "DAX_TEST_MCP_SERVER=1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	ws, err := workspace.Offer(local, []workspace.OfferedServer{{Server: workspace.Server{Name: "offered"}, Command: workspace.Command{Args: []string{os.Args[0]}}}})
	if err != nil {
		t.Fatal(err)
	}
	o := options(t, &scripted{})
	o.Dir, o.Workspace = "", ws
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if d, _ := parseWhere(t, leak(t, s, "mcp__offered__where")); !inDir(d, dir) {
		t.Errorf("the offered server ran in %s, want the workspace's %s", d, dir)
	}
	if _, err := s.AddMCP(ctx, "offered", os.Args[0]); err == nil || !strings.Contains(err.Error(), "server offered") {
		t.Errorf("/mcp add of an offered server's name: %v", err)
	}

	o2 := options(t, &scripted{})
	o2.Dir, o2.Workspace = "", ws
	o2.MCP = []MCPServer{{Name: "offered", Command: os.Args[0]}}
	if s2, err := New(ctx, o2); err == nil || !strings.Contains(err.Error(), "rename the server") {
		if s2 != nil {
			s2.Close()
		}
		t.Errorf("a configured server with an offered one's name: %v", err)
	}
}

// A project's servers start only once the user trusts the project's
// set: -trust-mcp trusts and records it, the same set is trusted after,
// a changed set or another project is not, and the user's own servers
// start either way.
func TestAProjectsServersNeedTrust(t *testing.T) {
	var warned syncBuffer
	defer CaptureWarnings(&warned)()
	file := filepath.Join(t.TempDir(), "dax", "mcp-trust.json")
	here := workspace.Descriptor{Kind: workspace.KindLocal, Root: "/src/p"}
	mine := MCPServer{Name: "mine", Command: "my-server"}
	lint := MCPServer{Name: "lint", Command: "lint-mcp", Project: true}
	docs := MCPServer{Name: "docs", URL: "https://docs.example/mcp", Project: true}
	names := func(ms []MCPServer) string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Name)
		}
		return strings.Join(out, ",")
	}

	if got := names(trusted([]MCPServer{mine, lint, docs}, here, false, file, false)); got != "mine" {
		t.Errorf("untrusted: %s", got)
	}
	if w := warned.String(); !strings.Contains(w, "lint, docs") || !strings.Contains(w, "-trust-mcp") {
		t.Errorf("warning %q", w)
	}
	if got := names(trusted([]MCPServer{mine, lint, docs}, here, false, file, true)); got != "mine,lint,docs" {
		t.Errorf("with -trust-mcp: %s", got)
	}
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("trust file: %v, %v", fi, err)
	}
	if di, err := os.Stat(filepath.Dir(file)); err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("trust directory: %v, %v", di, err)
	}
	// The same set, in another order, is trusted without the flag.
	if got := names(trusted([]MCPServer{docs, mine, lint}, here, false, file, false)); got != "docs,mine,lint" {
		t.Errorf("trusted set: %s", got)
	}
	// A changed command, a changed placement, a server more: another set.
	for what, set := range map[string][]MCPServer{
		"a changed command": {mine, {Name: "lint", Command: "lint-mcp --fix", Project: true}, docs},
		"moved to the host": {mine, {Name: "lint", Command: "lint-mcp", Host: true, Project: true}, docs},
		"a server more":     {mine, lint, docs, {Name: "x", Command: "x", Project: true}},
	} {
		if got := names(trusted(set, here, false, file, false)); got != "mine" {
			t.Errorf("%s: %s started", what, got)
		}
	}
	// Another project, at another root or on another machine, is not
	// trusted by this one's trust.
	for _, other := range []workspace.Descriptor{
		{Kind: workspace.KindLocal, Root: "/src/q"},
		{Kind: workspace.KindContainer, Ref: "box", Root: "/src/p"},
	} {
		if got := names(trusted([]MCPServer{mine, lint, docs}, other, false, file, false)); got != "mine" {
			t.Errorf("%+v: %s started", other, got)
		}
	}
	// Trusting a new set for the project replaces the old one.
	newSet := []MCPServer{mine, lint}
	trusted(newSet, here, false, file, true)
	if got := names(trusted([]MCPServer{mine, lint, docs}, here, false, file, false)); got != "mine" {
		t.Errorf("the old set after a new one was trusted: %s", got)
	}
	tf, err := readTrust(file)
	if err != nil || len(tf.Projects) != 1 {
		t.Errorf("trust file: %+v, %v", tf, err)
	}
	// No project servers: nothing to trust, nothing said.
	var quiet syncBuffer
	restore := CaptureWarnings(&quiet)
	if got := names(trusted([]MCPServer{mine}, here, false, file, false)); got != "mine" || quiet.String() != "" {
		t.Errorf("no project servers: %s, %q", got, quiet.String())
	}
	restore()
	// A trust file that is not JSON trusts nothing, and says so.
	if err := os.WriteFile(file, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := names(trusted([]MCPServer{mine, lint}, here, false, file, false)); got != "mine" {
		t.Errorf("a broken trust file: %s", got)
	}
	if _, err := readTrust(file); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("a broken trust file reads as %v", err)
	}
}

// A trust file another account could have written trusts nothing: its
// directory or itself writable by group or others.
func TestATrustFileOthersCouldWriteTrustsNothing(t *testing.T) {
	var warned syncBuffer
	defer CaptureWarnings(&warned)()
	here := workspace.Descriptor{Kind: workspace.KindLocal, Root: "/src/p"}
	set := []MCPServer{{Name: "lint", Command: "lint-mcp", Project: true}}
	for _, which := range []string{"directory", "file"} {
		t.Run(which, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "dax", "mcp-trust.json")
			trusted(set, here, false, file, true)
			if got := trusted(set, here, false, file, false); len(got) != 1 {
				t.Fatalf("the trusted set did not start: %v", got)
			}
			target := filepath.Dir(file)
			if which == "file" {
				target = file
			}
			fi, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, fi.Mode().Perm()|0o020); err != nil {
				t.Fatal(err)
			}
			if got := trusted(set, here, false, file, false); len(got) != 0 {
				t.Errorf("a group-writable %s: %v started", which, got)
			}
			if !strings.Contains(warned.String(), "writable by other users") {
				t.Errorf("warning %q", warned.String())
			}
		})
	}
}

// Two servers whose tools would be named alike do not both start: the
// -mcp flag's "cli" beside a configured "cli", or "a" beside "a_"; and
// /mcp add may not take a configured server's name.
func TestServersNamedAlikeAreRefused(t *testing.T) {
	ctx := context.Background()
	for _, pair := range [][2]string{{"cli", "cli"}, {"a", "a_"}, {"b_", "b"}} {
		o := options(t, &scripted{})
		o.MCP = []MCPServer{{Name: pair[0], Command: "x"}, {Name: pair[1], Command: "y"}}
		if s, err := New(ctx, o); err == nil || !strings.Contains(err.Error(), "named alike") {
			if s != nil {
				s.Close()
			}
			t.Errorf("%s beside %s: %v", pair[0], pair[1], err)
		}
	}
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	o := options(t, &scripted{})
	o.MCP = []MCPServer{{Name: "cfg", Command: os.Args[0]}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"cfg", "cfg_"} {
		if _, err := s.AddMCP(ctx, name, os.Args[0]); err == nil || !strings.Contains(err.Error(), "configured server cfg") {
			t.Errorf("/mcp add %s beside the configured cfg: %v", name, err)
		}
	}
}

// Info lists the session's MCP servers as they were connected: a
// configured one on this machine with its description, a URL's without
// what credentials it carries, and one /mcp add started in the
// workspace with the label /mcp remove takes, until it is removed.
func TestInfoListsTheMCPServers(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	ctx := context.Background()
	o := options(t, &scripted{})
	o.MCP = []MCPServer{{Name: "here", Command: os.Args[0], Host: true, Description: "runs here"}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	label, err := s.AddMCP(ctx, "added", os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	here := MCPServerInfo{Name: "here", Placement: PlacedHost, Description: "runs here"}
	added := MCPServerInfo{Name: "added", Placement: PlacedWorkspace, Where: where(s.Info().Workspace), Added: label}
	if got := s.Info().MCP; !slices.Equal(got, []MCPServerInfo{here, added}) {
		t.Errorf("Info.MCP = %+v", got)
	}
	if err := s.RemoveMCP(label); err != nil {
		t.Fatal(err)
	}
	if got := s.Info().MCP; !slices.Equal(got, []MCPServerInfo{here}) {
		t.Errorf("Info.MCP after /mcp remove = %+v", got)
	}
	if err := s.RemoveMCP("mcp:#9 nothing"); err == nil {
		t.Error("removing a server never added")
	}

	_, info, err := s.mcpDialOf(MCPServer{Name: "u", URL: "https://user:pw@MCP.example:443/mcp/?key=k#f"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Placement != PlacedURL || info.Where != "https://mcp.example/mcp" {
		t.Errorf("a URL's server listed as %+v", info)
	}
}
