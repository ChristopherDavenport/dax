package tool

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Analyzer decides which command lines dex runs without asking. It is
// an allow-list: a command line is auto-allowed only if it parses in
// the safe subset (parsePlan) and every stage is one of the commands
// below, run with arguments that can be shown to be read-only and to
// stay in the workspace. A stage whose command is one of these but
// whose arguments fail is not merely unlisted: it is a stage no allow
// rule may cover, because a rule for "git log" must not allow
// `git log --output=x`.
type Analyzer struct {
	// Dir is the workspace root.
	Dir string
	// MaxFile is the largest file cat, head, tail, wc and grep may be
	// given; zero is DefaultMaxRead.
	MaxFile int64
	// ConfigKey reads the exec-bearing key a repository's git
	// configuration names in dir; nil is ExecConfigKey. strict is set for
	// a line that will run as typed, without the auto-allow environment.
	ConfigKey func(ctx context.Context, dir string, strict bool) (string, error)
}

// StageCheck is the verdict on one stage of a command line.
type StageCheck struct {
	// Words are the stage's words as text; Text what a person is asked
	// about; Match what an allow rule is matched against.
	Words       []string
	Text, Match string
	// Governed is set for a command this check has opinions about.
	Governed bool
	// OK is whether the arguments of a governed command are acceptable.
	OK bool
	// Reads are the files the stage reads, relative to the workspace,
	// for the policy's path rules.
	Reads []string
	// Why says what was wrong, or, for a git stage, the key of the
	// repository's configuration that names a program.
	Why string
}

// Check is the verdict on a command line.
type Check struct {
	// Parsed is whether the line is in the safe subset. When it is not,
	// Stages is empty and nothing is auto-allowed.
	Parsed bool
	Stages []StageCheck
	// Auto is whether the whole line runs without asking.
	Auto bool
	plan *plan
}

// Render is the command bash is run with for an auto-allowed line.
func (c *Check) Render() string { return c.plan.render() }

var (
	numArg   = regexp.MustCompile(`^\d+$`)
	signed   = regexp.MustCompile(`^[+-]?\d+$`)
	listArg  = regexp.MustCompile(`^[0-9,-]+$`)
	numFlag  = regexp.MustCompile(`^-(\d+|[nUM]\d+)$`)
	attached = regexp.MustCompile(`^-[SGL].+$`)
)

func set2(a map[string]bool, more ...string) map[string]bool {
	m := map[string]bool{}
	for k := range a {
		m[k] = true
	}
	for _, k := range more {
		m[k] = true
	}
	return m
}

func union(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range sets {
		for k := range s {
			out[k] = true
		}
	}
	return out
}

// gitSpec says which flags a read-only git subcommand takes. bare flags
// take no value; valued flags take one after =; spaced flags take the
// next word; letters are the short flags that may be combined (-sb). A
// flag outside these, --output, --ext-diff, --textconv, -c, -C,
// --git-dir, --no-index, --contents, is not read-only as far as this
// can tell.
type gitSpec struct {
	bare, valued, spaced map[string]bool
	letters              string
	// positional is whether the subcommand takes revisions or paths at
	// all, and listOnly that it takes them only with one of list.
	positional bool
	listFlags  []string
}

