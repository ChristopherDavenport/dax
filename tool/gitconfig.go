package tool

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
)

// execKeys match the git configuration keys that name a program git
// will run when a read-only command is: a clean or smudge filter, a
// textconv or external diff, an askpass or editor, a proxy command, an
// upload-pack or receive-pack, a credential helper, a merge driver, a
// pack-objects hook, a pager for one command. The keys dax switches
// off itself, in gitEnv, are not here: core.fsmonitor, core.pager,
// core.sshCommand, core.hooksPath and the gpg programs.
var execKeys = regexp.MustCompile(`(?i)^(` +
	`filter\..+\.(clean|smudge|process)` +
	`|diff\..+\.(command|textconv)|diff\.external` +
	`|core\.(askpass|editor|gitproxy|attributesfile)` +
	`|remote\..+\.(uploadpack|receivepack|proxy|vcs)` +
	`|credential\.(.+\.)?helper` +
	`|sequence\.editor|merge\..+\.driver|uploadpack\.packobjectshook` +
	`|pager\..+` +
	`)$`)

var boolish = map[string]bool{"true": true, "false": true, "yes": true, "no": true, "on": true, "off": true, "1": true, "0": true, "": true}

// execConfigKey asks whether a git command run in dir could do more than
// read this workspace's repository, and says what, or returns "" when
// it could not. An error is returned when git could not say, which a
// caller treats as a reason to ask. It looks for:
//
//   - a .git that is a file or a link, which points git at a repository
//     somewhere else;
//   - a git directory or work tree that is not this one: git's own
//     answer (rev-parse, in the auto-allow environment) must be an
//     ancestor of dir and its .git;
//   - configuration the repository itself sets (its own config, a
//     worktree config, anything they include, and the configs of its
//     submodules) that names a program, or moves the work tree
//     (core.worktree), makes it bare, sets any extension, or runs a
//     submodule update command.
//
// strict adds the keys that the auto-allow environment switches off
// (core.fsmonitor, hooksPath, sshCommand, pager and the gpg programs),
// for a line that will not run with it.
//
// The user's own system and global configuration is trusted: it is
// theirs, and a git-lfs filter there is no hostile repository's.
//
// Everything is asked of the workspace's own machine, through f's
// workspace: git and the shell run there, dir is a path in its
// namespace, and the parents of dir it walks are its parents, so a
// repository in a container is judged by the container's files.
func execConfigKey(ctx context.Context, f *Files, dir string, strict bool) (key string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws := f.Workspace()
	rel, ok := f.view().rel(dir)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrOutside, dir)
	}
	exec := func(args ...string) (*workspace.Output, error) {
		return ws.Exec(ctx, workspace.Command{Args: args, Dir: rel, Env: append(gitEnv(), "CDPATH=", "LC_ALL=C")})
	}
	if why, err := gitDirRedirected(exec, filepath.Clean(dir)); err != nil || why != "" {
		return why, err
	}
	run := func(args ...string) ([]byte, error) {
		out, err := exec(append([]string{"git"}, args...)...)
		switch {
		case err != nil:
			return nil, fmt.Errorf("git %s: %w", args[0], err)
		case out.ExitCode != 0:
			return nil, fmt.Errorf("git %s: exit status %d: %s", args[0], out.ExitCode, strings.TrimSpace(string(out.Stderr)))
		}
		return out.Stdout, nil
	}
	list, err := run("config", "--list", "-z", "--show-origin", "--show-scope")
	if err != nil {
		return "", err
	}
	// With -z, each entry is scope NUL origin NUL key NL value NUL.
	fields := bytes.Split(list, []byte{0})
	for i := 0; i+2 < len(fields); i += 3 {
		scope, origin, kv := string(fields[i]), string(fields[i+1]), string(fields[i+2])
		if scope == "system" || scope == "global" || scope == "command" {
			continue
		}
		k, v, _ := strings.Cut(kv, "\n")
		if why := badKey(k, v, strict); why != "" {
			return fmt.Sprintf("%s (set in %s)", why, strings.TrimPrefix(origin, "file:")), nil
		}
	}
	// Where git thinks the repository is must be where the workspace is.
	if out, err := run("rev-parse", "--absolute-git-dir", "--show-toplevel"); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) == 2 {
			if why := repoElsewhere(exec, dir, lines[0], lines[1]); why != "" {
				return why, nil
			}
		}
		// A submodule's config is read when git descends into it.
		if len(lines) >= 1 {
			if why := submoduleConfigs(exec, lines[0], run); why != "" {
				return why, nil
			}
		}
	}
	return "", nil
}

// badKey says why a repository-scope config entry stops an unasked git
// command, or returns "".
func badKey(k, v string, strict bool) string {
	lk := strings.ToLower(k)
	if strict && (strictKeys.MatchString(k) && !boolish[strings.ToLower(v)] || strings.HasPrefix(lk, "core.hookspath") || strings.HasPrefix(lk, "core.sshcommand") || strings.HasPrefix(lk, "core.pager")) {
		return k
	}
	switch {
	case execKeys.MatchString(k) && !(strings.HasPrefix(lk, "pager.") && boolish[strings.ToLower(v)]):
		return k
	case protocolAllow.MatchString(k) && strings.EqualFold(v, "always"):
		return k + "=always"
	case lk == "core.worktree":
		return k + " moves the work tree"
	case lk == "core.bare" && strings.EqualFold(v, "true"):
		return k + "=true"
	case strings.HasPrefix(lk, "extensions."):
		return k + " changes how git reads the repository"
	case submoduleUpdate.MatchString(k) && strings.HasPrefix(v, "!"):
		return k + " runs a command"
	}
	return ""
}

