package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ChristopherDavenport/agentpolicy"

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
// the model is told at the next turn.
type modes struct {
	mu       sync.Mutex
	settings *policy.Settings // nil: the session has no policy
	mode     string
	pending  string
	// why is the reason the pending switch is recorded with.
	why  string
	note string
	// planAsks are the calls plan mode still asks about
	// (policy.PlanAsks): the plan's exit.
	planAsks map[string]bool
	// model is a model to move to before the next run starts:
	// Options.PlanModel, when plan mode was entered, or the model it
	// replaced, when plan mode was left unapproved.
	model string
	// before is the model plan_model replaced, put back when plan mode
	// is left any way but an approved exit ("" when plan_model was not
	// applied).
	before string
	// approved says an approval of the plan's exit was given since the
	// last run started: actModel has moved the model, and leaving plan
	// mode puts nothing back.
	approved bool
	// inForce is mode, read by the decisions without the lock, which a
	// switch holds while it replaces the policy.
	inForce atomic.Value
	// peers is the A2A callers' floor (peerFloor): an engine of the same
	// rules in the default mode, which no switch touches.
	peers *agentpolicy.Engine
	// swapped, set by a test, runs right after a switch has put its
	// policy in the engine.
	swapped func()
}

// current is the mode in force, as the decisions read it.
func (m *modes) current() string {
	v, _ := m.inForce.Load().(string)
	return v
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
// the next turn (modeTurn).
func (s *Session) SetMode(ctx context.Context, name string) (pending bool, err error) {
	return s.setMode(ctx, name, "/mode")
}

// setMode is SetMode, the switch recorded with why.
func (s *Session) setMode(ctx context.Context, name, why string) (pending bool, err error) {
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
	err = s.ag.WhileIdle(func() error { return s.applyMode(ctx, mode, why) })
	if errors.Is(err, agentturn.ErrRunning) {
		s.modes.mu.Lock()
		if mode == s.modes.mode {
			s.modes.pending, s.modes.why = "", ""
		} else {
			s.modes.pending, s.modes.why = mode, why+", at the next turn"
		}
		s.modes.mu.Unlock()
		return true, nil
	}
	if err == nil {
		// Plan mode's model, now that no run goes.
		s.modelBeforeRun()
	}
	return false, err
}

// actModel notes an approval in answers of the plan's exit, a call
// plan mode asks about (modes.planAsks), pending in plan mode, and moves
// to the model the plan is carried out on: Options.ActModel when one is
// set, else the model plan_model replaced (modes.before), unless the
// person chose another in plan mode. It does so before the Resume that
// runs the call, since SetModel refuses a running agent; the call itself
// moves the mode, and leaving plan mode so puts back nothing more.
func (s *Session) actModel(answers []agentturn.Answer) {
	mode, _ := s.Mode()
	if mode != policy.ModePlan {
		return
	}
	for _, p := range s.ag.State().Pending {
		if p.Call == nil || !s.modes.planAsks[p.Call.Name] {
			continue
		}
		for _, a := range answers {
			if a.CallID == p.Call.CallID && a.Output == nil {
				s.modes.mu.Lock()
				s.modes.approved = true
				to, what := s.opts.ActModel, "act_model"
				if to == "" && s.modes.before != "" && s.Model() == s.opts.PlanModel {
					to, what = s.modes.before, "the model before plan mode"
				}
				s.modes.mu.Unlock()
				if to == "" {
					return
				}
				if err := s.SetModel(to); err != nil {
					fmt.Fprintf(warnings{}, "dax: %s %s: %v\n", what, to, err)
				}
				return
			}
		}
	}
}

// modelBeforeRun moves to the model a switch asked for, before a run
// starts (SetModel refuses a running agent): plan_model while the
// session is still in plan mode, the model it replaced, keeping that
// to put back, or that model once plan mode was left unapproved. It
// starts the run with no approval of the plan's exit noted.
func (s *Session) modelBeforeRun() {
	s.modes.mu.Lock()
	m := s.modes.model
	s.modes.model, s.modes.approved = "", false
	toPlan := m != "" && s.modes.mode == policy.ModePlan
	if toPlan && s.modes.before == "" {
		s.modes.before = s.Model()
	}
	s.modes.mu.Unlock()
	if m == "" {
		return
	}
	if err := s.SetModel(m); err != nil {
		what := "the model before plan mode"
		if toPlan {
			what = "plan_model"
		}
		fmt.Fprintf(warnings{}, "dax: %s %s: %v\n", what, m, err)
	}
}

// leavePlan is what leaving plan mode does to the model, under
// modes.mu: an approved exit has moved the model already (actModel:
// act_model, or the model before plan mode) and puts nothing back; any other way puts back the model plan_model replaced,
// before the next run, unless the person chose another since. A
// plan_model not yet applied is not.
func (s *Session) leavePlan() {
	before := s.modes.before
	s.modes.before, s.modes.model = "", ""
	if s.modes.approved || before == "" {
		return
	}
	if s.Model() == s.opts.PlanModel {
		s.modes.model = before
	}
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
	s.modes.pending, s.modes.why = "", ""
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
	// The hooks read the looser of the two modes while the engine's
	// policy changes, so no call is decided under looser rules than the
	// hooks expect: an A2A caller is floored (peerFloor) from the first
	// call under a looser mode's rules, until the last under one it
	// leaves. dontAsk's refusal starts once the swap is done; its rules
	// are default's, so the calls between are asked as there.
	from := s.modes.mode
	during := mode
	if policy.Stricter(mode, from) {
		during = from
	}
	s.modes.inForce.Store(during)
	if err := eng.SetPolicy(ctx, p, "mode "+mode+" ("+why+")"); err != nil {
		s.modes.inForce.Store(from)
		return err
	}
	if s.modes.swapped != nil {
		s.modes.swapped()
	}
	s.modes.inForce.Store(mode)
	s.modes.mode, s.modes.settings.Mode = mode, mode
	s.modes.note = policy.DescribeMode(mode)
	switch {
	case mode == policy.ModePlan && s.opts.PlanModel != "":
		// Applied before the next run (modelBeforeRun).
		s.modes.model = s.opts.PlanModel
	case from == policy.ModePlan:
		s.leavePlan()
	}
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
	pending, why := s.modes.pending, s.modes.why
	s.modes.mu.Unlock()
	if pending == "" {
		return nil
	}
	if err := s.applyMode(ctx, pending, why); err != nil {
		fmt.Fprintf(warnings{}, "dax: mode %s: %v\n", pending, err)
	}
	return nil
}

// modeTurn is the session's BeforeTurn beside the kit's: a switch
// asked for during the run is applied here, before the turn's model
// call and so before any call of the turn is decided, and the model is
// told of a switch at the first turn after it, whatever started the
// run (a prompt, a steer through Deliver or Accept, a Resume). An A2A
// caller's run (PeerConfig) has the kit's BeforeTurn alone.
func (s *Session) modeTurn(ctx context.Context, _ agentturn.TurnStartInfo) (openresponses.Items, error) {
	s.modes.mu.Lock()
	pending, why := s.modes.pending, s.modes.why
	s.modes.mu.Unlock()
	if pending != "" {
		if err := s.applyMode(ctx, pending, why); err != nil {
			fmt.Fprintf(warnings{}, "dax: mode %s: %v\n", pending, err)
		}
	}
	if n := s.modeNote(); n != nil {
		return openresponses.Items{n}, nil
	}
	return nil, nil
}

// modeNote takes what the model is to be told of a mode switch, as a
// developer message for the next turn, or nil.
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

// peerFloor is the hook the session's engine folds into every decision
// (agentpolicy.WithHooks): an A2A caller's call, or its sub-agent's,
// is held to the default mode, whatever mode the person put the
// session in. While the mode is looser than default (acceptEdits,
// bypassPermissions), the caller's call is decided again by the peers'
// engine (modes.peers: the same rule sources, saved grants and box
// confinement, in the default mode), and that verdict stands when it
// is stricter, since the fold keeps the strictest. In default and
// dontAsk the session's policy is the default one, so the floor adds
// nothing there and a caller's skill grants, which live in the
// session's engine, decide as before; in dontAsk a caller's would-ask
// call is refused as the session's is (dontAskRefusal). A switch never touches the
// peers' engine, so a caller's batch decided across one is decided
// under the default mode throughout.
func (s *Session) peerFloor(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	if _, peer := ctx.Value(peerKey{}).(string); !peer || s.modes.peers == nil {
		return nil, nil
	}
	if !policy.Stricter(policy.ModeDefault, s.modes.current()) {
		return nil, nil
	}
	v, err := s.modes.peers.Would(ctx, asCalled(info))
	if err != nil {
		return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the policy could not decide this call: " + err.Error(), By: agentpolicy.ByPolicy}, nil
	}
	if v.Action == agentturn.Allow {
		return nil, nil
	}
	return &agentturn.ToolDecision{Action: v.Action, Reason: v.Reason, Subject: v.Subject, By: agentpolicy.ByPolicy}, nil
}

// PersonOnlyNote is the reason an A2A caller's call of a tool an
// extension keeps to the person (extension.Extension.PersonOnly) is
// refused with, behind the tool's name.
const PersonOnlyNote = "is the person's own (their memory, the skills written for them): an A2A caller cannot use it. " + policy.NoWorkAround

// personOnlyRefusal is the hook, folded into the session's engine
// beside peerFloor, that refuses an A2A caller's call, or its
// sub-agent's, of a tool an extension keeps to the person
// (extension.Extension.PersonOnly): the memory tools, skill_manage.
// It refuses outright, whatever the rules and the mode, and asks no
// one: the person's memory and learned skills reach every later
// session of theirs, and one approval in an attached view would let a
// caller read or write them. A refusal is not an ask, so it holds none
// of the batch's other calls.
func (s *Session) personOnlyRefusal(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	if _, peer := callerOf(ctx); !peer || info.Call == nil || !s.personOnly[info.Call.Name] {
		return nil, nil
	}
	return &agentturn.ToolDecision{Action: agentturn.Block, Reason: info.Call.Name + " " + PersonOnlyNote, By: agentpolicy.ByPolicy}, nil
}

// asCalled is info with the arguments the model gave: a hook is handed
// the call's rewrite (bash's stamp), which a decision of its own reads
// from the call's facts claim again.
func asCalled(info agentturn.ToolCallInfo) agentturn.ToolCallInfo {
	if info.Call != nil && info.Call.Arguments != "" {
		info.Args = json.RawMessage(info.Call.Arguments)
	}
	return info
}

// peerPolicy is the policy the A2A callers' floor decides under: st in
// the default mode.
func peerPolicy(st policy.Settings) (agentpolicy.Policy, error) {
	st.Mode = policy.ModeDefault
	return policy.Build(st)
}

// dontAskProbe marks the decision dontAskRefusal asks the engine for,
// which the hook itself sits out.
type dontAskProbe struct{}

// dontAskRefusal is the hook that makes dontAsk: folded into the
// session's engine, it refuses, with policy.DontAskNote, the session's
// call (or its sub-agent's) that the engine would defer, asking no
// one, while the mode is dontAsk. The rules stay default's, so a
// carve-out lifts only what it lifts there and a box's confinement
// still answers its own asks. A refusal is not an ask, so it holds
// none of the batch's other calls. An A2A caller's call is refused
// too: a caller is held to the stricter of default and the person's
// mode (peerFloor below default, this hook in dontAsk), so a person who
// asked for nothing to ask, or a project's default_mode dontAsk, is
// not asked on a caller's behalf.
//
// Plan mode refuses the same way, with policy.PlanNote, but for the
// calls its own rules ask about (modes.planAsks: the plan's exit),
// which stay asks.
func (s *Session) dontAskRefusal(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	mode := s.modes.current()
	note, refuses := policy.Refuses(mode)
	if !refuses || ctx.Value(dontAskProbe{}) != nil {
		return nil, nil
	}
	if mode == policy.ModePlan && info.Call != nil && s.modes.planAsks[info.Call.Name] {
		return nil, nil
	}
	eng := s.Kit.Engine()
	if eng == nil {
		return nil, nil
	}
	v, err := eng.Would(context.WithValue(ctx, dontAskProbe{}, true), asCalled(info))
	if err != nil {
		return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the policy could not decide this call: " + err.Error(), By: agentpolicy.ByPolicy}, nil
	}
	if v.Action != agentturn.Defer {
		return nil, nil
	}
	return &agentturn.ToolDecision{Action: agentturn.Block, Reason: note, Subject: v.Subject, By: agentpolicy.ByPolicy}, nil
}