var (
	diffBare = set("--stat", "--shortstat", "--numstat", "--name-only", "--name-status", "--summary",
		"--patch", "-p", "-u", "--no-patch", "-s", "--minimal", "--patience", "--histogram", "--no-renames",
		"-M", "--ignore-space-change", "-b", "--ignore-all-space", "-w", "--ignore-space-at-eol",
		"--ignore-blank-lines", "--function-context", "-W", "--full-index", "--binary", "--exit-code",
		"--quiet", "--check", "--compact-summary", "--raw", "-z", "--no-ext-diff", "--no-textconv",
		"--no-color", "-R", "--text", "-a", "--dirstat", "--word-diff", "--find-renames", "--abbrev",
		"--color", "--stat-count", "--color-moved", "--color-words")
	diffValued = set("--stat", "--unified", "--color", "--word-diff", "--abbrev", "--find-renames",
		"--diff-filter", "--dirstat", "--stat-width", "--stat-count", "--color-words", "--inter-hunk-context",
		"--color-moved", "--color-moved-ws")
	logBare = set("--oneline", "--graph", "--decorate", "--no-decorate", "--all", "--branches", "--tags",
		"--remotes", "-i", "--regexp-ignore-case", "-E", "--extended-regexp", "-F", "--fixed-strings",
		"--all-match", "--invert-grep", "--abbrev-commit", "--no-abbrev-commit", "--relative-date",
		"--reverse", "--merges", "--no-merges", "--first-parent", "--follow", "--topo-order", "--date-order",
		"--author-date-order", "--left-right", "--cherry-pick", "--cherry", "--no-walk", "--parents",
		"--children", "--source", "-m", "--stat", "--cached")
	logValued = set("--decorate", "--max-count", "--skip", "--since", "--until", "--after", "--before",
		"--author", "--committer", "--grep", "--pretty", "--format", "--date", "--abbrev-commit")
	logSpaced = set("-n", "-U", "-S", "-G", "--author", "--committer", "--grep", "--since", "--until",
		"--after", "--before", "--max-count", "--skip")

	gitSpecs = map[string]gitSpec{
		"status": {
			bare: set("-s", "--short", "-b", "--branch", "--porcelain", "--long", "--ignored", "-u", "-uno",
				"-unormal", "-uall", "--untracked-files", "--no-renames", "--renames", "-z", "--show-stash",
				"--ahead-behind", "--no-ahead-behind", "-v", "--verbose"),
			valued:     set("--porcelain", "--ignored", "--untracked-files"),
			letters:    "sbvz",
			positional: true,
		},
		"diff": {
			bare:       set2(diffBare, "--cached", "--staged", "--merge-base"),
			valued:     diffValued,
			spaced:     set("-U", "-S", "-G"),
			letters:    "pusbwRWzaM",
			positional: true,
		},
		"log": {
			bare:       union(diffBare, logBare),
			valued:     union(diffValued, logValued),
			spaced:     logSpaced,
			letters:    "pusbwRWzaMiEFm",
			positional: true,
		},
		"branch": {
			bare: set("-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose", "--list", "--no-color", "--color",
				"--show-current", "-i", "--ignore-case", "--merged", "--no-merged", "--contains", "--no-contains",
				"--column", "--no-column"),
			valued: set("--color", "--merged", "--no-merged", "--contains", "--no-contains", "--sort", "--format",
				"--points-at", "--abbrev", "--column"),
			spaced:     set("--merged", "--no-merged", "--contains", "--no-contains", "--sort", "--points-at"),
			letters:    "arvi",
			positional: true, listFlags: []string{"--list"},
		},
		"rev-parse": {
			bare: set("--show-toplevel", "--git-dir", "--absolute-git-dir", "--show-prefix", "--show-cdup",
				"--is-inside-work-tree", "--is-inside-git-dir", "--is-bare-repository", "--is-shallow-repository",
				"--abbrev-ref", "--symbolic", "--symbolic-full-name", "--verify", "-q", "--quiet", "--short",
				"--all", "--branches", "--tags", "--remotes"),
			valued:     set("--short", "--abbrev-ref", "--abbrev"),
			letters:    "q",
			positional: true,
		},
		"ls-files": {
			bare: set("-c", "--cached", "-d", "--deleted", "-m", "--modified", "-o", "--others", "-i", "--ignored",
				"-s", "--stage", "-u", "--unmerged", "-k", "--killed", "-z", "-t", "-v", "--full-name",
				"--exclude-standard", "--error-unmatch", "--eol", "--deduplicate", "--no-empty-directory",
				"--directory", "--abbrev"),
			valued:     set("--abbrev", "--format", "--exclude"),
			letters:    "cdmoiskzutv",
			positional: true,
		},
		"blame": {
			bare: set("-w", "-M", "-s", "-e", "-p", "--porcelain", "--line-porcelain", "--incremental", "-l", "-t",
				"-f", "--show-name", "-n", "--show-number", "--root", "-b", "--show-email"),
			valued:     set("--abbrev", "--date", "--since"),
			spaced:     set("-L"),
			letters:    "wsepltfnb",
			positional: true,
		},
		"tag": {
			bare: set("-l", "--list", "-i", "--ignore-case", "--column", "--no-column", "--contains", "--merged",
				"--no-merged", "--points-at", "--no-color", "--color"),
			valued: set("--sort", "--format", "--contains", "--no-contains", "--merged", "--no-merged", "--points-at",
				"--color", "--abbrev", "--column"),
			spaced:     set("--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--sort"),
			letters:    "li",
			positional: true, listFlags: []string{"-l", "--list"},
		},
		"describe": {
			bare: set("--tags", "--all", "--long", "--always", "--dirty", "--broken", "--contains", "--exact-match",
				"--first-parent"),
			valued:     set("--abbrev", "--match", "--exclude", "--candidates", "--dirty", "--broken"),
			spaced:     set("--match", "--exclude", "--candidates", "--abbrev"),
			positional: true,
		},
		"shortlog": {
			bare: set("-n", "--numbered", "-s", "--summary", "-e", "--email", "-c", "--committer", "--no-merges",
				"--merges"),
			valued:     set("--group", "--since", "--until", "--after", "--before", "--format"),
			spaced:     set("--since", "--until", "--after", "--before", "--group"),
			letters:    "nsec",
			positional: true,
		},
	}
)

