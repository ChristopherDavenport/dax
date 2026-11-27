package tool

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
)

// ErrNoGit is ReadCheckout's error for a workspace with no git to run:
// the program is not on its PATH. A session that sees it once stops
// asking, since nothing in the workspace can commit, switch or push
// without it.
var ErrNoGit = errors.New("git is not installed in the workspace")

// ErrGone is ReadCheckout's error for a directory that no longer
// exists: a worktree a call removed.
var ErrGone = errors.New("the directory no longer exists")

// GitCheckout is a git checkout's state, as ReadCheckout reads it for
// the session's env entry (agentsession's Checkout): where it is, which
// repository it belongs to, and the commit, branch and upstream it is
// on. It holds no remote's URL.
type GitCheckout struct {
	// Repo is whether the directory is in a git work tree; the rest is
	// empty when it is not.
	Repo bool
	// Top is the checkout's top directory and CommonDir the repository's
	// common git directory, which every worktree of one repository
	// shares, both absolute in the workspace's namespace.
	Top, CommonDir string
	// Revision is the commit HEAD is on, empty on a branch with no
	// commit yet. Branch is the branch HEAD names, short (main), empty
	// on a detached HEAD.
	Revision, Branch string
	// Dirty is whether git status shows anything: a change, staged or
	// not, or an untracked file that is not ignored.
	Dirty bool
	// Upstream is the branch's upstream as the checkout names it
	// (origin/main), and UpstreamRevision the commit it is on; both
	// empty when the branch has none.
	Upstream, UpstreamRevision string
	// Withheld says why the checkout's state was not read: its own
	// configuration names a program git would run, as GitHead withholds
	// it. Top and CommonDir are still read.
	Withheld string
	// JJ is a colocated Jujutsu repository (a .jj beside the .git),
	// whose working copy is a commit on a detached HEAD that every edit
	// moves; its state is not read, since git's reading of it is churn.
	JJ bool
}

// checkoutScript prints a checkout's state, one key=value a line, run
// at its top ($1): HEAD's commit, the branch, the upstream and its
// commit, whether status shows anything, and whether a .jj sits beside
// the .git. It runs git in the auto-allow environment and writes
// nothing: status takes no optional lock.
const checkoutScript = `top=$1
if [ -d "$top/.jj" ]; then echo jj=1; exit 0; fi
printf 'rev=%s\n' "$(git rev-parse -q --verify 'HEAD^{commit}' 2>/dev/null)"
printf 'branch=%s\n' "$(git symbolic-ref -q --short HEAD 2>/dev/null)"
ref=$(git symbolic-ref -q HEAD 2>/dev/null)
if [ -n "$ref" ]; then
	up=$(git for-each-ref --format='%(upstream:short)' -- "$ref" 2>/dev/null)
	full=$(git for-each-ref --format='%(upstream)' -- "$ref" 2>/dev/null)
	if [ -n "$up" ]; then
		printf 'up=%s\n' "$up"
		printf 'uprev=%s\n' "$(git rev-parse -q --verify "$full^{commit}" 2>/dev/null)"
	fi
fi
if [ -n "$(git --no-optional-locks status --porcelain=v1 -z 2>/dev/null | head -c 1)" ]; then echo dirty=1; fi
`

// ReadCheckout reads the state of the git checkout holding dir, a name
// relative to the workspace's root ("." for the root) or absolute in
// it, through f's workspace: git runs where the tools act, in the
// auto-allow environment, on a local directory, a container or a
// remote runtime alike. It never builds a diff and never reads a
// remote's URL. A directory not in a work tree is GitCheckout{} and no
// error; one outside the workspace is ErrOutside; one that no longer
// exists is ErrGone; a workspace with no git is ErrNoGit. A repository
// whose own configuration would run a program is read only as far as
// where it is (Withheld says why), as GitHead withholds it.
func ReadCheckout(ctx context.Context, f *Files, dir string) (GitCheckout, error) {
	rel, err := f.Rel(dir)
	if err != nil {
		return GitCheckout{}, err
	}
	if _, err := fs.Stat(f.Workspace().FS(), filepath.ToSlash(rel)); errors.Is(err, fs.ErrNotExist) {
		return GitCheckout{}, ErrGone
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ws := f.Workspace()
	env := append(gitEnv(), "CDPATH=", "LC_ALL=C")
	run := func(args ...string) (*workspace.Output, error) {
		out, err := ws.Exec(ctx, workspace.Command{Args: args, Dir: rel, Env: env})
		if err != nil && errors.Is(err, fs.ErrNotExist) {
			// The directory was there a moment ago, so what is missing
			// is the program.
			return nil, fmt.Errorf("%w: %v", ErrNoGit, err)
		}
		return out, err
	}
	out, err := run("git", "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return GitCheckout{}, fmt.Errorf("git rev-parse: %w", err)
	}
	if out.ExitCode != 0 {
		return GitCheckout{}, nil
	}
	lines := strings.Split(strings.TrimRight(string(out.Stdout), "\n"), "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		return GitCheckout{}, nil
	}
	c := GitCheckout{Repo: true, Top: filepath.Clean(lines[0]), CommonDir: filepath.Clean(lines[1])}
	key, err := checkoutConfigKey(ctx, f, rel)
	if err != nil {
		return c, err
	}
	if key != "" {
		c.Withheld = key
		return c, nil
	}
	out, err = run("sh", "-c", checkoutScript, "sh", c.Top)
	if err != nil {
		return c, fmt.Errorf("git status: %w", err)
	}
	if out.ExitCode != 0 {
		return c, fmt.Errorf("git status: exit status %d: %s", out.ExitCode, strings.TrimSpace(string(out.Stderr)))
	}
	sc := bufio.NewScanner(bytes.NewReader(out.Stdout))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "jj":
			c.JJ = true
		case "rev":
			c.Revision = v
		case "branch":
			c.Branch = v
		case "up":
			c.Upstream = v
		case "uprev":
			c.UpstreamRevision = v
		case "dirty":
			c.Dirty = true
		}
	}
	return c, nil
}

