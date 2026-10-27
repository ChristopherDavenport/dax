package dax

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses/echo"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/config"
)

// syncBuffer is a buffer the server writes from its goroutine while the
// test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
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

// servedWorkspace is a dax workspace serving dir in the test, until
// stop.
type servedWorkspace struct {
	url, tokenFile string
	out            *syncBuffer
	stop           func() error
}

// serveWorkspace starts dax workspace over dir with f's settings laid
// over the test's (a token directory of its own, a loopback address),
// and returns once it serves.
func serveWorkspace(t *testing.T, dir string, f func(*workspaceServer)) *servedWorkspace {
	t.Helper()
	tokens := t.TempDir()
	out := &syncBuffer{}
	ready := make(chan string, 1)
	w := &workspaceServer{name: "dax", dir: dir, addr: "127.0.0.1:0", out: out, tokenDir: tokens, ready: func(url string) { ready <- url }}
	if f != nil {
		f(w)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.run(ctx) }()
	s := &servedWorkspace{out: out}
	var once sync.Once
	var stopErr error
	s.stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case stopErr = <-done:
			case <-time.After(30 * time.Second):
				t.Error("dax workspace did not stop")
			}
		})
		return stopErr
	}
	t.Cleanup(func() { s.stop() })
	select {
	case s.url = <-ready:
	case err := <-done:
		t.Fatalf("dax workspace: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("dax workspace did not start")
	}
	entries, _ := os.ReadDir(tokens)
	if len(entries) == 1 {
		s.tokenFile = filepath.Join(tokens, entries[0].Name())
	}
	return s
}