func init() {
	// show takes what log takes.
	gitSpecs["show"] = gitSpecs["log"]
}

// gitArgsOK reports whether the arguments after a git subcommand are
// read-only, as gitSpec describes, and every revision or path names
// something inside the workspace.
func gitArgsOK(sub string, args []string, cwd, root string) (bool, string, []string) {
	var paths []string
	spec := gitSpecs[sub]
	list := len(spec.listFlags) == 0
	for _, a := range args {
		for _, f := range spec.listFlags {
			if a == f {
				list = true
			}
		}
	}
	afterDD := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case afterDD || !strings.HasPrefix(a, "-") || a == "-":
			if !spec.positional || !list {
				return false, "a name where " + sub + " takes none", nil
			}
			// rev:path, :path, :/path and :(magic) name what is in the
			// repository, which may be above the workspace.
			if strings.Contains(a, ":") || !inWorkspaceFrom(a, cwd, root) {
				return false, "the argument " + a + " may reach outside the workspace", nil
			}
			paths = append(paths, a)
		case a == "--":
			afterDD = true
		case numFlag.MatchString(a) && (spec.spaced["-n"] || spec.spaced["-U"]):
		case attached.MatchString(a) && (spec.spaced[a[:2]]):
		default:
			name, val, hasVal := strings.Cut(a, "=")
			// A flag that takes a value takes the next word when there is
			// one that is not a flag: --merged main, not --merged and a name.
			takesNext := !hasVal && spec.spaced[name] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-")
			switch {
			case hasVal && spec.valued[name] && !strings.Contains(val, "%G"):
			case takesNext:
				if strings.Contains(args[i+1], "%G") {
					return false, "the value of " + a + " asks git to run gpg", nil
				}
				i++
			case !hasVal && spec.bare[name]:
			case !hasVal && spec.spaced[name]:
				if i+1 >= len(args) || strings.Contains(args[i+1], "%G") {
					return false, "the flag " + a + " needs a value", nil
				}
				i++
			case !hasVal && combined(a, spec.letters):
			default:
				return false, "the flag " + a + " is not known to be read-only", nil
			}
		}
	}
	return true, "", paths
}

// combined reports whether a is -xyz with every letter a short flag
// that takes no value, as in git status -sb.
func combined(a, letters string) bool {
	if len(a) < 3 || a[0] != '-' || a[1] == '-' {
		return false
	}
	for _, r := range a[1:] {
		if !strings.ContainsRune(letters, r) {
			return false
		}
	}
	return true
}

