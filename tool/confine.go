package tool

// This file is what a box's confinement may cover of the coding tools'
// calls (box.confine, docs/sandbox.md "Fewer questions in a box"): a
// call the policy takes as confined skips a bare ask rule, so what is
// judged here is which calls that may be, from their facts claim and,
// for bash, the line itself. Neither function runs anything.
//
// A box confines what a call can reach outside the project; it does
// not confine the project, which is the user's own directory mounted
// read-write. So a call is confinable only when what it names stays
// out of the paths whose change reaches past the project's source, a
// repository's hooks and config, dax's own config, an environment
// file, CI and editor settings (protectedPath), and, for bash, only
// when its line hides nothing from that reading.

import (
	"encoding/json"
	"path"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// protectedNames are the path components a confined call may not name:
// writing under one changes what runs or is trusted beyond the
// project's source (git's hooks and config, dax's project config, an
// environment file a shell or tool loads, CI, editor tasks). A
// Makefile is not among them: it is source, and so is everything a
// build reads. protectedPrefixes match a component's start: .env,
// .env.local, .envrc.
var (
	protectedNames = []string{
		".git", ".dax", ".github", ".circleci", ".husky", ".vscode", ".idea",
		".gitlab-ci.yml", ".pre-commit-config.yaml",
	}
	protectedPrefixes = []string{".env"}
)

// protectedPath reports whether rel, a path relative to the workspace's
// root with / separators, has a protected component, in any case: the
// box's project may be on a file system that folds it.
func protectedPath(rel string) bool {
	for _, c := range strings.Split(strings.ToLower(rel), "/") {
		if protectedComponent(c) {
			return true
		}
	}
	return false
}

func protectedComponent(c string) bool {
	for _, n := range protectedNames {
		if c == n {
			return true
		}
	}
	for _, p := range protectedPrefixes {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// ConfinablePath reports whether a file tool's call (write, edit), as
// its facts claim says, may be taken as confined: every path it would
// touch, links followed, is inside the workspace, readable as such,
// and not protected. A claim that cannot say where a link leads, or a
// path outside the root, is not.
func ConfinablePath(f agenttool.Facts) bool {
	if f.Calls == nil {
		return false
	}
	for _, c := range f.Calls {
		if c.Tool != "" {
			return false
		}
		if !insidePath(c.Args) {
			return false
		}
	}
	return len(f.Calls) > 0
}

// insidePath reports whether a fact call's {"path": rel} is a path the
// claim read as inside the workspace and not protected.
func insidePath(args json.RawMessage) bool {
	var a struct {
		Path *string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.Path == nil {
		return false
	}
	rel := *a.Path
	if strings.HasPrefix(rel, sentinel) || path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	return !protectedPath(rel)
}

// ReadableBash reports whether a bash call's facts are the analyzer's
// full reading of its line: no subject is the sentinel, so every stage
// was read as a command and its words (BashSubjects). A line the
// analyzer cannot read (a substitution, a quote it does not follow, a
// governed command with arguments it refuses) carries the sentinel.
func ReadableBash(f agenttool.Facts) bool {
	if f.Calls == nil {
		return false
	}
	for _, c := range f.Calls {
		if c.Tool != "" {
			continue
		}
		var a struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(c.Args, &a) != nil || strings.HasPrefix(a.Command, sentinel) {
			return false
		}
	}
	return true
}

// ConfinableBash reports whether a bash call, by its arguments and its
// facts claim, may be taken as confined: the analyzer read the whole
// line (ReadableBash), and what it reads hides nothing and names
// nothing that reaches past the project's source:
//
//   - no expansion, substitution or escape ($, `, <(, >(, \), which can
//     build a name the text does not show;
//   - no stage whose program runs another program or code it is given
//     as text (a shell, eval, env, xargs, find, an interpreter), and no
//     git, which writes the repository's own .git;
//   - no word that is a protected path, the project's root (.), a
//     parent (..), an absolute path, ~, or a glob over dot files;
//   - no redirect or other subject that opens a protected path, links
//     followed.
//
// A line the analyzer could not read asks as it does without a box.
// What a confinable command's own program then does (a test that
// writes a hook, a cp through a link the repository already has) is
// the box's confinement and nothing finer; see docs/sandbox.md.
func ConfinableBash(args json.RawMessage, f agenttool.Facts) bool {
	if !ReadableBash(f) {
		return false
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return false
	}
	line := strings.TrimSpace(in.Command)
	if line == "" || strings.ContainsAny(line, "$`\\\n\r") || strings.Contains(line, "<(") || strings.Contains(line, ">(") {
		return false
	}
	// Quotes do not change what a name is: '.g'it is .git.
	bare := strings.NewReplacer("'", "", `"`, "").Replace(line)
	if namesProtected(bare) {
		return false
	}
	for _, w := range words(bare) {
		if !confinableWord(w) {
			return false
		}
	}
	for _, c := range f.Calls {
		switch c.Tool {
		case "":
			var a struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(c.Args, &a) != nil || !confinableStage(a.Command) {
				return false
			}
		case "write", "read":
			if !insidePath(c.Args) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// unconfinablePrograms run another program, or code given as text, so
// the line does not show what runs; git writes the repository's .git.
var unconfinablePrograms = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "fish": true, "csh": true, "tcsh": true, "busybox": true,
	"eval": true, "exec": true, "source": true, ".": true, "command": true, "builtin": true,
	"env": true, "xargs": true, "nohup": true, "timeout": true, "nice": true, "ionice": true, "stdbuf": true, "setsid": true,
	"sudo": true, "doas": true, "su": true, "runuser": true, "watch": true, "time": true, "unshare": true, "chroot": true, "nsenter": true,
	"script": true, "expect": true, "find": true, "parallel": true,
	"python": true, "python2": true, "python3": true, "pypy": true, "pypy3": true, "perl": true, "ruby": true, "irb": true,
	"node": true, "nodejs": true, "deno": true, "bun": true, "php": true, "lua": true, "luajit": true, "tclsh": true, "wish": true,
	"awk": true, "gawk": true, "mawk": true, "nawk": true, "Rscript": true, "pwsh": true, "powershell": true, "osascript": true,
	"git": true, "ln": true,
}

// confinableStage reports whether a stage's program, after any
// leading NAME=value assignments, is not one that hides what runs.
func confinableStage(stage string) bool {
	for _, w := range strings.Fields(stage) {
		if i := strings.IndexByte(w, '='); i > 0 && !strings.ContainsAny(w[:i], "/-.") {
			continue // an assignment
		}
		return !unconfinablePrograms[path.Base(strings.Trim(w, `'"`))]
	}
	return false
}

// namesProtected reports whether line names a protected component as a
// whole name: one that starts the line or follows a separator, and,
// but for a prefix, ends it or comes before one.
func namesProtected(line string) bool {
	s := strings.ToLower(line)
	for _, n := range protectedNames {
		if hasName(s, n, true) {
			return true
		}
	}
	for _, p := range protectedPrefixes {
		if hasName(s, p, false) {
			return true
		}
	}
	return false
}

func hasName(s, name string, whole bool) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], name)
		if j < 0 {
			return false
		}
		at := i + j
		end := at + len(name)
		left := at == 0 || isSep(s[at-1])
		right := !whole || end == len(s) || isSep(s[end])
		if left && right {
			return true
		}
		i = at + 1
	}
}

func isSep(b byte) bool { return strings.IndexByte(" \t/=:;|&()<>,{}", b) >= 0 }

// words are line's words, cut at spaces and at the shell's operators;
// a word's value after = is its own word (--output=x names x).
func words(line string) []string {
	f := strings.FieldsFunc(line, func(r rune) bool { return strings.ContainsRune(" \t;|&()<>", r) })
	var out []string
	for _, w := range f {
		out = append(out, w)
		if i := strings.LastIndexByte(w, '='); i >= 0 && i < len(w)-1 {
			out = append(out, w[i+1:])
		}
	}
	return out
}

// confinableWord refuses a word that names the root, a parent, a path
// outside the workspace, the home directory or a glob over dot files,
// any of which can reach a protected path without naming it.
func confinableWord(w string) bool {
	switch {
	case w == "." || w == "./" || w == "*/" || w == "/*":
		return false
	case strings.HasPrefix(w, "/"), strings.HasPrefix(w, "~"):
		return false
	case w == ".." || strings.HasPrefix(w, "../") || strings.HasSuffix(w, "/..") || strings.Contains(w, "/../"):
		return false
	case strings.ContainsAny(w, "*?[") && (strings.HasPrefix(w, ".") || strings.Contains(w, "/.")):
		return false
	}
	return true
}
