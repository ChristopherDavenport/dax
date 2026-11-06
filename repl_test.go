package dax

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

func TestAnswerLine(t *testing.T) {
	for _, tc := range []struct {
		line        string
		allow, stop bool
		note        string
	}{
		{"y", true, false, ""},
		{"Yes", true, false, ""},
		{"y keep it short", true, false, "keep it short"},
		{"yes, keep it short", true, false, "keep it short"},
		{"", false, false, ""},
		{"n", false, false, ""},
		{"no: wrong file", false, false, "wrong file"},
		{"not that one", false, false, "not that one"},
		{"yesterday", false, false, "yesterday"},
		// s refuses and stops the run (#150).
		{"s", false, true, ""},
		{"S not now", false, true, "not now"},
		{"stop: leave build alone", false, true, "leave build alone"},
		{"stopping", false, false, "stopping"},
	} {
		allow, stop, note := answerLine(tc.line)
		if allow != tc.allow || stop != tc.stop || note != tc.note {
			t.Errorf("answerLine(%q) = %v %v %q, want %v %v %q", tc.line, allow, stop, note, tc.allow, tc.stop, tc.note)
		}
	}
}

// toldOfRefusal is what the model was told of the last call on sess's
// record.
func toldOfRefusal(t *testing.T, sess *agent.Session) string {
	t.Helper()
	r, err := client.RecordOf(sess.Recorder())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := r.Read(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	var out string
	for _, e := range rec.Path(rec.Leaf()) {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if o, ok := it.Item.(*openresponses.FunctionCallOutput); ok {
				out = o.Output.Text
			}
		}
	}
	return out
}

// n refuses a call and the run goes on, and s refuses it and ends the
// run; the model is told the same words either way (#150). -p asks in
// line, so its answer is read only once the question is shown.
func TestARefusalGoesOnOrStops(t *testing.T) {
	for _, tc := range []struct {
		line   string
		reason agentturn.Reason
		status int
	}{
		{"n not that\n", agentturn.ReasonDone, 0},
		{"s not that\n", agentturn.ReasonStopped, exitStopped},
	} {
		t.Run(tc.line[:1], func(t *testing.T) {
			ctx := context.Background()
			m := &steps{parent: [][2]string{{"bash", `{"command":"touch REFUSED"}`}}}
			sess, dir := printSession(t, ctx, m)
			f := &printFront{info: frontInfo{Name: "dax", Prompt: "go"}, in: bufio.NewScanner(strings.NewReader(tc.line))}
			err := f.run(ctx, sess, sess)
			var ended *runEnded
			switch {
			case tc.status == 0 && err != nil:
				t.Fatalf("run: %v, want done", err)
			case tc.status != 0 && (!errors.As(err, &ended) || ended.status != tc.status):
				t.Fatalf("run: %v, want exit %d", err, tc.status)
			}
			if end := runEndOf(t, sess); end.Reason != string(tc.reason) {
				t.Errorf("the record ends %s, want %s", end.Reason, tc.reason)
			}
			if _, err := os.Stat(filepath.Join(dir, "REFUSED")); err == nil {
				t.Error("the refused call ran")
			}
			if out := toldOfRefusal(t, sess); !strings.Contains(out, "Do not get the same result another way") || !strings.HasSuffix(out, "Reason: not that") {
				t.Errorf("the model was told %q", out)
			}
		})
	}
}

