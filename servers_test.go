package dax

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	wsremote "github.com/ChristopherDavenport/agentworkspace/remote"
	"github.com/ChristopherDavenport/openresponses/echo"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/offer"
)

// testSecret is testSecretEnv's value in the tests that give it.
const testSecret = "the-offered-secret-9c2e"

// writeServers writes a -servers file offering the test binary as p,
// given secrets when it names any, and returns its path.
func writeServers(t *testing.T, secrets ...string) string {
	t.Helper()
	m := map[string]any{"command": os.Args[0], "description": "the test server"}
	if len(secrets) > 0 {
		m["secret_env"] = secrets
	}
	b, err := json.Marshal(map[string]any{"mcp_servers": map[string]any{"p": m}})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "servers.json")
	write(t, file, string(b))
	return file
}

// call runs the session's tool name with no arguments and returns its
// output.
func call(t *testing.T, s *agent.Session, name string) string {
	t.Helper()
	tl, ok := s.Kit.LookupTool(name)
	if !ok {
		t.Fatalf("no tool %s", name)
	}
	res, err := tl.Execute(t.Context(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(res.Output.Text)
}

// waitFor waits, at most ten seconds, for buf to hold s, which a
// process writes.
func waitFor(t *testing.T, buf *syncBuffer, s string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(buf.String(), s); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %q; have %q", s, buf.String())
		}
	}
	return buf.String()
}

// mustClient is an http.Client sending token to url's origin.
func mustClient(t *testing.T, url, token string) *http.Client {
	t.Helper()
	c, err := authn.Client(nil, url, token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// identify hides nothing the workspace is: over remote, each grant
// gives a client that is a Starter, a ServerProvider, both or neither,
// and identify's result asserts exactly as the client does, with its FS
// one that reads links when the client's does.
func TestIdentifyTellsTheTruth(t *testing.T) {
	t.Setenv(testMCPServerEnv, "1")
	dir := t.TempDir()
	local, err := workspace.NewLocal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	offering, err := offer.Offer(local, []offer.Server{{Name: "p", Command: os.Args[0]}}, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		served          workspace.Workspace
		scopes          []wsremote.Scope
		starts, servers bool
	}{
		{"read", offering, []wsremote.Scope{wsremote.ScopeRead}, false, false},
		{"exec", offering, []wsremote.Scope{wsremote.ScopeRead, wsremote.ScopeExec}, true, false},
		{"servers", offering, []wsremote.Scope{wsremote.ScopeServers}, false, true},
		{"exec and servers", offering, []wsremote.Scope{wsremote.ScopeRead, wsremote.ScopeExec, wsremote.ScopeServers}, true, true},
		{"servers of a workspace offering none", local, []wsremote.Scope{wsremote.ScopeRead, wsremote.ScopeServers}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const token = "the-served-token"
			h, err := wsremote.NewHandler(tc.served, wsremote.WithAuthenticator(authn.New(map[string]wsremote.Principal{
				token: {Name: "dax", Scopes: tc.scopes},
			}).Authenticator(wsremote.ErrUnauthenticated)))
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(h)
			defer srv.Close()
			defer h.Close()
			ws, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: srv.URL}, token)
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			_, starts := ws.(workspace.Starter)
			_, servers := ws.(workspace.ServerProvider)
			if starts != tc.starts || servers != tc.servers {
				t.Errorf("Starter %v, ServerProvider %v; want %v, %v", starts, servers, tc.starts, tc.servers)
			}
			raw, err := wsremote.Dial(t.Context(), srv.URL, wsremote.WithHTTPClient(mustClient(t, srv.URL, token)))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			_, rawStarts := raw.(workspace.Starter)
			_, rawServers := raw.(workspace.ServerProvider)
			_, rawLinks := raw.FS().(fs.ReadLinkFS)
			if _, links := ws.FS().(fs.ReadLinkFS); links != rawLinks || starts != rawStarts || servers != rawServers {
				t.Errorf("identify: ReadLinkFS %v, Starter %v, ServerProvider %v; the client's %v, %v, %v", links, starts, servers, rawLinks, rawStarts, rawServers)
			}
			if d := ws.Descriptor(); d.Ref != config.WorkspaceRef(srv.URL) {
				t.Errorf("descriptor %+v", d)
			}
			if servers {
				sp := ws.(workspace.ServerProvider)
				list, err := sp.Servers(t.Context())
				if err != nil || len(list) != 1 || list[0].Name != "p" {
					t.Errorf("servers %+v, %v", list, err)
				}
			}
		})
	}
}

