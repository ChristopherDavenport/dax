package dax

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentconsole/console"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/front/control"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/charmbracelet/x/ansi"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/authn"
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
	// attachURL is where attach dials, when not url; hc is the client it
	// dials with (attachHTTPClient), when not the default.
	attachURL string
	hc        *http.Client
}

func startServe(t *testing.T, m *steps, rules policy.Rules) *served {
	t.Helper()
	return startServeWith(t, m, rules, nil)
}

func startServeWith(t *testing.T, m *steps, rules policy.Rules, tweak func(*agent.Options)) *served {
	t.Helper()
	return startServeFront(t, m, rules, tweak, nil)
}

// startServeFront is startServeWith with front adjusting the serveFront
// before it runs: its address, its certificate.
func startServeFront(t *testing.T, m *steps, rules policy.Rules, tweak func(*agent.Options), front func(*serveFront)) *served {
	t.Helper()
	t.Setenv(serveTokenEnv, "")
	t.Setenv(a2aTokenEnv, "")
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
	if front != nil {
		front(f)
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

// a2aTokenFile is where dax serve -a2a wrote the A2A token, and
// a2aToken the token.
func (s *served) a2aTokenFile() string { return filepath.Join(s.dir, s.rig.sess.ID()+".a2a.token") }

func (s *served) a2aToken() string {
	s.t.Helper()
	data, err := os.ReadFile(s.a2aTokenFile())
	if err != nil {
		s.t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

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
		hc:      s.hc,
		name:    "dax",
		exts:    []extension.Extension{coding.New(0), agents.New(agents.Options{})},
		console: []console.Option{console.WithInput(pr), console.WithOutput(r.out), console.WithoutSignalHandler(), console.WithWindowSize(220, 50)},
		out:     r.pre,
	}
	url := s.url
	if s.attachURL != "" {
		url = s.attachURL
	}
	go func() { r.done <- f.Run(ctx, url, s.token) }()
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
	// The note is the person's own message, not a line of the policy's.
	if saw := m.sawInChild(); !strings.Contains(saw, "keep it short") || strings.Contains(saw, "The user said") {
		t.Errorf("the sub-agent saw %q, want the user's note as theirs", saw)
	}
	// Who answered is the served session's principal, as front/control
	// names it, not the attached view's terminal.
	checkRecordedQuestion(t, s.rig.sess, "the task sub-agent asks", "write", "accept", "keep it short", agentsession.Trigger{Kind: "control", Ref: "attach"})
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

	// A short one is refused, whatever it is for.
	t.Setenv(serveTokenEnv, "from-the-environment")
	if _, _, _, err := serveToken("s2", dir); err == nil || !strings.Contains(err.Error(), "$DAX_SERVE_TOKEN is 20 bytes; a token from the environment must be at least 32") {
		t.Errorf("a short token: %v", err)
	}
	long := "from-the-environment-and-long-enough"
	t.Setenv(serveTokenEnv, long)
	token, file, cleanup, err = serveToken("s2", dir)
	if err != nil || token != long || file != "" {
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
		{[]string{"serve", "-tls-cert", "cert.pem"}, "-tls-cert and -tls-key go together"},
		{[]string{"serve", "-tls-key", "key.pem", "-listen", "0.0.0.0:0"}, "-tls-cert and -tls-key go together"},
		{[]string{"-tls-cert", "cert.pem", "-tls-key", "key.pem", "-p", "hi"}, "-tls-cert is dax serve's"},
		{[]string{"-a2a", "-p", "hi"}, "-a2a is dax serve's"},
		{[]string{"-a2a-escalate", "-p", "hi"}, "-a2a-escalate is dax serve's"},
		{[]string{"serve", "-a2a-escalate"}, "-a2a-escalate goes with -a2a"},
		{[]string{"-a2a-url", "https://agent.example", "-p", "hi"}, "-a2a-url is dax serve's"},
		{[]string{"serve", "-a2a-url", "https://agent.example"}, "-a2a-url goes with -a2a"},
		{[]string{"serve", "-a2a", "-a2a-url", "http://agent.example"}, "want https, or http to loopback"},
		{[]string{"attach", "http://192.0.2.1:7878"}, "in the clear"},
		{[]string{"attach", "-tls-ca", "ca.pem", "http://127.0.0.1:1"}, "-tls-ca is for an https URL"},
		{[]string{"attach", "-token-file", filepath.Join(t.TempDir(), "missing"), "http://127.0.0.1:1"}, "-token-file"},
	} {
		err := run(context.Background(), tc.args, p)
		if err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.wants)
		}
	}
}

