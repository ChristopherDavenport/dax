package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// Set is an executor's tools bound as agenttool tools: what the kit
// offers the model, the policy reads and the sub-agents are given. It
// keeps the facts each decision in flight read, so that every reading
// of one call's claim within a decision is the same reading (Pin).
type Set struct {
	x     Executor
	bound []Bound

	mu   sync.Mutex
	pins map[key]*entry
}

// Bound is one of a Set's tools: what the executor said of it, and the
// adapter that stands for it.
type Bound struct {
	Tool
	// Adapter is the tool the kit is given. Its definition, scheduling,
	// annotations and claims are the executor's for the tool, and
	// running it is Executor.Call. It is not an io.Closer: the executor
	// owns closing.
	Adapter agenttool.Tool
}

// key is a call as the facts cache knows it: the tool and the exact
// bytes of its arguments, not their canonical form, so two calls whose
// keys differ only in case are never one entry.
type key struct{ tool, args string }

// entry is the facts of one pinned call, read at most once, by the
// first reader, under the context of the pin that made it, and kept,
// an error included, while any pin of it lasts.
type entry struct {
	n    int
	ctx  context.Context
	call Call

	once  sync.Once
	facts agenttool.Facts
	err   error
}

// Bind binds x's tools.
func Bind(ctx context.Context, x Executor) (*Set, error) {
	tools, err := x.Tools(ctx)
	if err != nil {
		return nil, err
	}
	s := &Set{x: x, pins: map[key]*entry{}}
	for _, t := range tools {
		s.bound = append(s.bound, Bound{Tool: t, Adapter: s.adapter(t)})
	}
	return s, nil
}

// adapter is t as an agenttool tool whose calls the executor runs.
// agenttool.Definition of it is t.Definition, so a request carries the
// same bytes as the tool itself would give, and what agenttool reads of
// a tool (IsSequential, ResourceOf, AnnotationsOf, IsStrict, IsFactual,
// ReplayOf) is what it reads of the tool.
func (s *Set) adapter(t Tool) agenttool.Tool {
	name := t.Name()
	opts := []agenttool.Option{agenttool.WithAnnotations(t.Annotations)}
	if t.Sequential {
		opts = append(opts, agenttool.WithSequential())
	}
	if t.Resource != "" {
		opts = append(opts, agenttool.WithResource(t.Resource))
	}
	if t.Definition.Strict != nil && *t.Definition.Strict {
		opts = append(opts, agenttool.WithStrict())
	}
	if t.Factual {
		opts = append(opts, agenttool.WithFacts(func(ctx context.Context, args json.RawMessage) (agenttool.Facts, error) {
			return s.facts(ctx, name, args)
		}))
	}
	if t.Replayable {
		opts = append(opts, agenttool.WithReplay(func(ctx context.Context, args json.RawMessage) agenttool.Replay {
			return s.x.Replay(ctx, Call{Tool: name, Call: callOf(ctx, args)})
		}))
	}
	return agenttool.NewFunc(name, t.Definition.Description, t.Definition.Parameters,
		func(ctx context.Context, c agenttool.Call) (agenttool.Result, error) {
			return s.x.Call(ctx, Call{Tool: name, Call: c})
		}, opts...)
}

// callOf is the call a reading of args is for: the call ctx carries
// when it is one of these arguments (agentturn attaches it before it
// asks whether a call may run again), else one with these arguments
// alone.
func callOf(ctx context.Context, args json.RawMessage) agenttool.Call {
	c := agenttool.Call{Args: args}
	if from, ok := agenttool.CallFrom(ctx); ok && bytes.Equal(from.Args, args) {
		c.ID, c.IdempotencyKey = from.ID, from.IdempotencyKey
	}
	return c
}

// Tools are the bound tools, in the executor's order.
func (s *Set) Tools() []Bound { return append([]Bound(nil), s.bound...) }

// ByName is the bound tool named name, the first if two have it.
func (s *Set) ByName(name string) (Bound, bool) {
	for _, b := range s.bound {
		if b.Name() == name {
			return b, true
		}
	}
	return Bound{}, false
}

// facts is the claim for a call of the tool named name with args: the
// pinned reading while a decision about that call holds a pin, else a
// reading of its own, which is not kept.
func (s *Set) facts(ctx context.Context, name string, args json.RawMessage) (agenttool.Facts, error) {
	s.mu.Lock()
	e := s.pins[key{name, string(args)}]
	s.mu.Unlock()
	if e == nil {
		return s.x.Facts(ctx, Call{Tool: name, Call: callOf(ctx, args)})
	}
	e.once.Do(func() { e.facts, e.err = s.x.Facts(e.ctx, e.call) })
	return e.facts, e.err
}

// Pin holds the facts of info's call for one decision about it, until
// unpin: every reading of the call's claim in between, the policy's
// subjects, a folded hook's, the session's own, is one reading, made
// under ctx by the first of them. So the rewrite the call runs with
// comes from the very reading its verdict was decided on, however the
// claim would answer a second time. The arguments a rewrite gives are
// another call, read afresh.
//
// Two pins of one call share it, and it is read once for both; the
// entry goes with the last unpin, so the cache never holds more than
// the decisions in flight. A failed reading is kept with it and blocks
// the call every time it is read. A call of a tool that makes no claim
// pins nothing. unpin may be called more than once.
func (s *Set) Pin(ctx context.Context, info agentturn.ToolCallInfo) (unpin func()) {
	if info.Call == nil {
		return func() {}
	}
	name := info.Call.Name
	if b, ok := s.ByName(name); !ok || !b.Factual {
		return func() {}
	}
	k := key{name, string(info.Args)}
	s.mu.Lock()
	e := s.pins[k]
	if e == nil {
		e = &entry{ctx: ctx, call: Call{Tool: name, Call: agenttool.Call{ID: info.Call.CallID, Args: info.Args}}}
		s.pins[k] = e
	}
	e.n++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if e.n--; e.n == 0 && s.pins[k] == e {
				delete(s.pins, k)
			}
		})
	}
}