// gitStage checks a git stage: the subcommand and its arguments.
func gitStage(w []string, cwd, root string) (ok bool, why string, paths []string) {
	if len(w) == 1 {
		return true, "", nil
	}
	sub := w[1]
	if strings.HasPrefix(sub, "-") {
		return false, "git " + sub + " before the subcommand (-c, -C, --git-dir ...)", nil
	}
	switch sub {
	case "remote":
		if len(w) == 2 || len(w) == 3 && (w[2] == "-v" || w[2] == "--verbose") {
			return true, "", nil
		}
		return false, "git remote with more than -v", nil
	case "stash":
		if len(w) < 3 || w[2] != "list" {
			return false, "git stash other than list", nil
		}
		ok, why := gitArgsOKNoPos(w[3:], cwd, root)
		return ok, why, nil
	case "config":
		ok, why := gitConfigArgs(w[2:])
		return ok, why, nil
	}
	if _, known := gitSpecs[sub]; !known {
		return true, "", nil // not a command this check governs
	}
	ok, why, paths = gitArgsOK(sub, w[2:], cwd, root)
	if sub != "diff" && sub != "log" && sub != "show" && sub != "blame" {
		paths = nil // names and not contents: ls-files, status, tag ...
	}
	return ok, why, paths
}

// gitArgsOKNoPos is the log flags with no revisions or paths, for
// git stash list.
func gitArgsOKNoPos(args []string, cwd, root string) (bool, string) {
	spec := gitSpecs["log"]
	spec.positional = false
	saved := gitSpecs["log"]
	gitSpecs["log"] = spec
	defer func() { gitSpecs["log"] = saved }()
	ok, why, _ := gitArgsOK("log", args, cwd, root)
	return ok, why
}

var configFlags = set("--get", "--get-all", "--show-origin", "--show-scope", "--local", "--worktree",
	"-z", "--null", "--includes", "--no-includes")

// configKeys are the keys git config may read unasked: ones that name
// no secret. http.*, credential.*, url.*.insteadOf, and anything
// else, which can hold an Authorization header, a token or a program,
// ask. The urls of remotes are here because the output of an
// auto-allowed command has the userinfo of a URL taken out.
var configKeys = regexp.MustCompile(`(?i)^(` +
	`user\.(name|email)|core\.(autocrlf|filemode|ignorecase|bare|eol|safecrlf)|init\.defaultbranch` +
	`|pull\.(rebase|ff)|push\.(default|autosetupremote)|merge\.(ff|conflictstyle)|diff\.(algorithm|renames)` +
	`|commit\.gpgsign|tag\.gpgsign|fetch\.prune|branch\.autosetupmerge` +
	`|branch\..+\.(remote|merge|rebase)|remote\..+\.(url|pushurl|fetch|push)` +
	`)$`)

// gitConfigArgs allows git config only to read one named key from the
// allowlist: --get or --get-all and the key, nothing that lists, no
// pattern, and no --global or --system, which reach outside the
// repository.
func gitConfigArgs(args []string) (bool, string) {
	var reading bool
	var pos []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "-"):
			if !configFlags[a] {
				return false, "git config " + a + " is not a plain read of a named key"
			}
			if a == "--get" || a == "--get-all" {
				reading = true
			}
		default:
			pos = append(pos, a)
		}
	}
	if reading && len(pos) == 1 && configKeys.MatchString(pos[0]) {
		return true, ""
	}
	return false, "git config other than reading a key that names no secret"
}

var (
	lsBare   = regexp.MustCompile(`^-[aAlh1dFtrSsiRkp]+$`)
	lsLong   = set("--all", "--almost-all", "--human-readable", "--classify", "--directory", "--recursive", "--reverse", "--size", "--inode", "--color", "--group-directories-first")
	lsValued = set("--color", "--time-style", "--sort")
	lsValues = regexp.MustCompile(`^[A-Za-z+-]+$`)
)

// lsStage checks ls: listing flags, and paths inside the workspace; a
// glob is expanded here, in the workspace, for the check.
func lsStage(ws []word, cwd, root string) (bool, string) {
	for _, w := range ws[1:] {
		a := w.text
		switch {
		case w.glob:
			if ok, why := globInside(a, cwd, root); !ok {
				return false, why
			}
		case a == "--":
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a, "=")
			if hasVal && (!lsValued[name] || !lsValues.MatchString(val)) || !hasVal && !lsLong[name] {
				return false, "the flag " + a + " is not a listing flag"
			}
		case strings.HasPrefix(a, "-"):
			if !lsBare.MatchString(a) {
				return false, "the flag " + a + " is not a listing flag"
			}
		case !inWorkspaceFrom(a, cwd, root):
			return false, "the path " + a + " is outside the workspace"
		}
	}
	return true, ""
}

