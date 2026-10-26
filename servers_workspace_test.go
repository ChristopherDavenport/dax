package dax

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsremote "github.com/ChristopherDavenport/agentworkspace/remote"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/config"
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
	if err == nil || !strings.Contains(err.Error(), "the servers workspace remote "+config.WorkspaceRef(wide.url)) || !strings.Contains(err.Error(), "grants more than servers (exec or write)") {
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