func (s *servedWorkspace) token(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(s.tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// dax workspace refuses, before it serves anything, an address that is
// not loopback without TLS, half a certificate, and an argument.
func TestServingAWorkspaceRefuses(t *testing.T) {
	home(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"workspace", "-listen", "0.0.0.0:0"}, "without TLS dax workspace listens on loopback only"},
		{[]string{"workspace", "-listen", "192.0.2.1:7779"}, "without TLS dax workspace listens on loopback only"},
		{[]string{"workspace", "-listen", "box.example:7779"}, "ssh -L 7779:127.0.0.1:7779"},
		{[]string{"workspace", "-listen", "7779"}, "-listen \"7779\""},
		{[]string{"workspace", "-tls-cert", "cert.pem"}, "dax workspace: -tls-cert and -tls-key go together"},
		{[]string{"workspace", "-tls-cert", "/nonexistent/cert.pem", "-tls-key", "/nonexistent/key.pem"}, "dax workspace: -tls-cert, -tls-key"},
		{[]string{"workspace", "-pass-env", "A=B"}, `workspace: -pass-env: "A=B" is not a variable name`},
		{[]string{"workspace", "-pass-env", "SSH_AUTH_SOCK,DAX_WORKSPACE_TOKEN"}, "-pass-env: DAX_WORKSPACE_TOKEN is this server's token"},
		{[]string{"workspace", "-insecure-no-auth"}, "flag provided but not defined: -insecure-no-auth"},
		{[]string{"workspace", "/srv/app"}, `unexpected argument "/srv/app"`},
		{[]string{"workspace", "-dir", "/nonexistent/dir", "-listen", "127.0.0.1:0"}, "workspace:"},
	} {
		err := run(context.Background(), tc.args, program{name: "dax"})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
}

// A token from the environment shorter than 32 bytes is refused before
// anything is served, and no file is written.
func TestAServedWorkspaceRefusesAShortToken(t *testing.T) {
	home(t)
	t.Setenv(config.WorkspaceTokenEnv, "x")
	tokens := t.TempDir()
	w := &workspaceServer{name: "dax", dir: t.TempDir(), addr: "127.0.0.1:0", out: &syncBuffer{}, tokenDir: tokens}
	if err := w.run(t.Context()); err == nil || !strings.Contains(err.Error(), "$DAX_WORKSPACE_TOKEN is 1 bytes; a token from the environment must be at least 32") {
		t.Errorf("a short token: %v", err)
	}
	if entries, _ := os.ReadDir(tokens); len(entries) != 0 {
		t.Errorf("files were written: %v", entries)
	}
}

// dax workspace serves behind its token alone: a request without it,
// or with another, is refused, whatever the route; the token it made is
// in a file only the user can read, which goes when it stops, and only
// the file's path is printed, beside how to connect. A token from
// $DAX_WORKSPACE_TOKEN is named, never printed, and makes no file.
func TestAServedWorkspaceTakesItsTokenAlone(t *testing.T) {
	home(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "README.md"), "# hi\n")
	s := serveWorkspace(t, dir, nil)
	token := s.token(t)
	if fi, err := os.Stat(s.tokenFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the token file %s: %v %v", s.tokenFile, fi.Mode(), err)
	}
	out := s.out.String()
	if strings.Contains(out, token) {
		t.Fatalf("dax workspace printed its token:\n%s", out)
	}
	for _, want := range []string{
		"dax workspace: serving " + dir + " at " + s.url,
		"token: " + s.tokenFile,
		`connect: DAX_WORKSPACE_TOKEN="$(cat ` + s.tokenFile + `)" dax -workspace ` + s.url,
		`{"workspace": {"url": "` + s.url + `", "token_env": "DAX_WORKSPACE_TOKEN"}}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dax workspace's lines lack %q:\n%s", want, out)
		}
	}
	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"another token", "Bearer not-the-token", http.StatusUnauthorized},
		{"the token not as a bearer", token, http.StatusUnauthorized},
		{"the token", "Bearer " + token, http.StatusOK},
	} {
		for _, route := range []string{"/v1/info", "/v1/fs/file?path=README.md"} {
			req, _ := http.NewRequest(http.MethodGet, s.url+route, nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("%s, %s: %d, want %d", tc.name, route, resp.StatusCode, tc.want)
			}
			if got := resp.Header.Get("WWW-Authenticate"); tc.want == http.StatusUnauthorized && got != authn.Challenge {
				t.Errorf("%s, %s: WWW-Authenticate %q, want %q", tc.name, route, got, authn.Challenge)
			}
		}
	}
	// The token is taken from its variable, which then leaves the
	// environment; none set is refused.
	t.Setenv(config.WorkspaceTokenEnv, "")
	if _, err := takeWorkspaceToken("-workspace", config.WorkspaceTokenEnv); err == nil || !strings.Contains(err.Error(), "no token: set $DAX_WORKSPACE_TOKEN") {
		t.Errorf("no token: %v", err)
	}
	t.Setenv("MY_WS_TOKEN", token)
	if got, err := takeWorkspaceToken("-workspace", "MY_WS_TOKEN"); err != nil || got != token {
		t.Fatalf("takeWorkspaceToken: %v", err)
	}
	if _, ok := os.LookupEnv("MY_WS_TOKEN"); ok {
		t.Error("the token's variable is still in this process's environment, for a child here to inherit")
	}
	if _, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: s.url}, "not-the-token"); err == nil || strings.Contains(err.Error(), "not-the-token") {
		t.Errorf("a dial with another token: %v", err)
	}
	ws, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: s.url}, token)
	if err != nil {
		t.Fatal(err)
	}
	if d := ws.Descriptor(); d.Kind != workspace.KindRemote || d.Ref != s.url || d.Root != dir || strings.Contains(d.Ref, token) {
		t.Errorf("descriptor %+v", d)
	}
	// Spellings of one address are one workspace: the descriptor's ref
	// is the URL normalised, and the workspace is still a Starter whose
	// files read links.
	spelled, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: strings.Replace(s.url, "127.0.0.1", "LOCALHOST", 1) + "/"}, token)
	if err != nil {
		t.Fatal(err)
	}
	if d := spelled.Descriptor(); d.Ref != strings.Replace(s.url, "127.0.0.1", "localhost", 1) {
		t.Errorf("the ref of another spelling: %q", d.Ref)
	}
	if _, ok := spelled.(workspace.Starter); !ok {
		t.Error("the identified workspace is no Starter")
	}
	if _, ok := spelled.FS().(fs.ReadLinkFS); !ok {
		t.Error("the identified workspace's files do not read links")
	}
	spelled.Close()
	line := workspaceLine(ws)
	if line != "remote "+s.url+" · "+dir {
		t.Errorf("the banner's line %q", line)
	}
	if lines := banner(frontInfo{Name: "dax", Workspace: line}, agent.Info{}); !strings.Contains(strings.Join(lines, "\n"), "\nworkspace: "+line) {
		t.Errorf("the banner %q does not name the workspace", lines)
	}
	if l := workspaceLine(struct{ workspace.Workspace }{ws}); !strings.HasSuffix(l, "starts no process there: your MCP servers run here, the project's not at all") {
		t.Errorf("the line of a workspace that cannot start a process: %q", l)
	}
	ws.Close()
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.tokenFile); !os.IsNotExist(err) {
		t.Errorf("the token file outlived dax workspace: %v", err)
	}

	t.Setenv(config.WorkspaceTokenEnv, "the-environment's-token-long-enough-to-take")
	s = serveWorkspace(t, dir, nil)
	if s.tokenFile != "" {
		t.Errorf("a token from the environment was written to %s", s.tokenFile)
	}
	if out := s.out.String(); strings.Contains(out, "the-environment's-token-long-enough-to-take") || !strings.Contains(out, "token: $DAX_WORKSPACE_TOKEN") {
		t.Errorf("dax workspace's lines with the environment's token:\n%s", out)
	}
	if ws, err = dialWorkspace(t.Context(), config.WorkspaceSettings{URL: s.url}, os.Getenv(config.WorkspaceTokenEnv)); err != nil {
		t.Fatal(err)
	}
	ws.Close()
}

// Stopping dax workspace ends every process a session started in it,
// and returns within a bounded wait however long they meant to run.
func TestStoppingAServedWorkspaceEndsItsProcesses(t *testing.T) {
	home(t)
	dir := t.TempDir()
	s := serveWorkspace(t, dir, nil)
	ws, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: s.url}, s.token(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	st, ok := ws.(workspace.Starter)
	if !ok {
		t.Fatal("dax workspace's token cannot start a process")
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
	start := time.Now()
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("stopping took %v", took)
	}
	for deadline := time.Now().Add(10 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a process outlived dax workspace")
		}
	}
}

// A client that streams a command's output and stops reading holds
// that Exec blocked writing to its connection; stopping still returns
// within its bounded waits, the connections closed under it, and the
// token file is removed.
func TestStoppingAServedWorkspaceUnderAStalledStream(t *testing.T) {
	home(t)
	s := serveWorkspace(t, t.TempDir(), func(w *workspaceServer) { w.drain = 500 * time.Millisecond })
	body := `{"args":["yes"],"stream":true}`
	conn, err := net.Dial("tcp", strings.TrimPrefix(s.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /v1/exec HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		strings.TrimPrefix(s.url, "http://"), s.token(t), len(body), body)
	// Read the first of the reply, so the command runs, then nothing.
	buf := make([]byte, 512)
	if _, err := io.ReadAtLeast(conn, buf, 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // the socket's buffers fill
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- s.stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("dax workspace did not stop under a stalled stream")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("stopping took %v", took)
	}
	if _, err := os.Stat(s.tokenFile); !os.IsNotExist(err) {
		t.Errorf("the token file outlived dax workspace: %v", err)
	}
}

// With a certificate dax workspace serves HTTPS on any address, and a
// session dials it verified against workspace.tls_ca; without the CA
// the certificate is not trusted, and nothing skips the check.
func TestAServedWorkspaceOverTLS(t *testing.T) {
	home(t)
	cert, key := selfSigned(t)
	dir := t.TempDir()
	s := serveWorkspace(t, dir, func(w *workspaceServer) { w.addr, w.tlsCert, w.tlsKey = "0.0.0.0:0", cert, key })
	if !strings.HasPrefix(s.url, "https://") || !strings.Contains(s.out.String(), `"tls_ca": "<the certificate's CA`) {
		t.Fatalf("dax workspace over TLS: %s\n%s", s.url, s.out.String())
	}
	url := "https://127.0.0.1:" + portOf(t, s.url)
	if _, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: url}, s.token(t)); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("a dial without the CA: %v", err)
	}
	ws, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: url, TLSCA: cert}, s.token(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if ws.Root() != dir {
		t.Errorf("root %s, want %s", ws.Root(), dir)
	}
	if _, err := workspaceHTTPClient("workspace", url, filepath.Join(dir, "missing.pem")); err == nil {
		t.Error("a CA file that is not there")
	}
	write(t, filepath.Join(dir, "junk.pem"), "not a certificate")
	if _, err := workspaceHTTPClient("workspace", url, filepath.Join(dir, "junk.pem")); err == nil || !strings.Contains(err.Error(), "holds no PEM certificate") {
		t.Errorf("a CA file with no certificate: %v", err)
	}
}

