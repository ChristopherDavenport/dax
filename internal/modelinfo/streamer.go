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

// Streamer keeps what the Describer says about each model, asking once
// per model name and keeping the answer, a failure included, for the
// life of the Streamer, and fits efforts to it through [Streamer.Fit].
//
// It never changes a request. A session records the request the loop
// built, so a change made here, below the recorder, would make the
// record say something other than what was sent. The fitting is done
// where the request is built instead: the caller asks Fit for the
// effort of each configuration it makes. A request that still reaches
// the Streamer with an effort its model does not take, from a path
// that did not ask Fit, is sent as it is and reported once, so the
// path can be found.
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

// Fit is the effort to ask model for in place of want: want when the
// model takes it or nothing is known, else the nearest effort it takes,
// as [Info.Fit] decides. A change, and a vendor that could not be
// asked, are reported once each.
func (s *Streamer) Fit(ctx context.Context, model string, want openresponses.ReasoningEffort) openresponses.ReasoningEffort {
	if want == "" {
		return want
	}
	info, err := s.Describe(ctx, model)
	if err != nil && ctx.Err() == nil {
		s.tell(model+"\x00err", fmt.Sprintf("[model info for %s unavailable: %v; reasoning is asked for as configured]", model, err))
	}
	got := info.Fit(want)
	if got != want {
		s.tell(model+"\x00"+string(want), fmt.Sprintf("[%s: reasoning effort %s is not accepted; asking for %s (%s)]", model, want, got, info.Source))
	}
	return got
}

// CreateStream sends req as it is. An effort the model does not take
// means a path built the request without asking Fit; it is reported
// once, and the provider decides.
func (s *Streamer) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if want := req.Reasoning.Effort; want != "" {
		if info, err := s.Describe(ctx, req.Model); err == nil {
			if got := info.Fit(want); got != want {
				s.tell(req.Model+"\x00unfitted"+string(want), fmt.Sprintf("[%s: a request asks for reasoning effort %s, which %s says is not accepted; sent as asked]", req.Model, want, info.Source))
			}
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
