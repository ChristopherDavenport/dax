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
	// ModeDontAsk refuses, without asking, every call that would ask:
	// only what a rule allows runs. The rules are those of the default
	// mode: the refusal is the session's, which refuses each call the
	// engine would defer (DontAskNote), so a carve-out lifts only what
	// it lifts in default and a box's confinement still answers its
	// asks.
	ModeDontAsk = "dontAsk"
	// ModeBypass asks about nothing: what no deny rule refuses runs.
	ModeBypass = "bypassPermissions"
)

// Modes are the modes in the order Shift-Tab cycles them.
var Modes = []string{ModeDefault, ModeAcceptEdits, ModeDontAsk, ModeBypass}

// strictness orders the modes, the strictest last: a project's
// default_mode may only move the user's toward it.
var strictness = map[string]int{ModeBypass: 0, ModeAcceptEdits: 1, ModeDefault: 2, ModeDontAsk: 3}

// ParseMode reads a mode's name, in any case; bypass is
// bypassPermissions.
func ParseMode(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "default":
		return ModeDefault, nil
	case "acceptedits":
		return ModeAcceptEdits, nil
	case "dontask":
		return ModeDontAsk, nil
	case "bypasspermissions", "bypass":
		return ModeBypass, nil
	}
	return "", fmt.Errorf("mode %q: want one of %s", s, strings.Join(Modes, ", "))
}

// Stricter reports whether mode a refuses or asks more than b.
func Stricter(a, b string) bool { return strictness[a] > strictness[b] }

// NextMode is the mode after m in Modes, the first after the last.
func NextMode(m string) string {
	i := slices.Index(Modes, m)
	return Modes[(i+1)%len(Modes)]
}

// DescribeMode is what the model is told when the mode changes.
func DescribeMode(m string) string {
	switch m {
	case ModeAcceptEdits:
		return "The permission mode is now acceptEdits: file writes and edits run without asking, but for paths that change what runs or is trusted; commands and other tools still ask the user."
	case ModeDontAsk:
		return "The permission mode is now dontAsk: only calls the rules allow run; every other call is refused without asking the user. " + NoWorkAround
	case ModeBypass:
		return "The permission mode is now bypassPermissions: calls run without asking the user, except those a deny rule refuses."
	}
	return "The permission mode is now default: reads run; writes, edits, commands and other tools ask the user first."
}

// DontAskNote is the reason of a call dontAsk refuses for asking.
const DontAskNote = "refused without asking in dontAsk mode. " + NoWorkAround

// shape is p as mode m makes it: what becomes of the asks and the
// default. acceptEdits' rules are the extensions' own, added before the
// merge; dontAsk's rules are default's (ModeDontAsk).
func shape(m string, fallback string, p agentpolicy.Policy) agentpolicy.Policy {
	switch m {
	case ModeBypass:
		p.Ask = nil
		if fallback != "deny" {
			p.Default = agentpolicy.Allow()
		}
	}
	return p
}