// maxGlobMatches bounds a glob the check expands.
const maxGlobMatches = 5000

// globInside expands an ls glob in the workspace and requires every
// match to be inside it, links resolved. The pattern may not leave the
// directory it starts in or name a character class.
func globInside(pattern, cwd, root string) (bool, string) {
	if strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, "~") {
		return false, "the glob " + pattern + " is not relative"
	}
	for _, c := range strings.Split(pattern, "/") {
		// bash expands .* to include .. , which the file system's glob
		// does not list, so a pattern that starts at a dot is not checked.
		if c == ".." || strings.HasPrefix(c, ".") && c != "." {
			return false, "the glob " + pattern + " has a dot component"
		}
	}
	matches, err := globFS(os.DirFS(cwd), pattern)
	if err != nil || len(matches) > maxGlobMatches {
		return false, "the glob " + pattern + " could not be checked"
	}
	for _, m := range matches {
		if !inWorkspaceFrom(m, cwd, root) {
			return false, "the glob " + pattern + " reaches outside the workspace"
		}
	}
	return true, ""
}

// globFS expands pattern in fsys both ways bash might: by character,
// as in a UTF-8 locale, and by byte, as in the C locale, where ? is
// one byte of a multibyte name. The result is the union, so every
// file bash could match is one that gets checked.
func globFS(fsys fs.FS, pattern string) ([]string, error) {
	pattern = path.Clean(pattern)
	byRune, err := fs.Glob(fsys, pattern)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, m := range byRune {
		seen[m] = true
	}
	out := byRune
	for _, m := range globBytes(fsys, strings.Split(pattern, "/"), ".") {
		if !seen[m] {
			out = append(out, m)
		}
	}
	return out, nil
}

// globBytes matches path components against a pattern in which * is
// any run of bytes and ? is one byte.
func globBytes(fsys fs.FS, comps []string, dir string) []string {
	if len(comps) == 0 {
		return nil
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !byteMatch(comps[0], e.Name()) {
			continue
		}
		p := e.Name()
		if dir != "." {
			p = dir + "/" + p
		}
		if len(comps) == 1 {
			out = append(out, p)
		} else if e.IsDir() {
			out = append(out, globBytes(fsys, comps[1:], p)...)
		}
	}
	return out
}

func byteMatch(pat, name string) bool {
	for len(pat) > 0 {
		switch pat[0] {
		case '*':
			for len(pat) > 0 && pat[0] == '*' {
				pat = pat[1:]
			}
			if pat == "" {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if byteMatch(pat, name[i:]) {
					return true
				}
			}
			return false
		case '?':
			if name == "" {
				return false
			}
		default:
			if name == "" || name[0] != pat[0] {
				return false
			}
		}
		pat, name = pat[1:], name[1:]
	}
	return name == ""
}

// fileCmd describes cat, head, tail, wc and grep run on files.
type fileCmd struct {
	bare    map[string]bool
	letters string
	// spaced flags take the next word, which must match.
	spaced map[string]*regexp.Regexp
	// pattern is whether the first positional word is not a file.
	pattern bool
	// valuedLong flags take =value matching numbers.
	valuedLong map[string]*regexp.Regexp
}

var fileCmds = map[string]fileCmd{
	"cat":  {bare: set("-n", "-b", "-s", "-E", "-T", "-A", "-v"), letters: "nbsETAv"},
	"head": {bare: set("-q", "-v"), letters: "qv", spaced: map[string]*regexp.Regexp{"-n": signed, "-c": signed}, valuedLong: map[string]*regexp.Regexp{"--lines": signed, "--bytes": signed}},
	"tail": {bare: set("-q", "-v"), letters: "qv", spaced: map[string]*regexp.Regexp{"-n": signed, "-c": signed}, valuedLong: map[string]*regexp.Regexp{"--lines": signed, "--bytes": signed}},
	"wc":   {bare: set("-l", "-w", "-c", "-m", "-L"), letters: "lwcmL"},
	"grep": {bare: set("-i", "-v", "-c", "-n", "-E", "-F", "-w", "-x", "-o", "-h", "-H", "-l", "-L", "-q", "-s", "-a", "-I"),
		letters: "ivcnEFwxohHlLqsaI",
		spaced:  map[string]*regexp.Regexp{"-m": numArg, "-A": numArg, "-B": numArg, "-C": numArg},
		pattern: true},
}

