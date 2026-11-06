// Package policy merges the agentpolicy rules a dax session runs under:
// the rules each extension ships for its own tools, each under a source
// of its own, the user's config, and the project's, which is not
// trusted. It knows no tool: dax's own rules are dax-coding's, and
// arrive here as any extension's do.
//
// A deny beats an ask and an ask beats an allow, whatever the source.
// Sources rank for carve-outs: a carve-out (a rule whose specifier
// starts with !) cancels a rule only of a source it does not rank
// below. The user's config ranks above the extensions, which rank
// above the project's config, so the user can lift an extension's ask
// and the project can lift nothing; an extension may not write a
// carve-out at all, so none can cancel another's rule.
package policy

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"
)

// Source names the layers in verdicts and in the session's record.
const (
	SourceUser    = "dax:config"
	SourceProject = "dax:project"
	// SourceBox is the rules a session's box.confine adds: a bare ask
	// for each tool the box confines, which a confined call of it skips
	// (agentpolicy's WithConfinement), so the tool asks only when the
	// call is not confined.
	SourceBox = "dax:box"
	// SourceSession is the rules a person granted for the session ("a",
	// yes for this session): a grant set under the conversation's grant
	// scope, at RankGrant.
	SourceSession = "dax:session"
	// SourceSaved is the rules a person kept for the project ("A", yes
	// always): allow rules from the user's grants file, at RankGrant.
	SourceSaved = "dax:saved"
)

// RankGrant is the rank of a person's grants, the session's and the
// saved ones: below every other source, the project's included, so a
// grant answers only what asks for want of a rule, never a rule's ask
// (dax-coding's secret paths, a project's or the user's own asks).
const RankGrant = -1

// SourceExtension is the source of the rules the extension called name
// ships: what a verdict a rule of them decides names in the session's
// record, so a call one allowed is recorded as that extension's
// decision.
func SourceExtension(name string) string { return "extension:" + name }

// Settings are the policy's rules, folded across the config's layers,
// and the extensions'.
type Settings struct {
	// Mode is the permission mode (Modes); "" is ModeDefault.
	Mode string
	// Builtin keeps the allow rules the extensions ship. Without it
	// their asks and denies still apply, so a repository that turns it
	// off loosens nothing.
	Builtin bool
	// Fallback is what a call no rule matches gets: allow, deny, or
	// ask for anything else.
	Fallback string
	// Shipped are the extensions' rules, one source each.
	Shipped []Shipped
	// User and Project are the user's config's rules and the project's.
	User    Rules
	Project Rules
	// Saved are the allow rules the user kept for this project ("yes,
	// always"), from their grants file: source SourceSaved, at
	// RankGrant. Only Allow is read.
	Saved []string
	// Box are the tools a session's box.confine takes as confined, each
	// a bare ask of source SourceBox: dax-coding's bash, write and edit
	// by name, and an MCP unit's tools as mcp__<unit>__*, the one glob
	// it takes, since a unit's tools are listed only once it connects.
	// What it names asks as the default would, unless the call is
	// confined.
	Box []string
}

// Rules are rule lists in the policy grammar, one string per entry.
type Rules struct{ Allow, Ask, Deny []string }

// Shipped are the rules one extension ships.
type Shipped struct {
	// Name names the extension; its source is SourceExtension(Name).
	// It must be set, and no two may share it.
	Name  string
	Rules Rules
	// Owns are the names its rules may use: its tools and its aliases.
	// A rule naming anything else is an error.
	Owns []string
	// Lifts are its tools whose asks a user's allow rule with a
	// specifier for the same tool lifts.
	Lifts []string
	// Modes are the rules it adds in a mode, by the mode's name: only
	// ModeAcceptEdits takes any. They are held to Owns as its rules
	// are, and kept whether or not Builtin is, since the user chose the
	// mode.
	Modes map[string]Rules
}

// Ranks of the sources: the user's config above the extensions, the
// extensions above the project's config.
const (
	rankProject   = 0
	rankExtension = 1
	rankUser      = 2
)

