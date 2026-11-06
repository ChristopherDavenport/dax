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

// A bash line's reads count as the file tools' paths do: a cat of a
// package's file gives the model that package's AGENTS.md (#170).
func TestABashReadBringsTheNestedAgentsMD(t *testing.T) {
	ctx := context.Background()
	m := &seen{scripted: scripted{calls: [][2]string{{"bash", `{"command":"cat svc/main.go"}`}}}}
	o := options(t, m)
	write(t, filepath.Join(o.Dir, "svc", "AGENTS.md"), "Never edit gen/.\n")
	write(t, filepath.Join(o.Dir, "svc", "main.go"), "package main\n")
	write(t, filepath.Join(o.Dir, "api", "AGENTS.md"), "Run make test-api.\n")
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := promptOn(ctx, s, "look at the service", func(*openresponses.FunctionCall, string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	reqs := slices2(m.reqs)
	m.mu.Unlock()
	if len(reqs) < 2 || !strings.Contains(reqs[len(reqs)-1], "Never edit gen/") || strings.Contains(reqs[len(reqs)-1], "make test-api") {
		t.Fatalf("requests:\n%s", strings.Join(reqs, "\n---\n"))
	}
}

// byAgent scripts the main agent and the explore sub-agent apart, and
// keeps the sub-agent's requests as text.
type byAgent struct {
	main, child scripted
	mu          sync.Mutex
	childReqs   []string
}

func (m *byAgent) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if !strings.HasPrefix(req.Instructions, "You are a read-only explorer") {
		return m.main.CreateStream(ctx, req, sink)
	}
	var b strings.Builder
	for _, it := range req.Input {
		if msg, ok := it.(*openresponses.Message); ok {
			b.WriteString(msg.Text() + "\n")
		}
	}
	m.mu.Lock()
	m.childReqs = append(m.childReqs, b.String())
	m.mu.Unlock()
	return m.child.CreateStream(ctx, req, sink)
}

// A sub-agent is given the AGENTS.md its own work reaches, as the main
// agent is (#170).
func TestASubagentIsGivenTheNestedAgentsMDItsWorkReaches(t *testing.T) {
	ctx := context.Background()
	m := &byAgent{
		main:  scripted{calls: [][2]string{{"explore", `{"input":"how does the handler work?"}`}}},
		child: scripted{calls: [][2]string{{"read", `{"path":"api/handler.go"}`}}},
	}
	o := withAgents(options(t, m), "")
	write(t, filepath.Join(o.Dir, "api", "AGENTS.md"), "Run make test-api, never go test ./... here.\n")
	write(t, filepath.Join(o.Dir, "api", "handler.go"), "package api\n")
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := promptOn(ctx, s, "explain the handler", nil); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	reqs := slices2(m.childReqs)
	m.mu.Unlock()
	if len(reqs) < 2 || strings.Contains(reqs[0], "make test-api") || !strings.Contains(reqs[1], "make test-api") {
		t.Fatalf("the sub-agent's requests:\n%s", strings.Join(reqs, "\n---\n"))
	}
}
