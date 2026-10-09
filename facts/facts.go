// Package facts is a tool's claim of what a call would touch, said
// before the call runs and without acting: the calls it amounts to, in
// tool-call terms, and the arguments it would run with if the policy
// allows it. An executor answers it; a policy decides on it and never
// touches the machine the call acts on.
//
// It is dax's draft of an optional per-call claim for agenttool's tool
// contract, beside Confined and Replayable, and is shaped to move
// there: it imports agenttool and the standard library and nothing of
// any policy (docs/plans/proposals/execution-boundary.md). Until it
// does, a claim added here is lost on a tool agenttool.Wrap wraps, since
// Wrap forwards only the claims agenttool knows; Of looks through such
// wrappers to the tool that claims.
//
// The package is pre-1.0: its API may change in a minor version.
package facts

import (
	"context"
	"encoding/json"
	"io"

	"github.com/ChristopherDavenport/agenttool"
)

// Call is one call a call amounts to: what a policy's rules are asked
// about. Tool empty is the called tool itself; another name is that
// tool's call, as a shell redirect is a write of its target. A claim
// names only tools of its own extension: dax's policy decides a call
// that names another's as one no rule names, so it asks. Text is what
// a question about it shows.
type Call struct {
	Tool string
	Args json.RawMessage
	Text string
}

// Facts is what a call would touch.
type Facts struct {
	// Calls are the calls it amounts to. Nil is the call itself, one
	// call with its own arguments; a claim that adds nothing returns
	// nil. A Call that names a tool or arguments no rule can name (a
	// sentinel) is how a claim says "this part cannot be read", which a
	// policy answers by asking.
	Calls []Call
	// Rewrite, when set, is the arguments the call runs with if the
	// policy allows it: bash's arguments carrying the stamp of the plan
	// its analysis approved, or with a stamp the model forged taken off.
	// It is a fact about the call because only the tool can make it: the
	// stamp's key is the executor's and never leaves it.
	Rewrite json.RawMessage
}

// Claimer is implemented by a tool that can say what a call would
// touch. It acts on nothing. An error is a call nothing can be said
// about, which a policy treats as one it cannot read: it blocks or
// asks, never allows.
type Claimer interface {
	Facts(ctx context.Context, args json.RawMessage) (Facts, error)
}

// Of is what a call of t with args would touch, and whether t claims
// at all. A tool that makes no claim reports false, which a policy
// reads as the call being its own one fact, its arguments as given.
// Of looks through agenttool.Wrap's wrappers and this package's own.
func Of(ctx context.Context, t agenttool.Tool, args json.RawMessage) (Facts, bool, error) {
	for t != nil {
		if c, ok := t.(Claimer); ok {
			f, err := c.Facts(ctx, args)
			return f, true, err
		}
		t = agenttool.Unwrap(t)
	}
	return Facts{}, false, nil
}

// Claims reports whether t, or a tool it wraps, claims.
func Claims(t agenttool.Tool) bool {
	for t != nil {
		if _, ok := t.(Claimer); ok {
			return true
		}
		t = agenttool.Unwrap(t)
	}
	return false
}

// With returns t making the claim fn answers, and t in every other way:
// its name, description, parameters and execution, and every property
// agenttool reads from a tool (strict, sequential, resource,
// annotations, confined, replay, closer), reported as t reports them,
// as agenttool.Wrap does. Embedding the tool would forward its four
// methods alone and drop the rest: a bash that is sequential would run
// in a parallel batch.
func With(t agenttool.Tool, fn func(ctx context.Context, args json.RawMessage) (Facts, error)) agenttool.Tool {
	if t == nil || fn == nil {
		panic("facts.With: nil tool or function")
	}
	c := &claiming{Tool: t, facts: fn}
	if cl, ok := t.(io.Closer); ok {
		return &claimingCloser{claiming: c, closer: cl}
	}
	return c
}

type claiming struct {
	agenttool.Tool
	facts func(ctx context.Context, args json.RawMessage) (Facts, error)
}

func (c *claiming) Facts(ctx context.Context, args json.RawMessage) (Facts, error) {
	return c.facts(ctx, args)
}

func (c *claiming) Strict() bool                       { return agenttool.IsStrict(c.Tool) }
func (c *claiming) Sequential() bool                   { return agenttool.IsSequential(c.Tool) }
func (c *claiming) Annotations() agenttool.Annotations { return agenttool.AnnotationsOf(c.Tool) }

// Resource forwards what the tool declares, as agenttool.Wrap does, so
// the sequential rule reads the same on the claim as on the tool.
func (c *claiming) Resource() string {
	r, ok := c.Tool.(agenttool.Resource)
	if !ok {
		return ""
	}
	return r.Resource()
}

func (c *claiming) Confined(ctx context.Context, args json.RawMessage) (bool, string) {
	return agenttool.ConfinedBy(ctx, c.Tool, args)
}

func (c *claiming) Replay(ctx context.Context, args json.RawMessage) agenttool.Replay {
	return agenttool.ReplayOf(ctx, c.Tool, args)
}

type claimingCloser struct {
	*claiming
	closer io.Closer
}

func (c *claimingCloser) Close() error { return c.closer.Close() }

var (
	_ agenttool.Tool       = (*claiming)(nil)
	_ Claimer              = (*claiming)(nil)
	_ agenttool.Strict     = (*claiming)(nil)
	_ agenttool.Sequential = (*claiming)(nil)
	_ agenttool.Resource   = (*claiming)(nil)
	_ agenttool.Annotated  = (*claiming)(nil)
	_ agenttool.Confined   = (*claiming)(nil)
	_ agenttool.Replayable = (*claiming)(nil)
	_ io.Closer            = (*claimingCloser)(nil)
)
