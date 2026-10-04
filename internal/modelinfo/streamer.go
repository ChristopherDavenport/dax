package modelinfo

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// describeTimeout bounds one call to a vendor's model endpoint, so a
// slow catalogue delays a first request by at most this long.
const describeTimeout = 10 * time.Second

// Streamer fits each request's reasoning effort to its model before
// passing it on. It asks the Describer once per model name, the first
// time that name is sent or Describe is called, and keeps the answer,
// a failure included, for the life of the Streamer. A change it makes
// is reported through Notify once per model and effort.
type Streamer struct {
	inner    openresponses.Streamer
	describe Describer
	notify   func(string)

	mu   sync.Mutex
	seen map[string]result
	told map[string]bool
}

type result struct {
	info Info
	err  error
}

// Wrap returns s fitting requests to what d says. A nil d describes
// nothing and every request goes out as it was asked. notify may be
// nil. The result implements Compact exactly when s does, so a caller
// that asks whether the model compacts gets the same answer.
func Wrap(s openresponses.Streamer, d Describer, notify func(string)) openresponses.Streamer {
	w := &Streamer{inner: s, describe: d, notify: notify, seen: map[string]result{}, told: map[string]bool{}}
	if c, ok := s.(compactor); ok {
		return &compactingStreamer{Streamer: w, c: c}
	}
	return w
}

type compactor interface {
	Compact(context.Context, openresponses.CompactRequest) (*openresponses.CompactResponse, error)
}

type compactingStreamer struct {
	*Streamer
	c compactor
}

func (s *compactingStreamer) Compact(ctx context.Context, req openresponses.CompactRequest) (*openresponses.CompactResponse, error) {
	return s.c.Compact(ctx, req)
}

// Of returns the Streamer s is or wraps, nil if s did not come from
// Wrap.
func Of(s openresponses.Streamer) *Streamer {
	switch w := s.(type) {
	case *Streamer:
		return w
	case *compactingStreamer:
		return w.Streamer
	}
	return nil
}

// Describe returns what is known about model, asking the vendor the
// first time. A failure is kept like an answer, so a vendor that is
// down is asked once, not on every request; a cancelled ctx is not
// kept.
func (s *Streamer) Describe(ctx context.Context, model string) (Info, error) {
	if s.describe == nil {
		return Info{Model: model}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.seen[model]; ok {
		return r.info, r.err
	}
	dctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	info, err := s.describe.Describe(dctx, model)
	info.Model = model
	if err != nil && ctx.Err() != nil {
		return Info{Model: model}, err
	}
	if err != nil {
		info = Info{Model: model}
	}
	s.seen[model] = result{info, err}
	return info, err
}

// CreateStream sends req with its reasoning effort fitted to its model.
func (s *Streamer) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if want := req.Reasoning.Effort; want != "" {
		info, err := s.Describe(ctx, req.Model)
		if err != nil && ctx.Err() == nil {
			s.tell(req.Model+"\x00err", fmt.Sprintf("[model info for %s unavailable: %v; reasoning is sent as asked]", req.Model, err))
		}
		if got := info.Fit(want); got != want {
			req.Reasoning.Effort = got
			if got == openresponses.ReasoningEffortNone {
				req.Reasoning.Summary = ""
			}
			s.tell(req.Model+"\x00"+string(want), fmt.Sprintf("[%s: reasoning effort %s is not accepted; sent %s (%s)]", req.Model, want, got, info.Source))
		}
	}
	return s.inner.CreateStream(ctx, req, sink)
}

// tell reports msg once per key.
func (s *Streamer) tell(key, msg string) {
	if s.notify == nil {
		return
	}
	s.mu.Lock()
	first := !s.told[key]
	s.told[key] = true
	s.mu.Unlock()
	if first {
		s.notify(msg)
	}
}
