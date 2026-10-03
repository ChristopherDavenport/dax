package tool

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// execKeys match the git configuration keys that name a program git
// will run when a read-only command is: a clean or smudge filter, a
// textconv or external diff, an askpass or editor, a proxy command, an
// upload-pack or receive-pack, a credential helper, a merge driver, a
// pack-objects hook, a pager for one command. The keys dex switches
// off itself, in GitEnv, are not here: core.fsmonitor, core.pager,
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

// ExecConfigKey asks whether a git command run in dir could do more than
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
// The user's own system and global configuration is trusted: it is
// theirs, and a git-lfs filter there is no hostile repository's.
func ExecConfigKey(ctx context.Context, dir string) (key string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if why := gitDirRedirected(dir); why != "" {
		return why, nil
	}
	env := append(DefaultEnv(nil), GitEnv()...)
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = env
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
		}
		return out.Bytes(), nil
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
		if why := badKey(k, v); why != "" {
			return fmt.Sprintf("%s (set in %s)", why, strings.TrimPrefix(origin, "file:")), nil
		}
	}
	// Where git thinks the repository is must be where the workspace is.
	if out, err := run("rev-parse", "--absolute-git-dir", "--show-toplevel"); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) == 2 {
			if why := repoElsewhere(dir, lines[0], lines[1]); why != "" {
				return why, nil
			}
		}
		// A submodule's config is read when git descends into it.
		if len(lines) >= 1 {
			if why := submoduleConfigs(ctx, lines[0], run); why != "" {
				return why, nil
			}
		}
	}
	return "", nil
}

// badKey says why a repository-scope config entry stops an unasked git
// command, or returns "".
func badKey(k, v string) string {
	lk := strings.ToLower(k)
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

var (
	protocolAllow   = regexp.MustCompile(`(?i)^protocol\..*allow$`)
	submoduleUpdate = regexp.MustCompile(`(?i)^submodule\..+\.update$`)
)

// gitDirRedirected looks for the .git git would find from dir upward
// and says if it is a file (a gitfile naming another directory) or a
// link.
func gitDirRedirected(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		fi, err := os.Lstat(filepath.Join(d, ".git"))
		if err == nil {
			switch {
			case fi.Mode()&fs.ModeSymlink != 0:
				return ".git in " + d + " is a symbolic link"
			case !fi.IsDir():
				return ".git in " + d + " is a file that points git at another directory"
			}
			return ""
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// repoElsewhere compares what git says with the workspace: the work
// tree must contain dir, and the git directory must be that work
// tree's .git.
func repoElsewhere(dir, gitDir, top string) string {
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	d, g, t := real(dir), real(gitDir), real(top)
	if rel, err := filepath.Rel(t, d); err != nil || !local(rel) {
		return "git's work tree " + top + " does not contain the workspace"
	}
	if g != filepath.Join(t, ".git") {
		return "git's directory " + gitDir + " is not " + filepath.Join(top, ".git")
	}
	return ""
}

// submoduleConfigs reads the config of every submodule under the git
// directory's modules/ and returns the first thing in one that runs a
// program. They are the repository's own, and hostile ones are
// possible: a submodule's config is as writable as the repository's.
func submoduleConfigs(ctx context.Context, gitDir string, run func(...string) ([]byte, error)) string {
	root := filepath.Join(gitDir, "modules")
	var configs []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || len(configs) >= 200 {
			return nil
		}
		if d.Type().IsRegular() && d.Name() == "config" {
			configs = append(configs, p)
		}
		return nil
	})
	for _, f := range configs {
		out, err := run("config", "--file", f, "--list", "-z")
		if err != nil {
			return "a submodule's config " + f + " could not be read"
		}
		for _, kv := range bytes.Split(out, []byte{0}) {
			k, v, _ := strings.Cut(string(kv), "\n")
			if why := badKey(k, v); why != "" && !strings.HasPrefix(strings.ToLower(k), "core.bare") && strings.ToLower(k) != "core.worktree" {
				return fmt.Sprintf("%s (set in %s)", why, f)
			}
		}
	}
	return ""
}
