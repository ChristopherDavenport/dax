package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// Set is an executor's tools bound as agenttool tools: what the kit
// offers the model, the policy reads and the sub-agents are given. It
// keeps the facts each model response's calls were read as while their
// decisions are in flight (PinBatch), and the facts of a decision of
// its own (Pin), so that every reading of one call's claim within its
// decision, and within its batch's, is the same reading.
type Set struct {
	x     Executor
	bound []Bound

	mu      sync.Mutex
	pins    map[key]*entry
	batches map[string]*batch // the batch in flight of each run, by run ID
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
	s := &Set{x: x, pins: map[key]*entry{}, batches: map[string]*batch{}}
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

// facts is the claim for a call of the tool named name with args: its
// batch's reading when ctx is a decision's that PinBatch marked (and
// the batch's error, which fails every reading under it, when its
// requests failed), else the pinned reading while a decision about that
// call holds a pin, else a reading of its own under ctx, which is not
// kept. The policy's subjects read under the context of the decision
// that asks (agentpolicy passes it), so a sibling the batch hold reads
// is its batch's reading, and a call no batch or pin covers is read
// under the asking decision's context.
func (s *Set) facts(ctx context.Context, name string, args json.RawMessage) (agenttool.Facts, error) {
	k := key{name, string(args)}
	if b, ok := ctx.Value(batchKey{}).(*batch); ok && b.set == s {
		if r, ok := b.lookup(k); ok {
			return r.facts, r.err
		}
	}
	s.mu.Lock()
	e := s.pins[k]
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

// batchKey is the context key under which PinBatch marks a decision's
// context with its batch.
type batchKey struct{}

// errReleased is the reading of a batch let go while a decision of it
// still read: nothing about the call is known any longer, so it fails.
var errReleased = errors.New("executor: the facts of this call's batch were let go before it was decided")

// batch is the facts of one model response's calls (agentturn's
// ToolCallInfo.Batch), read for the decisions about them: every
// claiming call of the response in one request, then every rewrite
// those claims ask for in a second, each rewrite read as the call of
// its own that the engine decides it as. A reading is the batch's only
// under a context PinBatch marked with it, so no other decision, nor a
// call made after the batch, ever sees what it read.
type batch struct {
	set   *Set
	run   string
	turn  int
	first **openresponses.FunctionCall // &Batch[0], which the response's calls share
	n     int
	ctx   context.Context // the first decision's, which the requests are made under
	calls []Call          // the response's claiming calls, each key once
	keys  map[key]bool    // the keys of calls
	stop  func() bool     // stops the release on the end of ctx

	once sync.Once

	mu       sync.Mutex
	entries  map[key]reading // once read: the calls and their rewrites
	err      error           // once read: the requests' error
	released bool
}

// reading is one call's claim as its batch read it.
type reading struct {
	facts agenttool.Facts
	err   error
}

// of reports whether info is a decision about one of b's calls.
func (b *batch) of(info agentturn.ToolCallInfo) bool {
	return b.run == info.RunID && b.turn == info.Turn && len(info.Batch) == b.n && b.first == &info.Batch[0]
}

// lookup is b's reading of k, made on the first lookup, and whether b
// answers for k: a key b did not read is not its to answer, unless b's
// requests failed or b was let go, which fail every reading under it.
func (b *batch) lookup(k key) (reading, bool) {
	b.once.Do(b.read)
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.released:
		return reading{err: errReleased}, true
	case b.err != nil:
		return reading{err: b.err}, true
	}
	r, ok := b.entries[k]
	return r, ok
}

// read makes b's two requests under b's context: the calls, then the
// rewrites their claims ask for, a rewrite that is itself one of the
// calls read once. Either request failing fails the batch, and nothing
// it read is kept.
func (b *batch) read() {
	b.mu.Lock()
	gone := b.released
	b.mu.Unlock()
	if gone {
		return
	}
	entries := make(map[key]reading, 2*len(b.calls))
	facts, errs, err := b.set.readAll(b.ctx, b.calls)
	var rewrites []Call
	if err == nil {
		for i, c := range b.calls {
			entries[key{c.Tool, string(c.Args)}] = reading{facts[i], errs[i]}
		}
		asked := map[key]bool{}
		for i, c := range b.calls {
			rk := key{c.Tool, string(facts[i].Rewrite)}
			if errs[i] != nil || facts[i].Rewrite == nil || b.keys[rk] || asked[rk] {
				continue
			}
			asked[rk] = true
			rewrites = append(rewrites, Call{Tool: c.Tool, Call: agenttool.Call{ID: c.ID, Args: facts[i].Rewrite}})
		}
	}
	if err == nil && len(rewrites) > 0 {
		facts, errs, err = b.set.readAll(b.ctx, rewrites)
		if err == nil {
			for i, c := range rewrites {
				entries[key{c.Tool, string(c.Args)}] = reading{facts[i], errs[i]}
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.released {
		return
	}
	if err != nil {
		b.err = err
		return
	}
	b.entries = entries
}

// readAll is the claims of calls in one request when the executor is a
// Batcher, and one by one otherwise.
func (s *Set) readAll(ctx context.Context, calls []Call) ([]agenttool.Facts, []error, error) {
	if bx, ok := s.x.(Batcher); ok {
		facts, errs, err := bx.BatchFacts(ctx, calls)
		if err == nil && (len(facts) != len(calls) || len(errs) != len(calls)) {
			err = fmt.Errorf("executor: %d claims and %d errors for %d calls", len(facts), len(errs), len(calls))
		}
		return facts, errs, err
	}
	facts, errs := make([]agenttool.Facts, len(calls)), make([]error, len(calls))
	for i, c := range calls {
		facts[i], errs[i] = s.x.Facts(ctx, c)
	}
	return facts, errs, nil
}

// PinBatch holds the facts of info's model response for the decisions
// about its calls, and returns ctx marked with them and done, which the
// decision calls when it returns. Every reading made under the marked
// context, the policy's subjects, a folded hook's, the engine's reading
// of a hook's rewrite and the batch hold's of a sibling, is the batch's:
// on the first of them, every claiming call of the response is read in
// one request and every rewrite those claims ask for in a second, under
// the first decision's context. So a call's verdict, the stamp it runs
// with, the decision about that stamp and each sibling's reading are
// one reading, however the claims would answer a second time, and a
// response of n claiming calls costs two requests, not one or more per
// reading. A rewrite is read as the call it is, never assumed to claim
// what the call it came from claimed.
//
// A batch is the run's, its turn's and its Batch slice's (agentturn
// hands every call of a response the same one; a nested Invoke is a
// batch of its own). Only a context marked with it reads it: an
// identical call decided outside it is read afresh. It is let go when
// the decision about its last call returns, when the first decision's
// context ends (the run ending), when a newer batch of the run starts
// (agentturn stops a response's decisions at a hook's error, so its
// last may never come), on Release and on ReleaseAll; a reading under
// it after that fails. A failed request fails every reading under the
// batch, so each of its calls is blocked, and nothing it read is kept.
//
// A call info's batch does not hold under its own arguments (a resumed
// call approved with arguments the response did not carry), or one with
// no batch, is pinned as Pin pins it.
func (s *Set) PinBatch(ctx context.Context, info agentturn.ToolCallInfo) (context.Context, func()) {
	if info.Call == nil || len(info.Batch) == 0 {
		return ctx, s.Pin(ctx, info)
	}
	s.mu.Lock()
	b := s.batches[info.RunID]
	if b != nil && !b.of(info) {
		s.releaseLocked(b)
		b = nil
	}
	if b == nil {
		b = s.newBatch(ctx, info)
		s.batches[info.RunID] = b
		b.stop = context.AfterFunc(ctx, func() { s.release(b) })
	}
	s.mu.Unlock()
	unpin := func() {}
	if !b.keys[key{info.Call.Name, string(info.Args)}] {
		unpin = s.Pin(ctx, info)
	}
	last := info.Index >= b.n-1
	var once sync.Once
	return context.WithValue(ctx, batchKey{}, b), func() {
		once.Do(func() {
			unpin()
			if last {
				s.release(b)
			}
		})
	}
}

// newBatch is the batch of info's response, its claiming calls with the
// arguments agentpolicy reads a sibling with, unread.
func (s *Set) newBatch(ctx context.Context, info agentturn.ToolCallInfo) *batch {
	b := &batch{set: s, run: info.RunID, turn: info.Turn, first: &info.Batch[0], n: len(info.Batch), ctx: ctx, keys: map[key]bool{}}
	for _, c := range info.Batch {
		if c == nil {
			continue
		}
		if t, ok := s.ByName(c.Name); !ok || !t.Factual {
			continue
		}
		args := json.RawMessage(c.Arguments)
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		k := key{c.Name, string(args)}
		if b.keys[k] {
			continue
		}
		b.keys[k] = true
		b.calls = append(b.calls, Call{Tool: c.Name, Call: agenttool.Call{ID: c.CallID, Args: args}})
	}
	return b
}

// Release lets go of the batch ctx is marked with, at once: a sub-agent's
// decision that is about to ask a person, so that a slow answer holds no
// reading open; the next call of the response is read in a batch of its
// own.
func (s *Set) Release(ctx context.Context) {
	if b, ok := ctx.Value(batchKey{}).(*batch); ok && b.set == s {
		s.release(b)
	}
}

// ReleaseAll lets go of every batch, as the session closes.
func (s *Set) ReleaseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.batches {
		s.releaseLocked(b)
	}
}

func (s *Set) release(b *batch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked(b)
}

// releaseLocked lets go of b, with s.mu held.
func (s *Set) releaseLocked(b *batch) {
	if s.batches[b.run] == b {
		delete(s.batches, b.run)
	}
	if b.stop != nil {
		b.stop()
	}
	b.mu.Lock()
	b.released, b.entries, b.err = true, nil, nil
	b.mu.Unlock()
}