// dax workspace -servers offers its servers to a session over
// -workspace URL and through workspace.hub alike: under -scopes
// servers the session reaches the server it offers, holding a secret,
// as mcp__p__<tool>, its project config is not read, and what the server
// writes reaches the session redacted; the start line names the
// server and the grant, never the secret.
func TestAWorkspaceOffersItsServers(t *testing.T) {
	home(t)
	t.Setenv(testMCPServerEnv, "1")
	t.Setenv(testSecretEnv, testSecret)
	offered, err := readOffered(writeServers(t, testSecretEnv))
	if err != nil {
		t.Fatal(err)
	}
	servers := []wsremote.Scope{wsremote.ScopeServers}
	for _, tc := range []struct {
		name string
		open func(t *testing.T, dir string) (wsremote.Workspace, *syncBuffer)
	}{
		{"-workspace URL", func(t *testing.T, dir string) (wsremote.Workspace, *syncBuffer) {
			s := serveWorkspace(t, dir, func(w *workspaceServer) { w.offered, w.scopes = offered, servers })
			ws, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: s.url}, s.token(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ws.Close() })
			if !strings.Contains(s.out.String(), "dax workspace: serving "+dir+" at "+s.url+", which may servers\n") {
				t.Errorf("the start line: %q", s.out.String())
			}
			return ws, s.out
		}},
		{"workspace.hub", func(t *testing.T, dir string) (wsremote.Workspace, *syncBuffer) {
			hub, _ := startTestHub(t, tunnelToken, 30*time.Second)
			c := connectWith(t, &workspaceServer{dir: dir, connect: hub.URL, token: tunnelToken, offered: offered, scopes: servers})
			if err := hub.await(t.Context()); err != nil {
				t.Fatalf("await: %v; the workspace said %s", err, c.out)
			}
			if !strings.Contains(c.out.String(), "to the hub at "+hub.URL+", which may servers\n") {
				t.Errorf("the start line: %q", c.out.String())
			}
			return hub.Workspace, c.out
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// A project config the session may not read is no layer.
			write(t, filepath.Join(dir, ".dax", "config.json"), `{"policy":{"deny":["mcp__p__secret"]}}`)
			ws, out := tc.open(t, dir)
			if _, ok := ws.(workspace.Starter); ok {
				t.Error("a servers-only workspace is a Starter")
			}
			if _, ok := ws.(workspace.ServerProvider); !ok {
				t.Fatal("a servers-only workspace offers no servers")
			}
			if line := out.String(); !strings.Contains(line, "dax workspace: offers MCP server p: the test server (given $"+testSecretEnv+", redacted from its output)") || strings.Contains(line, testSecret) {
				t.Errorf("the offered servers' lines: %q", line)
			}
			user, err := loadUser("")
			if err != nil {
				t.Fatal(err)
			}
			st, err := projectSettings("-workspace", ws, user, config.Flags{})
			if err != nil || len(st.Policy.Project.Deny) != 0 {
				t.Fatalf("the project's settings: %v, %v", st.Policy.Project.Deny, err)
			}
			var warned syncBuffer
			defer agent.CaptureWarnings(&warned)()
			sess, err := agent.New(t.Context(), agent.Options{Streamer: &echo.Adapter{}, Model: "echo", Dir: t.TempDir(), Root: filepath.Join(t.TempDir(), "sessions"), UserDir: t.TempDir(), Workspace: ws})
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			if got := call(t, sess, "mcp__p__secret"); !strings.HasPrefix(got, "secret=[[REDACTED "+testSecretEnv+"]]") {
				t.Errorf("the server's output: %q", got)
			}
			stderr := waitFor(t, &warned, "] end")
			if strings.Contains(stderr, testSecret) || !strings.Contains(stderr, "stderr secret=[[REDACTED "+testSecretEnv+"]] end") {
				t.Errorf("the server's standard error: %q", stderr)
			}
		})
	}
}

