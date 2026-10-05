package main

import (
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentconsole/console"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/internal/agent"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/policy"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// steps is a model that makes one call per step of a run (the number of
// tool outputs since the last user message), then answers. The explore
// child is told by its instructions and has steps of its own; what it
// was shown is kept.
type steps struct {
	parent, child [][2]string
	mu            sync.Mutex
	childSaw      map[string]string
}

func (m *steps) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	calls := m.parent
	isChild := strings.Contains(req.Instructions, "read-only explorer") || strings.Contains(req.Instructions, "You are a sub-agent of dax")
	if isChild {
		calls = m.child
	}
	step := 0
	m.mu.Lock()
	if m.childSaw == nil {
		m.childSaw = map[string]string{}
	}
	m.mu.Unlock()
	for _, it := range req.Input {
		switch v := it.(type) {
		case *openresponses.Message:
			if v.Role == openresponses.RoleUser {
				step = 0
			}
		case *openresponses.FunctionCallOutput:
			step++
			if isChild {
				m.mu.Lock()
				m.childSaw[v.CallID] = v.Output.Text
				m.mu.Unlock()
			}
		}
	}
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if step < len(calls) {
		call, err := em.FunctionCall("", calls[step][0])
		if err != nil {
			return err
		}
		if err := call.Arguments(calls[step][1]); err != nil {
			return err
		}
		if err := call.Close(); err != nil {
			return err
		}
		return em.Complete()
	}
	msg, err := em.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if err := msg.Text("all done"); err != nil {
		return err
	}
	if err := msg.Close(); err != nil {
		return err
	}
	return em.Complete()
}

func (m *steps) sawInChild() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []string
	for _, v := range m.childSaw {
		all = append(all, v)
	}
	return strings.Join(all, "\n")
}

// tuiRig is dax's terminal client over a scripted model, on a pipe.
type tuiRig struct {
	f    *tuiFront
	t    *testing.T
	dir  string
	in   *io.PipeWriter
	out  *syncBuf
	pre  *syncBuf // the start lines
	done chan error
	sess *agent.Session
}

func startTUI(t *testing.T, m *steps, rules config.Rules, tweak func(*agent.Options)) *tuiRig {
	return startRig(t, m, rules, tweak, nil, true)
}

// startFront builds the front and its session without running it.
func startFront(t *testing.T, m *steps, tweak func(*tuiFront)) *tuiRig {
	return startRig(t, m, config.Rules{}, nil, tweak, false)
}

func startRig(t *testing.T, m *steps, rules config.Rules, tweak func(*agent.Options), tweakFront func(*tuiFront), run bool) *tuiRig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	base := t.TempDir()
	o := agent.Options{
		Model: "scripted", Streamer: m, Dir: filepath.Join(base, "project"), Root: filepath.Join(base, "sessions"),
		UserDir: filepath.Join(base, "user"), MemoryDir: filepath.Join(base, "user", "memory"), Agents: true,
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(o.Dir, ".env"), []byte("SECRET_TOKEN=abc123\n"), 0o644)
	os.WriteFile(filepath.Join(o.Dir, "main.go"), []byte("package main\n"), 0o644)
	p, err := policy.Build(config.PolicySettings{Builtin: true, Fallback: "ask", User: rules})
	if err != nil {
		t.Fatal(err)
	}
	o.Policy = &p
	if tweak != nil {
		tweak(&o)
	}
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	r := &tuiRig{t: t, dir: o.Dir, in: pw, out: &syncBuf{}, pre: &syncBuf{}, done: make(chan error, 1)}
	f := &tuiFront{
		info:    frontInfo{Provider: "test", Model: "scripted", Dir: o.Dir, Policy: policySummary(config.PolicySettings{Builtin: true, Fallback: "ask"})},
		out:     r.pre,
		console: []console.Option{console.WithInput(pr), console.WithOutput(r.out), console.WithoutSignalHandler(), console.WithWindowSize(220, 50)},
	}
	r.f = f
	if tweakFront != nil {
		tweakFront(f)
	}
	// Nothing reads standard input once the client has the terminal:
	// there is no approver that would prompt on it, and the elicitor and
	// the sub-agents' Ask put their questions on the client's screen.
	a, e := f.Hooks()
	if a != nil {
		t.Fatal("the terminal client answers on its screen, not through a prompt on standard input")
	}
	o.Approve, o.Elicit = a, e
	f.Prepare(&o)
	if o.Ask == nil {
		t.Fatal("the sub-agents' questions have no way to the screen")
	}
	sess, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Agent != nil {
		t.Fatal("the session must not build an agent of its own; the client builds one")
	}
	r.sess = sess
	t.Cleanup(func() { sess.Close() })
	if run {
		go func() { r.done <- f.Run(ctx, sess) }()
	}
	return r
}

