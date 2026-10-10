package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	under []any // ctxKey's value on each reading's context
	fail  error
}

// ctxKey marks a context, so a test can say which one a reading was
// made under.
type ctxKey struct{}

func (c *counting) Tools(context.Context) ([]Tool, error) {
	return []Tool{
		{Extension: "acme", Definition: openresponses.NewFunctionTool("claim", "Claims.", json.RawMessage(`{"type":"object"}`)), Factual: true},
		{Extension: "acme", Definition: openresponses.NewFunctionTool("plain", "Plain.", json.RawMessage(`{"type":"object"}`))},
	}, nil
}

func (c *counting) Facts(ctx context.Context, call Call) (agenttool.Facts, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = append(c.reads, call)
	c.under = append(c.under, ctx.Value(ctxKey{}))
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
// policy's subjects make it, under a context that carries no call.
func read(t *testing.T, s *Set, name, args string) (string, error) {
	t.Helper()
	return readIn(t, context.Background(), s, name, args)
}

// readIn is read under ctx, the context of the decision that reads.
func readIn(t *testing.T, ctx context.Context, s *Set, name, args string) (string, error) {
	t.Helper()
	b, ok := s.ByName(name)
	if !ok {
		t.Fatalf("no %s", name)
	}
	f, _, err := agenttool.FactsOf(ctx, b.Adapter, json.RawMessage(args))
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

// A reading of a call no decision has pinned, as the batch hold's of a
// sibling, is made under the reader's context, which agentpolicy makes
// the decision's; a pinned call is read under its pin's context, by
// whichever reader comes first.
func TestAReadingIsMadeUnderItsDecisionsContext(t *testing.T) {
	x := &counting{}
	s := bind(t, x)
	reader := context.WithValue(t.Context(), ctxKey{}, "reader")
	if _, err := readIn(t, reader, s, "claim", `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	pin := context.WithValue(t.Context(), ctxKey{}, "pin")
	defer s.Pin(pin, info("c1", "claim", `{"a":2}`))()
	if _, err := readIn(t, reader, s, "claim", `{"a":2}`); err != nil {
		t.Fatal(err)
	}
	if want := []any{"reader", "pin"}; len(x.under) != 2 || x.under[0] != want[0] || x.under[1] != want[1] {
		t.Errorf("read under %v, want %v", x.under, want)
	}
}

// batching is counting's tools behind an executor that reads a batch of
// calls in one request, which it records. A call's claim names the
// request that read it and its arguments, and one whose arguments are
// not stamped asks for the stamped rewrite {"stamped": args}. Every
// request fails with fail when it is set.
type batching struct {
	counting
	requests [][]Call
	fail     error
}

func (b *batching) BatchFacts(_ context.Context, calls []Call) ([]agenttool.Facts, []error, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, calls)
	if b.fail != nil {
		return nil, nil, b.fail
	}
	facts, errs := make([]agenttool.Facts, len(calls)), make([]error, len(calls))
	for i, c := range calls {
		facts[i].Calls = []agenttool.FactCall{{Text: fmt.Sprintf("%d %s", len(b.requests), c.Args)}}
		if !strings.Contains(string(c.Args), "stamped") {
			facts[i].Rewrite = json.RawMessage(`{"stamped":` + string(c.Args) + `}`)
		}
	}
	return facts, errs, nil
}

func (b *batching) sent() [][]Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.requests)
}

// response is the decisions about one model response's calls of the
// run run, each as the loop hands a hook one: the calls share one
// Batch slice.
func response(run string, calls ...[2]string) []agentturn.ToolCallInfo {
	batch := make([]*openresponses.FunctionCall, len(calls))
	for i, c := range calls {
		batch[i] = &openresponses.FunctionCall{CallID: fmt.Sprintf("%s-c%d", run, i), Name: c[0], Arguments: c[1]}
	}
	infos := make([]agentturn.ToolCallInfo, len(calls))
	for i, c := range batch {
		infos[i] = agentturn.ToolCallInfo{RunID: run, Turn: 1, Call: c, Args: json.RawMessage(c.Arguments), Batch: batch, Index: i}
	}
	return infos
}

// decide reads what a decision about info reads, under ctx: the call,
// its rewrite, and every sibling, as the engine's subjects, the rewrite
// hook, the engine's decision about the rewrite and the batch hold do.
// It returns the call's and the rewrite's readings.
func decide(t *testing.T, ctx context.Context, s *Set, info agentturn.ToolCallInfo) (own, rewrite string) {
	t.Helper()
	b, _ := s.ByName(info.Call.Name)
	if !b.Factual {
		return "", ""
	}
	f, _, err := agenttool.FactsOf(ctx, b.Adapter, info.Args)
	if err != nil {
		t.Fatalf("%s: %v", info.Call.CallID, err)
	}
	own = f.Calls[0].Text
	if f.Rewrite != nil {
		rf, _, err := agenttool.FactsOf(ctx, b.Adapter, f.Rewrite)
		if err != nil {
			t.Fatalf("%s's rewrite: %v", info.Call.CallID, err)
		}
		rewrite = rf.Calls[0].Text
	}
	for _, c := range info.Batch {
		if bc, ok := s.ByName(c.Name); ok && bc.Factual {
			if _, _, err := agenttool.FactsOf(ctx, bc.Adapter, json.RawMessage(c.Arguments)); err != nil {
				t.Fatalf("sibling %s: %v", c.CallID, err)
			}
		}
	}
	return own, rewrite
}

func TestAResponsesFactsAreOneBatch(t *testing.T) {
	calls := [][2]string{{"claim", `{"a":1}`}, {"plain", `{}`}, {"claim", `{"a":2}`}, {"claim", `{"a":1}`}}
	t.Run("one request for the calls and one for their rewrites", func(t *testing.T) {
		x := &batching{}
		s := bind(t, x)
		for _, info := range response("r1", calls...) {
			ctx, done := s.PinBatch(t.Context(), info)
			own, rewrite := decide(t, ctx, s, info)
			done()
			if info.Call.Name == "claim" && (own != "1 "+string(info.Args) || rewrite != `2 {"stamped":`+string(info.Args)+`}`) {
				t.Errorf("%s read %q and its rewrite %q, want the batch's two requests", info.Call.CallID, own, rewrite)
			}
		}
		sent := x.sent()
		if len(sent) != 2 {
			t.Fatalf("%d requests, want 2: %v", len(sent), sent)
		}
		var first, second []string
		for _, c := range sent[0] {
			first = append(first, c.ID+" "+string(c.Args))
		}
		for _, c := range sent[1] {
			second = append(second, string(c.Args))
		}
		if want := []string{`r1-c0 {"a":1}`, `r1-c2 {"a":2}`}; !slices.Equal(first, want) {
			t.Errorf("the first request read %v, want each claiming call once, %v", first, want)
		}
		if want := []string{`{"stamped":{"a":1}}`, `{"stamped":{"a":2}}`}; !slices.Equal(second, want) {
			t.Errorf("the second request read %v, want each rewrite once, %v", second, want)
		}
		if x.count() != 0 || len(s.batches) != 0 {
			t.Errorf("%d single readings, %d batches kept after the last decision", x.count(), len(s.batches))
		}
	})
	t.Run("an identical call outside the batch is read afresh", func(t *testing.T) {
		x := &batching{}
		s := bind(t, x)
		infos := response("r1", calls...)
		ctx, done := s.PinBatch(t.Context(), infos[0])
		defer done()
		decide(t, ctx, s, infos[0])
		for i := range 2 {
			if got, err := read(t, s, "claim", `{"a":1}`); err != nil || got != fmt.Sprint(i+1) {
				t.Errorf("outside the batch read %q, %v; want a reading of its own", got, err)
			}
		}
		// Another run's decision about the same call is not the batch's.
		other, odone := s.PinBatch(t.Context(), response("r2", calls[0])[0])
		defer odone()
		if got, _ := readIn(t, other, s, "claim", `{"a":1}`); got != `3 {"a":1}` {
			t.Errorf("another run's batch read %q, want its own request", got)
		}
	})
	t.Run("let go when the last call's decision returns", func(t *testing.T) {
		x := &batching{}
		s := bind(t, x)
		infos := response("r1", calls...)
		var ctxs []context.Context
		for i, info := range infos {
			ctx, done := s.PinBatch(t.Context(), info)
			ctxs = append(ctxs, ctx)
			decide(t, ctx, s, info)
			done()
			if kept := len(s.batches); i < len(infos)-1 && kept != 1 || i == len(infos)-1 && kept != 0 {
				t.Errorf("after decision %d, %d batches kept", i, kept)
			}
		}
		if _, err := readIn(t, ctxs[0], s, "claim", `{"a":1}`); !errors.Is(err, errReleased) {
			t.Errorf("a reading under a batch let go: %v, want it refused", err)
		}
	})
	t.Run("let go when the decisions' context ends", func(t *testing.T) {
		x := &batching{}
		s := bind(t, x)
		run, cancel := context.WithCancel(t.Context())
		ctx, done := s.PinBatch(run, response("r1", calls...)[0])
		defer done()
		decide(t, ctx, s, response("r1", calls...)[0])
		cancel()
		deadline := time.Now().Add(5 * time.Second)
		for {
			s.mu.Lock()
			n := len(s.batches)
			s.mu.Unlock()
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the batch outlived its context")
			}
			time.Sleep(time.Millisecond)
		}
	})
	t.Run("let go when the run's next batch starts", func(t *testing.T) {
		x := &batching{}
		s := bind(t, x)
		first := response("r1", calls...)
		ctx, done := s.PinBatch(t.Context(), first[0]) // a hook's error ended the response here
		decide(t, ctx, s, first[0])
		done()
		next := response("r1", calls[0])
		nctx, ndone := s.PinBatch(t.Context(), next[0])
		defer ndone()
		if _, err := readIn(t, ctx, s, "claim", `{"a":1}`); !errors.Is(err, errReleased) {
			t.Errorf("a reading under the earlier batch: %v, want it refused", err)
		}
		if got, _ := readIn(t, nctx, s, "claim", `{"a":1}`); got != `3 {"a":1}` {
			t.Errorf("the next batch read %q, want a request of its own", got)
		}
		if len(s.batches) != 1 {
			t.Errorf("%d batches kept, want the next one", len(s.batches))
		}
	})
	t.Run("let go on Release and ReleaseAll", func(t *testing.T) {
		x := &batching{}
		s := bind(t, x)
		infos := response("r1", calls...)
		ctx, done := s.PinBatch(t.Context(), infos[0])
		decide(t, ctx, s, infos[0])
		done()
		s.Release(ctx)
		if len(s.batches) != 0 {
			t.Fatal("Release kept the batch")
		}
		// The next call of the response is read in a batch of its own.
		ctx, done = s.PinBatch(t.Context(), infos[2])
		decide(t, ctx, s, infos[2])
		done()
		if n := len(x.sent()); n != 4 {
			t.Errorf("%d requests, want two for each batch (4)", n)
		}
		s.PinBatch(t.Context(), response("r2", calls...)[0])
		s.PinBatch(t.Context(), response("r3", calls...)[0])
		s.ReleaseAll()
		if len(s.batches) != 0 {
			t.Errorf("%d batches after ReleaseAll", len(s.batches))
		}
	})
	t.Run("a failed request blocks every call and keeps nothing", func(t *testing.T) {
		x := &batching{fail: errors.New("executor gone")}
		s := bind(t, x)
		infos := response("r1", calls...)
		for _, info := range infos {
			if info.Call.Name != "claim" {
				continue
			}
			ctx, done := s.PinBatch(t.Context(), info)
			if _, err := readIn(t, ctx, s, "claim", string(info.Args)); err == nil || !strings.Contains(err.Error(), "executor gone") {
				t.Errorf("%s read with %v, want the request's failure", info.Call.CallID, err)
			}
			done()
		}
		if n := len(x.sent()); n != 1 {
			t.Errorf("%d requests, want the one that failed", n)
		}
		if len(s.batches) != 0 || len(s.pins) != 0 {
			t.Errorf("%d batches and %d pins kept", len(s.batches), len(s.pins))
		}
		x.mu.Lock()
		x.fail = nil
		x.mu.Unlock()
		ctx, done := s.PinBatch(t.Context(), response("r1", calls...)[0])
		defer done()
		if got, err := readIn(t, ctx, s, "claim", `{"a":1}`); err != nil || got != `2 {"a":1}` {
			t.Errorf("the next batch read %q, %v; want a request of its own", got, err)
		}
	})
	t.Run("an executor that cannot batch reads each call once", func(t *testing.T) {
		x := &counting{}
		s := bind(t, x)
		for _, info := range response("r1", calls...) {
			ctx, done := s.PinBatch(t.Context(), info)
			decide(t, ctx, s, info)
			done()
		}
		if n := x.count(); n != 2 {
			t.Errorf("%d readings, want one for each claiming call (2)", n)
		}
	})
	t.Run("a call its batch does not hold is pinned", func(t *testing.T) {
		x := &batching{}
		s := bind(t, x)
		info := response("r1", calls...)[0]
		info.Args = json.RawMessage(`{"a":1,"approved":true}`) // a resumed call, approved rewritten
		ctx, done := s.PinBatch(t.Context(), info)
		first, _ := readIn(t, ctx, s, "claim", string(info.Args))
		again, _ := readIn(t, ctx, s, "claim", string(info.Args))
		done()
		if first != "1" || again != "1" || len(s.pins) != 0 {
			t.Errorf("read %q then %q, %d pins kept; want one pinned reading, let go", first, again, len(s.pins))
		}
	})
}
