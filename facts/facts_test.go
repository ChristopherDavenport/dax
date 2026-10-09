package facts

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
)

func plain(name string, opts ...agenttool.Option) agenttool.Tool {
	return agenttool.NewFunc(name, "A tool.", nil, func(context.Context, agenttool.Call) (agenttool.Result, error) {
		return agenttool.Text("ran"), nil
	}, opts...)
}

func claim(calls ...Call) func(context.Context, json.RawMessage) (Facts, error) {
	return func(context.Context, json.RawMessage) (Facts, error) { return Facts{Calls: calls}, nil }
}

// A tool that makes no claim reports none: its call is its own fact.
func TestAToolWithoutAClaimClaimsNothing(t *testing.T) {
	f, ok, err := Of(context.Background(), plain("x"), json.RawMessage(`{}`))
	if ok || err != nil || f.Calls != nil || f.Rewrite != nil {
		t.Errorf("Of = %+v %v %v, want no claim", f, ok, err)
	}
	if Claims(plain("x")) {
		t.Error("Claims of a plain tool")
	}
}

// A claim survives agenttool.Wrap, which forwards only the claims
// agenttool knows: Of looks through the wrapper.
func TestAWrappedToolKeepsItsClaim(t *testing.T) {
	c := With(plain("x"), claim(Call{Tool: "read", Args: json.RawMessage(`{"path":"a"}`)}))
	w := agenttool.Wrap(c, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) { return c.Execute(ctx, call) })
	if _, ok := w.(Claimer); ok {
		t.Fatal("agenttool.Wrap forwarded a claim it does not know; Of's unwrapping is no longer needed")
	}
	f, ok, err := Of(context.Background(), w, json.RawMessage(`{}`))
	if !ok || err != nil || len(f.Calls) != 1 || f.Calls[0].Tool != "read" {
		t.Errorf("Of(wrapped) = %+v %v %v", f, ok, err)
	}
	if !Claims(w) {
		t.Error("Claims(wrapped) is false")
	}
}

// With is the tool in every way agenttool reads one: a sequential tool
// stays sequential, a closer stays a closer, and so on.
func TestWithForwardsEveryClaimAgenttoolReads(t *testing.T) {
	closed := false
	base := plain("x",
		agenttool.WithSequential(),
		agenttool.WithResource("shell:a"),
		agenttool.WithAnnotations(agenttool.Annotations{ReadOnly: true}),
		agenttool.WithConfined(func(context.Context, json.RawMessage) (bool, string) { return true, "box" }),
		agenttool.WithReplay(func(context.Context, json.RawMessage) agenttool.Replay { return agenttool.ReplaySafe }),
		agenttool.WithCloser(func() error { closed = true; return nil }),
	)
	c := With(base, claim())
	ctx := context.Background()
	if !agenttool.IsSequential(c) || agenttool.ResourceOf(c) != agenttool.ResourceOf(base) || !agenttool.AnnotationsOf(c).ReadOnly {
		t.Error("sequential, resource or annotations lost")
	}
	if ok, by := agenttool.ConfinedBy(ctx, c, nil); !ok || by != "box" {
		t.Error("confinement lost")
	}
	if agenttool.ReplayOf(ctx, c, nil) != agenttool.ReplaySafe {
		t.Error("replay lost")
	}
	if agenttool.IsStrict(c) != agenttool.IsStrict(base) {
		t.Error("strict changed")
	}
	cl, ok := c.(interface{ Close() error })
	if !ok {
		t.Fatal("a closer lost Close")
	}
	cl.Close()
	if !closed {
		t.Error("Close did not reach the tool")
	}
	if _, ok := With(plain("y"), claim()).(interface{ Close() error }); ok {
		t.Error("a claim on a tool that owns nothing is a closer")
	}
	if r, err := c.Execute(ctx, agenttool.Call{}); err != nil || agenttool.Text("ran").Output.Text != r.Output.Text {
		t.Errorf("Execute = %+v %v", r, err)
	}
}
