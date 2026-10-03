package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/ChristopherDavenport/agenttool"
)

const (
	defaultBashTimeout = 2 * time.Minute
	maxBashBytes       = 50 << 10
)

// BashArgs are the arguments of the bash tool.
type BashArgs struct {
	Command string `json:"command" desc:"The command to run"`
	Timeout int    `json:"timeout_seconds,omitempty" desc:"Kill the command after this many seconds (default 120)"`
}

// BashOption configures the bash tool.
type BashOption func(*bashConfig)

type bashConfig struct{ env []string }

// WithEnv sets the environment commands start with, before GitEnv is
// added. The default is the current one without its credentials; see
// ChildEnv.
func WithEnv(env []string) BashOption { return func(c *bashConfig) { c.env = env } }

// Bash returns a tool that runs a shell command in dir. It is
// sequential: a batch that contains a shell command runs one call at a
// time, so a command never races a concurrent edit of the same file.
func Bash(dir string, opts ...BashOption) agenttool.Tool {
	var cfg bashConfig
	for _, o := range opts {
		o(&cfg)
	}
	base := cfg.env
	if base == nil {
		base = DefaultEnv(nil)
	}
	return agenttool.New("bash", "Run a bash command in the working directory and return its combined output and exit code.",
		func(ctx context.Context, in BashArgs) (string, error) {
			if strings.TrimSpace(in.Command) == "" {
				return "", errors.New("command is required")
			}
			timeout := defaultBashTimeout
			if in.Timeout > 0 {
				timeout = time.Duration(in.Timeout) * time.Second
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			cmd := command(ctx, dir, in.Command, base)
			cmd.Dir = dir
			// Run in its own process group so cancellation reaches children too.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
			cmd.WaitDelay = 2 * time.Second

			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			runErr := cmd.Run()

			var b strings.Builder
			b.WriteString(truncate(out.String(), maxBashBytes))
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteByte('\n')
			}
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				fmt.Fprintf(&b, "[killed after %s]", timeout)
			case ctx.Err() != nil:
				return "", ctx.Err()
			case runErr == nil:
				b.WriteString("[exit 0]")
			default:
				var ee *exec.ExitError
				if errors.As(runErr, &ee) {
					fmt.Fprintf(&b, "[exit %d]", ee.ExitCode())
				} else {
					return "", runErr
				}
			}
			return b.String(), nil
		}, agenttool.WithSequential())
}

// command is what runs a bash call, and in which environment.
//
// A command the policy would auto-allow (one simple read-only command
// of the safe subset, whose git configuration names no program) runs
// with GitEnv added, and a git diff, log or show of it runs git
// directly with --no-ext-diff and --no-textconv after the subcommand,
// since the repository's own diff.external and textconv drivers are
// programs and no configuration switches them off. Everything else,
// every command a person approved, is bash -c in the environment the
// user has, minus credentials: their hooks, their sshCommand and their
// GIT_CONFIG_* are theirs.
func command(ctx context.Context, dir, command string, base []string) *exec.Cmd {
	if words, ok := SafeWords(strings.TrimSpace(command)); ok && Allowlisted(words) && ReadOnlyArgs(words, dir) && gitConfigClean(ctx, words, dir) {
		env := append(append([]string(nil), base...), GitEnv()...)
		var cmd *exec.Cmd
		if ReadOnlyGit(words, dir) {
			args := append([]string{words[1], "--no-ext-diff", "--no-textconv"}, words[2:]...)
			cmd = exec.CommandContext(ctx, "git", args...)
		} else {
			cmd = exec.CommandContext(ctx, "bash", "-c", command)
		}
		cmd.Env = env
		return cmd
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Env = append([]string(nil), base...)
	return cmd
}

// gitConfigClean is true for a command that is not git, or whose
// configuration names no program.
func gitConfigClean(ctx context.Context, words []string, dir string) bool {
	if words[0] != "git" {
		return true
	}
	key, err := ExecConfigKey(ctx, dir)
	return err == nil && key == ""
}