// With -workspace the project is the served one: this directory's
// .dax/config.json is not read, and the served one's is, through the
// workspace, as a project's layer that may only tighten; one that names
// a workspace of its own fails the start.
func TestTheProjectConfigIsReadThroughTheServedWorkspace(t *testing.T) {
	h := home(t)
	here, servedDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(here, ".dax", "config.json"), `{"policy":{"deny":["bash(rm:*)"]}}`)
	s := serveWorkspace(t, servedDir, nil)
	flags := config.Flags{Workspace: str(s.url)}
	if st, err := settingsOf(local(t, here), "", flags); err != nil || st.Workspace == nil || len(st.Policy.Project.Deny) != 0 {
		t.Fatalf("before the dial: %+v %v, %v", st.Workspace, st.Policy.Project.Deny, err)
	}
	write(t, filepath.Join(h, ".config", "dax", "config.json"), `{"workspace":{"url":"`+s.url+`"}}`)
	if st, err := settingsOf(local(t, here), "", config.Flags{}); err != nil || st.Workspace == nil || len(st.Policy.Project.Deny) != 0 {
		t.Fatalf("with the config's workspace: %+v %v, %v", st.Workspace, st.Policy.Project.Deny, err)
	}
	if st, err := settingsOf(local(t, here), "", config.Flags{Workspace: str("")}); err != nil || st.Workspace != nil || len(st.Policy.Project.Deny) != 1 {
		t.Fatalf("with -workspace \"\": %+v %v, %v", st.Workspace, st.Policy.Project.Deny, err)
	}
	ws, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: s.url}, s.token(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	user, err := loadUser("")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(servedDir, ".dax", "config.json")
	write(t, file, `{"policy":{"deny":["bash(make:*)"]}}`)
	st, err := projectSettings("-workspace", ws, user, flags)
	if err != nil || strings.Join(st.Policy.Project.Deny, ",") != "bash(make:*)" || st.Workspace == nil {
		t.Fatalf("through the workspace: %v %+v, %v", st.Policy.Project.Deny, st.Workspace, err)
	}
	write(t, file, `{"workspace":{"url":"https://evil.example"}}`)
	if _, err := projectSettings("-workspace", ws, user, flags); err == nil || !strings.Contains(err.Error(), servedDir+"/.dax/config.json: workspace: a project file may only tighten") {
		t.Errorf("a served project naming a workspace: %v", err)
	}
}