// dax workspace refuses, before it serves or dials anything, a -servers
// file it cannot offer from, a grant that does not match what it
// offers, and one that would give a secret-holding server's secret
// away: with exec or write beside servers, at an address or to a hub.
func TestOfferingServersRefuses(t *testing.T) {
	home(t)
	t.Setenv(testSecretEnv, testSecret)
	t.Setenv("DAX_SHORT_SECRET", "a1b2c3")
	t.Setenv(config.TunnelTokenEnv, tunnelToken)
	plain, holding := writeServers(t), writeServers(t, testSecretEnv)
	file := func(json string) string {
		f := filepath.Join(t.TempDir(), "servers.json")
		write(t, f, json)
		return f
	}
	hub := []string{"-connect", "wss://box.example/tunnel"}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-servers", file(`{"mcp_servers":{"gh":{"url":"https://mcp.example/"}}}`)}, "a server at a url is not the workspace's to offer"},
		{[]string{"-servers", file(`{"mcp_servers":{"gh":{"command":"x","in":"host"}}}`)}, `in "host" places a server on the session's machine`},
		{[]string{"-servers", file(`{"mcp_servers":{"Git_Hub":{"command":"x"}}}`)}, "an offered server's name is 1 to 32 lowercase letters"},
		{[]string{"-servers", file(`{"model":"x"}`)}, `unknown field "model"`},
		{[]string{"-servers", "/nonexistent/servers.json"}, "workspace: -servers: open /nonexistent/servers.json"},
		{[]string{"-scopes", "read,servers"}, "-scopes servers: the workspace offers none; name them with -servers FILE"},
		{[]string{"-servers", plain, "-scopes", "read,exec"}, "-scopes does not grant servers"},
		{[]string{"-servers", holding, "-listen", "127.0.0.1:0"}, "server p holds secrets (secret_env), which a session granted exec could take"},
		{[]string{"-scopes", "write"}, "-scopes: write without read: the session would act on files it may not read"},
		{[]string{"-scopes", "exec", "-listen", "127.0.0.1:0"}, "-scopes: exec without read"},
		{[]string{"-servers", plain, "-scopes", "servers,exec"}, "-scopes: exec without read"},
		{[]string{"-servers", plain, "-scopes", "servers,write"}, "-scopes: write without read"},
		{append([]string{"-servers", plain, "-scopes", "servers,write"}, hub...), "-scopes: write without read"},
		{[]string{"-servers", file(`{"mcp_servers":{"p":{"command":"x","secret_env":["` + testSecretEnv + `"]},"sh":{"command":"sh-mcp"}}}`), "-scopes", "servers", "-listen", "127.0.0.1:0"}, "server p holds secrets (secret_env), and a secret holder is offered alone"},
		{[]string{"-servers", file(`{"mcp_servers":{"p":{"command":"x","secret_env":["` + testSecretEnv + `"]},"p":{"command":"y"}}}`), "-scopes", "servers"}, `"p" is given twice`},
		{[]string{"-servers", file(`{"mcp_servers":{"p":{"command":"x","secret_env":["DAX_SHORT_SECRET"]}}}`), "-scopes", "servers", "-listen", "127.0.0.1:0"}, "server p: secret_env DAX_SHORT_SECRET is shorter than 8 bytes"},
		{[]string{"-servers", holding, "-scopes", "read,servers,exec"}, "granted exec could take"},
		{[]string{"-servers", holding, "-scopes", "read,write,servers"}, "granted write could take"},
		{append([]string{"-servers", holding}, hub...), "server p holds secrets"},
		{append([]string{"-servers", holding, "-scopes", "read,servers,exec"}, hub...), "granted exec could take"},
		{[]string{"-servers", holding, "-scopes", "servers", "-pass-env", testSecretEnv}, "-pass-env: " + testSecretEnv + " is server p's secret (secret_env)"},
		{[]string{"-servers", file(`{"mcp_servers":{"p":{"command":"x","secret_env":["DAX_WORKSPACE_TOKEN"]}}}`), "-scopes", "servers"}, "DAX_WORKSPACE_TOKEN is a served workspace's token"},
		{[]string{"-servers", file(`{"mcp_servers":{"p":{"command":"x","secret_env":["DAX_UNSET_SECRET"]}}}`), "-scopes", "servers", "-listen", "127.0.0.1:0"}, "server p: secret_env DAX_UNSET_SECRET is not set"},
		{[]string{"-scopes", "read,admin"}, `-scopes: no scope "admin"; want read, write, exec and servers`},
	} {
		// Bounded, so a grant wrongly taken fails here rather than
		// serving until the test's end.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := run(ctx, append([]string{"workspace"}, tc.args...), program{name: "dax"})
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), testSecret) {
			t.Errorf("%v: the error holds the secret: %v", tc.args, err)
		}
	}
	// A refused grant takes no token: it is still in the environment.
	if os.Getenv(config.TunnelTokenEnv) != tunnelToken {
		t.Error("a refused grant took the hub's token")
	}
}

