package dax

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if saw := m.sawInChild(); !strings.Contains(saw, "The user said: keep it short") {
		t.Errorf("the sub-agent saw %q, want the user's note", saw)
	}
}