// -workspace refuses what it cannot take before it dials, and a
// workspace it can reach without the token's variable set.
func TestWorkspaceRefusalsOnTheCommandLine(t *testing.T) {
	h := home(t)
	t.Chdir(t.TempDir())
	t.Setenv(config.WorkspaceTokenEnv, "")
	p := program{name: "dax", version: "test"}
	for _, tc := range []struct {
		args []string
		cfg  string
		want string
	}{
		{[]string{"-workspace", "http://box.example:7779"}, "", "http only to a loopback host"},
		{[]string{"-workspace", "https://me:pw@box.example"}, "", "carries credentials"},
		{[]string{"-workspace", "box.example:7779"}, "", "want the https:// or http:// URL"},
		{[]string{"-workspace", "https://box.example", "-executor", "ssh host dax execute"}, "", "-executor and -workspace both say where the tools act"},
		{[]string{"-workspace", "https://box.example", "-box"}, "", "-box and -workspace both say where the tools act"},
		{[]string{"-workspace", "http://127.0.0.1:1"}, "", "no token: set $DAX_WORKSPACE_TOKEN"},
		{nil, `{"workspace":{"url":"http://127.0.0.1:1","token_env":"MY_WS_TOKEN"}}`, "no token: set $MY_WS_TOKEN"},
		{[]string{"-executor", "ssh host dax execute"}, `{"workspace":{"url":"http://127.0.0.1:1"}}`, `-executor: your config's workspace says where the tools act; give -workspace ""`},
		{[]string{"-list", "-workspace", "http://box.example"}, "", "http only to a loopback host"},
	} {
		os.Remove(filepath.Join(h, ".config", "dax", "config.json"))
		if tc.cfg != "" {
			write(t, filepath.Join(h, ".config", "dax", "config.json"), tc.cfg)
		}
		err := run(context.Background(), append(tc.args, "-sessions", "", "-p", "hi"), p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v %s: err = %v, want %q", tc.args, tc.cfg, err, tc.want)
		}
	}
}

