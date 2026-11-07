package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// A command the user runs with ! runs in the session's workspace, not
// through the policy, and its line, output and exit status reach the
// model with the next prompt as the user's; the record says the user ran
// a command, and the output is not the person's words, which a fold
// keeps (#169, #183).
func TestAUserCommandReachesTheModelWithTheNextPrompt(t *testing.T) {
	ctx := context.Background()
	m := &seeing{}
	o := options(t, m)
	o.Policy = confirmPolicy(t)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Run as the fronts run it, under the person's trigger, which its
	// output does not take.
	c, err := s.RunUserCommand(agentturn.ContextWithTrigger(ctx, AtTerminal), "echo from-the-user; false")
	if err != nil {
		t.Fatal(err)
	}
	if c.ExitCode != 1 || !strings.Contains(c.Output, "from-the-user") {
		t.Fatalf("ran %+v", c)
	}
	if _, err := promptOn(ctx, s, "fix this", nil); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	seen := userTexts(m.last)
	m.mu.Unlock()
	joined := strings.Join(seen, "\n")
	if !strings.Contains(joined, "$ echo from-the-user; false") || !strings.Contains(joined, "from-the-user\n") || !strings.Contains(joined, "[exit 1]") || !strings.Contains(joined, "fix this") {
		t.Errorf("the model saw %q", seen)
	}
	// The record does not hold the output as the person's words, nor
	// as an unclaimed prompt a fold after a resume would keep.
	found := false
	for _, e := range s.Kit.Session().Path(s.Kit.Session().Leaf()) {
		ie, ok := e.(*agentsession.ItemEntry)
		if !ok {
			continue
		}
		if msg, ok := ie.Item.(*openresponses.Message); ok && strings.Contains(msg.Text(), "$ echo from-the-user") {
			found = true
			if w := agentsession.WordsOf(ie); w == agentsession.WordsPerson {
				t.Errorf("the command's output is recorded as the person's words (%v)", w)
			}
		}
	}
	if !found {
		t.Error("the record holds no message with the command's output")
	}
	for m := range unclaimed(s.Kit.Session(), s.ag.State().Transcript) {
		if strings.Contains(m.Text(), "$ echo from-the-user") {
			t.Error("a fold after a resume would keep the command's output as a prompt")
		}
	}
	data := string(projected(t, o, s))
	if !strings.Contains(data, `"`+userCommandNS+`"`) || !strings.Contains(data, `"exit_code":1`) {
		t.Error("the record does not say the user ran a command")
	}
}

// A command that exits while a job it left holds its output open is
// cut short with its own status (agentworkspace v0.0.12's
// ErrCutShort), which reaches the model and the record, not 0.
func TestAUserCommandCutShortKeepsItsStatus(t *testing.T) {
	ctx := context.Background()
	o := options(t, &seeing{})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.RunUserCommand(ctx, "sleep 5 & echo started; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if c.ExitCode != 3 || !strings.Contains(c.Output, "started") {
		t.Fatalf("ran %+v", c)
	}
}
