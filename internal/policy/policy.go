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

// BuiltinAllow is the allow list dex ships. go test, build, vet and
// list are not on it: they run the repository's code (TestMain, cgo,
// a vet tool, a toolchain the go.mod names). A user who trusts a
// repository allows them in the user config; see the README. Writes and bash in general
// are not on it: they fall to the default, which asks.
//
// A bash rule is matched against each subcommand of the command line
// on its own (see tool.BashSubjects), at a word boundary, so
// "git status" allows `git status -s` and not `git statusx`, and
// `git status && rm -rf x` asks about the rm.
const BuiltinAllow = "read glob grep ls skill explore memory_search " +
	"bash(git status:*) bash(git diff:*) bash(git log:*) bash(git show:*) " +
	"bash(go version) bash(go env:*) " +
	"bash(ls:*) bash(pwd) bash(cd:*)"

// Matchers are the per-tool specifier matchers: bash by its command,
// the file tools by their path, normalised against the workspace dir
// (a search by the directory it looks in). Rules for a path are written
// relative to the workspace: write(docs/**), read(.env).
func Matchers(dir string) map[string]agentpolicy.ToolMatcher {
	file := func(def string) agentpolicy.ToolMatcher {
		return agentpolicy.ToolMatcher{Match: agentpolicy.GlobMatcher("path"), Subjects: tool.PathSubjects(dir, "path", def)}
	}
	return map[string]agentpolicy.ToolMatcher{
		"bash":  {Match: agentpolicy.GlobMatcher("command"), Subjects: tool.BashSubjects(dir)},
		"read":  file(""),
		"write": file(""),
		"edit":  file(""),
		// A search is matched on where it looks, not on its pattern.
		"glob": file("."),
		"grep": file("."),
		"ls":   file("."),
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
// project's rules. The project's source is untrusted and ranks below
// the user's and the built-in list, so its allow rules are withheld,
// its ask and deny rules apply, and a carve-out in it cannot cancel a
// rule of theirs.
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
		sets = append(sets, agentpolicy.RuleSet{Source: agentpolicy.Source{Name: SourceBuiltin, Trusted: true, Rank: 1}, Allow: allow})
	}
	for _, l := range []struct {
		src   agentpolicy.Source
		rules config.Rules
	}{
		{agentpolicy.Source{Name: SourceUser, Trusted: true, Rank: 2}, s.User},
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
