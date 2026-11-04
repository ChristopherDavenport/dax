package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// thrashing reads go.mod on every call and never finishes, each
// response reporting perCall input tokens; a sub-agent's requests
// (dax's sub-agent prompt) are counted apart.
type thrashing struct {
	perCall     int
	calls, kids atomic.Int64
	// task, when set, has the main agent's first call start a task.
	task bool
}

func (m *thrashing) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	child := strings.Contains(req.Instructions, "You are a sub-agent of dax")
	if child {
		m.kids.Add(1)
	}
	n := m.calls.Add(1)
	resp := openresponses.NewResponse(req)
	resp.Usage = &openresponses.Usage{InputTokens: m.perCall, OutputTokens: 1, TotalTokens: m.perCall + 1}
	em := openresponses.NewEmitter(sink, resp)
	name, args := "read", `{"path":"go.mod"}`
	if m.task && !child && n == 1 {
		name, args = "task", `{"input":"read go.mod until told to stop"}`
	}
	call, err := em.FunctionCall("", name)
	if err != nil {
		return err
	}
	if err := call.Arguments(args); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return em.Complete()
}

// Each limit stops a run that would otherwise thrash, on dax's terms:
// stopped, with its cause on the record (#139).
func TestLimitsStopAThrashingRun(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		set   func(*Options)
		cause agentturn.StopCause
		calls int64
		err   string
	}{
		{name: "max turns", set: func(o *Options) { o.MaxTurns = 3 }, cause: agentturn.StopMaxTurns, calls: 3},
		{name: "max tokens", set: func(o *Options) { o.MaxTokens = 2500 }, cause: agentturn.StopGuard, calls: 3, err: "token limit"},
		{name: "max cost", set: func(o *Options) {
			o.MaxCost = 2.5
			o.Price = func(model string, u openresponses.Usage) (float64, bool) {
				return float64(u.InputTokens) / 1000, model == "echo"
			}
		}, cause: agentturn.StopGuard, calls: 3, err: "cost limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &thrashing{perCall: 1000}
			o := options(t, m)
			tc.set(&o)
			s, err := New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			end, err := promptOn(ctx, s, "go", nil)
			if err != nil {
				t.Fatal(err)
			}
			if end.Reason != agentturn.ReasonStopped || end.Cause != tc.cause {
				t.Fatalf("ended %s (%s): %v, want stopped by %s", end.Reason, end.Cause, end.Err, tc.cause)
			}
			if !errors.Is(end.Err, agentturn.ErrGuard) && tc.cause == agentturn.StopGuard || tc.err != "" && !strings.Contains(end.Err.Error(), tc.err) {
				t.Errorf("stopped with %v, want the guard's %q", end.Err, tc.err)
			}
			if got := m.calls.Load(); got != tc.calls {
				t.Errorf("%d model calls, want %d", got, tc.calls)
			}
		})
	}
}

// A sub-agent's spend is the run's: its calls count toward the limit,
// and once the limit is reached it makes none either.
func TestASubagentsCallsCountTowardTheLimit(t *testing.T) {
	ctx := context.Background()
	m := &thrashing{perCall: 1000, task: true}
	o := withAgents(options(t, m), "")
	o.MaxTokens = 4500
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	end, err := promptOn(ctx, s, "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonStopped || end.Cause != agentturn.StopGuard {
		t.Fatalf("ended %s (%s): %v, want stopped by the guard", end.Reason, end.Cause, end.Err)
	}
	// The main agent's one call, then the child's four reach 5000: its
	// fifth is refused, and the main agent's next turn stops.
	if kids, all := m.kids.Load(), m.calls.Load(); kids != 4 || all != 5 {
		t.Errorf("%d model calls, %d of them the child's; want 5 and 4", all, kids)
	}
}

// Under a cost limit a model without a price refuses to start: it would
// run unbounded.
func TestACostLimitNeedsAPrice(t *testing.T) {
	o := options(t, &thrashing{})
	o.MaxCost = 1
	o.Price = func(string, openresponses.Usage) (float64, bool) { return 0, false }
	if _, err := New(context.Background(), o); err == nil || !strings.Contains(err.Error(), "no price") {
		t.Errorf("New: %v, want a refusal for the unpriced model", err)
	}
	o.Price = nil
	if _, err := New(context.Background(), o); err == nil || !strings.Contains(err.Error(), "no price") {
		t.Errorf("New with no prices: %v, want a refusal", err)
	}
}
