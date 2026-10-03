package tool

import (
	"path/filepath"
	"regexp"
	"strings"
)

// SafeWords reads a command line that is inside the safe subset and
// returns its words, or reports that it is not.
//
// The subset is one simple command: words of letters, digits and
// _ . / : @ % + , = - (and ^ or ~ after the first byte of a word, for
// HEAD~1 and HEAD^), single-quoted strings, and double-quoted strings
// with no $, backtick or backslash inside, separated by spaces and
// tabs. No separators, no operators, no comments, no expansions, no
// globs, no braces, no escapes, no newline, no non-ASCII bytes. The
// first word may not contain =, which would make it an assignment.
//
// A command inside the subset means to bash exactly what these words
// say, which is why the policy can decide it from them. Everything
// outside it is not wrong, only not decided here: it asks.
func SafeWords(cmd string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
		case c == '\'':
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			body := cmd[i+1 : i+1+j]
			if !plain(body) {
				return nil, false
			}
			cur.WriteString(body)
			inWord = true
			i += j + 1
		case c == '"':
			j := strings.IndexByte(cmd[i+1:], '"')
			if j < 0 {
				return nil, false
			}
			body := cmd[i+1 : i+1+j]
			if !plain(body) || strings.ContainsAny(body, "$`\\") {
				return nil, false
			}
			cur.WriteString(body)
			inWord = true
			i += j + 1
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte("_./:@%+,=-", c) >= 0,
			(c == '^' || c == '~') && inWord:
			cur.WriteByte(c)
			inWord = true
		default:
			return nil, false
		}
	}
	flush()
	if len(words) == 0 || strings.Contains(words[0], "=") {
		return nil, false
	}
	return words, true
}

// plain reports a quoted string of printable ASCII, tab included.
func plain(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '\t' && (c < 0x20 || c > 0x7e) {
			return false
		}
	}
	return true
}

// gitFlags are the flags the read-only git commands take. A flag in
// bare takes no value; one in valued may carry one after =. Anything
// else, --output, --ext-diff, --textconv, -c, -C, --git-dir,
// --work-tree, --exec-path, --no-index, is not read-only, as far as
// this check can tell, and makes the call ask.
var (
	diffBare = set("--stat", "--shortstat", "--numstat", "--name-only", "--name-status", "--summary",
		"--patch", "-p", "-u", "--no-patch", "-s", "--minimal", "--patience", "--histogram", "--no-renames",
		"-M", "--ignore-space-change", "-b", "--ignore-all-space", "-w", "--ignore-space-at-eol",
		"--ignore-blank-lines", "--function-context", "-W", "--full-index", "--binary", "--exit-code",
		"--quiet", "--check", "--compact-summary", "--raw", "-z", "--no-ext-diff", "--no-textconv",
		"--no-color", "-R", "--text", "-a", "--dirstat", "--word-diff", "--find-renames", "--abbrev",
		"--color", "--stat-count")
	diffValued = set("--stat", "--unified", "--color", "--word-diff", "--abbrev", "--find-renames",
		"--diff-filter", "--dirstat", "--stat-width", "--stat-count", "--color-words", "--inter-hunk-context")
	statusBare = set("-s", "--short", "-b", "--branch", "--porcelain", "--long", "--ignored", "-u", "-uno",
		"-unormal", "-uall", "--untracked-files", "--no-renames", "--renames", "-z", "--show-stash",
		"--ahead-behind", "--no-ahead-behind", "-v", "--verbose")
	statusValued = set("--porcelain", "--ignored", "--untracked-files")
	logBare      = set("--oneline", "--graph", "--decorate", "--no-decorate", "--all", "--branches", "--tags",
		"--remotes", "-i", "--regexp-ignore-case", "-E", "--extended-regexp", "-F", "--fixed-strings",
		"--all-match", "--invert-grep", "--abbrev-commit", "--no-abbrev-commit", "--relative-date",
		"--reverse", "--merges", "--no-merges", "--first-parent", "--follow", "--topo-order", "--date-order",
		"--author-date-order", "--left-right", "--cherry-pick", "--cherry", "--no-walk", "--parents",
		"--children", "--source", "-m", "--stat", "--cached")
	logValued = set("--decorate", "--max-count", "--skip", "--since", "--until", "--after", "--before",
		"--author", "--committer", "--grep", "--pretty", "--format", "--date", "--abbrev-commit")
	numFlag = regexp.MustCompile(`^-(\d+|[nUM]\d+)$`)
)

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

var lsFlags = regexp.MustCompile(`^-[aAlh1dFtrSsiRkp]+$`)
var lsLong = set("--all", "--almost-all", "--human-readable", "--classify", "--directory", "--recursive",
	"--reverse", "--size", "--inode")

