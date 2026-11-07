package tool

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
)

// Git is what a front shows of the workspace's repository: the branch,
// and whether the work tree differs from it.
type Git struct {
	// Repo is whether the workspace's root is in a git work tree.
	Repo bool
	// Branch is the branch checked out, or the commit's short name on a
	// detached head; empty when it was not read.
	Branch string
	// Dirty is whether anything is changed, staged or untracked.
	Dirty bool
	// Withheld says why the repository was not read: its own config
	// names a program git status would run (a clean filter, say), which
	// a status line does not start unasked. Empty when it was read.
	Withheld string
}

// GitHead reads the branch and whether the tree is dirty, for a
// status line, through f's workspace: git runs where the tools act,
// in the auto-allow environment (no fsmonitor, hooks or pager), and
// takes no optional lock, so it never writes the index. A repository
// whose own configuration would run a program under git status, or
// that points git elsewhere, is not read (execConfigKey, the check an
// unasked read-only git command passes). It is exported so that a
// front built on dax, which knows no workspace's machine, can show
// the same.
func GitHead(ctx context.Context, f *Files) (Git, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws := f.Workspace()
	git := func(args ...string) (*workspace.Output, error) {
		return ws.Exec(ctx, workspace.Command{Args: append([]string{"git"}, args...), Env: append(gitEnv(), "CDPATH=", "LC_ALL=C")})
	}
	out, err := git("rev-parse", "--is-inside-work-tree")
	if err != nil {
		return Git{}, fmt.Errorf("git rev-parse: %w", err)
	}
	if out.ExitCode != 0 || strings.TrimSpace(string(out.Stdout)) != "true" {
		return Git{}, nil
	}
	key, err := execConfigKey(ctx, f, f.Dir(), false)
	if err != nil {
		return Git{Repo: true}, err
	}
	if key != "" {
		return Git{Repo: true, Withheld: key}, nil
	}
	out, err = git("--no-optional-locks", "status", "--porcelain=v2", "--branch")
	if err != nil {
		return Git{Repo: true}, fmt.Errorf("git status: %w", err)
	}
	if out.ExitCode != 0 {
		return Git{Repo: true}, fmt.Errorf("git status: exit status %d: %s", out.ExitCode, strings.TrimSpace(string(out.Stderr)))
	}
	g := Git{Repo: true}
	var oid string
	sc := bufio.NewScanner(bytes.NewReader(out.Stdout))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			g.Branch = strings.TrimPrefix(line, "# branch.head ")
		case strings.HasPrefix(line, "# branch.oid "):
			oid = strings.TrimPrefix(line, "# branch.oid ")
		case strings.HasPrefix(line, "#"), line == "":
		default:
			g.Dirty = true
		}
	}
	if g.Branch == "(detached)" {
		g.Branch = oid
		if len(g.Branch) > 7 {
			g.Branch = g.Branch[:7]
		}
	}
	return g, nil
}
