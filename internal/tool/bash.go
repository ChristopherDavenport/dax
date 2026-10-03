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

			cmd := command(ctx, dir, in.Command)
			cmd.Dir = dir
			cmd.Env = append(append([]string(nil), base...), GitEnv()...)
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

// command is what runs a bash call. A read-only git diff, log or show
// of the safe subset runs git directly, with --no-ext-diff and
// --no-textconv after the subcommand, since the repository's own
// diff.external and textconv drivers are programs and no configuration
// switches them off. Everything else is bash -c.
func command(ctx context.Context, dir, command string) *exec.Cmd {
	if words, ok := SafeWords(strings.TrimSpace(command)); ok && ReadOnlyGit(words, dir) {
		args := append([]string{words[1], "--no-ext-diff", "--no-textconv"}, words[2:]...)
		return exec.CommandContext(ctx, "git", args...)
	}
	return exec.CommandContext(ctx, "bash", "-c", command)
}