var upperName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// ReadOnlyArgs reports whether a command of the safe subset is one of the
// read-only invocations dex allows without asking, or is not one of
// the commands this check governs at all (and so is for the rules to
// decide). It returns false for a governed command with an argument it
// cannot show is read-only: a flag outside the allowlist, a path that
// leaves dir, or a global option before the git subcommand.
func ReadOnlyArgs(words []string, dir string) bool {
	switch words[0] {
	case "git":
		if len(words) == 1 {
			return true
		}
		if strings.HasPrefix(words[1], "-") {
			return false // -c, -C, --git-dir, --exec-path ...
		}
		var bare, valued map[string]bool
		switch words[1] {
		case "status":
			bare, valued = statusBare, statusValued
		case "diff":
			bare, valued = merge(diffBare), diffValued
			bare["--cached"], bare["--staged"], bare["--merge-base"] = true, true, true
		case "log", "show":
			bare, valued = merge(diffBare, logBare), merge(diffValued, logValued)
		default:
			return true
		}
		for _, a := range words[2:] {
			if a == "--" {
				continue
			}
			if strings.HasPrefix(a, "-") {
				name, val, hasVal := strings.Cut(a, "=")
				switch {
				case numFlag.MatchString(a):
				case hasVal && valued[name] && !strings.Contains(val, "%G"):
				case !hasVal && bare[name]:
				default:
					return false
				}
				continue
			}
			if !inWorkspace(a, dir) {
				return false
			}
		}
		return true
	case "ls":
		for _, a := range words[1:] {
			switch {
			case a == "--":
			case strings.HasPrefix(a, "--"):
				if !lsLong[a] {
					return false
				}
			case strings.HasPrefix(a, "-"):
				if !lsFlags.MatchString(a) {
					return false
				}
			case !inWorkspace(a, dir):
				return false
			}
		}
		return true
	case "pwd":
		return len(words) == 1
	case "go":
		if len(words) == 1 {
			return true
		}
		switch words[1] {
		case "version":
			return len(words) == 2
		case "env":
			for _, a := range words[2:] {
				if a != "-json" && !upperName.MatchString(a) {
					return false // -w, -u, -changed, a NAME=value
				}
			}
		}
		return true
	}
	return true
}

func merge(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range sets {
		for k := range s {
			out[k] = true
		}
	}
	return out
}

// inWorkspace reports whether a path argument, absolute or relative
// to dir, stays inside dir after cleaning and, where it exists, after
// its symbolic links are resolved. ~ is never a path here: bash would
// have expanded it, but a quoted one is a file named ~ and either way
// it is not the workspace.
func inWorkspace(arg, dir string) bool {
	if strings.HasPrefix(arg, "~") {
		return false
	}
	p := arg
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)
	roots := []string{dir}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		roots = append(roots, r)
	}
	within := func(p string) bool {
		for _, r := range roots {
			if rel, err := filepath.Rel(r, p); err == nil && local(rel) {
				return true
			}
		}
		return false
	}
	if !within(p) {
		return false
	}
	if real, err := filepath.EvalSymlinks(p); err == nil && !within(real) {
		return false
	}
	return true
}

// GitEnv is the environment the bash tool adds to every command so that
// the repository's own configuration cannot run a program under git
// status, diff, log or show: no fsmonitor hook, no pager, no ssh
// command, no hooks directory, no prompt on the terminal. An external
// diff cannot be switched off by configuration (an empty value is run
// as a command), so the bash tool adds --no-ext-diff and --no-textconv
// to a read-only git diff, log or show itself; see ReadOnlyGit.
func GitEnv() []string {
	return []string{
		"GIT_CONFIG_COUNT=8",
		"GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.pager", "GIT_CONFIG_VALUE_1=cat",
		"GIT_CONFIG_KEY_2=core.sshCommand", "GIT_CONFIG_VALUE_2=ssh",
		"GIT_CONFIG_KEY_3=core.hooksPath", "GIT_CONFIG_VALUE_3=/dev/null",
		"GIT_CONFIG_KEY_4=gpg.program", "GIT_CONFIG_VALUE_4=/bin/false",
		"GIT_CONFIG_KEY_5=gpg.openpgp.program", "GIT_CONFIG_VALUE_5=/bin/false",
		"GIT_CONFIG_KEY_6=gpg.x509.program", "GIT_CONFIG_VALUE_6=/bin/false",
		"GIT_CONFIG_KEY_7=gpg.ssh.program", "GIT_CONFIG_VALUE_7=/bin/false",
		"GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "PAGER=cat", "GIT_OPTIONAL_LOCKS=0",
	}
}

// ReadOnlyGit reports whether words is a read-only git diff, log or
// show in the safe subset, the commands whose output the bash tool
// runs with --no-ext-diff --no-textconv added.
func ReadOnlyGit(words []string, dir string) bool {
	if len(words) < 2 || words[0] != "git" || !ReadOnlyArgs(words, dir) {
		return false
	}
	switch words[1] {
	case "diff", "log", "show":
		return true
	}
	return false
}
