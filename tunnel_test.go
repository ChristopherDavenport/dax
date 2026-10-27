package dax

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	wsremote "github.com/ChristopherDavenport/agentworkspace/remote"
	wstunnel "github.com/ChristopherDavenport/agentworkspace/tunnel"
	"github.com/ChristopherDavenport/openresponses/echo"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/config"
)

// testMCPServerEnv set to 1 makes the test binary serveTestMCP.
const testMCPServerEnv = "DAX_TEST_MCP_SERVER"

// testSecretEnv is the variable serveTestMCP's secret tool reports.
const testSecretEnv = "DAX_TEST_SECRET"

// serveTestMCP is a stdio MCP server with two tools: where, which says
// the directory it runs in and its process id, and secret, which writes
// testSecretEnv's value to its standard error, a line ending "end", and
// returns it.
func serveTestMCP() int {
	srv := sdk.NewServer(&sdk.Implementation{Name: "t", Version: "1"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "where", Description: "report where the server runs"},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, struct{}, error) {
			wd, _ := os.Getwd()
			text := "dir=[" + wd + "] pid=[" + strconv.Itoa(os.Getpid()) + "]"
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}, struct{}{}, nil
		})
	sdk.AddTool(srv, &sdk.Tool{Name: "secret", Description: "report the secret"},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, struct{}, error) {
			v := os.Getenv(testSecretEnv)
			os.Stderr.WriteString("stderr secret=[" + v + "] end\n")
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "secret=[" + v + "]"}}}, struct{}{}, nil
		})
	if err := srv.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		return 1
	}
	return 0
}

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
	return connectWith(t, &workspaceServer{dir: dir, connect: url, token: token, scopes: scopes})
}

