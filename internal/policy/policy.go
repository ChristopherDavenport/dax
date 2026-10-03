// Package policy builds the agentpolicy rules dex runs under: its
// built-in allow list, the user's rules, and the project's, which are
// not trusted.
//
// The shipped default is: the read-only tools run; so do a few
// commands that only look (git status, go test, ...); every other
// call, which is every write, every other command and every tool an
// MCP server adds, asks. A rule in the config adds to that, and
// "builtin": false drops the allow list so the rules are all there is.
// A deny beats an ask and an ask beats an allow, so the way to make a
// tool the default asks about run unasked is an allow rule, and the way
// to stop an allowed one is an ask or deny rule.
package policy

import (
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"

	"github.com/ChristopherDavenport/dex/internal/config"
	"github.com/ChristopherDavenport/dex/internal/tool"
)

// BuiltinAllow is the allow list dex ships. Writes and bash in general
// are not on it: they fall to the default, which asks.
//
// A bash rule is matched against each subcommand of the command line
// on its own (see tool.BashSubjects), at a word boundary, so
// "git status" allows `git status -s` and not `git statusx`, and
// `git status && rm -rf x` asks about the rm.
const BuiltinAllow = "read glob grep ls skill explore memory_search " +
	"bash(git status:*) bash(git diff:*) bash(git log:*) bash(git show:*) " +
	"bash(go test:*) bash(go build:*) bash(go vet:*) bash(go list:*) bash(go version:*) " +
	"bash(ls:*) bash(pwd) bash(cd:*)"

// Matchers are the per-tool specifier matchers: bash by its command,
// the file tools by their path. A bash command is split into
// subcommands first.
func Matchers() map[string]agentpolicy.ToolMatcher {
	path := agentpolicy.ToolMatcher{Match: agentpolicy.GlobMatcher("path")}
	return map[string]agentpolicy.ToolMatcher{
		"bash":  {Match: agentpolicy.GlobMatcher("command"), Subjects: tool.BashSubjects},
		"read":  path,
		"write": path,
		"edit":  path,
		"glob":  {Match: agentpolicy.GlobMatcher("pattern")},
		"grep":  {Match: agentpolicy.GlobMatcher("pattern")},
		"ls":    path,
	}
}

// Options are the aliases that let rules, and a skill's allowed-tools,
// name the tools as the reference does: Bash, Read, Edit.
func Options() []agentpolicy.Option {
	return []agentpolicy.Option{agentpolicy.WithAliases(map[string][]string{
		"Bash":  {"bash"},
		"Read":  {"read", "grep", "glob", "ls"},
		"Write": {"write"},
		"Edit":  {"edit", "write"},
	})}
}

// Source names the layers in verdicts and in the session's record.
const (
	SourceBuiltin = "dex:builtin"
	SourceUser    = "dex:config"
	SourceProject = "dex:project"
)

// Build merges the built-in allow list with the user's and the
// project's rules. The project's source is untrusted, so its allow
// rules are withheld while its ask and deny rules apply.
func Build(s config.PolicySettings) (agentpolicy.Policy, error) {
	parse := func(what string, rules []string) ([]agentpolicy.Rule, error) {
		out, err := agentpolicy.ParseRules(strings.Join(rules, " "))
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", what, err)
		}
		return out, nil
	}
	var sets []agentpolicy.RuleSet
	if s.Builtin {
		allow, err := agentpolicy.ParseRules(BuiltinAllow)
		if err != nil {
			return agentpolicy.Policy{}, err
		}
		sets = append(sets, agentpolicy.RuleSet{Source: agentpolicy.Source{Name: SourceBuiltin, Trusted: true}, Allow: allow})
	}
	for _, l := range []struct {
		src   agentpolicy.Source
		rules config.Rules
	}{
		{agentpolicy.Source{Name: SourceUser, Trusted: true}, s.User},
		{agentpolicy.Source{Name: SourceProject}, s.Project},
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
