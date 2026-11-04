package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

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

// A fold keeps every prompt the user typed verbatim, so the task's exact
// words outlive any summary of them; a prompt past the bound, a pasted
// log, is folded like the rest (#173).
func TestAFoldKeepsTheUsersPromptsVerbatim(t *testing.T) {
	ctx := context.Background()
	m := &seeing{}
	o := options(t, m)
	o.Compact = 1
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pasted := "a log line\n" + strings.Repeat("x", maxPinnedBytes)
	prompts := []string{"write `u001 7` to answer.txt, one space, then a newline", pasted, "now sort it", "and say done"}
	for _, p := range prompts {
		if _, err := promptOn(ctx, s, p, nil); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	seen := userTexts(m.last)
	m.mu.Unlock()
	for _, p := range []string{prompts[0], prompts[2], prompts[3]} {
		if !strings.Contains(strings.Join(seen, "\n\x00"), p) {
			t.Errorf("the last request lost the prompt %q: it carries %q", p, seen)
		}
	}
	for _, s := range seen {
		if s == pasted {
			t.Error("a prompt past the bound was pinned whole")
		}
	}
	if data := string(projected(t, o, s)); !strings.Contains(data, `"pinned"`) {
		t.Error("no compaction entry names what it pinned")
	}
}
