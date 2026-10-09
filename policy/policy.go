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
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"
)

// Source names the layers in verdicts and in the session's record.
const (
	SourceUser    = "dax:config"
	SourceProject = "dax:project"
)

// SourceExtension is the source of the rules the extension called name
// ships: what a verdict a rule of them decides names in the session's
// record, so a call one allowed is recorded as that extension's
// decision.
func SourceExtension(name string) string { return "extension:" + name }

// Settings are the policy's rules, folded across the config's layers,
// and the extensions'.
type Settings struct {
	// Off runs every call without asking; Build is not called.
	Off bool
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
	p, err := agentpolicy.Merge(sets...)
	if err != nil {
		return agentpolicy.Policy{}, err
	}
	switch s.Fallback {
	case "allow":
		p.Default = agentpolicy.Allow()
	case "deny":
		p.Default = agentpolicy.Deny()
	default:
		p.Default = agentpolicy.Ask()
	}
	return p, nil
}

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
