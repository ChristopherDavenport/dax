package executor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
)

// counting is an executor of one claiming tool, claim, whose facts
// reading it counts and answers with the reading's number, or fails
// with fail.
type counting struct {
	mu    sync.Mutex
	reads []Call
	fail  error
}

func (c *counting) Tools(context.Context) ([]Tool, error) {
	return []Tool{
		{Extension: "acme", Definition: openresponses.NewFunctionTool("claim", "Claims.", json.RawMessage(`{"type":"object"}`)), Factual: true},
		{Extension: "acme", Definition: openresponses.NewFunctionTool("plain", "Plain.", json.RawMessage(`{"type":"object"}`))},
	}, nil
}

func (c *counting) Facts(_ context.Context, call Call) (agenttool.Facts, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = append(c.reads, call)
	if c.fail != nil {
		return agenttool.Facts{}, c.fail
	}
	return agenttool.Facts{Calls: []agenttool.FactCall{{Text: string(rune('0' + len(c.reads)))}}}, nil
}

func (c *counting) Replay(context.Context, Call) agenttool.Replay { return agenttool.ReplayUnknown }
func (c *counting) Call(context.Context, Call) (agenttool.Result, error) {
	return agenttool.Text("ran"), nil
}
func (c *counting) Descriptor() workspace.Descriptor { return workspace.Descriptor{} }
func (c *counting) Close() error                     { return nil }

func (c *counting) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reads)
}

func bind(t *testing.T, x Executor) *Set {
	t.Helper()
	s, err := Bind(context.Background(), x)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// info is a call of name with args, as the loop hands a hook one.
func info(id, name, args string) agentturn.ToolCallInfo {
	return agentturn.ToolCallInfo{Call: &openresponses.FunctionCall{CallID: id, Name: name, Arguments: args}, Args: json.RawMessage(args)}
}

// read is one reading of the claim through the adapter, as the
// policy's subjects make it: with no context of the call's.
func read(t *testing.T, s *Set, name, args string) (string, error) {
	t.Helper()
	b, ok := s.ByName(name)
	if !ok {
		t.Fatalf("no %s", name)
	}
	f, _, err := agenttool.FactsOf(context.Background(), b.Adapter, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	return f.Calls[0].Text, nil
}

func TestPinnedFactsAreReadOncePerDecision(t *testing.T) {
	t.Run("two pins of a call share one reading", func(t *testing.T) {
		x := &counting{}
		s := bind(t, x)
		u1 := s.Pin(context.Background(), info("c1", "claim", `{"a":1}`))
		u2 := s.Pin(context.Background(), info("c2", "claim", `{"a":1}`))
		for range 3 {
			if got, err := read(t, s, "claim", `{"a":1}`); err != nil || got != "1" {
				t.Fatalf("read %q, %v; want the first reading", got, err)
			}
		}
		u1()
		if got, _ := read(t, s, "claim", `{"a":1}`); got != "1" {
			t.Errorf("after one unpin read %q, want the pinned reading", got)
		}
		u2()
		if n := x.count(); n != 1 {
			t.Errorf("%d readings, want 1", n)
		}
		if x.reads[0].ID != "c1" || string(x.reads[0].Args) != `{"a":1}` {
			t.Errorf("the reading was of %+v", x.reads[0])
		}
	})
	t.Run("a reading with no pin is not kept", func(t *testing.T) {
		x := &counting{}
		s := bind(t, x)
		read(t, s, "claim", `{"a":1}`)
		read(t, s, "claim", `{"a":1}`)
		// Pinned, other arguments are another call; case is not folded.
		defer s.Pin(context.Background(), info("c1", "claim", `{"a":1}`))()
		read(t, s, "claim", `{"A":1}`)
		read(t, s, "claim", `{"A":1}`)
		if n := x.count(); n != 4 {
			t.Errorf("%d readings, want 4", n)
		}
	})
	t.Run("the entry goes with the last unpin", func(t *testing.T) {
		x := &counting{}
		s := bind(t, x)
		u := s.Pin(context.Background(), info("c1", "claim", `{}`))
		read(t, s, "claim", `{}`)
		u()
		u() // a second unpin is a no-op
		if len(s.pins) != 0 {
			t.Fatalf("%d entries after the last unpin", len(s.pins))
		}
		if got, _ := read(t, s, "claim", `{}`); got != "2" {
			t.Errorf("read %q after the unpin, want a fresh reading", got)
		}
	})
	t.Run("an error is kept for the life of the pin", func(t *testing.T) {
		x := &counting{fail: errors.New("cannot read")}
		s := bind(t, x)
		u := s.Pin(context.Background(), info("c1", "claim", `{}`))
		for range 2 {
			if _, err := read(t, s, "claim", `{}`); err == nil {
				t.Fatal("the failed reading was not kept")
			}
		}
		x.mu.Lock()
		x.fail = nil
		x.mu.Unlock()
		if _, err := read(t, s, "claim", `{}`); err == nil {
			t.Error("a pinned call read again after its reading failed")
		}
		u()
		if n := x.count(); n != 1 {
			t.Errorf("%d readings while pinned, want 1", n)
		}
		if _, err := read(t, s, "claim", `{}`); err != nil {
			t.Errorf("after the unpin: %v", err)
		}
	})
	t.Run("a call of a tool with no claim pins nothing", func(t *testing.T) {
		x := &counting{}
		s := bind(t, x)
		defer s.Pin(context.Background(), info("c1", "plain", `{}`))()
		defer s.Pin(context.Background(), agentturn.ToolCallInfo{})()
		if len(s.pins) != 0 || x.count() != 0 {
			t.Errorf("%d entries, %d readings", len(s.pins), x.count())
		}
	})
	t.Run("a pin reads nothing until it is read", func(t *testing.T) {
		x := &counting{}
		s := bind(t, x)
		s.Pin(context.Background(), info("c1", "claim", `{}`))()
		if n := x.count(); n != 0 {
			t.Errorf("%d readings for a pin nobody read", n)
		}
	})
}