// Build merges the extensions' rules with the user's and the project's.
// The project's source is untrusted and ranks below the rest, so its
// allow rules are withheld, its ask and deny rules apply, and a
// carve-out in it cannot cancel a rule of theirs.
func Build(s Settings) (agentpolicy.Policy, error) {
	mode, err := ParseMode(s.Mode)
	if err != nil || (s.Mode != "" && s.Mode != mode) {
		return agentpolicy.Policy{}, fmt.Errorf("policy: mode %q: want one of %s", s.Mode, strings.Join(Modes, ", "))
	}
	parse := func(what string, rules []string) ([]agentpolicy.Rule, error) {
		out, err := agentpolicy.ParseRules(strings.Join(rules, " "))
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", what, err)
		}
		return out, nil
	}
	var sets []agentpolicy.RuleSet
	lifts := map[string]bool{}
	names := map[string]bool{}
	// The shipped rules are read whether or not their allow rules are
	// kept, so a program's bad rule is its error and not one a user's
	// "builtin": false hides.
	for _, sh := range s.Shipped {
		if sh.Name == "" {
			return agentpolicy.Policy{}, errors.New("policy: shipped rules without an extension name")
		}
		if names[sh.Name] {
			return agentpolicy.Policy{}, fmt.Errorf("policy: two extensions named %q ship rules", sh.Name)
		}
		names[sh.Name] = true
		set := agentpolicy.RuleSet{Source: agentpolicy.Source{Name: SourceExtension(sh.Name), Trusted: true, Rank: rankExtension}}
		for _, l := range []struct {
			what string
			in   []string
			out  *[]agentpolicy.Rule
		}{{"allow", sh.Rules.Allow, &set.Allow}, {"ask", sh.Rules.Ask, &set.Ask}, {"deny", sh.Rules.Deny, &set.Deny}} {
			rules, err := parse("extension "+sh.Name+" "+l.what, l.in)
			if err != nil {
				return agentpolicy.Policy{}, err
			}
			for _, r := range rules {
				if err := owned(sh, r); err != nil {
					return agentpolicy.Policy{}, fmt.Errorf("policy extension %s %s %s: %w", sh.Name, l.what, ruleText(r), err)
				}
			}
			*l.out = rules
		}
		if !s.Builtin {
			set.Allow = nil
		}
		for m := range sh.Modes {
			if _, ok := strictness[m]; !ok || m != ModeAcceptEdits {
				return agentpolicy.Policy{}, fmt.Errorf("policy extension %s: rules for mode %q: only %s takes an extension's rules", sh.Name, m, ModeAcceptEdits)
			}
		}
		if mr, ok := sh.Modes[mode]; ok {
			for _, l := range []struct {
				what string
				in   []string
				out  *[]agentpolicy.Rule
			}{{"allow", mr.Allow, &set.Allow}, {"ask", mr.Ask, &set.Ask}, {"deny", mr.Deny, &set.Deny}} {
				rules, err := parse("extension "+sh.Name+" "+mode+" "+l.what, l.in)
				if err != nil {
					return agentpolicy.Policy{}, err
				}
				for _, r := range rules {
					if err := owned(sh, r); err != nil {
						return agentpolicy.Policy{}, fmt.Errorf("policy extension %s %s %s %s: %w", sh.Name, mode, l.what, ruleText(r), err)
					}
				}
				*l.out = append(*l.out, rules...)
			}
		}
		for _, t := range sh.Lifts {
			lifts[strings.ToLower(t)] = true
		}
		sets = append(sets, set)
	}
	for _, l := range []struct {
		src   agentpolicy.Source
		rules Rules
	}{
		{agentpolicy.Source{Name: SourceUser, Trusted: true, Rank: rankUser}, s.User},
		{agentpolicy.Source{Name: SourceProject, Rank: rankProject}, s.Project},
	} {
		var set agentpolicy.RuleSet
		var err error
		set.Source = l.src
		if set.Allow, err = parse("allow", l.rules.Allow); err != nil {
			return agentpolicy.Policy{}, err
		}
		if set.Ask, err = parse("ask", l.rules.Ask); err != nil {
			return agentpolicy.Policy{}, err
		}
		if set.Deny, err = parse("deny", l.rules.Deny); err != nil {
			return agentpolicy.Policy{}, err
		}
		if l.src.Trusted {
			// Allowing a path is meant: an allow rule with a specifier
			// for a tool an extension lifts for cancels that
			// extension's ask for it, which precedence alone would not.
			for _, r := range set.Allow {
				if r.Spec != "" && !strings.HasPrefix(r.Spec, "!") && lifts[strings.ToLower(r.Tool)] {
					set.Ask = append(set.Ask, agentpolicy.Rule{Tool: r.Tool, Spec: "!" + r.Spec})
				}
			}
		}
		sets = append(sets, set)
	}
	if len(s.Saved) > 0 {
		set := agentpolicy.RuleSet{Source: agentpolicy.Source{Name: SourceSaved, Trusted: true, Rank: RankGrant}}
		if set.Allow, err = parse("saved", s.Saved); err != nil {
			return agentpolicy.Policy{}, err
		}
		sets = append(sets, set)
	}
	if len(s.Box) > 0 {
		set := agentpolicy.RuleSet{Source: agentpolicy.Source{Name: SourceBox, Trusted: true, Rank: rankExtension}}
		for _, name := range s.Box {
			if !boxTool(name) {
				return agentpolicy.Policy{}, fmt.Errorf("policy box: %q: want bash, write, edit or mcp__<unit>__*", name)
			}
			set.Ask = append(set.Ask, agentpolicy.Rule{Tool: name})
		}
		sets = append(sets, set)
	}
	p, err := agentpolicy.Merge(sets...)
	if err != nil {
		return agentpolicy.Policy{}, err
	}
	switch s.Fallback {
	case "allow":
		p.Default = agentpolicy.Allow()
	case "deny":
		// The model reads the reason of a call denied by default; the
		// note tells it not to work around the denial (#150).
		p.Default = agentpolicy.Deny().WithNote(NoWorkAround)
	default:
		p.Default = agentpolicy.Ask()
	}
	return shape(mode, s.Fallback, p), nil
}

