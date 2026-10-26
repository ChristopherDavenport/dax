package dax

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	wsremote "github.com/ChristopherDavenport/agentworkspace/remote"
	"github.com/ChristopherDavenport/openresponses/echo"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/config"
)

// tunnelToken is a token long enough to enroll a workspace.
const tunnelToken = "the-tunnel-token-of-at-least-32-bytes"

var allScopes = []wsremote.Scope{wsremote.ScopeRead, wsremote.ScopeWrite, wsremote.ScopeExec}

// connecting is a dax workspace -connect serving dir to url with token
// in the test, until stop.
type connecting struct {
	out  *syncBuffer
	stop func() error
	done chan error
}

func connectWorkspace(t *testing.T, dir, url, token string, scopes ...wsremote.Scope) *connecting {
	t.Helper()
	if scopes == nil {
		scopes = allScopes
	}
	c := &connecting{out: &syncBuffer{}, done: make(chan error, 1)}
	w := &workspaceServer{name: "dax", dir: dir, out: c.out, connect: url, token: token, scopes: scopes}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { c.done <- w.run(ctx) }()
	var err error
	stopped := false
	c.stop = func() error {
		if !stopped {
			stopped = true
			cancel()
			select {
			case err = <-c.done:
			case <-time.After(30 * time.Second):
				t.Error("dax workspace -connect did not stop")
			}
		}
		return err
	}
	t.Cleanup(func() { c.stop() })
	return c
}

