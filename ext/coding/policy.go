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
	"context"
	"encoding/json"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
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
	"bash(ls:*) bash(pwd) " +
	// The background tool's start is decided as bash's call of its line;
	// listing, reading and stopping the session's own processes look
	// at nothing else and run (a policy test says what a compound
	// start does).
	"background(list) background(output) background(stop) " +
	// A cut tool result's whole, from the session's own blobs.
	"read_output"

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

// protectedPaths are what a write or an edit asks about in acceptEdits
// mode: what changes what runs or is trusted beyond the project's
// source (git's hooks and config, dax's project config, an environment
// file a shell or tool loads, CI, editor tasks), as a box's
// confinement takes them (tool/confine.go's protectedNames). A name has
// a form for the workspace root and one below it, each the name and
// what is under it.
var protectedPaths = []string{
	".git", ".dax", ".github", ".circleci", ".husky", ".vscode", ".idea", ".gitlab-ci.yml", ".pre-commit-config.yaml",
}

// acceptEditsRules are dax-coding's rules in acceptEdits mode: write and
// edit run unasked, but for a secret-looking or protected path; bash
// and the rest still ask. Edit names edit and write.
func acceptEditsRules() policy.Rules {
	var ask []string
	for _, s := range secretPaths {
		ask = append(ask, "Edit(ci:"+s+")")
		if !strings.HasPrefix(s, "*") {
			ask = append(ask, "Edit(ci:*/"+s+")")
		}
	}
	for _, n := range protectedPaths {
		ask = append(ask, "Edit(ci:"+n+")", "Edit(ci:"+n+"/**)", "Edit(ci:*/"+n+")", "Edit(ci:*/"+n+"/**)")
	}
	return policy.Rules{Allow: []string{"write", "edit"}, Ask: ask}
}

// grantPrefixes are the programs "yes, always" offers a prefix rule
// for, bash(<program>:*): listing tools with no flag that runs a
// command or writes a file. Any other line is offered exactly as it
// is (exactGrant), never as a prefix: a build or test tool runs
// whatever a later flag names (go test -exec, -toolexec, -vettool;
// make --eval, -f, SHELL=, CC=), as do shells, interpreters, launchers
// and wrappers (uv run, docker run, git rebase --exec, tar
// --to-command), and a deny list of them kept missing some.
var grantPrefixes = map[string]bool{"ls": true, "wc": true, "du": true, "df": true}

// grantPrefix is the allowlisted prefix of a stage's words, "" for
// none.
func grantPrefix(words []string) string {
	if grantPrefixes[words[0]] {
		return words[0]
	}
	return ""
}

// exactGrant is the rule that allows line, one stage whose match is
// match, exactly: "" when the match is not the line as written (the
// rule matches the words joined, so a quoted line would be shown
// unquoted, another line than the one approved), would read as a
// pattern (a *) or a carve-out (a leading !), or does not come back
// from parsing as itself.
func exactGrant(line, match string) string {
	if match == "" || match != strings.TrimSpace(line) || strings.Contains(match, "*") || strings.HasPrefix(match, "!") {
		return ""
	}
	rule := "bash(" + match + ")"
	if r, err := agentpolicy.ParseRules(rule); err != nil || len(r) != 1 || r[0].Spec != match {
		return ""
	}
	return rule
}

// grantRule is the rule "yes, always" grants for a call of dax-coding's
// tools: for bash, a line the analyzer reads (tool.Analyzer's safe
// subset) whose stages, cd aside, all share one allowlisted prefix
// (grantPrefixes) is offered bash(<prefix>:*), so the grant answers
// such lines next time; a line of one stage otherwise is offered
// exactly, when it holds no quoting (exactGrant). For any other tool, and any other line,
// none.
func grantRule(env extension.ToolEnv) func(ctx context.Context, name string, args json.RawMessage) string {
	a := &tool.Analyzer{Files: env.Files, MaxFile: env.MaxReadBytes}
	return func(ctx context.Context, name string, args json.RawMessage) string {
		if name != "bash" {
			return ""
		}
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(args, &in) != nil {
			return ""
		}
		c := a.Check(ctx, in.Command)
		if !c.Parsed {
			return ""
		}
		var stages []tool.StageCheck
		for _, st := range c.Stages {
			if len(st.Words) == 0 || st.Words[0] == "cd" {
				continue
			}
			stages = append(stages, st)
		}
		if len(stages) == 0 {
			return ""
		}
		prefix := grantPrefix(stages[0].Words)
		for _, st := range stages[1:] {
			if grantPrefix(st.Words) != prefix {
				prefix = ""
				break
			}
		}
		if prefix != "" {
			rule := "bash(" + prefix + ":*)"
			if r, err := agentpolicy.ParseRules(rule); err == nil && len(r) == 1 && r[0].Spec == prefix+":*" {
				return rule
			}
			return ""
		}
		if len(c.Stages) == 1 {
			return exactGrant(in.Command, c.Stages[0].Match)
		}
		return ""
	}
}

// lifts are the tools whose asks a user's allow rule with a specifier
// lifts: the ones that read.
var lifts = []string{"read", "grep", "glob", "ls"}

// matchers are how a rule's specifier matches the tools' calls: bash's
// by its command, the file tools' by their path. They match only; what
// a call is matched as, its subjects (bash's stages, a path normalised
// against the workspace and what its links lead to), comes from the
// tool's own facts claim (tool.BashSubjects, tool.PathSubjects), which
// the session reads. Rules for a path are written relative to the
// workspace: write(docs/**), read(.env).
func matchers() map[string]agentpolicy.ToolMatcher {
	path := agentpolicy.ToolMatcher{Match: pathMatcher}
	return map[string]agentpolicy.ToolMatcher{
		"bash": {Match: agentpolicy.GlobMatcher("command")},
		// The background tool by its action; a start's claim is bash's.
		"background": {Match: agentpolicy.GlobMatcher("action")},
		"read":       path,
		"write":      path,
		"edit":       path,
		// A search is matched on where it looks, not on its pattern: its
		// facts name the directory, "." when it names none.
		"glob": path,
		"grep": path,
		"ls":   path,
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
