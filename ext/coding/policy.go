package coding

// This file is dax-coding's policy: the rules it ships, the matchers
// that read its tools' calls, and the aliases rules may use for them.
//
// What it ships: the read-only tools run, and so do a few commands that
// only look (git status, git log, cat, ...); every other call of its
// tools, every write and every other command, falls to the policy's
// default, which asks. Reading a secret-looking path asks, whichever
// tool reads it. A rule in the user's config adds to that, and
// "builtin": false drops the allow list; the asks stay.

import (
	"encoding/json"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"

	"github.com/ChristopherDavenport/dax/tool"
)

// allowRules is the allow list dax-coding ships. go test, build, vet and
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
const allowRules = "read glob grep ls " +
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

// askRules is the ask list dax-coding ships: reads of the secret-looking
// paths above. An ask beats the bare read allow; a user's allow rule for
// a path (read(.env), Read(config/.env)) opens it, because the file
// tools are in Lifts and policy.Build gives each such rule a carve-out
// from this list.
func askRules() string {
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

// lifts are the tools whose asks a user's allow rule with a specifier
// lifts: the ones that read.
var lifts = []string{"read", "grep", "glob", "ls"}

// matchers are the per-tool specifier matchers: bash by its command,
// the file tools by their path, normalised against the workspace dir
// (a search by the directory it looks in). Rules for a path are written
// relative to the workspace: write(docs/**), read(.env).
func matchers(f *tool.Files, maxFile int64) map[string]agentpolicy.ToolMatcher {
	file := func(def string) agentpolicy.ToolMatcher {
		return agentpolicy.ToolMatcher{Match: pathMatcher, Subjects: tool.PathSubjects(f, "path", def)}
	}
	return map[string]agentpolicy.ToolMatcher{
		"bash":  {Match: agentpolicy.GlobMatcher("command"), Subjects: tool.BashSubjects(f, maxFile)},
		"read":  file(""),
		"write": file(""),
		"edit":  file(""),
		// A search is matched on where it looks, not on its pattern.
		"glob": file("."),
		"grep": file("."),
		"ls":   file("."),
	}
}

// aliases let rules, and a skill's allowed-tools, name the tools as the
// reference does: Bash, Read, Edit.
var aliases = map[string][]string{
	"Bash":  {"bash"},
	"Read":  {"read", "grep", "glob", "ls"},
	"Write": {"write"},
	"Edit":  {"edit", "write"},
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
