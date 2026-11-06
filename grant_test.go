package dax

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/policy"
)

// In the terminal client a, beside the rule dax offers, approves the
// call and grants the rule for the session: the same call again runs
// unasked.
func TestTheTUIGrantsForTheSession(t *testing.T) {
	m := &steps{parent: [][2]string{{"bash", `{"command":"echo hello"}`}}}
	r := startTUI(t, m, policy.Rules{}, nil)
	r.type_("go\r")
	r.waitOutput("bash(echo hello)")
	r.waitPanel()
	r.type_("a")
	r.waitOutput("all done")
	mark := r.mark()
	r.type_("go\r")
	r.waitOutputSince(mark, "all done")
	if strings.Contains(r.out.String()[mark:], "[y] approve") {
		t.Error("the call was asked about again")
	}
	r.quit()
}

// The REPL's answer a grants the offered rule for the session, and A
// keeps it for the project; the question says which rule.
func TestGrantLine(t *testing.T) {
	o := agent.GrantOffer{Rule: "bash(go test:*)", Saved: true}
	for _, tc := range []struct {
		line string
		kind agent.GrantKind
		note string
		ok   bool
	}{
		{"a", agent.GrantSession, "", true},
		{"a  flaky ones too", agent.GrantSession, "flaky ones too", true},
		{"A", agent.GrantSaved, "", true},
		{"y", "", "", false},
		{"all of it", "", "", false},
	} {
		kind, note, ok := grantLine(tc.line, o)
		if kind != tc.kind || note != tc.note || ok != tc.ok {
			t.Errorf("%q: %q %q %v", tc.line, kind, note, ok)
		}
	}
	if _, _, ok := grantLine("A", agent.GrantOffer{Rule: "bash(go test:*)"}); ok {
		t.Error("A kept a rule the session cannot keep")
	}
	if _, _, ok := grantLine("a", agent.GrantOffer{}); ok {
		t.Error("a granted with nothing offered")
	}
	if q := questionOffer(&openresponses.FunctionCall{Name: "bash", Arguments: `{"command":"go test ./..."}`}, "no rule allows bash", o); !strings.Contains(q, "[y/a/A/N] (a: allow bash(go test:*) for this session, A: always in this project)") {
		t.Errorf("question %q", q)
	}
}

// In the REPL, a approves the call and grants the offered rule for the
// session: asked again, the same call runs unasked.
func TestTheREPLGrantsForTheSession(t *testing.T) {
	ctx := context.Background()
	m := &steps{parent: [][2]string{{"bash", `{"command":"echo hello"}`}}}
	sess, _ := printSession(t, ctx, m)
	pr, pw := io.Pipe()
	out := &syncBuf{}
	f := &replFront{info: frontInfo{Name: "dax"}, in: bufio.NewScanner(pr), out: out}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, sess) }()
	pw.Write([]byte("go\n"))
	waitIn(t, out, "(a: allow bash(echo hello) for this session, A: always in this project)")
	pw.Write([]byte("a\n"))
	waitIn(t, out, "[bash(echo hello) allowed for this session]")
	waitIn(t, out, "[done")
	mark := len(out.String())
	pw.Write([]byte("go\n"))
	deadline := time.Now().Add(15 * time.Second)
	for strings.Count(out.String(), "[done") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the second run never ended:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(out.String()[mark:], "? allow") {
		t.Errorf("asked again:\n%s", out.String()[mark:])
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