// -scopes applies at an address as through a hub: a workspace served
// with read alone gives a session that reads and starts nothing.
func TestScopesApplyToListen(t *testing.T) {
	home(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "README.md"), "# hi\n")
	s := serveWorkspace(t, dir, func(w *workspaceServer) { w.scopes = []wsremote.Scope{wsremote.ScopeRead} })
	if !strings.Contains(s.out.String(), ", which may read\n") {
		t.Errorf("the start line: %q", s.out.String())
	}
	ws, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: s.url}, s.token(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if _, ok := ws.(workspace.Starter); ok {
		t.Error("a read-only grant is a Starter")
	}
	if b, err := workspace.Read(ws, "README.md", 1<<10); err != nil || string(b) != "# hi\n" {
		t.Errorf("read %q, %v", b, err)
	}
	if err := ws.WriteFile(t.Context(), "made.txt", []byte("x"), 0o644); err == nil {
		t.Error("a write without write")
	}
	// The default is read, write and exec.
	d := serveWorkspace(t, t.TempDir(), nil)
	if !strings.Contains(d.out.String(), ", which may read, write, exec\n") {
		t.Errorf("the default start line: %q", d.out.String())
	}
}

// A grant of exec or write without read would act on the project under
// none of its rules, which only tighten: dax workspace refuses it, and a
// session over another server granting it fails at the project's
// config rather than starting without it; a servers-only grant, which
// can touch nothing, starts with the config left out.
func TestAGrantWithoutReadKeepsTheProjectsRules(t *testing.T) {
	home(t)
	t.Setenv(testMCPServerEnv, "1")
	offered, err := readOffered(writeServers(t))
	if err != nil {
		t.Fatal(err)
	}
	user, err := loadUser("")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		scopes []wsremote.Scope
		want   string
	}{
		{[]wsremote.Scope{wsremote.ScopeServers, wsremote.ScopeExec}, "exec without read"},
		{[]wsremote.Scope{wsremote.ScopeServers, wsremote.ScopeWrite}, "write without read"},
		{[]wsremote.Scope{wsremote.ScopeServers}, ""},
	} {
		w := &workspaceServer{scopes: tc.scopes, offered: offered}
		if err := w.settle(); (err == nil) != (tc.want == "") || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: settle = %v, want %q", tc.scopes, err, tc.want)
		}
		// The same grant from a server dax does not run.
		dir := t.TempDir()
		write(t, filepath.Join(dir, ".dax", "config.json"), `{"policy":{"deny":["bash"]}}`)
		local, err := workspace.NewLocal(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		o, err := offer.Offer(local, offered, os.LookupEnv)
		if err != nil {
			t.Fatal(err)
		}
		const token = "the-served-token"
		h, err := wsremote.NewHandler(o, wsremote.WithAuthenticator(authn.New(map[string]wsremote.Principal{
			token: {Name: "dax", Scopes: tc.scopes},
		}).Authenticator(wsremote.ErrUnauthenticated)))
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h)
		ws, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: srv.URL}, token)
		if err != nil {
			t.Fatal(err)
		}
		st, err := projectSettings("-workspace", ws, user, config.Flags{})
		switch {
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), "grant read too")):
			t.Errorf("%v: the project's settings %v, %v; want refused", tc.scopes, st.Policy.Project.Deny, err)
		case tc.want == "" && (err != nil || len(st.Policy.Project.Deny) != 0):
			t.Errorf("%v: servers alone: %v, %v", tc.scopes, st.Policy.Project.Deny, err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".dax", "config.json")); statErr != nil {
			t.Errorf("%v: the probe touched the project: %v", tc.scopes, statErr)
		}
		ws.Close()
		srv.Close()
		h.Close()
		o.Close()
	}
}
