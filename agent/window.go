package agent

// The context a session's requests are shaped under follows its model
// (#207): every switch (/model, plan mode's plan_model and act_model,
// an attached view's control, all SetModel) asks for the new model's
// context window, and a compaction budget sized from the window is
// sized again from the new one, while a budget the person set
// (-compact) stays. The header records the context the session started
// with; each change after, and a resume under other settings, is a
// custom entry holding the new value under the same member name
// (agentsession RFC 0001, Header). Each fold records the budget and
// the model it was made under itself (agentturn's fold member), so
// nothing here records those.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"

	"github.com/ChristopherDavenport/dax/internal/modelinfo"
)

// contextNS is the namespace of the custom entry that records a
// change of the session's context: the header's "dax.context" slug.
const contextNS = "dax"

// sizes is the part of the session's context that follows its model:
// the compaction budget and where it came from, and the window and
// where that came from, as Options names them.
type sizes struct {
	Compact     int
	CompactFrom string
	Window      int64
	WindowFrom  string
}

// sizing is the context in force, which SetModel changes.
type sizing struct {
	mu  sync.Mutex
	now sizes
}

// sizesOf is the context o opens a session with.
func sizesOf(o Options) sizes {
	return sizes{Compact: o.Compact, CompactFrom: o.CompactFrom, Window: o.Window, WindowFrom: o.WindowFrom}
}

// withSizes is o under the context z.
func withSizes(o Options, z sizes) Options {
	o.Compact, o.CompactFrom, o.Window, o.WindowFrom = z.Compact, z.CompactFrom, z.Window, z.WindowFrom
	return o
}

// sizes is the context in force.
func (s *Session) sizes() sizes {
	s.sizing.mu.Lock()
	defer s.sizing.mu.Unlock()
	return s.sizing.now
}

// compactsByWindow says whether a session under o builds its
// compaction although its budget is 0: the budget is the window's,
// which a switch to a model whose window is known sizes.
func compactsByWindow(o Options) bool { return o.CompactFrom == "window" && o.WindowOf != nil }

// resize sizes the context for model, which the session has just
// switched to: the window Options.WindowOf gives it, and a budget from
// the window sized from the new one, applied to the kit's compaction
// from the next request; a budget the person set is kept. A change is
// said and recorded.
func (s *Session) resize(ctx context.Context, model string) {
	if s.opts.WindowOf == nil {
		return
	}
	window, from := s.opts.WindowOf(ctx, model)
	s.sizing.mu.Lock()
	was := s.sizing.now
	now := was
	now.Window, now.WindowFrom = window, from
	if was.CompactFrom == "window" {
		now.Compact = modelinfo.Info{ContextWindow: window}.CompactBudget()
	}
	if now == was {
		s.sizing.mu.Unlock()
		return
	}
	s.sizing.now = now
	s.sizing.mu.Unlock()
	if now.Compact != was.Compact {
		switch err := s.Kit.SetCompactionBudget(now.Compact); {
		case errors.Is(err, agentkit.ErrNoCompaction):
		case err != nil:
			fmt.Fprintf(warnings{}, "dax: compaction budget for %s: %v\n", model, err)
		case now.Compact > 0:
			s.opts.log("[compaction budget %d tokens from the next request: three quarters of %s's %d-token context window]", now.Compact, model, now.Window)
		default:
			s.opts.log("[no compaction budget from the next request: %s's context window is unknown; /compact still folds]", model)
		}
	}
	s.recordContext(ctx, now)
}

// contextData is the custom entry's data for the context z: the
// header's members of it under their own names.
func (s *Session) contextData(z sizes) map[string]any {
	o := withSizes(s.opts, z)
	from := o.CompactFrom
	if o.Compact <= 0 {
		from = "off"
	}
	return map[string]any{
		headerContext:     contextOf(o),
		headerContextFrom: recordedContextFrom{BudgetFrom: from, WindowFrom: o.WindowFrom},
	}
}

// recordContext writes z, the context in force from now, into the
// session's record; nothing for a session not recorded.
func (s *Session) recordContext(ctx context.Context, z sizes) {
	rec := s.Kit.Recorder()
	if rec == nil {
		return
	}
	if _, err := rec.Annotate(context.WithoutCancel(ctx), contextNS, s.contextData(z)); err != nil {
		fmt.Fprintf(warnings{}, "dax: recording the context: %v\n", err)
	}
}

// lastContext is the context the path to sess's leaf last recorded:
// the header's, or the last custom entry's after it; false when
// neither holds one.
func lastContext(sess *agentsession.Session) (recordedContext, bool) {
	var c recordedContext
	raw, ok := member(sess.Header(), headerContext)
	if ok && json.Unmarshal(raw, &c) != nil {
		ok = false
	}
	for _, e := range sess.Path(sess.Leaf()) {
		ce, isCustom := e.(*agentsession.CustomEntry)
		if !isCustom || ce.NS != contextNS {
			continue
		}
		var data map[string]json.RawMessage
		if json.Unmarshal(ce.Data, &data) != nil {
			continue
		}
		var next recordedContext
		if raw, has := data[headerContext]; has && json.Unmarshal(raw, &next) == nil {
			c, ok = next, true
		}
	}
	return c, ok
}

// resumedContext records the context the resumed session goes on
// under when it is not the one its path last recorded.
func (s *Session) resumedContext(ctx context.Context) {
	sess := s.Kit.Session()
	if sess == nil {
		return
	}
	now := s.sizes()
	if was, ok := lastContext(sess); ok && was == contextOf(withSizes(s.opts, now)) {
		return
	}
	s.recordContext(ctx, now)
}
