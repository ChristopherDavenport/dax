package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// A command the user runs with ! runs in the session's workspace, not
// through the policy, and its line, output and exit status reach the
// model with the next prompt as the user's; the record says the user ran
// a command, and a fold does not pin its output as a request (#169).
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
	c, err := s.RunUserCommand(ctx, "echo from-the-user; false")
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
	for _, it := range m.last {
		if msg, ok := it.(*openresponses.Message); ok && strings.Contains(msg.Text(), "$ echo from-the-user") && pinPrompts(msg) {
			t.Error("a fold would pin the command's output as the user's request")
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
