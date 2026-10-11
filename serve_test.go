package dax

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentconsole/console"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// served is a session dax serve serves over a scripted model.
type served struct {
	t     *testing.T
	rig   *tuiRig // the session and its directory; its front is not run
	url   string
	token string
	out   *syncBuf
	done  chan error
	stop  context.CancelFunc
	dir   string // where the token file is written
}

func startServe(t *testing.T, m *steps, rules policy.Rules) *served {
	t.Helper()
	return startServeWith(t, m, rules, nil)
}

func startServeWith(t *testing.T, m *steps, rules policy.Rules, tweak func(*agent.Options)) *served {
	t.Helper()
	t.Setenv(serveTokenEnv, "")
	rig := startRig(t, m, rules, tweak, nil, false)
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{t: t, rig: rig, out: &syncBuf{}, done: make(chan error, 1), stop: cancel, dir: t.TempDir()}
	ready := make(chan string, 1)
	f := &serveFront{
		info:     frontInfo{Name: "dax", Provider: "test", Model: "scripted", Dir: rig.dir},
		addr:     "127.0.0.1:0",
		out:      s.out,
		tokenDir: s.dir,
		ready:    func(url string) { ready <- url },
	}
	go func() { s.done <- f.Run(ctx, rig.sess) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Error("dax serve did not stop")
		}
	})
	select {
	case s.url = <-ready:
	case err := <-s.done:
		t.Fatalf("serve: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("dax serve never listened")
	}
	data, err := os.ReadFile(s.tokenFile())
	if err != nil {
		t.Fatal(err)
	}
	s.token = strings.TrimSpace(string(data))
	return s
}

func (s *served) tokenFile() string { return filepath.Join(s.dir, s.rig.sess.ID()+".token") }

// attach runs dax attach's terminal client against the served session,
// on a pipe.
func (s *served) attach() *tuiRig {
	t := s.t
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	r := &tuiRig{t: t, dir: s.rig.dir, in: pw, out: &syncBuf{}, pre: &syncBuf{}, done: make(chan error, 1), sess: s.rig.sess}
	f := &attachFront{
		name:    "dax",
		exts:    []extension.Extension{coding.New(0), agents.New(agents.Options{})},
		console: []console.Option{console.WithInput(pr), console.WithOutput(r.out), console.WithoutSignalHandler(), console.WithWindowSize(220, 50)},
		out:     r.pre,
	}
	go func() { r.done <- f.Run(ctx, s.url, s.token) }()
	return r
}

// A view attached to a served session drives it: a prompt runs there,
// the policy's question is answered from the view, the call runs where
// the session's tools act, and the reply is shown from the record the
// view follows over the wire.
func TestAnAttachedViewDrivesTheServedSession(t *testing.T) {
	s := startServe(t, &steps{parent: [][2]string{{"bash", `{"command":"touch SERVED"}`}}}, policy.Rules{})
	r := s.attach()
	r.type_("go\r")
	r.waitPanel()
	r.waitFile("SERVED", false)
	r.type_("y")
	r.waitFile("SERVED", true)
	r.waitOutput("all done")
	r.quit()
	if !strings.Contains(r.pre.String(), resumeLine("dax", s.rig.sess.ID())) {
		t.Errorf("attach left %q, want the served session's resume command", r.pre.String())
	}
	if strings.Contains(s.out.String(), s.token) {
		t.Error("dax serve printed its token")
	}
	for _, want := range []string{"serving at " + s.url, "token: " + s.tokenFile(), "attach: dax attach -token-file " + s.tokenFile() + " " + s.url} {
		if !strings.Contains(s.out.String(), want) {
			t.Errorf("dax serve's lines lack %q:\n%s", want, s.out.String())
		}
	}
}

// A sub-agent's question, asked while the served session's run goes, is
// answered from the attached view with a note, which reaches the child.
func TestASubagentsQuestionIsAnsweredWithANoteFromAnAttachedView(t *testing.T) {
	m := &steps{
		parent: [][2]string{{"task", `{"input":"write the file"}`}},
		child:  [][2]string{{"write", `{"path":"NOTED","content":"x"}`}},
	}
	s := startServe(t, m, policy.Rules{})
	r := s.attach()
	r.type_("go\r")
	r.waitOutput("Question (1/1): write")
	r.type_("a")
	r.waitOutput("A note with the answer")
	r.type_("keep it short\r")
	r.waitFile("NOTED", true)
	r.waitOutput("all done")
	if saw := m.sawInChild(); !strings.Contains(saw, "The user said: keep it short") {
		t.Errorf("the sub-agent saw %q, want the user's note", saw)
	}
	r.quit()
}

