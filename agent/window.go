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
	// switching is held through a switch's whole re-size (resize), mu
	// while a field is read or written.
	switching sync.Mutex
	mu        sync.Mutex
	now       sizes
	// callers is the budget the A2A callers' model needs, once
	// PeerConfig handed their configuration out: they stay on the model
	// the session ran then, and share the kit's one compaction with the
	// person (servingCallers). nil while no caller is served.
	callers *int
	// noEndpoint is set when -compact-server found no compaction
	// endpoint, so no budget is sized; said once a switch finds a
	// window (noCompaction).
	noEndpoint, said bool
}

// noCompaction notes that the session's compaction, sized by the
// window, could not be built (-compact-server on a provider without
// the endpoint): its budget stays 0, recorded as off.
func (s *Session) noCompaction() {
	s.sizing.mu.Lock()
	defer s.sizing.mu.Unlock()
	s.sizing.noEndpoint = true
	s.sizing.now.CompactFrom = ""
	s.opts.CompactFrom = ""
}

// servingCallers notes that A2A callers run on the session's model and
// budget as they are now, which a later switch of the person's keeps
// in force for them: the kit's budget, kit-wide, is the smaller of
// theirs and the person's from then on (resize).
func (s *Session) servingCallers() {
	s.sizing.mu.Lock()
	defer s.sizing.mu.Unlock()
	if s.sizing.callers == nil {
		b := s.sizing.now.Compact
		s.sizing.callers = &b
	}
}

// lowerBudget is the smaller of two budgets, 0 being none, so the
// other.
func lowerBudget(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	}
	return min(a, b)
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

// ContextWindow is the context window, in tokens, the session believes
// the model it now runs has (Model), as the start or the last switch of
// model resolved it; 0 unknown. A status line divides by it.
func (s *Session) ContextWindow() int64 { return s.sizes().Window }

// budgetFrom is where the budget of a session under o came from, as
// dax:context_from records it: "off" when nothing compacts, else
// CompactFrom, "window" with a budget of 0 for a window not known, when
// compaction folds only on request until a switch sizes it.
func budgetFrom(o Options) string {
	if o.Compact <= 0 && !compactsByWindow(o) {
		return "off"
	}
	return o.CompactFrom
}

// compactsByWindow says whether a session under o builds its
// compaction although its budget is 0: the budget is the window's,
// which a switch to a model whose window is known sizes.
func compactsByWindow(o Options) bool { return o.CompactFrom == "window" && o.WindowOf != nil }

// resize sizes the context for model, which the session has just
// switched to: the window Options.WindowOf gives it, and a budget from
// the window sized from the new one, applied to the kit's compaction
// from the next request; a budget the person set is kept. While A2A
// callers are served, the kit's budget is held to theirs too
// (servingCallers). A change is said and recorded.
func (s *Session) resize(ctx context.Context, model string) {
	if s.opts.WindowOf == nil {
		return
	}
	// One switch at a time, from the window asked for to the entry
	// written, so two views switching at once leave the kit's budget,
	// the sizes in force and the record in the same order.
	s.sizing.switching.Lock()
	defer s.sizing.switching.Unlock()
	window, from := s.opts.WindowOf(ctx, model)
	s.sizing.mu.Lock()
	was := s.sizing.now
	callers := s.sizing.callers
	tell := s.sizing.noEndpoint && window > 0 && !s.sizing.said
	s.sizing.said = s.sizing.said || tell
	s.sizing.mu.Unlock()
	if tell {
		s.opts.log("[-compact-server: this provider has no compaction endpoint, so no compaction budget is sized for %s's %d-token context window]", model, window)
	}
	now := was
	now.Window, now.WindowFrom = window, from
	held := false
	if was.CompactFrom == "window" {
		now.Compact = modelinfo.Info{ContextWindow: window}.CompactBudget()
		if callers != nil {
			b := lowerBudget(*callers, now.Compact)
			held, now.Compact = b != now.Compact, b
		}
	}
	if now.Compact != was.Compact {
		switch err := s.Kit.SetCompactionBudget(now.Compact); {
		case errors.Is(err, agentkit.ErrNoCompaction):
			// Nothing compacts, so no budget is in force or recorded.
			now.Compact, now.CompactFrom = 0, ""
		case err != nil:
			fmt.Fprintf(warnings{}, "dax: compaction budget for %s: %v\n", model, err)
			now.Compact = was.Compact
		case held:
			s.opts.log("[compaction budget %d tokens from the next request: the A2A callers' model's, which they stay on, under %s's]", now.Compact, model)
		case now.Compact > 0:
			s.opts.log("[compaction budget %d tokens from the next request: three quarters of %s's %d-token context window]", now.Compact, model, now.Window)
		default:
			s.opts.log("[no compaction budget from the next request: %s's context window is unknown; /compact still folds]", model)
		}
	}
	s.sizing.mu.Lock()
	s.sizing.now = now
	s.sizing.mu.Unlock()
	// Where the window came from is not a setting: a switch that
	// changes only that records nothing.
	if contextOf(withSizes(s.opts, now)) != contextOf(withSizes(s.opts, was)) {
		s.recordContext(ctx, now)
	}
}

// contextData is the custom entry's data for the context z: the
// header's members of it under their own names.
func (s *Session) contextData(z sizes) map[string]any {
	o := withSizes(s.opts, z)
	return map[string]any{
		headerContext:     contextOf(o),
		headerContextFrom: recordedContextFrom{BudgetFrom: budgetFrom(o), WindowFrom: o.WindowFrom},
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
