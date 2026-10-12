package dax

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// asker is a tool that asks the user for a line of text and returns
// what it got: the action, then the text.
type asker struct{}

func (asker) Name() string        { return "ask" }
func (asker) Description() string { return "Asks the user which colour." }
func (asker) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (asker) Execute(ctx context.Context, _ agenttool.Call) (agenttool.Result, error) {
	elicit, ok := agenttool.ElicitorFrom(ctx)
	if !ok {
		return agenttool.Text("no elicitor"), nil
	}
	q := extension.TextQuestion("which colour?")
	ans, err := elicit(ctx, q)
	if err != nil {
		return agenttool.Result{}, err
	}
	text, _ := extension.TextAnswer(q, ans)
	return agenttool.Text(string(ans.Action) + ":" + text), nil
}

// askSession is a session whose model calls ask once, allowed.
func askSession(t *testing.T) *agent.Session {
	t.Helper()
	base := t.TempDir()
	o := agent.Options{
		Model: "scripted", Streamer: &steps{parent: [][2]string{{"ask", `{}`}}},
		Dir: filepath.Join(base, "project"), Root: filepath.Join(base, "sessions"), UserDir: filepath.Join(base, "user"),
		Extensions: []extension.Extension{{Name: "acme-ask", Tools: func(extension.ToolEnv) []agenttool.Tool { return []agenttool.Tool{asker{}} }}},
		Policy:     &policy.Settings{Builtin: true, Fallback: "ask", User: policy.Rules{Allow: []string{"ask"}}},
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sess, err := agent.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// A question for text is answered with the line typed: one that is not
// an explicit no is the answer, a bare yes is "yes", and a no refuses.
// At the end of -p's input nobody answers it, which is not a no: the
// answer is a cancel, and the record names nobody.
func TestPrintAnswersAQuestionForText(t *testing.T) {
	for _, tc := range []struct {
		name, input, out, action, note string
		content                        string
	}{
		{"a reply", "blue\n", "accept:blue", "accept", "", `{"answer":"blue"}`},
		{"a reply after yes", "y the blue one\n", "accept:the blue one", "accept", "", `{"answer":"the blue one"}`},
		{"a bare yes", "yes\n", "accept:yes", "accept", "", `{"answer":"yes"}`},
		{"a no with a reason", "n pick yourself\n", "decline:", "decline", "pick yourself", ""},
		{"the end of the input", "", "cancel:", "cancel", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := askSession(t)
			f := &printFront{info: frontInfo{Name: "dax", Prompt: "go"}, in: bufio.NewScanner(strings.NewReader(tc.input))}
			if err := f.run(context.Background(), sess, sess); err != nil {
				t.Fatal(err)
			}
			var answered *session.Elicitation
			for _, e := range recordedCustom(t, sess, session.ElicitationNS) {
				var q session.Elicitation
				if err := json.Unmarshal(e, &q); err != nil {
					t.Fatal(err)
				}
				if q.Phase == session.ElicitationAnswer {
					answered = &q
				}
			}
			if answered == nil {
				t.Fatal("no answer on the record")
			}
			if answered.Action != tc.action || answered.Note != tc.note || string(answered.Content) != tc.content {
				t.Errorf("recorded %q, note %q, content %s; want %q, %q, %s", answered.Action, answered.Note, answered.Content, tc.action, tc.note, tc.content)
			}
			if tc.input == "" && answered.Source != nil {
				t.Errorf("an answer nobody gave names %+v", answered.Source)
			}
			if out := toolOutput(t, sess); out != tc.out {
				t.Errorf("the tool got %q, want %q", out, tc.out)
			}
		})
	}
}

// toolOutput is the output of the session's one call.
func toolOutput(t *testing.T, sess *agent.Session) string {
	t.Helper()
	for _, it := range sess.Agent().State().Transcript {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok {
			return o.Output.Text
		}
	}
	t.Fatal("no call output")
	return ""
}

// The terminal client is shown a question for text without its form,
// which it cannot fill, so it answers with a note, which the session
// takes as the answer.
func TestTheTerminalClientSeesATextQuestionWithoutItsForm(t *testing.T) {
	var src fakeSource
	var got agentturn.Event
	textQuestions{&src}.Subscribe(func(_ context.Context, ev agentturn.Event) error { got = ev; return nil })
	q := &agentturn.Question{ID: "q1", Elicitation: extension.TextQuestion("which colour?")}
	src.fn(context.Background(), q)
	if g, ok := got.(*agentturn.Question); !ok || g.Elicitation.Schema != nil || g.Elicitation.Message != "which colour?" {
		t.Errorf("the client got %+v", got)
	}
	if q.Elicitation.Schema == nil {
		t.Error("the session's own event was changed")
	}
	form := &agentturn.Question{ID: "q2", Elicitation: agenttool.Elicitation{Message: "fill", Schema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"}}}`)}}
	src.fn(context.Background(), form)
	if got.(*agentturn.Question).Elicitation.Schema == nil {
		t.Error("a form that is not one text field lost its schema")
	}
}

type fakeSource struct {
	fn func(context.Context, agentturn.Event) error
}

func (f *fakeSource) Subscribe(fn func(context.Context, agentturn.Event) error) func() {
	f.fn = fn
	return func() {}
}

func (f *fakeSource) State() agentturn.State { return agentturn.State{} }
