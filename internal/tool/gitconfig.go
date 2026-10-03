package tool

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
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

// ExecConfigKey reads the git configuration that applies in dir, with
// the environment an auto-allowed git command runs in, and returns the
// first key the repository itself sets (its own config, a worktree
// config, anything they include) that names a program git would run.
// The user's own system and global configuration is trusted: it is
// theirs, and a git-lfs filter there is no hostile repository's. key
// is empty when there is none. An error is returned when git could not
// say, which a caller treats as a reason to ask.
func ExecConfigKey(ctx context.Context, dir string) (key string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "config", "--list", "-z", "--show-origin", "--show-scope")
	cmd.Dir = dir
	cmd.Env = append(DefaultEnv(nil), GitEnv()...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git config --list: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	// With -z, each entry is scope NUL origin NUL key NL value NUL.
	fields := bytes.Split(out.Bytes(), []byte{0})
	for i := 0; i+2 < len(fields); i += 3 {
		scope, origin, kv := string(fields[i]), string(fields[i+1]), string(fields[i+2])
		if scope == "system" || scope == "global" || scope == "command" {
			continue
		}
		k, v, _ := strings.Cut(kv, "\n")
		switch {
		case execKeys.MatchString(k) && !(strings.HasPrefix(strings.ToLower(k), "pager.") && boolish[strings.ToLower(v)]):
			return fmt.Sprintf("%s (set in %s)", k, strings.TrimPrefix(origin, "file:")), nil
		case regexp.MustCompile(`(?i)^protocol\..*allow$`).MatchString(k) && strings.EqualFold(v, "always"):
			return fmt.Sprintf("%s=always (set in %s)", k, strings.TrimPrefix(origin, "file:")), nil
		}
	}
	return "", nil
}