// The REPL's own n and s, typed once the question is shown: n refuses
// and the run goes on, s refuses and ends it (#150).
func TestTheREPLRefusesAndGoesOnOrStops(t *testing.T) {
	for _, tc := range []struct {
		line   string
		reason agentturn.Reason
	}{
		{"n not that\n", agentturn.ReasonDone},
		{"s not that\n", agentturn.ReasonStopped},
	} {
		t.Run(tc.line[:1], func(t *testing.T) {
			ctx := context.Background()
			m := &steps{parent: [][2]string{{"bash", `{"command":"touch REFUSED"}`}}}
			sess, dir := printSession(t, ctx, m)
			pr, pw := io.Pipe()
			out := &syncBuf{}
			f := &replFront{info: frontInfo{Name: "dax"}, in: bufio.NewScanner(pr), out: out}
			done := make(chan error, 1)
			go func() { done <- f.Run(ctx, sess) }()
			pw.Write([]byte("go\n"))
			waitIn(t, out, "? allow bash")
			pw.Write([]byte(tc.line))
			deadline := time.Now().Add(15 * time.Second)
			for !strings.Contains(out.String(), "["+string(tc.reason)) {
				if time.Now().After(deadline) {
					t.Fatalf("the run never ended %s:\n%s", tc.reason, out.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			pw.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if end := runEndOf(t, sess); end.Reason != string(tc.reason) {
				t.Errorf("the record ends %s, want %s", end.Reason, tc.reason)
			}
			if _, err := os.Stat(filepath.Join(dir, "REFUSED")); err == nil {
				t.Error("the refused call ran")
			}
			if out := toldOfRefusal(t, sess); !strings.HasSuffix(out, "Reason: not that") {
				t.Errorf("the model was told %q", out)
			}
		})
	}
}

// The REPL answers a question asked while a call runs from a line of
// standard input, and what the user says after the y reaches the
// sub-agent with the result.
func TestTheREPLAnswersAQuestionWithANote(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &steps{
		parent: [][2]string{{"task", `{"input":"write the file"}`}},
		child:  [][2]string{{"write", `{"path":"FROM_REPL","content":"x"}`}},
	}
	base := t.TempDir()
	o := agent.Options{
		Model: "scripted", Streamer: m, Dir: filepath.Join(base, "project"), Root: filepath.Join(base, "sessions"),
		UserDir:    filepath.Join(base, "user"),
		Extensions: []extension.Extension{coding.New(0), agents.New(agents.Options{})},
		Policy:     &policy.Settings{Builtin: true, Fallback: "ask"},
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sess, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	ended := make(chan *agentturn.RunEnd, 4)
	defer sess.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		if e, ok := ev.(*agentturn.RunEnd); ok {
			ended <- e
		}
		return nil
	})()
	pr, pw := io.Pipe()
	out := &syncBuf{}
	f := &replFront{info: frontInfo{Name: "dax"}, in: bufio.NewScanner(pr), out: out}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, sess) }()

	// The answer is typed once the question is shown: a line typed
	// before it would not answer it.
	pw.Write([]byte("go\n"))
	asked := make(chan *agentturn.Question, 1)
	defer sess.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		if e, ok := ev.(*agentturn.Question); ok {
			asked <- e
		}
		return nil
	})()
	select {
	case q := <-asked:
		if q.Call == nil || q.Call.Name != "write" || !strings.Contains(q.Elicitation.Message, "the task sub-agent asks") {
			t.Fatalf("asked %+v", q)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no question was asked")
	}
	waitIn(t, out, "? allow write")
	pw.Write([]byte("y keep it short\n"))
	select {
	case end := <-ended:
		if end.Reason != agentturn.ReasonDone {
			t.Fatalf("the run ended %s", end.Reason)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the run did not end")
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "FROM_REPL")); err != nil {
		t.Errorf("the allowed call did not run: %v", err)
	}
	// The note is the person's own message, not a line of the policy's.
	if saw := m.sawInChild(); !strings.Contains(saw, "keep it short") || strings.Contains(saw, "The user said") {
		t.Errorf("the sub-agent saw %q, want the user's note as theirs", saw)
	}
	checkRecordedQuestion(t, sess, "the task sub-agent asks", "write", "accept", "keep it short", atTerminal)
}

// The REPL shows what a sub-agent's question is about from the asked
// call's decision, once, beside the question's own text.
func TestTheREPLShowsWhatAQuestionIsAbout(t *testing.T) {
	call := func(subject string) *agentturn.AskedCall {
		return &agentturn.AskedCall{CallID: "c1", Name: "bash", Args: json.RawMessage(`{"command":"touch CHILD"}`),
			Decision: &agentturn.ToolDecision{Action: agentturn.Defer, Subject: subject}}
	}
	cases := []struct {
		name, message, subject, want string
	}{
		{"a subject", "the explore sub-agent asks: approval required by default", "touch CHILD", "(the explore sub-agent asks: approval required by default; about: touch CHILD)"},
		{"no subject", "the explore sub-agent asks: approval required by default", "", "(the explore sub-agent asks: approval required by default)"},
		{"named already", "the explore sub-agent asks: touch CHILD needs approval", "touch CHILD", "(the explore sub-agent asks: touch CHILD needs approval)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := questionText(agentturn.Question{ID: "q1", Call: call(tc.subject), Elicitation: agenttool.Elicitation{Message: tc.message}})
			if !strings.Contains(got, tc.want) {
				t.Errorf("questionText = %q, want it to hold %q", got, tc.want)
			}
		})
	}
}