// worktreeScript says whether the checkout at $1 is a linked worktree
// git made, and prints its repository's main work tree and the
// worktree's own git directory if so: $1/.git is a regular file whose
// gitdir is <common>/worktrees/<name>, <common> a directory called
// .git, and that directory's gitdir and commondir files lead back to
// $1/.git and to <common>, as git's own worktree add writes them. A
// gitfile that names any other directory exits 1: git would be pointed
// at a repository the checks never saw.
const worktreeScript = `top=$(cd -- "$1" 2>/dev/null && pwd -P) || exit 1
f="$top/.git"
[ -f "$f" ] && [ ! -L "$f" ] || exit 1
line=$(head -n 1 -- "$f")
case "$line" in "gitdir: "*) ;; *) exit 1;; esac
p=${line#gitdir: }
case "$p" in /*) ;; *) p="$top/$p";; esac
p=$(cd -- "$p" 2>/dev/null && pwd -P) || exit 1
c=$(dirname -- "$(dirname -- "$p")")
[ "$(dirname -- "$p")" = "$c/worktrees" ] && [ "$(basename -- "$c")" = .git ] || exit 1
[ -f "$p/gitdir" ] && [ ! -L "$p/gitdir" ] && [ -f "$p/commondir" ] && [ ! -L "$p/commondir" ] || exit 1
back=$(head -n 1 -- "$p/gitdir")
case "$back" in /*) ;; *) back="$p/$back";; esac
b=$(cd -- "$(dirname -- "$back")" 2>/dev/null && pwd -P) || exit 1
[ "$b/$(basename -- "$back")" = "$f" ] || exit 1
cd -- "$p" || exit 1
cr=$(cd -- "$(head -n 1 commondir)" 2>/dev/null && pwd -P) || exit 1
[ "$cr" = "$c" ] || exit 1
printf '%s\n%s\n' "$(dirname -- "$c")" "$p"`

// checkoutConfigKey is execConfigKey for the checkout at rel, but for
// a linked worktree git made: its .git is a file, which execConfigKey
// refuses as pointing git elsewhere, and the checkout is read when
// the gitfile is git's own link to its repository (worktreeScript),
// that repository's main work tree is in the workspace and passes
// execConfigKey (its config is the worktree's), and the worktree has
// no submodules of its own. The analyzer's own checks (Check) are not
// widened: an unasked git command in a worktree still asks.
func checkoutConfigKey(ctx context.Context, f *Files, rel string) (string, error) {
	key, err := execConfigKey(ctx, f, filepath.Join(f.Dir(), rel), false)
	if err != nil || key == "" {
		return key, err
	}
	out, err := f.Workspace().Exec(ctx, workspace.Command{Args: []string{"sh", "-c", worktreeScript, "sh", "."}, Dir: rel, Env: append(gitEnv(), "CDPATH=", "LC_ALL=C")})
	if err != nil || out.ExitCode != 0 {
		return key, nil
	}
	lines := strings.Split(strings.TrimRight(string(out.Stdout), "\n"), "\n")
	if len(lines) != 2 {
		return key, nil
	}
	main, gitDir := lines[0], lines[1]
	if _, err := f.Rel(main); err != nil {
		return key, nil
	}
	mainKey, err := execConfigKey(ctx, f, main, false)
	if err != nil {
		return key, err
	}
	if mainKey != "" {
		return mainKey, nil
	}
	if out, err := f.Workspace().Exec(ctx, workspace.Command{Args: []string{"sh", "-c", modulesScript, "sh", gitDir}, Env: append(gitEnv(), "CDPATH=", "LC_ALL=C")}); err != nil || out.ExitCode != 0 || len(bytes.TrimSpace(out.Stdout)) > 0 {
		return "the worktree's submodules are not read", nil
	}
	return "", nil
}