// NoWorkAround is what the model is told with a refusal, a person's or
// the policy's by default: not to get the same result another way, and
// to leave the choice to the user (#150).
const NoWorkAround = "Do not get the same result another way (another tool, a script, another path); if it is still needed, say so and let the user decide."

// boxTool reports whether name is one SourceBox may name: a file tool
// or the shell by name, or every tool of one MCP unit, whose name is
// letters, digits and dashes (box.UnitName's).
func boxTool(name string) bool {
	switch name {
	case "bash", "write", "edit":
		return true
	}
	unit, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return false
	}
	unit, ok = strings.CutSuffix(unit, "__*")
	return ok && boxUnit.MatchString(unit)
}

var boxUnit = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// owned refuses a shipped rule that reaches past its extension: one
// naming a tool or alias the extension does not own, a tool pattern,
// or a carve-out, which would cancel another source's rule.
func owned(sh Shipped, r agentpolicy.Rule) error {
	if strings.HasPrefix(r.Spec, "!") {
		return errors.New("an extension may not ship a carve-out")
	}
	if strings.ContainsAny(r.Tool, "*?[") {
		return errors.New("an extension's rule names its tools, not a pattern")
	}
	for _, o := range sh.Owns {
		if strings.EqualFold(o, r.Tool) {
			return nil
		}
	}
	return fmt.Errorf("%q is not one of the extension's tools", r.Tool)
}

// ruleText is a rule as it was written.
func ruleText(r agentpolicy.Rule) string {
	if r.Spec == "" {
		return r.Tool
	}
	return r.Tool + "(" + r.Spec + ")"
}
