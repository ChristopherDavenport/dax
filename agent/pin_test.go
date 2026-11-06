package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// seeing is the echo model, keeping the input of the last request that
// was not a fold's summary.
type seeing struct {
	echo.Adapter
	mu   sync.Mutex
	last openresponses.Items
}

func (m *seeing) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if msg, ok := req.Input[len(req.Input)-1].(*openresponses.Message); !ok || msg.Text() != compact.DefaultSummaryPrompt {
		m.mu.Lock()
		m.last = req.Input
		m.mu.Unlock()
	}
	return m.Adapter.CreateStream(ctx, req, sink)
}

// userTexts are the user's messages in items.
func userTexts(items openresponses.Items) []string {
	var out []string
	for _, it := range items {
		if m, ok := it.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
			out = append(out, m.Text())
		}
	}
	return out
}

// A fold keeps the person's own messages verbatim, those sent under a
// trigger of kind human (agentturn's compact default, agentkit v0.0.17),
// and folds a message nobody claims, which dax no longer guesses is
// the person's; a message past the bound, a pasted log, is folded like
// the rest (#173, agentturn#298).
func TestAFoldKeepsThePersonsMessagesVerbatim(t *testing.T) {
	ctx := context.Background()
	m := &seeing{}
	o := options(t, m)
	// A budget the pasted log alone passes, so it folds, with room for
	// the person's messages (a quarter of the budget, agentturn's
	// default total).
	o.Compact = 20000
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	byPerson := agentturn.ContextWithTrigger(ctx, AtTerminal)
	pasted := "a log line\n" + strings.Repeat("x y ", 25000)
	unclaimed := "a line no trigger claims, from a program"
	for _, p := range []struct {
		ctx  context.Context
		text string
	}{
		{byPerson, "write `u001 7` to answer.txt, one space, then a newline"},
		{byPerson, pasted},
		{ctx, unclaimed},
		{byPerson, "now sort it"},
		{byPerson, "and say done"},
	} {
		if _, err := s.Prompt(p.ctx, openresponses.UserText(p.text)); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	seen := userTexts(m.last)
	m.mu.Unlock()
	joined := strings.Join(seen, "\n\x00")
	for _, p := range []string{"write `u001 7` to answer.txt, one space, then a newline", "now sort it"} {
		if !strings.Contains(joined, p) {
			t.Errorf("the last request lost the prompt %q: it carries %q", p, seen)
		}
	}
	for _, s := range seen {
		if s == pasted {
			t.Error("a prompt past the bound was kept whole")
		}
		if s == unclaimed {
			t.Error("a message nobody claimed was kept as the person's")
		}
	}
	if data := string(projected(t, o, s)); !strings.Contains(data, `"pinned"`) {
		t.Error("no compaction entry names what it kept")
	}
}

// The task an autonomous run is given (Drive's, -p's prompt) claims no
// person, since a script may have given it, and a fold still keeps it
// verbatim: the run's exact instructions outlive any summary (#173).
func TestAFoldKeepsDrivesTaskVerbatim(t *testing.T) {
	ctx := context.Background()
	m := &seeing{}
	o := options(t, m)
	o.Compact = 1
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task := "write `u001 7` to answer.txt, one space, then a newline"
	if _, err := promptOn(ctx, s, task, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a line no trigger claims", "another", "and another"} {
		if _, err := s.Prompt(ctx, openresponses.UserText(p)); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	seen := userTexts(m.last)
	m.mu.Unlock()
	if !slices.Contains(seen, task) {
		t.Errorf("the last request lost Drive's task: it carries %q", seen)
	}
	if slices.Contains(seen, "a line no trigger claims") {
		t.Error("a message nobody claimed was kept")
	}
}

// A prompt template's message and an MCP prompt's are composed (their
// layers say so, and agentturn does not count them as the person's),
// but each is the request the person chose to send: a fold keeps it
// verbatim as it keeps Drive's task, whichever front sent it (#173).
func TestAFoldKeepsATemplatesAndAnMCPPromptsRequestVerbatim(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	ctx := context.Background()
	m := &seeing{}
	o := options(t, m)
	o.Compact = 1
	o.MCP = []MCPServer{{Name: "team", Command: os.Args[0]}}
	write(t, filepath.Join(o.UserDir, "prompts", "review.md"), "Review $ARGUMENTS for races; start with $1.\n")
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	byPerson := agentturn.ContextWithTrigger(ctx, AtTerminal)
	tmpl, err := s.Template("review", "internal/box tool")
	if err != nil {
		t.Fatal(err)
	}
	// Sent as the fronts send it, under the person's trigger, not
	// through Drive.
	if _, err := s.Prompt(byPerson, tmpl); err != nil {
		t.Fatal(err)
	}
	mp, err := s.MCPPrompt(ctx, "mcp__team__review_pr", "456 the error paths")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(byPerson, mp...); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a line no trigger claims", "another", "and another"} {
		if _, err := s.Prompt(ctx, openresponses.UserText(p)); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	seen := userTexts(m.last)
	m.mu.Unlock()
	joined := strings.Join(seen, "\n\x00")
	for _, want := range []string{"Review internal/box tool for races; start with internal/box.", "Review PR 456 for the error paths."} {
		if !strings.Contains(joined, want) {
			t.Errorf("the last request lost the request %q: it carries %q", want, seen)
		}
	}
}

// A record written before format 0.21 claims no typed line as the
// person's, and neither does -p's prompt in any record: a fold after a
// resume keeps such an unclaimed message verbatim, within agentturn's
// bound and total for the person's own, as the record allows a host to
// (agentsession 0.21: an unclaimed message MAY be taken as unclaimed
// input). The output of a ! command, which an older dax sent unclaimed
// too, is folded like the rest (#173).
func TestAFoldAfterAResumeKeepsUnclaimedPrompts(t *testing.T) {
	task := "write `u001 7` to answer.txt, one space, then a newline"
	command := userCommandLead + "echo from-a-command\nfrom-a-command\n[exit 0]"
	for _, tc := range []struct {
		name string
		// older rewrites the record as format 0.20 and imports it.
		older bool
		send  func(context.Context, *Session) error
	}{
		{"recorded before 0.21", true, func(ctx context.Context, s *Session) error {
			for _, p := range []string{task, command} {
				if _, err := s.Prompt(ctx, openresponses.UserText(p)); err != nil {
					return err
				}
			}
			return nil
		}},
		{"-p's prompt, resumed", false, func(ctx context.Context, s *Session) error {
			if _, err := promptOn(ctx, s, task, nil); err != nil {
				return err
			}
			_, err := s.Prompt(ctx, openresponses.UserText(command))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			o := options(t, &echo.Adapter{})
			s, err := New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			id := s.ID()
			if err := tc.send(ctx, s); err != nil {
				t.Fatal(err)
			}
			if tc.older {
				data := projected(t, o, s)
				older := strings.Replace(string(data), `"format":"agentsession/0.21"`, `"format":"agentsession/0.20"`, 1)
				if older == string(data) {
					t.Fatal("the record names no format 0.21")
				}
				file := filepath.Join(t.TempDir(), "older.jsonl")
				if err := os.WriteFile(file, []byte(older), 0o644); err != nil {
					t.Fatal(err)
				}
				o.Root = filepath.Join(t.TempDir(), "store")
				if id, err = Import(ctx, o.Root, file); err != nil {
					t.Fatal(err)
				}
			} else if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			m := &seeing{}
			o.Streamer = m
			// A budget the pasted log alone passes, with room for the
			// person's messages, as in the fold test above.
			o.Compact = 20000
			r, err := Resume(ctx, o, id)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			pasted := "a log line\n" + strings.Repeat("x y ", 25000)
			for _, p := range []string{pasted, "and say done"} {
				if _, err := r.Prompt(ctx, openresponses.UserText(p)); err != nil {
					t.Fatal(err)
				}
			}
			m.mu.Lock()
			seen := userTexts(m.last)
			m.mu.Unlock()
			if !slices.Contains(seen, task) {
				t.Errorf("the last request lost the earlier prompt: it carries %q", seen)
			}
			for _, s := range seen {
				if s == command {
					t.Error("a command's output was kept as a prompt")
				}
				if s == pasted {
					t.Error("a prompt past the bound was kept whole")
				}
			}
		})
	}
}
