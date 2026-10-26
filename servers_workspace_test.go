package dax

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	wsremote "github.com/ChristopherDavenport/agentworkspace/remote"
	"github.com/ChristopherDavenport/openresponses/echo"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/offer"
	"github.com/ChristopherDavenport/dax/policy"
)

// A servers workspace on the command line: dax workspace, granted
// servers alone, says to connect to it with -servers-workspace and its
// own token variable; a session started with it takes that token
// before anything starts, connects its offered servers beside this
// directory, and records it, so -list names it after the session's
// line. One granted more than servers is refused.
func TestAServersWorkspaceOnTheCommandLine(t *testing.T) {
	h := home(t)
	t.Setenv(testMCPServerEnv, "1")
	t.Setenv(testSecretEnv, testSecret)
	here := t.TempDir()
	t.Chdir(here)
	root := filepath.Join(t.TempDir(), "sessions")
	// The model is unreachable, so the prompt fails; the session has
	// opened, its servers connected, before it.
	write(t, filepath.Join(h, ".config", "dax", "config.json"), `{"provider":"openresponses","model":"m","base_url":"http://127.0.0.1:1/v1"}`)
	offered, err := readOffered(writeServers(t, testSecretEnv))
	if err != nil {
		t.Fatal(err)
	}
	s := serveWorkspace(t, t.TempDir(), func(w *workspaceServer) {
		w.offered, w.scopes = offered, []wsremote.Scope{wsremote.ScopeServers}
	})
	out := s.out.String()
	for _, want := range []string{
		`connect: DAX_SERVERS_WORKSPACE_TOKEN="$(cat ` + s.tokenFile + `)" dax -servers-workspace ` + s.url,
		`{"servers_workspace": {"url": "` + s.url + `", "token_env": "DAX_SERVERS_WORKSPACE_TOKEN"}}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dax workspace's lines lack %q:\n%s", want, out)
		}
	}
	t.Setenv(config.ServersWorkspaceTokenEnv, s.token(t))
	p := program{name: "dax", version: "test"}
	err = run(context.Background(), []string{"-servers-workspace", s.url, "-sessions", root, "-p", "hi"}, p)
	if err == nil || strings.Contains(err.Error(), "servers workspace") {
		t.Fatalf("run: %v; want the model's failure alone", err)
	}
	if v := os.Getenv(config.ServersWorkspaceTokenEnv); v != "" {
		t.Errorf("$%s is still %q", config.ServersWorkspaceTokenEnv, v)
	}
	var listed bytes.Buffer
	if err := runList(context.Background(), &listed, root, here, "", config.Flags{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(listed.String()); strings.Count(got, "\n") != 0 || !strings.HasSuffix(got, "  servers remote "+config.WorkspaceRef(s.url)) {
		t.Errorf("-list: %q, want this directory's session, naming the servers workspace", got)
	}

	// A grant with exec and write is refused, whatever it offers.
	plain, err := readOffered(writeServers(t))
	if err != nil {
		t.Fatal(err)
	}
	wide := serveWorkspace(t, t.TempDir(), func(w *workspaceServer) { w.offered = plain })
	if !strings.Contains(wide.out.String(), "dax -workspace "+wide.url) {
		t.Errorf("a full grant's lines: %s", wide.out.String())
	}
	t.Setenv(config.ServersWorkspaceTokenEnv, wide.token(t))
	err = run(context.Background(), []string{"-servers-workspace", wide.url, "-sessions", "", "-p", "hi"}, p)
	if err == nil || !strings.Contains(err.Error(), "the servers workspace remote "+config.WorkspaceRef(wide.url)) || !strings.Contains(err.Error(), "grants more than servers (read,write,exec,servers)") {
		t.Errorf("a servers workspace granting exec and write: %v", err)
	}
}

// -servers-workspace and servers_workspace are refused as -workspace's
// are, saying which setting, and a hub's waits and token as
// workspace.hub's do, under their own default variables.
func TestServersWorkspaceRefusalsOnTheCommandLine(t *testing.T) {
	h := home(t)
	t.Chdir(t.TempDir())
	t.Setenv(config.ServersWorkspaceTokenEnv, "")
	t.Setenv(config.ServersTunnelTokenEnv, "")
	p := program{name: "dax", version: "test"}
	for _, tc := range []struct {
		args         []string
		cfg, project string
		token        string
		want         string
	}{
		{args: []string{"-servers-workspace", "http://box.example:7781"}, want: "-servers-workspace \"http://box.example:7781\": http only to a loopback host"},
		{args: []string{"-servers-workspace", "https://me:pw@box.example"}, want: "carries credentials"},
		{args: []string{"-servers-workspace", "http://127.0.0.1:1"}, want: "-servers-workspace: no token: set $DAX_SERVERS_WORKSPACE_TOKEN"},
		{cfg: `{"servers_workspace":{"url":"http://127.0.0.1:1","token_env":"GH_WS_TOKEN"}}`, want: "-servers-workspace: no token: set $GH_WS_TOKEN"},
		{cfg: `{"servers_workspace":{"url":"http://127.0.0.1:1"}}`, token: "x", want: "-servers-workspace http://127.0.0.1:1: "},
		{cfg: `{"servers_workspace":{"hub":{"listen":"127.0.0.1:0","name":"gh","wait":"300ms"}}}`, want: "servers_workspace.hub: no token: set $DAX_SERVERS_TUNNEL_TOKEN"},
		{cfg: `{"servers_workspace":{"hub":{"listen":"127.0.0.1:0","name":"gh","wait":"300ms"}}}`, token: tunnelToken, want: "servers_workspace.hub: workspace gh did not connect to ws://127.0.0.1:"},
		{cfg: `{"servers_workspace":{"hub":{"listen":"127.0.0.1:0","name":"gh","wait":"300ms"}}}`, token: tunnelToken, want: "/tunnel -scopes servers -servers FILE"},
		{cfg: `{"servers_workspace":{"hub":{"listen":"0.0.0.0:0","name":"gh"}}}`, want: "servers_workspace.hub.listen \"0.0.0.0:0\": without tls_cert and tls_key"},
		{cfg: `{"workspace":{"url":"http://127.0.0.1:7779"},"servers_workspace":{"url":"http://127.0.0.1:7779/"}}`, want: "servers_workspace http://127.0.0.1:7779 is the session's workspace too"},
		{cfg: `{"servers_workspace":{"url":"http://127.0.0.1:7781","token_env":"SHARED"},"workspace":{"url":"http://127.0.0.1:7779","token_env":"SHARED"}}`, want: "servers_workspace.token_env: SHARED is workspace.token_env too"},
		{project: `{"servers_workspace":{"url":"https://evil.example"}}`, want: "servers_workspace: a project file may only tighten"},
	} {
		os.Remove(filepath.Join(h, ".config", "dax", "config.json"))
		if tc.cfg != "" {
			write(t, filepath.Join(h, ".config", "dax", "config.json"), tc.cfg)
		}
		os.RemoveAll(".dax")
		if tc.project != "" {
			write(t, filepath.Join(".dax", "config.json"), tc.project)
		}
		env := config.ServersWorkspaceTokenEnv
		if strings.Contains(tc.cfg, `"hub"`) {
			env = config.ServersTunnelTokenEnv
		}
		t.Setenv(env, tc.token)
		err := run(context.Background(), append(tc.args, "-sessions", "", "-p", "hi"), p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v %s %s: err = %v, want %q", tc.args, tc.cfg, tc.project, err, tc.want)
		} else if tc.token != "" && len(tc.token) > 1 && strings.Contains(err.Error(), tc.token) {
			t.Errorf("the error holds the token: %v", err)
		}
	}
}

// The banner names the servers workspace on a line of its own, after
// where the tools act.
func TestTheBannerNamesTheServersWorkspace(t *testing.T) {
	lines := strings.Join(banner(frontInfo{Name: "dax", Workspace: "remote https://box.example · /work", Servers: "remote https://gh.example · its servers alone"}, agent.Info{}), "\n")
	if !strings.Contains(lines, "\nworkspace: remote https://box.example · /work\nservers: remote https://gh.example · its servers alone") {
		t.Errorf("banner:\n%s", lines)
	}
	if lines := strings.Join(banner(frontInfo{Name: "dax"}, agent.Info{}), "\n"); strings.Contains(lines, "servers:") {
		t.Errorf("a banner without one:\n%s", lines)
	}
}

// Every front says where the servers come from without -v: the
// terminal client prints the servers line with the workspace's before
// it takes the screen, and the REPL prints elsewhereLines.
func TestTheServersLineIsShownWithoutVerbose(t *testing.T) {
	r := startRig(t, &steps{}, policy.Rules{}, nil, func(f *tuiFront) {
		f.info.Workspace = "remote https://box.example · /work"
		f.info.Servers = "remote https://gh.example · its servers alone"
	}, true)
	r.quit()
	want := "workspace: remote https://box.example · /work\nservers: remote https://gh.example · its servers alone\nTo resume this session: dax -resume " + r.sess.ID() + "\n"
	if pre := r.pre.String(); pre != want {
		t.Errorf("the output is %q, want %q", pre, want)
	}
	if got := elsewhereLines(frontInfo{Servers: "remote https://gh.example · its servers alone"}); len(got) != 1 || got[0] != "servers: remote https://gh.example · its servers alone" {
		t.Errorf("elsewhereLines with the tools here: %q", got)
	}
}

// One token for both connections is refused whatever their kinds: the
// servers workspace's host holds it, and would hold the session's
// workspace's grant with it. Both are taken first, so neither is left
// in the environment.
func TestOneTokenForBothConnectionsIsRefused(t *testing.T) {
	h := home(t)
	t.Chdir(t.TempDir())
	p := program{name: "dax", version: "test"}
	const url, hub = `{"url":"http://127.0.0.1:1"}`, `{"hub":{"listen":"127.0.0.1:0","name":"%s","wait":"300ms"}}`
	for _, tc := range []struct {
		name, ws, sw string
		envs         [2]string
	}{
		{"url and url", url, `{"url":"http://127.0.0.1:2"}`, [2]string{config.WorkspaceTokenEnv, config.ServersWorkspaceTokenEnv}},
		{"url and hub", url, strings.Replace(hub, "%s", "gh", 1), [2]string{config.WorkspaceTokenEnv, config.ServersTunnelTokenEnv}},
		{"hub and url", strings.Replace(hub, "%s", "build", 1), `{"url":"http://127.0.0.1:2"}`, [2]string{config.TunnelTokenEnv, config.ServersWorkspaceTokenEnv}},
		{"hub and hub", strings.Replace(hub, "%s", "build", 1), strings.Replace(hub, "%s", "gh", 1), [2]string{config.TunnelTokenEnv, config.ServersTunnelTokenEnv}},
	} {
		write(t, filepath.Join(h, ".config", "dax", "config.json"), `{"workspace":`+tc.ws+`,"servers_workspace":`+tc.sw+`}`)
		for _, env := range tc.envs {
			t.Setenv(env, tunnelToken)
		}
		err := run(context.Background(), []string{"-sessions", "", "-p", "hi"}, p)
		if err == nil || !strings.Contains(err.Error(), "servers_workspace: its token is the session's workspace's too") || strings.Contains(err.Error(), tunnelToken) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
		for _, env := range tc.envs {
			if v := os.Getenv(env); v != "" {
				t.Errorf("%s: $%s is still set", tc.name, env)
			}
		}
	}
}

// A dialed servers workspace whose host stops answering, without a
// reset, does not hold the exit: past the close's deadline its requests
// are given up, as a hub is closed under the session.
func TestADialedServersWorkspaceThatHangsDoesNotHoldTheClose(t *testing.T) {
	home(t)
	t.Setenv(testMCPServerEnv, "1")
	plain, err := readOffered(writeServers(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	local, err := workspace.NewLocal(dir, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	o, err := offer.Offer(local, plain, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	const token = "the-served-token"
	h, err := wsremote.NewHandler(o, wsremote.WithAuthenticator(authn.New(map[string]wsremote.Principal{
		token: {Name: "dax", Scopes: []wsremote.Scope{wsremote.ScopeServers}},
	}).Authenticator(wsremote.ErrUnauthenticated)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var hang atomic.Bool
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() {
			<-released
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	defer close(released)
	sw, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: srv.URL, Field: config.ServersField}, token)
	if err != nil {
		t.Fatal(err)
	}
	defer sw.Close()
	sess, err := agent.New(t.Context(), agent.Options{Streamer: &echo.Adapter{}, Model: "echo", Dir: t.TempDir(), Root: filepath.Join(t.TempDir(), "sessions"), UserDir: t.TempDir(), ServersWorkspace: sw})
	if err != nil {
		t.Fatal(err)
	}
	if got := call(t, sess, "mcp__p__where"); !strings.HasPrefix(got, "dir=[") {
		t.Fatalf("where = %q", got)
	}
	hang.Store(true)
	a, ok := sw.(abandoner)
	if !ok {
		t.Fatal("a dialed workspace cannot be abandoned")
	}
	start := time.Now()
	closeSession(sess, nil, a)
	took := time.Since(start)
	t.Logf("closed in %v", took)
	if took > hubCloseWait+3*time.Second {
		t.Errorf("closing with the host hung took %v, want about %v", took, hubCloseWait)
	}
}
