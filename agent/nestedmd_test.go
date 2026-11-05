package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// seen is scripted, keeping each request's input as text.
type seen struct {
	scripted
	mu   sync.Mutex
	reqs []string
}

func (m *seen) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	var b strings.Builder
	for _, it := range req.Input {
		if msg, ok := it.(*openresponses.Message); ok {
			b.WriteString(msg.Text() + "\n")
		}
	}
	m.mu.Lock()
	m.reqs = append(m.reqs, b.String())
	m.mu.Unlock()
	return m.scripted.CreateStream(ctx, req, sink)
}

// A package's AGENTS.md below where the session started reaches the
// model the turn after a tool touches its directory, once, recorded as
// composed from that file; a directory nothing touched adds nothing
// (#170).
func TestANestedAgentsMDArrivesWhenTheWorkReachesIt(t *testing.T) {
	ctx := context.Background()
	m := &seen{scripted: scripted{calls: [][2]string{{"read", `{"path":"api/handler.go"}`}}}}
	o := options(t, m)
	write(t, filepath.Join(o.Dir, "api", "AGENTS.md"), "Run make test-api, never go test ./... here.\n")
	write(t, filepath.Join(o.Dir, "api", "handler.go"), "package api\n")
	write(t, filepath.Join(o.Dir, "svc", "AGENTS.md"), "Never edit gen/.\n")
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "fix the handler", nil); err != nil {
		t.Fatal(err)
	}
	m.scripted.calls = nil
	if _, err := promptOn(ctx, s, "and again", nil); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	reqs := slices2(m.reqs)
	m.mu.Unlock()
	if len(reqs) < 3 || strings.Contains(reqs[0], "make test-api") || !strings.Contains(reqs[1], "make test-api") {
		t.Fatalf("requests:\n%s", strings.Join(reqs, "\n---\n"))
	}
	last := reqs[len(reqs)-1]
	if strings.Count(last, "make test-api") != 1 || strings.Contains(last, "Never edit gen/") {
		t.Errorf("the last request has the api file %d time(s), svc's: %v", strings.Count(last, "make test-api"), strings.Contains(last, "Never edit gen/"))
	}
	if data := string(projected(t, o, s)); !strings.Contains(data, `"instructions_part":{"id":"api/AGENTS.md","source":"agentsmd"}`) {
		t.Error("the record does not hold the file as the message's instructions part")
	}
}

func slices2(s []string) []string { return append([]string(nil), s...) }