// A slash command in the attached view runs against the served session
// through the served commands.
func TestASlashCommandRunsAgainstTheServedSession(t *testing.T) {
	s := startServe(t, &steps{}, policy.Rules{})
	r := s.attach()
	r.type_("/model other-model\r")
	r.waitOutput("[model other-model from the next request]")
	if got := s.rig.sess.Agent().Config().ModelName; got != "other-model" {
		t.Errorf("the served session's model is %q, want other-model", got)
	}
	r.type_("/session\r")
	r.waitOutput(s.rig.sess.ID())
	r.quit()
}

// Two views attached to one served session drive one conversation: what
// one asks, the other shows, and the other can go on with it.
func TestTwoAttachedViewsDriveOneConversation(t *testing.T) {
	s := startServe(t, &steps{}, policy.Rules{})
	a, b := s.attach(), s.attach()
	// Both views are up before either prompts.
	a.waitOutput("say something")
	b.waitOutput("say something")
	a.type_("first question\r")
	a.waitOutput("all done")
	b.waitOutput("first question")
	b.waitOutput("all done")
	b.type_("second question\r")
	a.waitOutput("second question")
	b.quit()
	a.quit()
}

// Nothing is served without the token, on either prefix.
func TestServingNeedsTheToken(t *testing.T) {
	s := startServe(t, &steps{}, policy.Rules{})
	id := s.rig.sess.ID()
	for _, path := range []string{controlPrefix + "/", controlPrefix + "/state", storePrefix + "/v1/", storePrefix + "/v1/sessions/" + id} {
		for _, tc := range []struct {
			name, token string
			want        int
		}{
			{"no token", "", http.StatusUnauthorized},
			{"a wrong token", "not-the-token", http.StatusUnauthorized},
			{"the token", s.token, http.StatusOK},
		} {
			req, err := http.NewRequest(http.MethodGet, s.url+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("GET %s with %s: %d, want %d", path, tc.name, resp.StatusCode, tc.want)
			}
		}
	}
}

// dax serve listens on loopback only, and says to tunnel otherwise.
func TestServeListensOnLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:7878", "[::]:0", "example.com:80"} {
		if _, err := listenLoopback(addr); err == nil || !strings.Contains(err.Error(), "ssh -L") {
			t.Errorf("listen %s: %v, want a refusal that says to tunnel", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:0"} {
		ln, err := listenLoopback(addr)
		if err != nil {
			t.Errorf("listen %s: %v", addr, err)
			continue
		}
		ln.Close()
	}
}

// The token dax serve makes is written where only the user can read it,
// a file left from before included, and is removed when serving ends;
// a token from the environment is written nowhere.
func TestTheServeTokenFileIsPrivate(t *testing.T) {
	t.Setenv(serveTokenEnv, "")
	dir := filepath.Join(t.TempDir(), "serve")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "s1.token")
	if err := os.WriteFile(stale, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	token, file, cleanup, err := serveToken("s1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if file != stale {
		t.Fatalf("token file %s, want %s", file, stale)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %04o, want 0600", fi.Mode().Perm())
	}
	if di, err := os.Stat(dir); err != nil || di.Mode().Perm()&0o077 != 0 {
		t.Errorf("token directory: %v, mode %04o, want private", err, di.Mode().Perm())
	}
	if data, _ := os.ReadFile(file); strings.TrimSpace(string(data)) != token || len(token) < 40 {
		t.Errorf("token file holds %q, want the %d-character token", data, len(token))
	}
	cleanup()
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the token file is still there after cleanup: %v", err)
	}

	t.Setenv(serveTokenEnv, "from-the-environment")
	token, file, cleanup, err = serveToken("s2", dir)
	if err != nil || token != "from-the-environment" || file != "" {
		t.Errorf("with $%s: token %q, file %q, %v", serveTokenEnv, token, file, err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Join(dir, "s2.token")); !os.IsNotExist(err) {
		t.Error("a token from the environment was written to a file")
	}
}

// Serving ends when its context does: the token file goes, and the
// command that resumes the session is printed.
func TestServeStopsAndSaysHowToResume(t *testing.T) {
	s := startServe(t, &steps{}, policy.Rules{})
	s.stop()
	select {
	case err := <-s.done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		s.done <- nil // for the cleanup's wait
	case <-time.After(10 * time.Second):
		t.Fatal("dax serve did not stop")
	}
	if !strings.Contains(s.out.String(), resumeLine("dax", s.rig.sess.ID())) {
		t.Errorf("dax serve left %q, want the resume command", s.out.String())
	}
	if _, err := os.Stat(s.tokenFile()); !os.IsNotExist(err) {
		t.Errorf("the token file is still there: %v", err)
	}
}