// connectWith runs w, its defaults filled in, as connectWorkspace does.
func connectWith(t *testing.T, w *workspaceServer) *connecting {
	t.Helper()
	if w.scopes == nil {
		w.scopes = allScopes
	}
	if w.hubName == "" {
		w.hubName = "build" // startTestHub's
	}
	c := &connecting{out: &syncBuffer{}, done: make(chan error, 1)}
	w.name, w.out = "dax", c.out
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
		{[]string{"-connect", "wss://box.example/tunnel", "-name", "build"}, "", "no token: set $DAX_TUNNEL_TOKEN"},
		{[]string{"-connect", "wss://box.example/tunnel", "-name", "build"}, "short", "$DAX_TUNNEL_TOKEN is 5 bytes; the token must be at least 32"},
		{[]string{"-connect", "wss://box.example/tunnel", "-listen", "127.0.0.1:0"}, tunnelToken, "-listen and -connect are exclusive"},
		{[]string{"-connect", "wss://box.example/tunnel", "-tls-cert", "c.pem", "-tls-key", "k.pem"}, tunnelToken, "-tls-cert and -tls-key are -listen's"},
		{[]string{"-connect", "wss://box.example/tunnel", "-scopes", "read,admin"}, tunnelToken, `-scopes: no scope "admin"`},
		{[]string{"-connect", "wss://box.example/tunnel", "-scopes", ""}, tunnelToken, "-scopes: none given"},
		{[]string{"-connect", "wss://box.example/tunnel", "-scopes", " , "}, tunnelToken, "-scopes: none given"},
		{[]string{"-connect", "wss://box.example/tunnel", "-name", "build", "-tls-ca", "/nonexistent/ca.pem"}, tunnelToken, "workspace: -tls-ca"},
		{[]string{"-connect", "wss://box.example/tunnel", "-name", "build", "-pass-env", "DAX_TUNNEL_TOKEN"}, tunnelToken, "-pass-env: DAX_TUNNEL_TOKEN is this server's token"},
		{[]string{"-connect", "ws://127.0.0.1:7780/tunnel", "-tls-ca", "ca.pem"}, tunnelToken, "-tls-ca is for a wss:// hub"},
		{[]string{"-tls-ca", "ca.pem"}, tunnelToken, "-tls-ca goes with -connect"},
		{[]string{"-name", "build"}, tunnelToken, "-name goes with -connect"},
		{[]string{"-connect", "wss://box.example/tunnel"}, tunnelToken, "-connect: give -name"},
		{[]string{"-connect", "wss://box.example/tunnel", "-name", "Not_A_Label"}, tunnelToken, "-connect: give -name"},
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
	run(context.Background(), []string{"workspace", "-connect", "wss://box.example/tunnel", "-name", "build", "-tls-ca", "/nonexistent/ca.pem"}, program{name: "dax"})
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
	if out := c.out.String(); !strings.Contains(out, "dax workspace: serving "+dir+" to the hub at "+hub.URL+", which may read, write, exec") || !strings.Contains(out, "token: $DAX_TUNNEL_TOKEN") ||
		!strings.Contains(out, "note: ws:// is not encrypted; the hub must prove it holds the token before anything is served") ||
		!strings.Contains(out, "against a user of this host who takes "+strings.TrimSuffix(strings.TrimPrefix(hub.URL, "ws://"), "/tunnel")+", but not against one who can also reach the hub some other way and relay to it: then use wss://") {
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
		kept             bool // refused before the token is read
	}{
		{`{"workspace":{"hub":{"listen":"127.0.0.1:0","name":"build","wait":"300ms"}}}`, "", "workspace.hub: no token: set $DAX_TUNNEL_TOKEN", false},
		{`{"workspace":{"hub":{"listen":"127.0.0.1:0","name":"build","wait":"300ms"}}}`, "short", "workspace.hub: $DAX_TUNNEL_TOKEN is 5 bytes", false},
		{`{"workspace":{"hub":{"listen":"127.0.0.1:0","name":"build","wait":"300ms"}}}`, tunnelToken, "workspace build did not connect to ws://127.0.0.1:", false},
		{`{"workspace":{"hub":{"listen":"127.0.0.1:0","name":"build","wait":"300ms"}}}`, tunnelToken, "within 300ms; on its host run: DAX_TUNNEL_TOKEN=... dax workspace -connect ws://127.0.0.1:", false},
		// Refused with the config, before the token is read: it is
		// still set after.
		{`{"workspace":{"hub":{"listen":"0.0.0.0:0","name":"build"}}}`, tunnelToken, "the hub listens on loopback only", true},
		{`{"workspace":{"hub":{"listen":"127.0.0.1:0","name":"Build"}}}`, tunnelToken, `workspace.hub.name "Build"`, true},
		{`{"workspace":{"hub":{"listen":"127.0.0.1:0","name":"build","tls_cert":"/nonexistent/c.pem","tls_key":"/nonexistent/k.pem"}}}`, tunnelToken, "workspace.hub: tls_cert, tls_key: open /nonexistent/c.pem", false},
	} {
		write(t, cfgPath, tc.cfg)
		t.Setenv(config.TunnelTokenEnv, tc.token)
		err := run(context.Background(), []string{"-sessions", "", "-p", "hi"}, p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s with token %q: err = %v, want %q", tc.cfg, tc.token, err, tc.want)
		} else if tc.token != "" && strings.Contains(err.Error(), tc.token) {
			t.Errorf("the error holds the token: %v", err)
		}
		switch v := os.Getenv(config.TunnelTokenEnv); {
		case tc.kept && v != tc.token:
			t.Errorf("%s: refused before the token was read, yet $DAX_TUNNEL_TOKEN is %q", tc.cfg, v)
		case !tc.kept && v != "":
			t.Errorf("%s: $DAX_TUNNEL_TOKEN is still %q", tc.cfg, v)
		}
	}
}