// printSession is a recorded session with dax-coding and dax-agents
// under the shipped policy, on m, for -p's tests.
func printSession(t *testing.T, ctx context.Context, m *steps) (*agent.Session, string) {
	t.Helper()
	base := t.TempDir()
	o := agent.Options{
		Model: "scripted", Streamer: m, Dir: filepath.Join(base, "project"), Root: filepath.Join(base, "sessions"),
		UserDir:    filepath.Join(base, "user"),
		Extensions: []extension.Extension{coding.New(0), agents.New(agents.Options{})},
		Policy:     &policy.Settings{Builtin: true, Fallback: "ask"},
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sess, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess, o.Dir
}

// -p with its input at an end declines a sub-agent's question itself:
// nobody answered, so the record names nobody, not the person at the
// terminal.
func TestPrintAtTheEndOfItsInputAnswersForNobody(t *testing.T) {
	ctx := context.Background()
	m := &steps{
		parent: [][2]string{{"task", `{"input":"write the file"}`}},
		child:  [][2]string{{"write", `{"path":"NEVER","content":"x"}`}},
	}
	sess, dir := printSession(t, ctx, m)
	f := &printFront{info: frontInfo{Name: "dax", Prompt: "go"}, in: bufio.NewScanner(strings.NewReader(""))}
	if err := f.run(ctx, sess, sess); err != nil {
		t.Fatal(err)
	}
	checkRecordedQuestion(t, sess, "the task sub-agent asks", "write", "cancel", "", agentsession.Trigger{})
	// Nobody answered, so the sub-agent is not told the user denied
	// its call: its call stays deferred and the task call fails
	// input-required (#163).
	m.mu.Lock()
	saw := strings.Join(slices.Collect(maps.Values(m.childSaw)), "\n")
	m.mu.Unlock()
	if strings.Contains(saw, "user denied") {
		t.Errorf("the sub-agent was told %q", saw)
	}
	if _, err := os.Stat(filepath.Join(dir, "NEVER")); err == nil {
		t.Error("the unanswered write ran")
	}
}

// An interrupt stops a -p run while a question waits on a line: the
// rules wait off the question's delivery, so the abort is not held
// behind them.
func TestPrintStopsWhileAQuestionWaitsOnALine(t *testing.T) {
	ctx := context.Background()
	m := &steps{
		parent: [][2]string{{"task", `{"input":"write the file"}`}},
		child:  [][2]string{{"write", `{"path":"NEVER","content":"x"}`}},
	}
	sess, dir := printSession(t, ctx, m)
	pr, pw := io.Pipe()
	defer pw.Close()
	defer sess.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		if _, ok := ev.(*agentturn.Question); ok {
			go sess.Abort()
		}
		return nil
	})()
	f := &printFront{info: frontInfo{Name: "dax", Prompt: "go"}, in: bufio.NewScanner(pr)}
	done := make(chan error, 1)
	go func() { done <- f.run(ctx, sess, sess) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the run did not stop while the question waited on a line")
	}
	if _, err := os.Stat(filepath.Join(dir, "NEVER")); err == nil {
		t.Error("the call the question was about ran")
	}
}

// waitIn waits for sub in what w holds.
func waitIn(t *testing.T, w *syncBuf, sub string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(w.String(), sub) {
		if time.Now().After(deadline) {
			t.Fatalf("%q never shown:\n%s", sub, w.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A line typed before a question was shown does not answer it: a steer
// such as "yes, also fix the tests" typed while a sub-agent's call was
// being asked about must not approve a call nobody saw. It goes on as a
// message, and the question waits for a line typed after it. The REPL
// is kept busy (a !sleep) while the question arrives and the line is
// typed, so both are waiting when it looks.
func TestALineTypedBeforeAQuestionDoesNotAnswerIt(t *testing.T) {
	ctx := context.Background()
	m := &steps{
		parent: [][2]string{{"task", `{"input":"write the file"}`}},
		child:  [][2]string{{"write", `{"path":"NEVER","content":"x"}`}},
	}
	sess, dir := printSession(t, ctx, m)
	pr, pw := io.Pipe()
	out := &syncBuf{}
	f := &replFront{info: frontInfo{Name: "dax"}, in: bufio.NewScanner(pr), out: out}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, sess) }()
	waitIn(t, out, "> ")
	// Subscribed after the REPL, so it has taken the question in (not
	// shown it) when the line is typed.
	var busy, typed sync.Once
	defer sess.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		switch e := ev.(type) {
		case *agentturn.ToolStart:
			if e.Name == "task" {
				busy.Do(func() { go pw.Write([]byte("!sleep 1\n")) })
			}
		case *agentturn.Question:
			typed.Do(func() { go pw.Write([]byte("yes, also fix the tests\n")) })
		}
		return nil
	})()
	pw.Write([]byte("go\n"))
	waitIn(t, out, "? allow write")
	time.Sleep(500 * time.Millisecond)
	pw.Close()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the REPL did not end")
	}
	if _, err := os.Stat(filepath.Join(dir, "NEVER")); err == nil {
		t.Fatalf("a line typed before the question approved the call:\n%s", out.String())
	}
}

// What the person types at the REPL is recorded as their words (format
// 0.21's trigger kind human); the output of a command they ran with !
// is not (#183).
func TestTheREPLRecordsTypedInputAsThePersons(t *testing.T) {
	ctx := context.Background()
	sess, _ := printSession(t, ctx, &steps{})
	pr, pw := io.Pipe()
	out := &syncBuf{}
	f := &replFront{info: frontInfo{Name: "dax"}, in: bufio.NewScanner(pr), out: out}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, sess) }()
	pw.Write([]byte("!echo from-a-command\n"))
	waitIn(t, out, "[exit 0")
	pw.Write([]byte("hello\n"))
	waitIn(t, out, "all done")
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	words := wordsOf(t, sess)
	if got, ok := words["hello"]; !ok || got != agentsession.WordsPerson {
		t.Errorf("the typed prompt is recorded as %v (found %v), want the person's: %v", got, ok, words)
	}
	for text, w := range words {
		if strings.Contains(text, "from-a-command") && w == agentsession.WordsPerson {
			t.Errorf("a command's output is recorded as the person's words: %q", text)
		}
	}
}