var dashNum = regexp.MustCompile(`^-\d+$`)

// fileStage checks cat, head, tail, wc and grep with files: flags from
// a short list, and every file an in-workspace regular file no larger
// than max. With needFiles false (a later stage of a pipeline) there
// must be no file at all.
func fileStage(name string, ws []word, cwd, root string, max int64, needFiles bool) (bool, string, []string) {
	spec := fileCmds[name]
	var pos []string
	afterDD := false
	haveE := false
	args := ws[1:]
	for i := 0; i < len(args); i++ {
		a := args[i].text
		if args[i].glob {
			return false, name + " with a glob", nil
		}
		switch {
		case afterDD || !strings.HasPrefix(a, "-") || a == "-":
			if a == "-" {
				return false, name + " reading standard input", nil
			}
			pos = append(pos, a)
		case a == "--":
			afterDD = true
		case spec.bare[a]:
		case dashNum.MatchString(a) && (name == "head" || name == "tail" || name == "grep"):
		case spec.spaced[a] != nil:
			if i+1 >= len(args) || !spec.spaced[a].MatchString(args[i+1].text) {
				return false, "the flag " + a + " needs a number", nil
			}
			i++
		case name == "grep" && a == "-e":
			if i+1 >= len(args) {
				return false, "-e needs a pattern", nil
			}
			haveE = true
			i++
		case strings.Contains(a, "="):
			n, v, _ := strings.Cut(a, "=")
			if re := spec.valuedLong[n]; re == nil || !re.MatchString(v) {
				return false, "the flag " + a + " is not known to be read-only", nil
			}
		case combined(a, spec.letters):
		default:
			return false, "the flag " + a + " is not known to be read-only", nil
		}
	}
	if spec.pattern && !haveE {
		if len(pos) == 0 {
			return false, "grep needs a pattern", nil
		}
		pos = pos[1:]
	}
	if !needFiles {
		if len(pos) > 0 {
			return false, name + " with a file in a pipeline", nil
		}
		return true, "", nil
	}
	if len(pos) == 0 || len(pos) > 20 {
		return false, name + " needs one to twenty files", nil
	}
	if max <= 0 {
		max = DefaultMaxRead
	}
	for _, f := range pos {
		if !inWorkspaceFrom(f, cwd, root) {
			return false, "the file " + f + " is outside the workspace", nil
		}
		p := f
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		fi, err := os.Stat(p)
		switch {
		case os.IsNotExist(err): // cat of nothing is an error, not a read
		case err != nil || !fi.Mode().IsRegular():
			return false, "the file " + f + " is not a regular file", nil
		case fi.Size() > max:
			return false, "the file " + f + " is larger than the read limit", nil
		}
	}
	return true, "", pos
}

// filterCmds are the stages allowed after a pipe: they read standard
// input and take no file.
var filterCmds = map[string]fileCmd{
	"sort": {bare: set("-r", "-n", "-u", "-f", "-h", "-V", "-b", "-d"), letters: "rnufhVbd",
		spaced: map[string]*regexp.Regexp{"-k": regexp.MustCompile(`^[0-9.,nrb]+$`), "-t": regexp.MustCompile(`^.$`)}},
	"uniq": {bare: set("-c", "-d", "-u", "-i"), letters: "cdui",
		spaced: map[string]*regexp.Regexp{"-f": numArg, "-s": numArg, "-w": numArg}},
	"cut": {bare: set("--complement", "-s"), letters: "s",
		spaced: map[string]*regexp.Regexp{"-d": regexp.MustCompile(`^.$`), "-f": listArg, "-c": listArg, "-b": listArg}},
}

