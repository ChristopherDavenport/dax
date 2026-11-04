package agent

// This file is the limits on a session's spend: the turns of a run
// (MaxTurns, agentturn's own stop), and the tokens and cost of every
// model call the session makes, its sub-agents' among them (MaxTokens,
// MaxCost), metered where the calls go out.

import (
	"context"
	"fmt"
	"sync"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/openresponses"
)

// Price is what a model call cost in US dollars, from the model's name
// and the call's usage; false when the model has no price.
type Price func(model string, u openresponses.Usage) (float64, bool)

// meter sums the usage of every model call the session makes and says
// when a limit is reached. A sub-agent's calls are the run's spend, as
// agentsession counts a linked child's cost with its parent's.
type meter struct {
	maxTokens int
	maxCost   float64
	price     Price

	mu       sync.Mutex
	tokens   int
	cost     float64
	unpriced string // a model a call went to that has no price
}

// newMeter is nil when o sets no limit on tokens or cost.
func newMeter(o Options) (*meter, error) {
	if o.MaxTokens <= 0 && o.MaxCost <= 0 {
		return nil, nil
	}
	if o.MaxCost > 0 {
		if o.Price == nil {
			return nil, fmt.Errorf("a cost limit of $%.2f and no price for any model: give a pricing file", o.MaxCost)
		}
		for _, model := range append([]string{o.Model}, o.PricedModels...) {
			if model == "" {
				continue
			}
			if _, ok := o.Price(model, openresponses.Usage{}); !ok {
				return nil, fmt.Errorf("a cost limit of $%.2f and no price for model %s: add it to the pricing file", o.MaxCost, model)
			}
		}
	}
	return &meter{maxTokens: o.MaxTokens, maxCost: o.MaxCost, price: o.Price}, nil
}

// add counts one response to a request for model.
func (m *meter) add(model string, u *openresponses.Usage) {
	if u == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens += u.InputTokens + u.OutputTokens
	if m.maxCost > 0 {
		c, ok := m.price(model, *u)
		if !ok {
			m.unpriced = model
		}
		m.cost += c
	}
}

// reached is the error wrapping agentturn.ErrGuard that stops a run
// once a limit is reached, nil before.
func (m *meter) reached() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.maxTokens > 0 && m.tokens >= m.maxTokens:
		return fmt.Errorf("%w: token limit %d reached (%d used)", agentturn.ErrGuard, m.maxTokens, m.tokens)
	case m.maxCost > 0 && m.unpriced != "":
		return fmt.Errorf("%w: cost limit $%.2f, and model %s has no price", agentturn.ErrGuard, m.maxCost, m.unpriced)
	case m.maxCost > 0 && m.cost >= m.maxCost:
		return fmt.Errorf("%w: cost limit $%.2f reached ($%.4f spent)", agentturn.ErrGuard, m.maxCost, m.cost)
	}
	return nil
}

// beforeTurn stops the run, before the turn's model call, once a limit
// is reached.
func (m *meter) beforeTurn(context.Context, agentturn.TurnStartInfo) (openresponses.Items, error) {
	return nil, m.reached()
}

// wrap is s with every call it makes metered, and refused once a limit
// is reached: a sub-agent's run, which the main agent's guard does not
// see, ends there too. A server's compaction endpoint is kept.
func (m *meter) wrap(s openresponses.Streamer) openresponses.Streamer {
	if m == nil || s == nil {
		return s
	}
	ms := metered{inner: s, m: m}
	if c, ok := s.(compact.Compactor); ok {
		return meteredCompactor{metered: ms, Compactor: c}
	}
	return ms
}

type metered struct {
	inner openresponses.Streamer
	m     *meter
}

func (s metered) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if err := s.m.reached(); err != nil {
		return err
	}
	return s.inner.CreateStream(ctx, req, openresponses.EventSinkFunc(func(ev openresponses.StreamEvent) error {
		switch e := ev.(type) {
		case *openresponses.ResponseCompletedEvent:
			if e.Response != nil {
				s.m.add(req.Model, e.Response.Usage)
			}
		case *openresponses.ResponseIncompleteEvent:
			if e.Response != nil {
				s.m.add(req.Model, e.Response.Usage)
			}
		}
		return sink.Send(ev)
	}))
}

type meteredCompactor struct {
	metered
	compact.Compactor
}

// Compact is the server's compaction, metered and refused once a limit
// is reached, as a model call is.
func (s meteredCompactor) Compact(ctx context.Context, req openresponses.CompactRequest) (*openresponses.CompactResponse, error) {
	if err := s.m.reached(); err != nil {
		return nil, err
	}
	res, err := s.Compactor.Compact(ctx, req)
	if res != nil {
		s.m.add(req.Model, res.Usage)
	}
	return res, err
}

// seed counts what the session id already spent, as its record holds
// it, its sub-agents' sessions included, so a resumed session's limits
// go on from there. A record that cannot be read counts nothing more.
func (m *meter) seed(ctx context.Context, r agentsession.Reader, id, model string) {
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		rec, err := r.Read(ctx, id)
		if err != nil {
			return
		}
		for _, e := range rec.Entries() {
			switch e := e.(type) {
			case *agentsession.ResponseEntry:
				m.add(e.Model, e.Usage)
			case *agentsession.CompactionEntry:
				m.add(model, e.Usage)
			case *agentsession.LinkEntry:
				if e.Rel == agentsession.RelSubsession {
					walk(e.Session)
				}
			}
		}
	}
	walk(id)
}