// gitMoving are the git subcommands that can move a checkout's commit,
// branch or upstream, or make or remove one: a session that follows
// checkouts reads the checkout again after a command that runs one. A
// change to the tree alone (an edit, git add) is read at the run's end.
var gitMoving = set2(nil, "worktree", "commit", "switch", "checkout", "merge", "rebase", "reset",
	"pull", "fetch", "push", "branch", "stash", "cherry-pick", "am", "tag", "revert", "clone", "init")

// GitMoves reads a bash command line for the git commands in it that
// can move a checkout (gitMoving), and says where: dirs are the
// directories they run in, and those a `git worktree add` or `git
// clone` makes or a `git worktree remove` removes, each a name
// relative to the workspace's root. A directory outside the workspace
// is left out. A line outside the parser's subset (parsePlan) that
// names git anywhere is all: the caller cannot tell where it ran, so
// it reads every checkout it follows. A line that names no git is
// neither.
func GitMoves(cmd string, f *Files) (dirs []string, all bool) {
	p, ok := parsePlan(strings.Trim(cmd, " \t"))
	if !ok {
		return nil, mentionsGit(cmd)
	}
	cwd := f.Dir()
	seen := map[string]bool{}
	add := func(dir string) {
		if rel, err := f.Rel(dir); err == nil && !seen[rel] {
			seen[rel] = true
			dirs = append(dirs, filepath.ToSlash(rel))
		}
	}
	for _, pl := range p.pipelines {
		for _, st := range pl {
			w := st.texts()
			if len(w) == 0 {
				continue
			}
			if w[0] == "cd" && len(pl) == 1 && len(w) == 2 {
				next := w[1]
				if !filepath.IsAbs(next) {
					next = filepath.Join(cwd, next)
				}
				cwd = filepath.Clean(next)
				continue
			}
			if w[0] != "git" {
				continue
			}
			dir, sub, rest := gitInvocation(w[1:], cwd)
			if !gitMoving[sub] {
				continue
			}
			add(dir)
			for _, made := range gitMadeDirs(sub, rest) {
				if !filepath.IsAbs(made) {
					made = filepath.Join(dir, made)
				}
				add(made)
			}
		}
	}
	return dirs, false
}

// gitInvocation reads git's global options before the subcommand: the
// directory -C moves it to (each relative to the one before), and the
// subcommand with its arguments.
func gitInvocation(args []string, cwd string) (dir, sub string, rest []string) {
	dir = cwd
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" && i+1 < len(args):
			i++
			d := args[i]
			if !filepath.IsAbs(d) {
				d = filepath.Join(dir, d)
			}
			dir = filepath.Clean(d)
		case a == "-c" && i+1 < len(args):
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return dir, a, args[i+1:]
		}
	}
	return dir, "", nil
}

// gitMadeDirs are the directories a git subcommand makes or removes
// beside the one it runs in: worktree add's and remove's path, clone's
// target.
func gitMadeDirs(sub string, args []string) []string {
	positional := func(args []string, valued map[string]bool) []string {
		var out []string
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "--":
				return append(out, args[i+1:]...)
			case valued[a]:
				i++
			case strings.HasPrefix(a, "-"):
			default:
				out = append(out, a)
			}
		}
		return out
	}
	switch sub {
	case "worktree":
		if len(args) == 0 {
			return nil
		}
		switch args[0] {
		case "add":
			if p := positional(args[1:], set2(nil, "-b", "-B", "--reason")); len(p) > 0 {
				return p[:1]
			}
		case "remove", "move":
			return positional(args[1:], nil)
		}
	case "clone":
		if p := positional(args, set2(nil, "-b", "--branch", "-o", "--origin", "--depth", "-c", "--config", "--reference", "-u", "--upload-pack", "--template", "--separate-git-dir", "-j", "--jobs", "--filter")); len(p) == 2 {
			return p[1:]
		}
	}
	return nil
}

// mentionsGit is whether a command line names git as a word.
func mentionsGit(cmd string) bool {
	for _, f := range strings.FieldsFunc(cmd, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '/' || r == '.' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9')
	}) {
		if f == "git" || strings.HasSuffix(f, "/git") {
			return true
		}
	}
	return false
}
