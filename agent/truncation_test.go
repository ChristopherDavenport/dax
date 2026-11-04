package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// shrinking answers each request with fewer input tokens than the one
// before, as a server that drops the oldest messages past its window
// does, though the conversation grows.
type shrinking struct {
	mu sync.Mutex
	in int
}

func (m *shrinking) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	if m.in == 0 {
		m.in = 1000
	} else {
		m.in -= 100
	}
	in := m.in
	m.mu.Unlock()
	resp := openresponses.NewResponse(req)
	resp.Usage = &openresponses.Usage{InputTokens: in, OutputTokens: 1, TotalTokens: in + 1}
	em := openresponses.NewEmitter(sink, resp)
	msg, err := em.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if err := msg.Text("ok"); err != nil {
		return err
	}
	if err := msg.Close(); err != nil {
		return err
	}
	return em.Complete()
}

// A server that counts fewer input tokens than the call before while
// the conversation grew is dropping what it cannot fit, which is the one
// sign of it the client sees: the session says so, once (#171).
func TestTheSessionWarnsWhenTheServerSeemsToTruncate(t *testing.T) {
	ctx := context.Background()
	o := options(t, &shrinking{})
	var mu sync.Mutex
	var notes []string
	o.Log = func(format string, args ...any) {
		mu.Lock()
		notes = append(notes, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range []string{"one", "two", "three"} {
		if _, err := promptOn(ctx, s, p, nil); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, l := range notes {
		if strings.Contains(l, "fewer input tokens") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("warned %d time(s), want once: %q", n, notes)
	}
}
