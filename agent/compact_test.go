package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// folds is a model that answers "done", keeping the instructions of
// each summary request a fold sends.
type folds struct {
	scripted
	mu   sync.Mutex
	asks []string
}

func (m *folds) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if compact.IsFold(ctx) {
		var b strings.Builder
		b.WriteString(req.Instructions)
		for _, it := range req.Input {
			if msg, ok := it.(*openresponses.Message); ok {
				b.WriteString("\n" + msg.Text())
			}
		}
		m.mu.Lock()
		m.asks = append(m.asks, b.String())
		m.mu.Unlock()
	}
	return m.scripted.CreateStream(ctx, req, sink)
}

// Compact folds at the next request whatever the budget, with the
// instructions given for that fold alone, and the fold is noted and
// recorded as any other (#165).
func TestCompactFoldsTheNextRequestWithItsInstructions(t *testing.T) {
	ctx := context.Background()
	model := &folds{}
	o := options(t, model)
	// Under budget throughout, with an older part over the eighth of
	// the budget a fold needs (compact.WithMinFold's default).
	o.Compact = 20000
	var notes []string
	o.Log = func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"one ", "two ", "three ", "four ", "five "} {
		if _, err := promptOn(ctx, s, strings.Repeat(p, 1000/len(p)*4), nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(model.asks) != 0 {
		t.Fatalf("folded %d time(s) under budget", len(model.asks))
	}
	if err := s.Compact(ctx, "keep the word PELICAN"); err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "six", nil); err != nil {
		t.Fatal(err)
	}
	if len(model.asks) != 1 || !strings.Contains(model.asks[0], "keep the word PELICAN") {
		t.Fatalf("%d summary request(s), want one carrying the instructions", len(model.asks))
	}
	if !strings.Contains(strings.Join(notes, "\n"), "[compacted on request:") {
		t.Errorf("notes %q, want the fold asked for", notes)
	}
	if data := string(projected(t, o, s)); strings.Count(data, `"type":"compaction"`) != 1 {
		t.Errorf("the fold is not recorded once")
	}
}

// With compaction off there is nothing to fold with, which Compact
// says rather than doing nothing.
func TestCompactWithoutCompactionIsRefused(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Compact(ctx, ""); !errors.Is(err, agentkit.ErrNoCompaction) || !strings.Contains(err.Error(), "-compact") {
		t.Errorf("Compact = %v, want compaction off named", err)
	}
}