// A second host with the token is refused for the session's life: the
// hub holds its workspace to the first instance that connected, so
// every read lands on the first host's directory. The second run stops
// at once, saying the workspace is held; the hub warns that it refused
// it, without either token.
func TestASecondHostIsRefused(t *testing.T) {
	home(t)
	a, b := t.TempDir(), t.TempDir()
	write(t, filepath.Join(a, "who"), "A")
	write(t, filepath.Join(b, "who"), "B")
	warned := &syncBuffer{}
	defer agent.CaptureWarnings(warned)()
	hub, _ := startTestHub(t, tunnelToken, 30*time.Second)
	connectWorkspace(t, a, hub.URL, tunnelToken)
	if err := hub.await(t.Context()); err != nil {
		t.Fatal(err)
	}
	second := connectWorkspace(t, b, hub.URL, tunnelToken)
	select {
	case err := <-second.done:
		if err == nil || !strings.Contains(err.Error(), "the hub's workspace is held by another host") || !strings.Contains(err.Error(), "403") {
			t.Errorf("the second host: %v", err)
		}
		second.done <- err
	case <-time.After(30 * time.Second):
		t.Fatal("the second host kept dialling")
	}
	for i := range 20 {
		got, err := workspace.Read(hub.Workspace, "who", 10)
		if err != nil || string(got) != "A" {
			t.Fatalf("read %d: %q, %v; want the first host's", i, got, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if root := hub.Workspace.Root(); root != a {
		t.Errorf("root %s, want %s", root, a)
	}
	// The first host's own redials keep its instance: the hub still
	// takes it.
	if out := warned.String(); !strings.Contains(out, "refused a host for workspace build: the workspace is held by another host") || strings.Contains(out, tunnelToken) {
		t.Errorf("the hub's warnings: %q", out)
	}
	if !strings.Contains(warned.String(), "tunnel: connected") {
		t.Errorf("the hub did not say the first host connected: %q", warned.String())
	}
}

// The hub's enrollment refuses an upgrade proved with the token and no
// instance id, or one not as -connect makes it, before the hub sees it.
func TestTheSessionsHubRefusesAnUpgradeWithNoInstance(t *testing.T) {
	home(t)
	defer agent.CaptureWarnings(&syncBuffer{})()
	hub, _ := startTestHub(t, tunnelToken, 30*time.Second)
	for _, id := range []string{"", "not-an-id", strings.Repeat("A", 32)} {
		req := upgradeTo(t, hub.URL, authn.NewProver(tunnelToken, "build").Header(hubNonce(t, hub.URL), id))
		if id != "" {
			req.Header.Set(instanceHeader, id)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("instance %q: %s, want 403", id, resp.Status)
		}
	}
}

// testCA makes a self-signed CA certificate for 127.0.0.1, and its key,
// in dir.
func testCA(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "hub"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	write(t, certFile, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	write(t, keyFile, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})))
	return certFile, keyFile
}

// Over wss: a hub serving tls_cert and tls_key takes a workspace that
// verifies it with -tls-ca, which replaces the system's roots; one
// whose -tls-ca did not sign the hub's certificate stops at once,
// saying so, as does one dialling a path where no hub is (404), and
// neither prints the token. No ws:// warning is printed.
func TestAWorkspaceOverWSS(t *testing.T) {
	home(t)
	cert, key := testCA(t, t.TempDir())
	other, _ := testCA(t, t.TempDir())
	defer agent.CaptureWarnings(&syncBuffer{})()
	h, err := startHub(config.HubSettings{Listen: "127.0.0.1:0", Name: "build", TokenEnv: config.TunnelTokenEnv, TLSCert: cert, TLSKey: key, Wait: 30 * time.Second}, tunnelToken, "dax", &syncBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	if !regexp.MustCompile(`^wss://127\.0\.0\.1:\d+/tunnel$`).MatchString(h.URL) {
		t.Fatalf("the hub's URL: %s", h.URL)
	}
	for _, tc := range []struct {
		name, url, ca, want string
	}{
		{"another CA", h.URL, other, "the hub's certificate is not one the roots verify"},
		{"the system's roots", h.URL, "", "the hub's certificate is not one the roots verify"},
		{"no hub at the path", strings.TrimSuffix(h.URL, "/tunnel") + "/elsewhere", cert, "the hub answered 404 Not Found: no hub at that path"},
	} {
		c := connectWith(t, &workspaceServer{dir: t.TempDir(), connect: tc.url, token: tunnelToken, tlsCA: tc.ca})
		select {
		case err := <-c.done:
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tunnelToken) {
				t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
			}
			c.done <- err
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: dax workspace -connect kept dialling", tc.name)
		}
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "who"), "A")
	c := connectWith(t, &workspaceServer{dir: dir, connect: h.URL, token: tunnelToken, tlsCA: cert})
	if err := h.await(t.Context()); err != nil {
		t.Fatalf("await: %v; the workspace said %s", err, c.out)
	}
	if got, err := workspace.Read(h.Workspace, "who", 10); err != nil || string(got) != "A" {
		t.Errorf("read over wss: %q, %v", got, err)
	}
	if out := c.out.String(); strings.Contains(out, "ws:// is not encrypted") || strings.Contains(out, tunnelToken) {
		t.Errorf("the workspace's end said %q", out)
	}
}