// strictKeys are the keys the auto-allow environment switches off, which
// matter when a line runs without it: core.fsmonitor (unless it is only
// on or off) and the gpg programs.
var strictKeys = regexp.MustCompile(`(?i)^(core\.fsmonitor|gpg\.program|gpg\..+\.program)$`)

var (
	protocolAllow   = regexp.MustCompile(`(?i)^protocol\..*allow$`)
	submoduleUpdate = regexp.MustCompile(`(?i)^submodule\..+\.update$`)
)

// execFn runs a command in the workspace, at the directory being
// checked.
type execFn func(args ...string) (*workspace.Output, error)

// gitDirScript walks from $1 upward to the first .git and says whether
// it is a link or a file (a gitfile naming another directory), with the
// directory it is in. A .git directory, or none, prints nothing.
const gitDirScript = `d=$1
while :; do
	if [ -L "$d/.git" ]; then printf 'link\n%s' "$d"; exit 0; fi
	if [ -e "$d/.git" ]; then
		[ -d "$d/.git" ] && exit 0
		printf 'file\n%s' "$d"; exit 0
	fi
	p=$(dirname -- "$d")
	[ "$p" = "$d" ] && exit 0
	d=$p
done`

// gitDirRedirected looks for the .git git would find from dir upward
// and says if it is a file (a gitfile naming another directory) or a
// link. An error is a walk that could not run, which a caller treats
// as a reason to ask.
func gitDirRedirected(exec execFn, dir string) (string, error) {
	out, err := exec("sh", "-c", gitDirScript, "sh", dir)
	if err != nil {
		return "", fmt.Errorf("looking for .git: %w", err)
	}
	if out.ExitCode != 0 {
		return "", fmt.Errorf("looking for .git: exit status %d: %s", out.ExitCode, strings.TrimSpace(string(out.Stderr)))
	}
	kind, d, _ := strings.Cut(string(out.Stdout), "\n")
	switch kind {
	case "link":
		return ".git in " + d + " is a symbolic link", nil
	case "file":
		return ".git in " + d + " is a file that points git at another directory", nil
	}
	return "", nil
}

// realScript prints each argument with its links resolved, or as it
// is when it cannot be.
const realScript = `for p; do
	if r=$(cd -- "$p" 2>/dev/null && pwd -P); then printf '%s\n' "$r"; else printf '%s\n' "$p"; fi
done`

// repoElsewhere compares what git says with the workspace: the work
// tree must contain dir, and the git directory must be that work
// tree's .git. The paths are resolved where git runs.
func repoElsewhere(exec execFn, dir, gitDir, top string) string {
	reals := []string{filepath.Clean(dir), filepath.Clean(gitDir), filepath.Clean(top)}
	if out, err := exec("sh", "-c", realScript, "sh", dir, gitDir, top); err == nil && out.ExitCode == 0 {
		if lines := strings.Split(strings.TrimSuffix(string(out.Stdout), "\n"), "\n"); len(lines) == 3 {
			for i, l := range lines {
				reals[i] = filepath.Clean(l)
			}
		}
	}
	d, g, t := reals[0], reals[1], reals[2]
	if rel, err := filepath.Rel(t, d); err != nil || !local(rel) {
		return "git's work tree " + top + " does not contain the workspace"
	}
	if g != filepath.Join(t, ".git") {
		return "git's directory " + gitDir + " is not " + filepath.Join(top, ".git")
	}
	return ""
}

// modulesScript lists the regular files called config under
// $1/modules, links not followed, at most 200, in byte order.
const modulesScript = `[ -d "$1/modules" ] || exit 0
find "$1/modules" -type f -name config 2>/dev/null | sort | head -n 200`

// submoduleConfigs reads the config of every submodule under the git
// directory's modules/ and returns the first thing in one that runs a
// program. They are the repository's own, and hostile ones are
// possible: a submodule's config is as writable as the repository's.
func submoduleConfigs(exec execFn, gitDir string, run func(...string) ([]byte, error)) string {
	out, err := exec("sh", "-c", modulesScript, "sh", gitDir)
	if err != nil || out.ExitCode != 0 {
		return "the submodules' configs under " + gitDir + " could not be listed"
	}
	var configs []string
	for _, l := range strings.Split(string(out.Stdout), "\n") {
		if l != "" {
			configs = append(configs, l)
		}
	}
	for _, f := range configs {
		out, err := run("config", "--file", f, "--list", "-z")
		if err != nil {
			return "a submodule's config " + f + " could not be read"
		}
		for _, kv := range bytes.Split(out, []byte{0}) {
			k, v, _ := strings.Cut(string(kv), "\n")
			if why := badKey(k, v, true); why != "" && !strings.HasPrefix(strings.ToLower(k), "core.bare") && strings.ToLower(k) != "core.worktree" {
				return fmt.Sprintf("%s (set in %s)", why, f)
			}
		}
	}
	return ""
}