// -list with a served workspace lists the sessions recorded in it,
// found by its URL in the record, without dialing: the server is gone
// when it lists.
func TestListFindsTheServedWorkspacesSessions(t *testing.T) {
	h := home(t)
	root := filepath.Join(t.TempDir(), "sessions")
	here, servedDir := t.TempDir(), t.TempDir()
	s := serveWorkspace(t, servedDir, nil)
	ws, err := dialWorkspace(t.Context(), config.WorkspaceSettings{URL: s.url}, s.token(t))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := agent.New(t.Context(), agent.Options{Streamer: &echo.Adapter{}, Model: "echo", Dir: here, Root: root, UserDir: t.TempDir(), Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	sess.Close()
	ws.Close()
	if _, err := agent.New(t.Context(), agent.Options{Streamer: &echo.Adapter{}, Model: "echo", Dir: here, Root: root, UserDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	s.stop()

	at := "  remote " + s.url + ":" + servedDir
	list := func(cfg string, f config.Flags) string {
		t.Helper()
		var out bytes.Buffer
		if err := runList(context.Background(), &out, root, here, cfg, f); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String())
	}
	if out := list("", config.Flags{Workspace: str(s.url + "/")}); !strings.Contains(out, id) || strings.Count(out, "\n") != 0 || !strings.HasSuffix(out, at) {
		t.Errorf("-list -workspace: %q, want the one session, ending %q", out, at)
	}
	cfg := filepath.Join(h, "ws.json")
	write(t, cfg, `{"workspace":{"url":"`+s.url+`"}}`)
	if out := list(cfg, config.Flags{}); !strings.Contains(out, id) || !strings.HasSuffix(out, at) {
		t.Errorf("-list with the config's workspace: %q", out)
	}
	if out := list(cfg, config.Flags{Workspace: str("")}); strings.Contains(out, id) || out == "" {
		t.Errorf("-list -workspace \"\" lists %q, want this directory's session alone", out)
	}
	// -list resolves where the tools act as a run does: -executor ""
	// leaves the config's workspace, and -executor with it is refused.
	if out := list(cfg, config.Flags{Executor: str("")}); !strings.Contains(out, id) || !strings.HasSuffix(out, at) {
		t.Errorf("-list -executor \"\" with the config's workspace: %q", out)
	}
	if err := runList(context.Background(), io.Discard, root, here, cfg, config.Flags{Executor: str("ssh host dax execute")}); err == nil || !strings.Contains(err.Error(), `-executor: your config's workspace says where the tools act`) {
		t.Errorf("-list -executor beside the config's workspace: %v", err)
	}
	if err := runList(context.Background(), io.Discard, root, here, cfg, config.Flags{Box: ptrTo(true)}); err == nil || !strings.Contains(err.Error(), `-box: your config's workspace says where the tools act`) {
		t.Errorf("-list -box beside the config's workspace: %v", err)
	}
}

func ptrTo[T any](v T) *T { return &v }

// The workspace's token leaves dax's environment before anything here
// starts a process: the key command, which provider.New runs at once,
// finds no $DAX_WORKSPACE_TOKEN.
func TestTheKeyCommandDoesNotInheritTheWorkspaceToken(t *testing.T) {
	h := home(t)
	t.Chdir(t.TempDir())
	s := serveWorkspace(t, t.TempDir(), nil)
	seen := filepath.Join(t.TempDir(), "seen")
	write(t, filepath.Join(h, ".config", "dax", "config.json"), `{"provider":"openresponses","model":"m","base_url":"http://127.0.0.1:1/v1",`+
		`"api_key_command":["sh","-c","printf 'token=[%s]' \"$DAX_WORKSPACE_TOKEN\" > `+seen+`; echo key"]}`)
	t.Setenv(config.WorkspaceTokenEnv, s.token(t))
	// The model is unreachable, so the prompt fails; the key command ran
	// before it.
	err := run(context.Background(), []string{"-workspace", s.url, "-sessions", "", "-p", "hi"}, program{name: "dax", version: "test"})
	t.Logf("run: %v", err)
	b, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the key command did not run: %v", err)
	}
	if string(b) != "token=[]" {
		t.Errorf("the key command saw %q", b)
	}
}
