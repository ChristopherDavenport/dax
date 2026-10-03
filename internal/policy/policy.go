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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"

	"github.com/ChristopherDavenport/dex/internal/config"
	"github.com/ChristopherDavenport/dex/internal/tool"
)

// BuiltinAllow is the allow list dex ships. go test, build, vet and
// list are not on it: they run the repository's code (TestMain, cgo,
// a vet tool, a toolchain the go.mod names). A user who trusts a
// repository allows them in the user config; see the README. Writes
// and bash in general are not on it: they fall to the default, which
// asks.
//
// A bash rule is matched against each stage of the command line on its
// own (see tool.BashSubjects), at a word boundary, so "git status"
// allows `git status -s` and not `git statusx`, and `git status && rm
// -rf x` asks about the rm. For the commands tool.Analyzer governs the
// rule only names them: whether this call's arguments are read-only,
// inside the workspace and free of programs is the analyzer's, and a
// call that fails it asks whatever the rule says.
const BuiltinAllow = "read glob grep ls skill explore memory_search " +
	"bash(git status:*) bash(git diff:*) bash(git log:*) bash(git show:*) " +
	"bash(go version) bash(go env:*) " +
	"bash(git branch:*) bash(git rev-parse:*) bash(git ls-files:*) bash(git remote:*) bash(git blame:*) " +
	"bash(git stash list:*) bash(git tag:*) bash(git describe:*) bash(git shortlog:*) bash(git config:*) " +
	"bash(cat:*) bash(head:*) bash(tail:*) bash(wc:*) bash(grep:*) bash(sort:*) bash(uniq:*) bash(cut:*) bash(cd:*) " +
	"bash(ls:*) bash(pwd)"

// secretPaths are names that look like they hold a credential or a
// key. Reading one asks, whichever tool reads it: Read(...) names read,
// grep, glob and ls, and cat, head, tail, wc, grep and git show of a
// path are decided as a read of it. A pattern here has a form for the
// workspace root and one for any directory below it, since a rule's *
// runs across slashes but does not match nothing before a dot.
var secretPaths = []string{
	".env*", ".npmrc", ".netrc", ".pgpass", ".git-credentials", "id_*", "credentials*", "*.pem", "*.key", "*.p12", "*.pfx",
	"*secret*", ".kube/config", ".docker/config.json", ".aws/**", ".ssh/**", ".aws", ".ssh",
}

// BuiltinAsk is the ask list dex ships: reads of the paths above. It
// ranks with the built-in allow list, so an ask beats the bare read
// allow; a user's allow rule for a path (read(.env), Read(config/.env))
// opens it, because Build gives each such rule a carve-out from this
// list.
func BuiltinAsk() string {
	var b strings.Builder
	for _, s := range secretPaths {
		// ci: marks a pattern matched without regard to case: .ENV and
		// ID_RSA are the same names on a case-folding file system and
		// plausible ones on any other.
		b.WriteString("Read(ci:" + s + ") ")
		if !strings.HasPrefix(s, "*") {
			b.WriteString("Read(ci:*/" + s + ") ")
		}
	}
	return strings.TrimSpace(b.String())
}

var fileTools = map[string]bool{"read": true, "grep": true, "glob": true, "ls": true}

// Matchers are the per-tool specifier matchers: bash by its command,
// the file tools by their path, normalised against the workspace dir
// (a search by the directory it looks in). Rules for a path are written
// relative to the workspace: write(docs/**), read(.env).
func Matchers(dir string, maxFile int64) map[string]agentpolicy.ToolMatcher {
	file := func(def string) agentpolicy.ToolMatcher {
		return agentpolicy.ToolMatcher{Match: pathMatcher, Subjects: tool.PathSubjects(dir, "path", def)}
	}
	return map[string]agentpolicy.ToolMatcher{
		"bash":  {Match: agentpolicy.GlobMatcher("command"), Subjects: tool.BashSubjects(dir, maxFile)},
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
		ask, err := agentpolicy.ParseRules(BuiltinAsk())
		if err != nil {
			return agentpolicy.Policy{}, err
		}
		sets = append(sets, agentpolicy.RuleSet{Source: agentpolicy.Source{Name: SourceBuiltin, Trusted: true, Rank: 1}, Allow: allow, Ask: ask})
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
		if l.src.Trusted {
			// Allowing a path is meant: an allow rule with a specifier
			// for a file tool cancels the built-in ask for it, which
			// precedence alone would not.
			for _, r := range set.Allow {
				if r.Spec != "" && !strings.HasPrefix(r.Spec, "!") && fileTools[strings.ToLower(r.Tool)] {
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

var pathGlob = agentpolicy.GlobMatcher("path")

// pathMatcher is the glob matcher over a call's path, with one
// addition: a specifier that starts with ci: is matched without regard
// to case, the way the built-in secret-path asks are.
func pathMatcher(spec string, args json.RawMessage) bool {
	rest, ci := strings.CutPrefix(spec, "ci:")
	if !ci {
		return pathGlob(spec, args)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return false
	}
	var path string
	if err := json.Unmarshal(m["path"], &path); err != nil {
		return false
	}
	m["path"], _ = json.Marshal(strings.ToLower(path))
	lower, _ := json.Marshal(m)
	return pathGlob(strings.ToLower(rest), lower)
}
