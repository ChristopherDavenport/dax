package tool

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
)

// GitChange is the workspace's change against a commit, as GitDiff
// reads it: a patch `git apply --binary` applies to the commit, at the
// repository's top, to give the workspace's tree.
type GitChange struct {
	// Repo is whether the workspace's root is in a git work tree.
	Repo bool
	// Base is what the patch applies to: the commit asked for, or, when
	// none was asked for, HEAD's commit, or the empty tree in a
	// repository with no commit yet.
	Base string
	// Commit is whether Base is a commit: false for the empty tree.
	Commit bool
	// Size is the patch's length in bytes; 0 is a workspace that is
	// Base's tree.
	Size int64
	// Data is the patch, nil when Size is over the most GitDiff was
	// asked to return.
	Data []byte
	// Withheld says why the repository was not read: its own config
	// names a program git would run (a clean filter, say), or it points
	// git at a repository elsewhere, as GitHead withholds it. Empty when
	// it was read.
	Withheld string
}

// diffScript prints the change of the work tree under the directory it
// runs in against $1 (HEAD, or the empty tree, when empty), as the line
// "<base> commit|tree <size>" and then, when size is at most $2, the
// patch. It reads the repository through an index of its own: a copy
// of the repository's, so git's record of which files are unchanged
// stays good, in a directory it makes and removes, so the repository's
// index, its objects and the work tree are left as they were. git add
// -N records the untracked files that are not ignored as files to be
// added, which writes no object, so the patch holds them as new files;
// an ignored file is not in it. Exit 10 is a directory not in a work
// tree.
const diffScript = `set -eu
if [ "$(git rev-parse --is-inside-work-tree 2>/dev/null)" != true ]; then exit 10; fi
base=$1 kind=commit
if [ -z "$base" ]; then
	base=$(git rev-parse --verify -q 'HEAD^{commit}') || { base=$(git hash-object -t tree /dev/null); kind=tree; }
else
	base=$(git rev-parse --verify -q "$base^{commit}")
fi
d=$(mktemp -d "${TMPDIR:-/tmp}/dax-snapshot.XXXXXX")
trap 'rm -rf "$d"' EXIT
idx=$(git rev-parse --git-path index)
GIT_INDEX_FILE=$d/index
export GIT_INDEX_FILE
if [ -f "$idx" ]; then cp "$idx" "$d/index"; fi
git add -A -N -- . || { rm -f "$d/index"; git read-tree "$base" && git add -A -N -- .; }
git diff --binary --no-color --no-ext-diff --no-textconv --no-relative --src-prefix=a/ --dst-prefix=b/ "$base" -- . >"$d/diff"
size=$(wc -c <"$d/diff")
printf '%s %s %s\n' "$base" "$kind" $size
if [ "$size" -le "$2" ]; then cat "$d/diff"; fi
`

// GitDiff reads the change of the workspace's root against base, a
// commit, or against HEAD when base is empty, through f's workspace: git
// runs where the tools act, in the auto-allow environment (no
// fsmonitor, hooks or pager), on a local directory, in a container or
// in a remote runtime alike. The change is limited to the workspace's
// root, the directory the tools act in, and its paths are from the
// repository's top, so the patch applies there. Untracked files that
// are not ignored are in it; ignored files are not. The repository's
// own index and work tree are not changed (diffScript). A patch longer
// than max is not returned: Data is nil and Size says how long it is.
//
// A directory not in a git work tree is GitChange{} and no error. A
// repository whose own configuration would run a program is not read,
// as for GitHead (execConfigKey): Withheld says why.
func GitDiff(ctx context.Context, f *Files, base string, max int64) (GitChange, error) {
	ws := f.Workspace()
	env := append(gitEnv(), "CDPATH=", "LC_ALL=C")
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := ws.Exec(probe, workspace.Command{Args: []string{"git", "rev-parse", "--is-inside-work-tree"}, Env: env})
	cancel()
	if err != nil {
		return GitChange{}, fmt.Errorf("git rev-parse: %w", err)
	}
	if out.ExitCode != 0 || strings.TrimSpace(string(out.Stdout)) != "true" {
		return GitChange{}, nil
	}
	key, err := execConfigKey(ctx, f, f.Dir(), false)
	if err != nil {
		return GitChange{Repo: true}, err
	}
	if key != "" {
		return GitChange{Repo: true, Withheld: key}, nil
	}
	if strings.HasPrefix(base, "-") {
		return GitChange{Repo: true}, fmt.Errorf("git diff: base %q is not a commit", base)
	}
	ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err = ws.Exec(ctx, workspace.Command{Args: []string{"sh", "-c", diffScript, "sh", base, strconv.FormatInt(max, 10)}, Env: env})
	switch {
	case err != nil:
		return GitChange{Repo: true}, fmt.Errorf("git diff: %w", err)
	case out.ExitCode == 10:
		return GitChange{}, nil
	case out.ExitCode != 0:
		return GitChange{Repo: true}, fmt.Errorf("git diff: exit status %d: %s", out.ExitCode, strings.TrimSpace(string(out.Stderr)))
	}
	line, data, _ := bytes.Cut(out.Stdout, []byte{'\n'})
	fields := strings.Fields(string(line))
	if len(fields) != 3 {
		return GitChange{Repo: true}, fmt.Errorf("git diff: unexpected output %q", line)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return GitChange{Repo: true}, fmt.Errorf("git diff: unexpected size %q", fields[2])
	}
	c := GitChange{Repo: true, Base: fields[0], Commit: fields[1] == "commit", Size: size}
	if size <= max {
		if int64(len(data)) != size {
			return GitChange{Repo: true}, fmt.Errorf("git diff: %d bytes of a %d-byte patch arrived", len(data), size)
		}
		c.Data = data
	}
	return c, nil
}