// A session nothing is kept of has no record to follow, so it is not
// served.
func TestServeRefusesASessionNothingIsKeptOf(t *testing.T) {
	rig := startRig(t, &steps{}, policy.Rules{}, func(o *agent.Options) { o.Root = "" }, nil, false)
	f := &serveFront{info: frontInfo{Name: "dax"}, addr: "127.0.0.1:0", out: &syncBuf{}, tokenDir: t.TempDir()}
	if err := f.Run(context.Background(), rig.sess); err == nil || !strings.Contains(err.Error(), "session store") {
		t.Errorf("serve of an unrecorded session: %v, want a refusal", err)
	}
}

// gated is a model whose answer waits until it is let go, so a run is
// still going while a test acts.
type gated struct {
	*steps
	release chan struct{}
	once    sync.Once
}

func (g *gated) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.steps.CreateStream(ctx, req, sink)
}

func (g *gated) let() { g.once.Do(func() { close(g.release) }) }

// Leaving an attached view with /quit leaves the served session's run
// going: other views, or a later one, follow it. ctrl+c in a view that
// is up still aborts it.
func TestLeavingAnAttachedViewLeavesTheRunGoing(t *testing.T) {
	g := &gated{steps: &steps{}, release: make(chan struct{})}
	t.Cleanup(g.let)
	s := startServeWith(t, g.steps, policy.Rules{}, func(o *agent.Options) { o.Streamer = g })
	ends := make(chan *agentturn.RunEnd, 4)
	defer s.rig.sess.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		if e, ok := ev.(*agentturn.RunEnd); ok {
			ends <- e
		}
		return nil
	})()

	r := s.attach()
	r.type_("go\r")
	deadline := time.Now().Add(10 * time.Second)
	for !s.rig.sess.State().Running {
		if time.Now().After(deadline) {
			t.Fatal("the run never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.type_("/quit\r")
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the view did not leave")
	}
	if !s.rig.sess.State().Running {
		t.Fatal("leaving the view stopped the served run")
	}
	g.let()
	select {
	case e := <-ends:
		if e.Reason != agentturn.ReasonDone {
			t.Errorf("the run ended %q, want done", e.Reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never ended")
	}

	// A view that is up aborts the run it shows with ctrl+c.
	g2 := s.attach()
	g.release = make(chan struct{})
	g.once = sync.Once{}
	g2.waitOutput("say something")
	g2.type_("again\r")
	for deadline := time.Now().Add(10 * time.Second); !s.rig.sess.State().Running; {
		if time.Now().After(deadline) {
			t.Fatal("the second run never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	g2.type_("\x03")
	select {
	case e := <-ends:
		if e.Reason != agentturn.ReasonAborted {
			t.Errorf("the run ended %q after ctrl+c, want aborted", e.Reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ctrl+c did not abort the run")
	}
	g2.quit()
}

// The command line refuses what dax serve and dax attach cannot do
// before anything starts.
func TestServeAndAttachRefusalsOnTheCommandLine(t *testing.T) {
	home(t)
	t.Chdir(t.TempDir())
	t.Setenv(serveTokenEnv, "")
	p := program{name: "dax", version: "test"}
	for _, tc := range []struct {
		args  []string
		wants string
	}{
		{[]string{"-listen", "127.0.0.1:0", "-p", "hi"}, "-listen is dax serve's"},
		{[]string{"serve", "-p", "hi"}, "dax serve has no front of its own"},
		{[]string{"serve", "-front", "repl"}, "dax serve has no front of its own"},
		{[]string{"serve", "-sessions", ""}, "dax serve needs a session store"},
		{[]string{"serve", "-listen", "0.0.0.0:7878"}, "ssh -L 7878:127.0.0.1:7878"},
		{[]string{"serve", "-listen", ":0"}, "loopback only"},
		{[]string{"attach"}, "want the URL dax serve printed"},
		{[]string{"attach", "http://127.0.0.1:1"}, "no token"},
		{[]string{"attach", "-token-file", filepath.Join(t.TempDir(), "missing"), "http://127.0.0.1:1"}, "-token-file"},
	} {
		err := run(context.Background(), tc.args, p)
		if err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.wants)
		}
	}
}