// With the host gone mid-session, a request fails within the hub's
// request timeout, and the session's close, its MCP servers in the
// workspace among what it closes, and the hub's close after it end
// within the one deadline.
func TestClosingASessionWithTheHostGone(t *testing.T) {
	home(t)
	t.Setenv(testMCPServerEnv, "1")
	defer agent.CaptureWarnings(&syncBuffer{})()
	dir := t.TempDir()
	hub, _ := startTestHub(t, tunnelToken, 30*time.Second)
	c := connectWorkspace(t, dir, hub.URL, tunnelToken)
	if err := hub.await(t.Context()); err != nil {
		t.Fatal(err)
	}
	sess, err := agent.New(t.Context(), agent.Options{
		Streamer: &echo.Adapter{}, Model: "echo", Dir: t.TempDir(), Root: filepath.Join(t.TempDir(), "sessions"), UserDir: t.TempDir(),
		Workspace: hub.Workspace, MCP: []agent.MCPServer{{Name: "ws", Command: os.Args[0]}, {Name: "ws2", Command: os.Args[0]}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.stop(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := workspace.Read(hub.Workspace, "who", 10); err == nil {
		t.Error("a read with the host gone succeeded")
	}
	if took := time.Since(start); took > hubRequestTimeout+5*time.Second {
		t.Errorf("a read with the host gone took %v", took)
	}
	start = time.Now()
	closeSession(sess, []*hostedHub{hub})
	hub.Close()
	if took := time.Since(start); took > hubCloseWait+3*time.Second {
		t.Errorf("closing with the host gone took %v, want about %v", took, hubCloseWait)
	}
}

// oddRoot is a workspace whose reported root holds terminal controls,
// as a host may report.
type oddRoot struct{ *workspace.Local }

func (o oddRoot) Root() string { return "/work\x1b]0;pwned\x07\x1b[2J" }
func (o oddRoot) Descriptor() workspace.Descriptor {
	return workspace.Descriptor{Kind: workspace.KindRemote, Ref: "http://build.tunnel.invalid", Root: o.Root()}
}

// The start line cleans the root a served workspace reports.
func TestTheBannerCleansTheReportedRoot(t *testing.T) {
	l, err := workspace.NewLocal(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, line := range []string{workspaceLine(oddRoot{l}), elsewhereBanner(frontInfo{Workspace: workspaceLine(oddRoot{l})})} {
		if strings.ContainsAny(line, "\x1b\x07") {
			t.Errorf("the line holds controls: %q", line)
		}
	}
}

// upgradeTo is a WebSocket upgrade request to the hub at url with
// header (its proof) on it.
func upgradeTo(t *testing.T, url string, header http.Header) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http"+strings.TrimPrefix(url, "ws"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header.Clone()
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return req
}

// hubNonce is a nonce the hub at url issues, as -connect asks for it.
func hubNonce(t *testing.T, url string) string {
	t.Helper()
	nonce, err := authn.FetchNonce(t.Context(), http.DefaultClient, url)
	if err != nil {
		t.Fatal(err)
	}
	return nonce
}

// The session's hub takes a proof only over a nonce it issued, for its
// own name, and once: the same upgrade sent again is refused, as is one
// over a nonce whatever held the port made up (the upgrade it captured,
// replayed through the forward), one for another hub's name, one
// carrying the token itself as a bearer token, or none; each 401 names
// the proof's scheme, and the hub's 101 carries its own MAC.
func TestTheSessionsHubRefusesAReplayedProof(t *testing.T) {
	home(t)
	var warned syncBuffer
	defer agent.CaptureWarnings(&warned)()
	hub, _ := startTestHub(t, tunnelToken, 30*time.Second)
	id := newInstance()
	prover := authn.NewProver(tunnelToken, "build")
	proved := func(nonce string) http.Header {
		h := prover.Header(nonce, id)
		h.Set(instanceHeader, id)
		return h
	}
	fresh := proved(hubNonce(t, hub.URL))
	// An impostor that held the port issued its own nonce, and kept the
	// upgrade -connect made over it.
	captured := proved(authn.NewHub(tunnelToken, "build").Nonce())
	other := authn.NewProver(tunnelToken, "other").Header(hubNonce(t, hub.URL), id)
	other.Set(instanceHeader, id)
	bearer := http.Header{"Authorization": {"Bearer " + tunnelToken}, instanceHeader: {id}}
	for _, tc := range []struct {
		name   string
		header http.Header
		want   int
	}{
		{"an upgrade captured over an impostor's nonce", captured, http.StatusUnauthorized},
		{"a proof for another hub's name", other, http.StatusUnauthorized},
		{"a fresh proof", fresh, http.StatusSwitchingProtocols},
		{"the same proof again", fresh, http.StatusUnauthorized},
		{"the token as a bearer", bearer, http.StatusUnauthorized},
		{"nothing", http.Header{instanceHeader: {id}}, http.StatusUnauthorized},
	} {
		req := upgradeTo(t, hub.URL, tc.header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: %s, want %d", tc.name, resp.Status, tc.want)
		}
		switch tc.want {
		case http.StatusUnauthorized:
			if got := resp.Header.Get("WWW-Authenticate"); got != authn.ProofChallenge {
				t.Errorf("%s: WWW-Authenticate %q, want %q", tc.name, got, authn.ProofChallenge)
			}
		case http.StatusSwitchingProtocols:
			if err := prover.CheckHub(req, resp, id); err != nil {
				t.Errorf("%s: the hub's 101 does not prove it: %v", tc.name, err)
			}
		}
	}
	for _, want := range []string{"nonce was used before", "a nonce this hub did not issue"} {
		if got := warned.String(); !strings.Contains(got, want) {
			t.Errorf("the hub's warnings %q, want %q", got, want)
		}
	}
	// A nonce is for no browser.
	req, _ := http.NewRequest(http.MethodGet, "http"+strings.TrimPrefix(hub.URL, "ws"), nil)
	req.Header.Set("Origin", "https://evil.example")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden || resp.Header.Get(authn.NonceHeader) != "" {
		t.Errorf("a nonce asked from a web page: %v, %v", resp, err)
	}
}

// impostor is a tunnel hub that takes every upgrade as build and answers
// with proof, and answers a GET of its URL with nonce (none when "").
func impostor(t *testing.T, nonce string, proof func(r *http.Request) http.Header, sent *[]string, mu *sync.Mutex) http.Handler {
	t.Helper()
	imp, err := wstunnel.NewHub(
		wstunnel.WithEnrollment(func(r *http.Request) (string, error) {
			mu.Lock()
			*sent = append(*sent, r.Header.Get("Authorization"))
			mu.Unlock()
			return "build", nil
		}),
		wstunnel.WithProofHeader(func(r *http.Request, _ string) (http.Header, error) {
			return proof(r), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { imp.Close() })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			if nonce != "" {
				w.Header().Set(authn.NonceHeader, nonce)
			}
			return
		}
		imp.ServeHTTP(w, r)
	})
}

// dax workspace -connect serves nothing to a hub that cannot prove it
// holds the token (an impostor that took the port), or that issues no
// nonce, and stops saying so, never dialling it again; the upgrade it
// was sent carries no token, and is over the impostor's own nonce.
func TestAWorkspaceServesNoHubWithoutTheToken(t *testing.T) {
	home(t)
	for _, tc := range []struct {
		name    string
		nonce   string
		proof   func(r *http.Request) http.Header
		want    string
		upgrade int
	}{
		{"no proof", "1.AAAAAAAAAAAAAAAAAAAAAAAAAA.0123", func(*http.Request) http.Header { return nil }, "is not the session's hub", 1},
		{"the workspace's own MAC sent back", "1.AAAAAAAAAAAAAAAAAAAAAAAAAA.0123", func(r *http.Request) http.Header {
			_, mac, _ := strings.Cut(r.Header.Get("Authorization"), `proof="`)
			return http.Header{authn.HubProofHeader: {strings.TrimSuffix(mac, `"`)}}
		}, "is not the session's hub", 1},
		{"no nonce", "", func(*http.Request) http.Header { return nil }, "issued no nonce: it answered 200 OK without one", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "README.md"), "# hi\n")
			var mu sync.Mutex
			var sent []string
			mux := http.NewServeMux()
			mux.Handle(config.TunnelPath, impostor(t, tc.nonce, tc.proof, &sent, &mu))
			srv := httptest.NewServer(mux)
			defer srv.Close()
			url := "ws" + strings.TrimPrefix(srv.URL, "http") + config.TunnelPath
			c := connectWorkspace(t, dir, url, tunnelToken)
			select {
			case err := <-c.done:
				if err == nil || !strings.Contains(err.Error(), tc.want) || tc.upgrade == 1 && (!errors.Is(err, wstunnel.ErrHubProof) || !strings.Contains(err.Error(), "rotate $DAX_TUNNEL_TOKEN")) {
					t.Errorf("-connect to an impostor: %v", err)
				}
				c.done <- err
			case <-time.After(30 * time.Second):
				t.Fatal("dax workspace -connect kept dialling a hub that failed the proof")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(sent) != tc.upgrade {
				t.Errorf("the impostor was sent %d upgrades, want %d", len(sent), tc.upgrade)
			}
			for _, a := range sent {
				if strings.Contains(a, tunnelToken) || !strings.HasPrefix(a, authn.ProofScheme+` nonce="`+tc.nonce+`", proof="`) {
					t.Errorf("the upgrade carried %q", a)
				}
			}
			if out := c.out.String(); strings.Contains(out, tunnelToken) {
				t.Errorf("the token was printed: %s", out)
			}
		})
	}
}

// After it has connected, -connect stops at the first hub that fails
// the proof too: an impostor that takes the port after a drop is sent
// one upgrade, and the run ends rather than dial it again within the
// lease.
func TestAWorkspaceStopsAtAnImpostorAfterADrop(t *testing.T) {
	home(t)
	defer agent.CaptureWarnings(&syncBuffer{})()
	p, genuine := genuineHub(t)
	var mu sync.Mutex
	var sent []string
	imp := impostor(t, "1.AAAAAAAAAAAAAAAAAAAAAAAAAA.0123", func(*http.Request) http.Header { return nil }, &sent, &mu)
	var taken atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if taken.Load() {
			imp.ServeHTTP(w, r)
			return
		}
		p.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := connectWorkspace(t, t.TempDir(), "ws"+strings.TrimPrefix(srv.URL, "http")+config.TunnelPath, tunnelToken)
	wctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ws, err := genuine.Workspace(wctx, "build")
	if err != nil {
		t.Fatalf("the real hub: %v; the workspace said %s", err, c.out)
	}
	ws.Close()
	// The port changes hands, and the connection drops.
	taken.Store(true)
	genuine.Close()
	select {
	case err := <-c.done:
		if err == nil || !errors.Is(err, wstunnel.ErrHubProof) || !strings.Contains(err.Error(), "this run stops rather than dial it again") {
			t.Errorf("-connect after a drop to an impostor: %v", err)
		}
		c.done <- err
	case <-time.After(30 * time.Second):
		t.Fatal("dax workspace -connect kept dialling the impostor")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 {
		t.Errorf("the impostor was sent %d upgrades, want 1", len(sent))
	}
}

// genuineHub is a session's hub as startHub makes it, for build under
// tunnelToken, to mount on a server of the test's own: pin in front of
// the tunnel's Hub.
func genuineHub(t *testing.T) (*pin, *wstunnel.Hub) {
	t.Helper()
	p := &pin{name: "build", proof: authn.NewHub(tunnelToken, "build"), log: hubLogger("workspace.hub")}
	h, err := wstunnel.NewHub(wstunnel.WithEnrollment(p.enroll), wstunnel.WithProofHeader(p.hubProof))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	p.hub = h
	return p, h
}

// awaitBuild waits for build to connect to h.
func awaitBuild(t *testing.T, h *wstunnel.Hub, c *connecting) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ws, err := h.Workspace(ctx, "build")
	if err != nil {
		t.Fatalf("the hub: %v; the workspace said %s", err, c.out)
	}
	ws.Close()
}

// What a hub may answer for a while is dialled again, not taken as an
// impostor: a refused connection (the hub not up yet), and a nonce's
// GET answered 503, 502, 429 or 408, as a proxy in front of a
// restarting hub does.
func TestAWorkspaceDialsAgainAtAHubNotReady(t *testing.T) {
	home(t)
	defer agent.CaptureWarnings(&syncBuffer{})()
	t.Run("refused", func(t *testing.T) {
		p, h := genuineHub(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		c := connectWorkspace(t, t.TempDir(), "ws://"+addr+config.TunnelPath, tunnelToken)
		time.Sleep(700 * time.Millisecond) // a dial or two refused
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			t.Skipf("the port was taken meanwhile: %v", err)
		}
		srv := &http.Server{Handler: p}
		go srv.Serve(ln)
		defer srv.Close()
		awaitBuild(t, h, c)
	})
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusTooManyRequests, http.StatusRequestTimeout} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			p, h := genuineHub(t)
			var answered atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Upgrade") == "" && answered.Add(1) == 1 {
					w.WriteHeader(code)
					return
				}
				p.ServeHTTP(w, r)
			}))
			defer srv.Close()
			c := connectWorkspace(t, t.TempDir(), "ws"+strings.TrimPrefix(srv.URL, "http")+config.TunnelPath, tunnelToken)
			awaitBuild(t, h, c)
			if n := answered.Load(); n < 2 {
				t.Errorf("%d nonces asked for, want the GET tried again", n)
			}
		})
	}
}

// An impostor that hands out a nonce (one it fetched from the hub while
// it could) and drops every upgrade it takes, never answering, is sent
// three, and the run stops.
func TestAWorkspaceStopsAtAnImpostorThatDropsUpgrades(t *testing.T) {
	home(t)
	var upgrades atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			w.Header().Set(authn.NonceHeader, authn.NewHub(tunnelToken, "build").Nonce())
			return
		}
		upgrades.Add(1)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer srv.Close()
	c := connectWorkspace(t, t.TempDir(), "ws"+strings.TrimPrefix(srv.URL, "http")+config.TunnelPath, tunnelToken)
	select {
	case err := <-c.done:
		if err == nil || !strings.Contains(err.Error(), "3 upgrades in a row") || !strings.Contains(err.Error(), "the token was not exposed") {
			t.Errorf("-connect to an impostor dropping upgrades: %v", err)
		}
		c.done <- err
	case <-time.After(60 * time.Second):
		t.Fatal("dax workspace -connect kept dialling an impostor that drops its upgrades")
	}
	if n := upgrades.Load(); n != 3 {
		t.Errorf("the impostor took %d upgrades, want 3", n)
	}
}