// filterStage checks a stage after a pipe.
func filterStage(name string, ws []word, cwd, root string, max int64) (bool, string) {
	if spec, ok := filterCmds[name]; ok {
		args := ws[1:]
		for i := 0; i < len(args); i++ {
			a := args[i].text
			switch {
			case args[i].glob:
				return false, name + " with a glob"
			case spec.bare[a]:
			case spec.spaced[a] != nil:
				if i+1 >= len(args) || !spec.spaced[a].MatchString(args[i+1].text) {
					return false, "the flag " + a + " needs a value"
				}
				i++
			case name == "cut" && len(a) > 2 && (a[:2] == "-d" || a[:2] == "-f" || a[:2] == "-c") && !strings.HasPrefix(a, "--"):
				// -d, -f1-3: attached values.
				if a[:2] == "-d" && len(a) != 3 || a[:2] != "-d" && !listArg.MatchString(a[2:]) {
					return false, "the flag " + a + " is not known to be read-only"
				}
			case combined(a, spec.letters):
			default:
				return false, "cut, sort and uniq take no file after a pipe, and only listed flags (" + a + ")"
			}
		}
		return true, ""
	}
	switch name {
	case "head", "tail", "wc", "grep":
		ok, why, _ := fileStage(name, ws, cwd, root, max, false)
		return ok, why
	}
	return false, name + " is not allowed after a pipe"
}

var governedFirst = set("git", "ls", "pwd", "go", "cat", "head", "tail", "wc", "grep", "cd")
var governedLater = set("head", "tail", "wc", "grep", "sort", "uniq", "cut")

// stageFirst checks the first stage of a pipeline.
func (a *Analyzer) stageFirst(ws []word, cwd string) (governed, ok bool, why string, reads []string) {
	name := ws[0].text
	w := make([]string, len(ws))
	for i := range ws {
		w[i] = ws[i].text
	}
	hasGlob := false
	for _, x := range ws {
		hasGlob = hasGlob || x.glob
	}
	if hasGlob && name != "ls" {
		return governedFirst[name] || governedLater[name], false, name + " with a glob", nil
	}
	switch name {
	case "git":
		ok, why, paths := gitStage(w, cwd, a.Dir)
		return true, ok, why, a.rels(cwd, paths)
	case "ls":
		ok, why := lsStage(ws, cwd, a.Dir)
		return true, ok, why, nil
	case "pwd":
		return true, len(w) == 1, "pwd takes no arguments", nil
	case "go":
		ok := goStage(w)
		return true, ok, "go other than version and env NAME", nil
	case "cat", "head", "tail", "wc", "grep":
		ok, why, files := fileStage(name, ws, cwd, a.Dir, a.MaxFile, true)
		return true, ok, why, a.rels(cwd, files)
	case "sort", "uniq", "cut":
		return true, false, name + " reads a file only after a pipe", nil
	}
	return false, true, "", nil
}

// rels turns the paths a stage reads into names relative to the
// workspace root, for the policy's path rules; one that leaves is left
// as it is, since the stage was refused for it already.
func (a *Analyzer) rels(cwd string, paths []string) []string {
	real, _ := filepath.EvalSymlinks(a.Dir)
	var out []string
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if rel, ok := NormalizePath(a.Dir, real, p); ok {
			out = append(out, rel)
		}
	}
	return out
}

// goStage allows go version and go env with named variables.
func goStage(w []string) bool {
	if len(w) < 2 {
		return true // a bare go prints help; no rule names it
	}
	switch w[1] {
	case "version":
		return len(w) == 2
	case "env":
		names := 0
		for _, a := range w[2:] {
			switch {
			case a == "-json":
			case upperName.MatchString(a):
				names++
			default:
				return false
			}
		}
		return names > 0
	}
	return true // not a command this check governs
}

var upperName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// autoName says whether a governed first-stage command is one that
// can be auto-allowed at all, as opposed to one the rules decide.
func autoFirst(w []string) bool {
	switch w[0] {
	case "ls", "pwd", "cat", "head", "tail", "wc", "grep":
		return true
	case "git":
		if len(w) < 2 {
			return false
		}
		if _, ok := gitSpecs[w[1]]; ok {
			return true
		}
		return w[1] == "remote" || w[1] == "stash" || w[1] == "config"
	case "go":
		return len(w) > 1 && (w[1] == "version" || w[1] == "env")
	}
	return false
}