func (r *tuiRig) type_(s string) {
	r.t.Helper()
	if _, err := r.in.Write([]byte(s)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *tuiRig) waitOutput(sub string) {
	r.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(r.out.String(), sub) {
		if time.Now().After(deadline) {
			r.t.Fatalf("%q never shown; output:\n%q", sub, r.out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *tuiRig) waitFile(name string, want bool) {
	r.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := os.Stat(filepath.Join(r.dir, name))
		if (err == nil) == want {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("file %s: exists=%v, want %v", name, err == nil, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *tuiRig) quit() {
	r.t.Helper()
	time.Sleep(150 * time.Millisecond)
	r.type_("\x03")
	select {
	case err := <-r.done:
		if err != nil {
			r.t.Errorf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		r.t.Fatal("the client did not return")
	}
}

func TestTheTUIShowsTheStartLinesBeforeItTakesTheScreen(t *testing.T) {
	r := startTUI(t, &steps{}, config.Rules{}, nil)
	r.quit()
	pre := r.pre.String()
	for _, want := range []string{"dax · test scripted · ", "session ", "policy: built-in allow list and secret-path asks", "tools: read, write, edit, glob, grep, ls, bash", "explore"} {
		if !strings.Contains(pre, want) {
			t.Errorf("start lines lack %q:\n%s", want, pre)
		}
	}
}

func TestAPolicyAskIsAnsweredOnTheScreen(t *testing.T) {
	m := &steps{parent: [][2]string{{"read", `{"path":"main.go"}`}, {"bash", `{"command":"touch APPROVED"}`}}}
	r := startTUI(t, m, config.Rules{}, nil)
	r.type_("go\r")
	// The read is allowed and shown; the touch is a permission, with the
	// policy's reason.
	r.waitOutput("touch APPROVED")
	r.waitOutput("no rule allows bash")
	r.waitFile("APPROVED", false)
	r.type_("y")
	r.waitFile("APPROVED", true)
	r.waitOutput("all done")
	r.quit()

	// Refused, it does not run.
	m2 := &steps{parent: [][2]string{{"bash", `{"command":"touch REFUSED"}`}}}
	r2 := startTUI(t, m2, config.Rules{}, nil)
	r2.type_("go\r")
	r2.waitOutput("no rule allows bash")
	r2.type_("n")
	time.Sleep(100 * time.Millisecond)
	r2.type_("not now\r")
	// An ended call's row hides its output, the refusal text with it;
	// the policy's verdict stays on the row.
	r2.waitOutput("policy: reject")
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(r2.dir, "REFUSED")); err == nil {
		t.Error("a refused call ran")
	}
	r2.quit()
}

func TestASecretPathAskNamesTheRuleAndTheFile(t *testing.T) {
	m := &steps{parent: [][2]string{{"bash", `{"command":"cat .ENV"}`}}}
	os.Setenv("DAX_TEST", "1")
	r := startTUI(t, m, config.Rules{}, func(o *agent.Options) {
		os.WriteFile(filepath.Join(o.Dir, ".ENV"), []byte("SECRET_TOKEN=upper\n"), 0o644)
	})
	r.type_("go\r")
	r.waitOutput("cat .ENV")
	// The question says what is being asked about, from the read rule.
	r.waitOutput("reads .ENV")
	r.type_("n")
	time.Sleep(100 * time.Millisecond)
	r.type_("\r")
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(r.out.String(), "SECRET_TOKEN=upper") {
		t.Error("the secret was shown after the user refused")
	}
	r.quit()
}

func TestATUISessionShowsTheGitConfigKeyInTheQuestion(t *testing.T) {
	m := &steps{parent: [][2]string{{"bash", `{"command":"git status && echo hi"}`}}}
	r := startTUI(t, m, config.Rules{Allow: []string{"bash(echo:*)"}}, func(o *agent.Options) {
		for _, args := range [][]string{{"init", "-q"}, {"config", "core.fsmonitor", "/bin/true-not-bool"}} {
			if err := gitIn(o.Dir, args...); err != nil {
				t.Skip("no git")
			}
		}
	})
	r.type_("go\r")
	r.waitOutput("core.fsmonitor")
	r.type_("n")
	time.Sleep(100 * time.Millisecond)
	r.type_("\r")
	r.quit()
}

// The explore child's held calls are questions on the screen, one at a
// time, while the run goes; refused, nothing runs and the child is told
// so. They were refused unasked before the client could ask a running
// call's question.
func TestTheExploreChildsHeldCallsAreAskedOnTheScreen(t *testing.T) {
	m := &steps{
		parent: [][2]string{{"explore", `{"input":"look"}`}},
		child:  [][2]string{{"bash", `{"command":"touch CHILD"}`}, {"read", `{"path":".env"}`}},
	}
	r := startTUI(t, m, config.Rules{}, nil)
	r.type_("go\r")
	r.waitOutput("Question (1/1): bash")
	r.waitOutput("the explore sub-agent asks")
	r.type_("n")
	r.waitOutput("Reason for refusing")
	r.type_("\r")
	r.waitOutput("Question (1/1): read")
	r.type_("n\r")
	r.waitOutput("all done")
	r.quit()
	if _, err := os.Stat(filepath.Join(r.dir, "CHILD")); err == nil {
		t.Error("the child's touch ran")
	}
	saw := m.sawInChild()
	if strings.Count(saw, "the user denied this call") != 2 {
		t.Errorf("the child saw %q, want both calls denied", saw)
	}
	if strings.Contains(saw, "abc123") {
		t.Error("the child read .env")
	}
}

func TestANoteDaxMakesWhileTheClientHasTheScreenIsShownAfter(t *testing.T) {
	r := startTUI(t, &steps{}, config.Rules{}, func(o *agent.Options) {
		o.Compact = 1
	})
	r.quit()
	// Nothing was written to the terminal during the run through dax's
	// own Log; what was noted is printed once the client lets go.
	if strings.Contains(r.out.String(), "compacted") {
		t.Error("a log line reached the screen")
	}
}

func gitIn(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	return cmd.Run()
}

// A panic in the client does not lose what dax noted, nor the session's
// ID, and the panic goes on.
func TestTheBufferedNotesAreFlushedEvenOnAPanic(t *testing.T) {
	var out syncBuf
	for _, panics := range []bool{false, true} {
		out = syncBuf{}
		r := startFront(t, &steps{}, func(f *tuiFront) {
			f.out = &out
			f.run = func(context.Context, client.Backend, ...console.Option) error {
				f.log = append(f.log, "[compaction failed after 2 call(s): boom]")
				if panics {
					panic("the client fell over")
				}
				return nil
			}
		})
		func() {
			defer func() {
				got := recover()
				if panics != (got != nil) {
					t.Errorf("panics=%v but recovered %v", panics, got)
				}
			}()
			r.f.Run(context.Background(), r.sess)
		}()
		if !strings.Contains(out.String(), "compaction failed after 2 call(s): boom") || !strings.Contains(out.String(), "session "+r.sess.ID()) {
			t.Errorf("panics=%v: the notes were not flushed:\n%s", panics, out.String())
		}
	}
}

// If the session cannot be opened after the front held warnings back, they
// are shown before the error.
func TestWarningsHeldBackAreShownWhenTheSessionCannotOpen(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(root, 0o755)
	var errOut syncBuf
	f := &tuiFront{errOut: &errOut}
	o := agent.Options{Model: "x", Streamer: &steps{}, Dir: filepath.Join(base, "does-not-exist"), Root: root, UserDir: base}
	f.Prepare(&o)
	_, err := agent.New(context.Background(), o)
	if err == nil {
		t.Fatal("opening a session in a missing directory should fail")
	}
	f.Abandon()
	if !strings.Contains(errOut.String(), "was readable by other users") {
		t.Errorf("the held-back warning was dropped: %q", errOut.String())
	}
	// The capture is undone: later warnings go to standard error again.
	var w syncBuf
	undo := agent.CaptureWarnings(&w)
	undo()
}

// A sub-agent's call the policy asks about is asked on the screen while
// the run goes, and the answer reaches the call: it was refused, with a
// reason telling the model to make the call itself, before the client
// could ask a running call's question.
func TestASubagentsAskIsAnsweredOnTheScreen(t *testing.T) {
	m := &steps{
		parent: [][2]string{{"task", `{"input":"write the file"}`}},
		child:  [][2]string{{"write", `{"path":"FROM_TASK","content":"x"}`}},
	}
	r := startTUI(t, m, config.Rules{}, nil)
	r.type_("go\r")
	r.waitOutput("Question (1/1): write")
	r.waitOutput("the task sub-agent asks")
	r.waitFile("FROM_TASK", false)
	r.type_("y")
	r.waitFile("FROM_TASK", true)
	r.waitOutput("all done")
	r.quit()
}

func TestRefusingASubagentsAskOnTheScreenTellsItWhy(t *testing.T) {
	m := &steps{
		parent: [][2]string{{"task", `{"input":"write the file"}`}},
		child:  [][2]string{{"write", `{"path":"REFUSED","content":"x"}`}},
	}
	r := startTUI(t, m, config.Rules{}, nil)
	r.type_("go\r")
	r.waitOutput("Question (1/1): write")
	r.type_("n")
	r.waitOutput("Reason for refusing")
	r.type_("wrong file\r")
	r.waitOutput("all done")
	if _, err := os.Stat(filepath.Join(r.dir, "REFUSED")); err == nil {
		t.Error("a refused call ran")
	}
	m.mu.Lock()
	saw := strings.Join(slices.Collect(maps.Values(m.childSaw)), "\n")
	m.mu.Unlock()
	if !strings.Contains(saw, "Reason: wrong file") {
		t.Errorf("the sub-agent saw %q, want the user's reason", saw)
	}
	r.quit()
}
