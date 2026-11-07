package policy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"
)

// The permission modes: the same rules, made stricter or looser as a
// whole, which the user picks at start (-mode, policy.default_mode) and
// changes in the session (/mode, Shift-Tab). A deny rule, whoever's,
// holds in every mode.
const (
	// ModeDefault is the rules as they are: what no rule allows asks.
	ModeDefault = "default"
	// ModeAcceptEdits adds each extension's acceptEdits rules
	// (Shipped.Modes): dax-coding's write and edit run unasked but for
	// the paths that change what runs or is trusted; commands still ask.
	ModeAcceptEdits = "acceptEdits"
	// ModePlan is for reading and planning: every call that would ask
	// is refused without asking, as in dontAsk, but for the asks an
	// extension adds for the mode (dax-plan's exit_plan_mode, which puts
	// the plan to the user), and each extension's plan rules apply
	// (dax-coding denies write and edit there, over any allow). The
	// user's approval of the plan moves the session to acceptEdits.
	ModePlan = "plan"
	// ModeDontAsk refuses, without asking, every call that would ask:
	// only what a rule allows runs. The rules are those of the default
	// mode: the refusal is the session's, which refuses each call the
	// engine would defer (DontAskNote), so a carve-out lifts only what
	// it lifts in default and a box's confinement still answers its
	// asks.
	ModeDontAsk = "dontAsk"
	// ModeBypass asks about nothing: what no deny rule refuses runs,
	// whatever the fallback.
	ModeBypass = "bypassPermissions"
)

// Modes are the modes there are.
var Modes = []string{ModeDefault, ModeAcceptEdits, ModePlan, ModeDontAsk, ModeBypass}

// Cycle are the modes Shift-Tab cycles through, in order. Bypass is
// not among them: one key must not turn every ask off, so it is
// reached only by its name (/mode bypassPermissions, -mode,
// -no-policy).
var Cycle = []string{ModeDefault, ModeAcceptEdits, ModePlan, ModeDontAsk}

// strictness orders the modes, the strictest last: a project's
// default_mode may only move the user's toward it.
var strictness = map[string]int{ModeBypass: 0, ModeAcceptEdits: 1, ModeDefault: 2, ModePlan: 3, ModeDontAsk: 4}

// ParseMode reads a mode's name, in any case; bypass is
// bypassPermissions.
func ParseMode(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "default":
		return ModeDefault, nil
	case "acceptedits":
		return ModeAcceptEdits, nil
	case "plan":
		return ModePlan, nil
	case "dontask":
		return ModeDontAsk, nil
	case "bypasspermissions", "bypass":
		return ModeBypass, nil
	}
	return "", fmt.Errorf("mode %q: want one of %s", s, strings.Join(Modes, ", "))
}

// Stricter reports whether mode a refuses or asks more than b.
func Stricter(a, b string) bool { return strictness[a] > strictness[b] }

// NextMode is the mode after m in Cycle, the first after the last or
// after a mode not in it (bypassPermissions).
func NextMode(m string) string {
	i := slices.Index(Cycle, m)
	return Cycle[(i+1)%len(Cycle)]
}

// DescribeMode is what the model is told when the mode changes.
func DescribeMode(m string) string {
	switch m {
	case ModeAcceptEdits:
		return "The permission mode is now acceptEdits: file writes and edits run without asking, but for paths that change what runs or is trusted; commands and other tools still ask the user."
	case ModePlan:
		return "The permission mode is now plan: research and plan only. Reads and read-only commands run; writes and edits are refused, and so is every call that would ask the user. When your plan is ready, call exit_plan_mode with it: the user approves it before anything changes. " + NoWorkAround
	case ModeDontAsk:
		return "The permission mode is now dontAsk: only calls the rules allow run; every other call is refused without asking the user. " + NoWorkAround
	case ModeBypass:
		return "The permission mode is now bypassPermissions: calls run without asking the user, except those a deny rule refuses."
	}
	return "The permission mode is now default: reads run; writes, edits, commands and other tools ask the user first."
}

// DontAskNote is the reason of a call dontAsk refuses for asking.
const DontAskNote = "refused without asking in dontAsk mode. " + NoWorkAround

// PlanNote is the reason of a call plan mode refuses for asking.
const PlanNote = "refused without asking in plan mode: only reading runs until the user approves a plan (exit_plan_mode). " + NoWorkAround

// Refuses reports whether mode m refuses, without asking, a call that
// would ask, and with what reason: dontAsk and plan do.
func Refuses(m string) (note string, ok bool) {
	switch m {
	case ModeDontAsk:
		return DontAskNote, true
	case ModePlan:
		return PlanNote, true
	}
	return "", false
}

// shape is p as mode m makes it: what becomes of the asks and the
// default. acceptEdits' rules are the extensions' own, added before the
// merge; dontAsk's rules are default's (ModeDontAsk).
func shape(m string, p agentpolicy.Policy) agentpolicy.Policy {
	switch m {
	case ModeBypass:
		// The asks become allows, their carve-outs dropped, and the
		// default allows whatever the fallback: what no deny rule
		// refuses runs, as -no-policy's session did, a fallback "deny"
		// (the user's or a project's) included.
		for _, r := range p.Ask {
			if _, carve := r.CarveOut(); !carve {
				p.Allow = append(p.Allow, r)
			}
		}
		p.Ask = nil
		p.Default = agentpolicy.Allow()
	}
	return p
}
