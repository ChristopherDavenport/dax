package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/policy"
)

// modeNS is the record's entry for the permission mode: at start when
// it is not the default, and at each switch, beside agentpolicy's own
// record of the replaced policy.
const modeNS = "dax.mode"

// modeEntry is what a dax.mode entry says: the mode now in force, the
// one it replaced ("" at start), and why.
type modeEntry struct {
	Mode   string `json:"mode"`
	From   string `json:"from,omitempty"`
	Reason string `json:"reason"`
}

// modes is the session's permission mode: the settings the policy is
// rebuilt from (the box's confinement and the extensions' rules
// included), the mode in force, one asked for during a run, and what
// the model is told at the next prompt.
type modes struct {
	mu       sync.Mutex
	settings *policy.Settings // nil: the session has no policy
	mode     string
	pending  string
	note     string
}

// ErrNoPolicy refuses a mode on a session built without a policy.
var ErrNoPolicy = errors.New("the session has no policy to put in a mode")

// Mode is the permission mode in force, and one asked for during a run
// that the next turn starts under ("" when none waits).
func (s *Session) Mode() (mode, pending string) {
	s.modes.mu.Lock()
	defer s.modes.mu.Unlock()
	return s.modes.mode, s.modes.pending
}

// SetMode puts the session in the permission mode name (policy.Modes):
// the policy is rebuilt from the session's settings in that mode and
// replaces the engine's (agentpolicy's SetPolicy, recorded with the
// mode as its reason, and a dax.mode entry beside it), which the
// sub-agents' decisions read too. Scoped grants survive it. While a run
// goes the switch waits for the run's next turn or its end, so no batch
// is decided under two policies; pending says so. The model is told at
// the next prompt.
func (s *Session) SetMode(ctx context.Context, name string) (pending bool, err error) {
	mode, err := policy.ParseMode(name)
	if err != nil {
		return false, err
	}
	s.modes.mu.Lock()
	if s.modes.settings == nil {
		s.modes.mu.Unlock()
		return false, ErrNoPolicy
	}
	s.modes.mu.Unlock()
	err = s.ag.WhileIdle(func() error { return s.applyMode(ctx, mode, "/mode") })
	if errors.Is(err, agentturn.ErrRunning) {
		s.modes.mu.Lock()
		if mode == s.modes.mode {
			s.modes.pending = ""
		} else {
			s.modes.pending = mode
		}
		s.modes.mu.Unlock()
		return true, nil
	}
	return false, err
}

// NextMode moves to the mode after the one in force, or after the one
// waiting (policy.NextMode), as Shift-Tab does, and returns it.
func (s *Session) NextMode(ctx context.Context) (string, error) {
	cur, pending := s.Mode()
	if pending != "" {
		cur = pending
	}
	next := policy.NextMode(cur)
	_, err := s.SetMode(ctx, next)
	return next, err
}

// applyMode replaces the policy with the one of mode, unless it is in
// force already.
func (s *Session) applyMode(ctx context.Context, mode, why string) error {
	s.modes.mu.Lock()
	defer s.modes.mu.Unlock()
	s.modes.pending = ""
	if mode == s.modes.mode {
		return nil
	}
	eng := s.Kit.Engine()
	if eng == nil {
		return ErrNoPolicy
	}
	st := *s.modes.settings
	st.Mode = mode
	p, err := policy.Build(st)
	if err != nil {
		return err
	}
	if err := eng.SetPolicy(ctx, p, "mode "+mode+" ("+why+")"); err != nil {
		return err
	}
	from := s.modes.mode
	s.modes.mode, s.modes.settings.Mode = mode, mode
	s.modes.note = policy.DescribeMode(mode)
	s.annotateMode(ctx, modeEntry{Mode: mode, From: from, Reason: why})
	return nil
}

// annotateMode writes a dax.mode entry; a write that fails is lost, as
// a verdict's is.
func (s *Session) annotateMode(ctx context.Context, e modeEntry) {
	if s.Kit == nil {
		return
	}
	if rec := s.Kit.Recorder(); rec != nil {
		_, _ = rec.Annotate(ctx, modeNS, e)
	}
}

// modeAtTurn applies a mode asked for during a run at the run's next
// turn, before its model call and so before any call of the turn is
// decided, or at the run's end.
func (s *Session) modeAtTurn(ctx context.Context, ev agentturn.Event) error {
	switch ev.(type) {
	case *agentturn.TurnStart, *agentturn.RunEnd:
	default:
		return nil
	}
	s.modes.mu.Lock()
	pending := s.modes.pending
	s.modes.mu.Unlock()
	if pending == "" {
		return nil
	}
	if err := s.applyMode(ctx, pending, "/mode, at the next turn"); err != nil {
		fmt.Fprintf(warnings{}, "dax: mode %s: %v\n", pending, err)
	}
	return nil
}

// modeNote takes what the model is to be told of a mode switch, as a
// developer message for the next prompt, or nil.
func (s *Session) modeNote() openresponses.Item {
	s.modes.mu.Lock()
	defer s.modes.mu.Unlock()
	if s.modes.note == "" {
		return nil
	}
	n := s.modes.note
	s.modes.note = ""
	return openresponses.DeveloperText(n)
}
