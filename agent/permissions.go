package agent

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"

	"github.com/ChristopherDavenport/dax/policy"
)

// PolicyInfo is the policy in force, as /permissions lists it: the
// mode, the default, each source with the file it came from and its
// rules, the box's confinement claim, and the grant sets in force with
// their scope.
type PolicyInfo struct {
	Mode    string `json:"mode"`
	Pending string `json:"pending,omitempty"`
	// Default is what a call no rule names gets, with its note.
	Default string         `json:"default"`
	Sources []PolicySource `json:"sources"`
	// Confined says which tools box.confine takes as confined, and how,
	// "" for none.
	Confined string        `json:"confined,omitempty"`
	Grants   []PolicyGrant `json:"grants,omitempty"`
}

// PolicySource is one source the policy was merged from and its rules
// in force, each as written (an alias expanded).
type PolicySource struct {
	Name    string   `json:"name"`
	Path    string   `json:"path,omitempty"`
	Trusted bool     `json:"trusted"`
	Rank    int      `json:"rank"`
	Allow   []string `json:"allow,omitempty"`
	Ask     []string `json:"ask,omitempty"`
	Deny    []string `json:"deny,omitempty"`
}

// PolicyGrant is a grant set in force: a skill's, a person's for the
// session, under its grant scope ("" for one that decides every
// conversation's calls).
type PolicyGrant struct {
	Source string   `json:"source"`
	Scope  string   `json:"scope,omitempty"`
	Allow  []string `json:"allow,omitempty"`
	Ask    []string `json:"ask,omitempty"`
	Deny   []string `json:"deny,omitempty"`
}

// policyInfo reads the policy in force from the engine.
func (s *Session) policyInfo() *PolicyInfo {
	if s.Kit == nil || s.Kit.Engine() == nil {
		return nil
	}
	eng := s.Kit.Engine()
	p := eng.Policy()
	info := &PolicyInfo{Default: p.Default.String()}
	info.Mode, info.Pending = s.Mode()
	texts := func(rules []agentpolicy.Rule, source string) []string {
		var out []string
		for _, r := range rules {
			if r.Source.Name == source {
				out = append(out, r.String())
			}
		}
		return out
	}
	sources := eng.Sources()
	named := map[string]bool{}
	for _, src := range sources {
		named[src.Name] = true
		info.Sources = append(info.Sources, PolicySource{
			Name: src.Name, Path: src.Path, Trusted: src.Trusted, Rank: src.Rank,
			Allow: texts(p.Allow, src.Name), Ask: texts(p.Ask, src.Name), Deny: texts(p.Deny, src.Name),
		})
	}
	// Rules of no source the merge listed: a grant of the engine's own.
	if a, k, d := texts(p.Allow, ""), texts(p.Ask, ""), texts(p.Deny, ""); len(a)+len(k)+len(d) > 0 && !named[""] {
		info.Sources = append(info.Sources, PolicySource{Name: "(granted)", Trusted: true, Allow: a, Ask: k, Deny: d})
	}
	s.modes.mu.Lock()
	if st := s.modes.settings; st != nil && len(st.Box) > 0 {
		info.Confined = "box.confine " + s.opts.BoxConfine + ": " + strings.Join(st.Box, ", ") + " ask unless the call runs confined in the box"
	}
	s.modes.mu.Unlock()
	ctx := context.Background()
	unscoped := len(eng.GrantsFor(ctx))
	scope := s.Kit.GrantScope(ctx)
	for i, set := range eng.GrantsFor(agentpolicy.ContextWithGrantScope(ctx, scope)) {
		g := PolicyGrant{Source: set.Source.Name}
		if i >= unscoped {
			g.Scope = scope
		}
		for _, r := range set.Allow {
			g.Allow = append(g.Allow, r.String())
		}
		for _, r := range set.Ask {
			g.Ask = append(g.Ask, r.String())
		}
		for _, r := range set.Deny {
			g.Deny = append(g.Deny, r.String())
		}
		info.Grants = append(info.Grants, g)
	}
	// The sub-agents' own grants for the session (the memory pass's
	// "yes, always"), each under its scope, as granted: the engine may
	// hold one only from the sub-agent's next decision (ownGrantsHeld).
	s.grants.mu.Lock()
	scopes := slices.Sorted(maps.Keys(s.grants.own))
	for _, sc := range scopes {
		info.Grants = append(info.Grants, PolicyGrant{Source: policy.SourceSession, Scope: sc, Allow: slices.Clone(s.grants.own[sc])})
	}
	s.grants.mu.Unlock()
	return info
}