// recorded reads the served session's record as it is now.
func (s *served) recorded() *agentsession.Session {
	s.t.Helper()
	rec, err := client.RecordOf(s.rig.sess.Recorder())
	if err != nil {
		s.t.Fatal(err)
	}
	sess, err := rec.Read(s.t.Context(), "")
	if err != nil {
		s.t.Fatal(err)
	}
	return sess
}

// userTexts are the user's messages on the path to the session's head.
func userTexts(sess *agentsession.Session) []string {
	var out []string
	for _, e := range sess.Path(sess.Leaf()) {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if m, ok := it.Item.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
				out = append(out, m.Text())
			}
		}
	}
	return out
}

// leafLabels counts the head moves on the record.
func leafLabels(sess *agentsession.Session) int {
	n := 0
	for _, e := range sess.Entries() {
		if l, ok := e.(*agentsession.LabelEntry); ok && l.Label != nil && *l.Label == agentsession.LeafLabel {
			n++
		}
	}
	return n
}

// waitPath waits until the head's path holds exactly the user's
// messages want, in order.
func (s *served) waitPath(want ...string) *agentsession.Session {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		sess := s.recorded()
		if got := userTexts(sess); slices.Equal(got, want) && !s.rig.sess.State().Running {
			return sess
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("the head's path holds %q, want %q", userTexts(sess), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An attached view continues from an earlier entry as a local one does:
// the served session's head moves there, the move is on the record as a
// leaf label, and the next prompt continues from there.
func TestAnAttachedViewContinuesFromAnEarlierEntry(t *testing.T) {
	s := startServe(t, &steps{}, policy.Rules{})
	r := s.attach()
	r.waitOutput("say something")
	r.type_("first question\r")
	s.waitPath("first question")
	r.type_("second question\r")
	s.waitPath("first question", "second question")
	// The row cursor goes up from the last row: the second reply, the
	// second question, then the first reply.
	for range 3 {
		r.type_("\x10") // ctrl+p
	}
	// The view takes the move once it has seen the second run end, which
	// can be after the record has its entries; until then it says a run
	// goes, and the key is pressed again.
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(ansi.Strip(r.out.String()), "head moved to"); {
		if time.Now().After(deadline) {
			t.Fatalf("the head never moved; output:\n%q", r.out.String())
		}
		r.type_("\x02") // ctrl+b
		time.Sleep(200 * time.Millisecond)
	}
	r.type_("third question\r")
	sess := s.waitPath("first question", "third question")
	if leafLabels(sess) == 0 {
		t.Error("the record has no leaf label for the move")
	}
	r.quit()
}

// A move the served session refuses comes back to the attached view's
// controller as the session's own error: an entry that leaves a call
// without its output, and any move while a run goes.
func TestAMoveTheServedSessionRefusesComesBack(t *testing.T) {
	g := &gated{steps: &steps{parent: [][2]string{{"bash", `{"command":"touch MOVED"}`}}}, release: make(chan struct{})}
	g.let()
	s := startServeWith(t, g.steps, policy.Rules{Allow: []string{"bash(touch:*)"}}, func(o *agent.Options) { o.Streamer = g })
	ctx := t.Context()
	ctl, err := control.Dial(ctx, s.url+controlPrefix, control.WithHTTPClient(bearerClient(t, s.url, s.token)))
	if err != nil {
		t.Fatal(err)
	}
	act := attachController(ctl, &remoteControls{c: ctl})
	if err := act(ctx, client.Prompt{Items: []openresponses.Item{openresponses.UserText("go")}}); err != nil {
		t.Fatal(err)
	}
	sess := s.waitPath("go")
	var call string
	for _, e := range sess.Path(sess.Leaf()) {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if _, ok := it.Item.(*openresponses.FunctionCall); ok {
				call = e.Base().ID
			}
		}
	}
	if call == "" {
		t.Fatal("no call on the record")
	}
	err = act(ctx, client.ContinueFrom{EntryID: call})
	if err == nil || !strings.Contains(err.Error(), "without an output") {
		t.Errorf("continuing from the call: %v, want the session's refusal", err)
	}
	if n := leafLabels(s.recorded()); n != 0 {
		t.Errorf("a refused move left %d leaf label(s) on the record", n)
	}

	// While a run goes, the session refuses every move.
	g.release = make(chan struct{})
	g.once = sync.Once{}
	t.Cleanup(g.let)
	if err := act(ctx, client.Prompt{Items: []openresponses.Item{openresponses.UserText("again")}}); err != nil {
		t.Fatal(err)
	}
	if err := act(ctx, client.ContinueFrom{EntryID: sess.Leaf()}); !errors.Is(err, agentturn.ErrRunning) {
		t.Errorf("a move while a run goes: %v, want %v", err, agentturn.ErrRunning)
	}
}

// selfSigned writes a self-signed certificate for 127.0.0.1, ::1 and
// localhost, which is its own CA, and its key, as PEM files.
func selfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dax serve test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// portOf is the port of a URL dax serve printed.
func portOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

// With a certificate dax serve serves HTTPS and may listen on any
// address; a view attached over https with the certificate's CA drives
// the session, and the token is still needed.
func TestServingOverTLSTakesAnyAddress(t *testing.T) {
	cert, key := selfSigned(t)
	s := startServeFront(t, &steps{}, policy.Rules{}, nil, func(f *serveFront) {
		f.addr, f.tlsCert, f.tlsKey = "0.0.0.0:0", cert, key
	})
	if !strings.HasPrefix(s.url, "https://") {
		t.Fatalf("dax serve printed %s, want an https URL", s.url)
	}
	if !strings.Contains(s.out.String(), "attach: dax attach [-tls-ca <the certificate's CA>] -token-file ") {
		t.Errorf("dax serve's attach line does not name -tls-ca:\n%s", s.out.String())
	}
	s.attachURL = "https://127.0.0.1:" + portOf(t, s.url)
	hc, err := attachHTTPClient(s.attachURL, cert)
	if err != nil {
		t.Fatal(err)
	}
	s.hc = hc

	for _, tc := range []struct {
		token string
		want  int
	}{{"", http.StatusUnauthorized}, {s.token, http.StatusOK}} {
		req, err := http.NewRequest(http.MethodGet, s.attachURL+controlPrefix+"/state", nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("GET state over TLS with token %v: %d, want %d", tc.token != "", resp.StatusCode, tc.want)
		}
	}

	r := s.attach()
	r.type_("over tls\r")
	r.waitOutput("all done")
	r.quit()
}

// dax attach verifies the server's certificate: against the system's
// roots, a self-signed one is refused, and nothing is sent the session.
func TestAttachVerifiesTheServersCertificate(t *testing.T) {
	cert, key := selfSigned(t)
	s := startServeFront(t, &steps{}, policy.Rules{}, nil, func(f *serveFront) {
		f.tlsCert, f.tlsKey = cert, key
	})
	hc, err := attachHTTPClient(s.url, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	f := &attachFront{name: "dax", hc: hc, out: &syncBuf{}}
	err = f.Run(ctx, s.url, s.token)
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Errorf("attach without the CA: %v, want the certificate refused", err)
	}
}

// dax attach refuses a URL that would send the token in the clear, and
// any it cannot verify, before anything is sent.
func TestAttachTakesOnlyAURLItCanSendTheTokenTo(t *testing.T) {
	cert, _ := selfSigned(t)
	for _, tc := range []struct {
		url, ca, wants string
	}{
		{"http://192.0.2.1:7878", "", "in the clear"},
		{"http://example.com", "", "in the clear"},
		{"http://[2001:db8::1]:80", "", "in the clear"},
		{"ftp://127.0.0.1:1", "", "want the http or https URL"},
		{"http://127.0.0.1:1", cert, "-tls-ca is for an https URL"},
		{"https://127.0.0.1:1", filepath.Join(t.TempDir(), "missing"), "-tls-ca"},
		{"https://127.0.0.1:1", cert + "-not-pem", "-tls-ca"},
	} {
		if tc.ca == cert+"-not-pem" {
			if err := os.WriteFile(tc.ca, []byte("not a certificate"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := attachHTTPClient(tc.url, tc.ca); err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%s with -tls-ca %q: %v, want %q", tc.url, tc.ca, err, tc.wants)
		}
	}
	for _, ok := range []string{"http://127.0.0.1:1", "http://localhost:1", "http://[::1]:1", "https://example.com"} {
		if _, err := attachHTTPClient(ok, ""); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

// Without a certificate dax serve stays on loopback; with one it takes
// any address, and one of the two files alone is refused.
func TestServeTakesAnyAddressOnlyWithTLS(t *testing.T) {
	for _, tc := range []struct {
		addr, cert, key string
		ok              bool
	}{
		{"0.0.0.0:0", "", "", false},
		{"127.0.0.1:0", "", "", true},
		{"0.0.0.0:0", "c", "k", true},
		{"0.0.0.0:0", "c", "", false},
		{"127.0.0.1:0", "", "k", false},
	} {
		if err := checkServeAddr(tc.addr, tc.cert, tc.key); (err == nil) != tc.ok {
			t.Errorf("%s with cert %q key %q: %v, want ok=%v", tc.addr, tc.cert, tc.key, err, tc.ok)
		}
	}
}

// bearerClient is a client that sends token to url's origin, as dax's
// own clients do (internal/authn).
func bearerClient(t *testing.T, url, token string) *http.Client {
	t.Helper()
	c, err := authn.Client(nil, url, token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
