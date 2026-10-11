package dax

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

func TestAnswerLine(t *testing.T) {
	for _, tc := range []struct {
		line  string
		allow bool
		note  string
	}{
		{"y", true, ""},
		{"Yes", true, ""},
		{"y keep it short", true, "keep it short"},
		{"yes, keep it short", true, "keep it short"},
		{"", false, ""},
		{"n", false, ""},
		{"no: wrong file", false, "wrong file"},
		{"not that one", false, "not that one"},
		{"yesterday", false, "yesterday"},
	} {
		allow, note := answerLine(tc.line)
		if allow != tc.allow || note != tc.note {
			t.Errorf("answerLine(%q) = %v %q, want %v %q", tc.line, allow, note, tc.allow, tc.note)
		}
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
	f := &replFront{info: frontInfo{Name: "dax"}, in: bufio.NewScanner(pr)}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, sess) }()

	// The REPL subscribes before it reads a line, so once "go" is read
	// a subscriber added now is told of a question after the REPL is (or
	// is sent it on subscribing, when it is already waiting): the answer
	// written then is read as the question's.
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
	sess, _ := printSession(t, ctx, m)
	f := &printFront{info: frontInfo{Name: "dax", Prompt: "go"}, in: bufio.NewScanner(strings.NewReader(""))}
	if err := f.run(ctx, sess, sess); err != nil {
		t.Fatal(err)
	}
	checkRecordedQuestion(t, sess, "the task sub-agent asks", "write", "decline", "", agentsession.Trigger{})
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