// startTestHub is a session's hub on a loopback port of its own,
// enrolling token as build, waiting at most wait.
func startTestHub(t *testing.T, token string, wait time.Duration) (*hostedHub, *syncBuffer) {
	t.Helper()
	out := &syncBuffer{}
	h, err := startHub(config.HubSettings{Listen: "127.0.0.1:0", Name: "build", TokenEnv: config.TunnelTokenEnv, Wait: wait}, token, "dax", out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h, out
}

// dax workspace -connect refuses, before it dials anything, a hub it
// would reach in the clear or with credentials in its URL, a token it
// lacks or one too short, -listen and its certificate beside it, scopes
// it does not know or none, and its own flags without it.
func TestConnectingAWorkspaceRefuses(t *testing.T) {
	home(t)
	for _, tc := range []struct {
		args  []string
		token string
		want  string
	}{
		{[]string{"-connect", "ws://box.example:7780/tunnel"}, tunnelToken, "ws:// only to a loopback host"},
		{[]string{"-connect", "ws://10.0.0.1:7780/tunnel"}, tunnelToken, "ws:// only to a loopback host"},
		{[]string{"-connect", "https://box.example/tunnel"}, tunnelToken, "want the wss:// URL of a session's hub"},
		{[]string{"-connect", "box.example:7780"}, tunnelToken, "want the wss:// URL of a session's hub"},
		{[]string{"-connect", ""}, tunnelToken, "-connect: give the hub's wss:// URL"},
		{[]string{"-connect", "wss://me:pw@box.example/tunnel"}, tunnelToken, "carries credentials"},
		{[]string{"-connect", "wss://box.example/tunnel?token=x"}, tunnelToken, "a query or fragment"},
		{[]string{"-connect", "wss://box.example/tunnel"}, "", "no token: set $DAX_TUNNEL_TOKEN"},
		{[]string{"-connect", "wss://box.example/tunnel"}, "short", "$DAX_TUNNEL_TOKEN is 5 bytes; the token must be at least 32"},
		{[]string{"-connect", "wss://box.example/tunnel", "-listen", "127.0.0.1:0"}, tunnelToken, "-listen and -connect are exclusive"},
		{[]string{"-connect", "wss://box.example/tunnel", "-tls-cert", "c.pem", "-tls-key", "k.pem"}, tunnelToken, "-tls-cert and -tls-key are -listen's"},
		{[]string{"-connect", "wss://box.example/tunnel", "-scopes", "read,admin"}, tunnelToken, `-scopes: no scope "admin"`},
		{[]string{"-connect", "wss://box.example/tunnel", "-scopes", ""}, tunnelToken, "-scopes: none given"},
		{[]string{"-connect", "wss://box.example/tunnel", "-scopes", " , "}, tunnelToken, "-scopes: none given"},
		{[]string{"-connect", "wss://box.example/tunnel", "-tls-ca", "/nonexistent/ca.pem"}, tunnelToken, "workspace: -tls-ca"},
		{[]string{"-connect", "wss://box.example/tunnel", "-pass-env", "DAX_TUNNEL_TOKEN"}, tunnelToken, "-pass-env: DAX_TUNNEL_TOKEN is this server's token"},
		{[]string{"-tls-ca", "ca.pem"}, tunnelToken, "-tls-ca goes with -connect"},
		{[]string{"-scopes", "read"}, tunnelToken, "-scopes goes with -connect"},
	} {
		t.Setenv(config.TunnelTokenEnv, tc.token)
		err := run(context.Background(), append([]string{"workspace"}, tc.args...), program{name: "dax"})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
		if tc.token != "" && strings.Contains(err.Error(), tc.token) {
			t.Errorf("%v: the error holds the token: %v", tc.args, err)
		}
	}
	// The token is taken out of the environment as soon as it is read.
	t.Setenv(config.TunnelTokenEnv, tunnelToken)
	run(context.Background(), []string{"workspace", "-connect", "wss://box.example/tunnel", "-tls-ca", "/nonexistent/ca.pem"}, program{name: "dax"})
	if v, ok := os.LookupEnv(config.TunnelTokenEnv); ok {
		t.Errorf("$DAX_TUNNEL_TOKEN is still set: %q", v)
	}
}

// dax workspace -connect dials the session's hub, which takes it as its
// workspace: files and processes are the served directory's, the banner
// names it through the hub, neither end prints the token, and stopping
// the workspace's end ends the process the session started there,
// within a bounded wait. -list then finds the session by the hub's
// name in the config, listening on nothing.
func TestAWorkspaceConnectsToTheSessionsHub(t *testing.T) {
	h := home(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "README.md"), "# hi\n")
	hub, hubOut := startTestHub(t, tunnelToken, 30*time.Second)
	if !strings.HasPrefix(hub.URL, "ws://127.0.0.1:") || !strings.HasSuffix(hub.URL, "/tunnel") {
		t.Fatalf("the hub's URL: %s", hub.URL)
	}
	c := connectWorkspace(t, dir, hub.URL, tunnelToken)
	if err := hub.await(t.Context()); err != nil {
		t.Fatalf("await: %v; the workspace said %s", err, c.out)
	}
	ws := hub.Workspace
	if d := ws.Descriptor(); d.Kind != workspace.KindRemote || d.Ref != "http://build.tunnel.invalid" || d.Root != dir {
		t.Errorf("descriptor %+v", d)
	}
	if line := hubLine(hub, "build"); line != "remote build through the hub at "+hub.URL+" · "+dir {
		t.Errorf("banner %q", line)
	}
	if b, err := workspace.Read(ws, "README.md", 1<<10); err != nil || string(b) != "# hi\n" {
		t.Errorf("read %q, %v", b, err)
	}
	if err := ws.WriteFile(t.Context(), "made.txt", []byte("made"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "made.txt")); string(b) != "made" {
		t.Errorf("the write landed %q", b)
	}
	st, ok := ws.(workspace.Starter)
	if !ok {
		t.Fatal("a workspace connected with exec is no Starter")
	}
	p, err := st.Start(t.Context(), workspace.Command{Args: []string{"sh", "-c", "echo $$ > pid && exec sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the process never wrote its pid")
		}
		b, _ := os.ReadFile(filepath.Join(dir, "pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}

	// A session through it, recorded for -list.
	root := filepath.Join(t.TempDir(), "sessions")
	here := t.TempDir()
	sess, err := agent.New(t.Context(), agent.Options{Streamer: &echo.Adapter{}, Model: "echo", Dir: here, Root: root, UserDir: t.TempDir(), Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	sess.Close()

	start := time.Now()
	if err := c.stop(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("stopping took %v", took)
	}
	for deadline := time.Now().Add(10 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a process outlived dax workspace -connect")
		}
	}
	for _, out := range []string{hubOut.String(), c.out.String()} {
		if strings.Contains(out, tunnelToken) {
			t.Errorf("the token was printed: %s", out)
		}
	}
	if out := c.out.String(); !strings.Contains(out, "dax workspace: serving "+dir+" to the hub at "+hub.URL+", which may read, write, exec") || !strings.Contains(out, "token: $DAX_TUNNEL_TOKEN") {
		t.Errorf("the workspace's end said %q", out)
	}
	if out := hubOut.String(); !strings.Contains(out, "waiting up to 30s for workspace build at "+hub.URL+"; on its host run: DAX_TUNNEL_TOKEN=... dax workspace -connect "+hub.URL) {
		t.Errorf("the hub said %q", out)
	}
	hub.Close()

	// -list: the config's hub names an address this test holds, which
	// a listing that listened could not take.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg := filepath.Join(h, "hub.json")
	write(t, cfg, `{"workspace":{"hub":{"listen":"`+busy.Addr().String()+`","name":"build"}}}`)
	var out bytes.Buffer
	if err := runList(context.Background(), &out, root, here, cfg, config.Flags{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); !strings.Contains(got, id) || !strings.HasSuffix(got, "  remote http://build.tunnel.invalid:"+dir) {
		t.Errorf("-list with the config's hub: %q", got)
	}
	write(t, cfg, `{"workspace":{"hub":{"listen":"`+busy.Addr().String()+`","name":"other"}}}`)
	out.Reset()
	if err := runList(context.Background(), &out, root, here, cfg, config.Flags{}); err != nil || strings.Contains(out.String(), id) {
		t.Errorf("-list with another name: %q, %v", out.String(), err)
	}
}

// -scopes read,write is a workspace the session reads and writes but
// starts no process in: no Starter, Exec forbidden, and the banner says
// where the MCP servers run.
func TestAWorkspaceConnectedWithoutExec(t *testing.T) {
	home(t)
	dir := t.TempDir()
	hub, _ := startTestHub(t, tunnelToken, 30*time.Second)
	connectWorkspace(t, dir, hub.URL, tunnelToken, wsremote.ScopeRead, wsremote.ScopeWrite)
	if err := hub.await(t.Context()); err != nil {
		t.Fatal(err)
	}
	ws := hub.Workspace
	if _, ok := ws.(workspace.Starter); ok {
		t.Error("a workspace connected without exec is a Starter")
	}
	if _, err := ws.Exec(t.Context(), workspace.Command{Args: []string{"true"}}); err == nil {
		t.Error("Exec without exec ran")
	}
	if err := ws.WriteFile(t.Context(), "made.txt", []byte("made"), 0o644); err != nil {
		t.Errorf("a write with write: %v", err)
	}
	if line := hubLine(hub, "build"); !strings.HasSuffix(line, "starts no process there: your MCP servers run here, the project's not at all") {
		t.Errorf("banner %q", line)
	}
}

// The session's hub refuses a workspace whose token is not its own:
// dax workspace -connect stops at the first refusal, saying so without
// either token, and the session, which no workspace reached, says what
// to run on the workspace's host once its wait is up.
func TestTheSessionsHubRefusesAnotherToken(t *testing.T) {
	home(t)
	hub, hubOut := startTestHub(t, tunnelToken, 2*time.Second)
	other := "another-token-of-at-least-32-bytes-long"
	c := connectWorkspace(t, t.TempDir(), hub.URL, other)
	select {
	case err := <-c.done:
		if err == nil || !strings.Contains(err.Error(), "workspace: -connect:") || !strings.Contains(err.Error(), "401") {
			t.Errorf("a refused token: %v", err)
		} else if strings.Contains(err.Error(), other) || strings.Contains(err.Error(), tunnelToken) {
			t.Errorf("the error holds a token: %v", err)
		}
		c.done <- err
	case <-time.After(30 * time.Second):
		t.Fatal("dax workspace -connect kept dialling a hub that refused its token")
	}
	err := hub.await(t.Context())
	if err == nil || !strings.Contains(err.Error(), "workspace build did not connect to "+hub.URL+" within 2s; on its host run: DAX_TUNNEL_TOKEN=... dax workspace -connect "+hub.URL) {
		t.Errorf("await: %v", err)
	}
	if hub.Workspace != nil || len(hub.hub.Connected()) != 0 {
		t.Errorf("a workspace connected with another token: %v", hub.hub.Connected())
	}
	for _, out := range []string{hubOut.String(), c.out.String(), err.Error()} {
		if strings.Contains(out, tunnelToken) || strings.Contains(out, other) {
			t.Errorf("a token was printed: %s", out)
		}
	}
}

// A session with workspace.hub takes its token before anything starts,
// refuses a short one, listens as the config says and, with no
// workspace connecting within its wait, fails saying what to run on the
// workspace's host, never the token.
func TestASessionsHubOnTheCommandLine(t *testing.T) {
	h := home(t)
	t.Chdir(t.TempDir())
	p := program{name: "dax", version: "test"}
	cfgPath := filepath.Join(h, ".config", "dax", "config.json")
	for _, tc := range []struct {
		cfg, token, want string
	}{
		{`{"workspace":{"hub":{"listen":"127.0.0.1:0","name":"build","wait":"300ms"}}}`, "", "workspace.hub: no token: set $DAX_TUNNEL_TOKEN"},
		{`{"workspace":{"hub":{"listen":"127.0.0.1:0","name":"build","wait":"300ms"}}}`, "short", "workspace.hub: $DAX_TUNNEL_TOKEN is 5 bytes"},
		{`{"workspace":{"hub":{"listen":"127.0.0.1:0","name":"build","wait":"300ms"}}}`, tunnelToken, "workspace build did not connect to ws://127.0.0.1:"},
		{`{"workspace":{"hub":{"listen":"127.0.0.1:0","name":"build","wait":"300ms"}}}`, tunnelToken, "within 300ms; on its host run: DAX_TUNNEL_TOKEN=... dax workspace -connect ws://127.0.0.1:"},
		// Refused with the config, before the token is read.
		{`{"workspace":{"hub":{"listen":"0.0.0.0:0","name":"build"}}}`, "", "the hub listens on loopback only"},
		{`{"workspace":{"hub":{"listen":"127.0.0.1:0","name":"build","tls_cert":"/nonexistent/c.pem","tls_key":"/nonexistent/k.pem"}}}`, tunnelToken, "workspace.hub: -tls-cert, -tls-key"},
	} {
		write(t, cfgPath, tc.cfg)
		t.Setenv(config.TunnelTokenEnv, tc.token)
		err := run(context.Background(), []string{"-sessions", "", "-p", "hi"}, p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s with token %q: err = %v, want %q", tc.cfg, tc.token, err, tc.want)
		} else if tc.token != "" && strings.Contains(err.Error(), tc.token) {
			t.Errorf("the error holds the token: %v", err)
		}
		if v := os.Getenv(config.TunnelTokenEnv); v != "" {
			t.Errorf("%s: $DAX_TUNNEL_TOKEN is still %q", tc.cfg, v)
		}
	}
}