// checkCd checks a cd stage and returns the directory it leaves the
// following stages in.
func (a *Analyzer) checkCd(st stage, cwd string) (next string, ok bool, why string) {
	if len(st.words) != 2 || st.words[1].glob || len(st.redirects) > 0 || strings.HasPrefix(st.words[1].text, "-") {
		return cwd, false, "cd takes one directory"
	}
	arg := st.words[1].text
	if !inWorkspaceFrom(arg, cwd, a.Dir) {
		return cwd, false, "the directory " + arg + " is outside the workspace"
	}
	p := arg
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return cwd, false, "the directory " + arg + " does not exist"
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return cwd, false, arg + " is not a directory"
	}
	return filepath.Clean(p), true, ""
}

// Check analyses a command line.
func (a *Analyzer) Check(ctx context.Context, cmd string) *Check {
	c := &Check{}
	p, ok := parsePlan(strings.Trim(cmd, " \t"))
	if !ok {
		return c
	}
	c.Parsed, c.plan = true, p
	cwd := a.Dir
	auto := true
	gitDirs := map[string]bool{}
	for _, pl := range p.pipelines {
		for si, st := range pl {
			w := st.texts()
			sc := StageCheck{Words: w, Text: strings.Join(w, " "), Match: strings.Join(w, " "), OK: true}
			switch {
			case len(pl) == 1 && w[0] == "cd":
				next, ok, why := a.checkCd(st, cwd)
				sc.Governed, sc.OK, sc.Why = true, ok, why
				if ok {
					cwd = next
				}
			case w[0] == "cd":
				sc.Governed, sc.OK, sc.Why = true, false, "cd in a pipeline"
			case si == 0:
				sc.Governed, sc.OK, sc.Why, sc.Reads = a.stageFirst(st.words, cwd)
				if sc.Governed && sc.OK && !autoFirst(w) {
					auto = false // the rules decide, not the allow-list
				}
				if sc.Governed && sc.OK && w[0] == "git" {
					gitDirs[cwd] = true
				}
			default:
				sc.Governed = governedFirst[w[0]] || governedLater[w[0]]
				if sc.Governed {
					sc.OK, sc.Why = filterStage(w[0], st.words, cwd, a.Dir, a.MaxFile)
				}
				if !sc.Governed {
					auto = false
				}
			}
			if !sc.Governed {
				auto = false
			}
			if sc.Governed && !sc.OK {
				auto = false
			}
			c.Stages = append(c.Stages, sc)
		}
	}
	if len(gitDirs) > 0 {
		// A line that is not auto-allowed runs as typed, which neutralises
		// nothing, so the keys the auto-allow environment switches off
		// count too: whatever the rules say about the other stages, git
		// in a repository whose config names a program asks.
		strict := !auto
		keyFn := a.ConfigKey
		if keyFn == nil {
			keyFn = ExecConfigKey
		}
		for dir := range gitDirs {
			key, err := keyFn(ctx, dir, strict)
			if err != nil {
				key = "git's configuration could not be read: " + err.Error()
			}
			if key != "" {
				auto = false
				// Mark the first git stage so the question names the key.
				for i := range c.Stages {
					if c.Stages[i].Words[0] == "git" && c.Stages[i].OK {
						c.Stages[i].OK = false
						c.Stages[i].Why = "git config runs a program: " + key
						break
					}
				}
			}
		}
	}
	c.Auto = auto
	return c
}

// ReadOnlyArgs reports whether a single simple command's arguments are
// acceptable to the check, or the command is not one the check governs
// (and so is for the rules to decide). It is the first-stage check
// with the workspace as the current directory.
func ReadOnlyArgs(words []string, dir string) bool {
	ws := make([]word, len(words))
	for i, w := range words {
		ws[i] = word{text: w}
	}
	a := &Analyzer{Dir: dir}
	governed, ok, _, _ := a.stageFirst(ws, dir)
	return !governed || ok
}

// Allowlisted reports whether a single simple command is one dex runs
// without asking when ReadOnlyArgs agrees, as opposed to one the user's
// rules decide.
func Allowlisted(words []string) bool { return autoFirst(words) }
