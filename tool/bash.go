package tool

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/workspace"
)

const (
	defaultBashTimeout = 2 * time.Minute
	maxBashBytes       = 50 << 10
	// progressEvery is how often a running command's output is reported
	// as progress, and progressBytes the most a report holds: what the
	// command printed since the last one, or its last progressBytes when
	// it printed more. The final result the model sees is untouched.
	progressEvery = 250 * time.Millisecond
	progressBytes = 4096
)

// BashArgs are the arguments of the bash tool.
type BashArgs struct {
	Command string `json:"command" desc:"The command to run"`
	Timeout int    `json:"timeout_seconds,omitempty" desc:"Kill the command after this many seconds (default 120)"`
	// Stamp is set by dax when the policy allowed the command without
	// asking; a value that dax did not set makes the call fail.
	Stamp string `json:"dax_stamp,omitempty" desc:"Set by dax; leave it out"`
}

// BashOption configures the bash tool.
type BashOption func(*bashConfig)

type bashConfig struct {
	maxFile int64
}

// WithMaxFile sets the largest file the auto-allowed cat, head, tail,
// wc and grep may be given; see Analyzer.
func WithMaxFile(n int64) BashOption { return func(c *bashConfig) { c.maxFile = n } }

// Bash returns a tool that runs a shell command at the workspace's
// root, on whichever machine the workspace is, with the environment the
// workspace gives its processes. It is sequential: a batch that
// contains a shell command runs one call at a time, so a command never
// races a concurrent edit of the same file.
//
// Its facts claim is what a call would touch, from the same analysis the
// policy's bash rules read (BashSubjects), and, for a line the analysis
// allows unasked, the arguments carrying the stamp of that plan: the
// tool then runs only that plan (command).
func Bash(f *Files, opts ...BashOption) agenttool.Tool {
	var cfg bashConfig
	for _, o := range opts {
		o(&cfg)
	}
	an := &Analyzer{Files: f, MaxFile: cfg.maxFile}
	return exactArgs[BashArgs](bashTool(f, cfg, agenttool.WithFacts(func(ctx context.Context, args json.RawMessage) (agenttool.Facts, error) {
		calls, rewrite, err := bashFacts(ctx, an, args, true)
		return agenttool.Facts{Calls: calls, Rewrite: rewrite}, err
	})))
}

func bashTool(f *Files, cfg bashConfig, claim agenttool.Option) agenttool.Tool {
	return agenttool.New("bash", "Run a bash command in the working directory and return its combined output and exit code.",
		func(ctx context.Context, in BashArgs) (string, error) {
			if strings.TrimSpace(in.Command) == "" {
				return "", errors.New("command is required")
			}
			timeout := defaultBashTimeout
			if in.Timeout > 0 {
				timeout = time.Duration(in.Timeout) * time.Second
			}
			cmd, err := command(ctx, &Analyzer{Files: f, MaxFile: cfg.maxFile}, in)
			if err != nil {
				return "", err
			}
			out := &capWriter{max: maxBashBytes}
			cmd.Stream = io.MultiWriter(out, &progressWriter{ctx: ctx})
			cmd.Timeout = timeout
			res, runErr := f.Workspace().Exec(ctx, cmd)
			if in.Stamp != "" {
				out.redact = true
			}

			var b strings.Builder
			b.WriteString(out.String())
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteByte('\n')
			}
			switch {
			case runErr != nil:
				return "", runErr
			case res.TimedOut:
				fmt.Fprintf(&b, "[killed after %s]", timeout)
			default:
				fmt.Fprintf(&b, "[exit %d]", res.ExitCode)
			}
			return b.String(), nil
		}, agenttool.WithSequential(), claim)
}

// progressWriter reports a command's output as it arrives: as
// [agenttool.Progress] the window since the last report, at most one
// report per progressEvery, so the terminal client shows the command
// running instead of waiting for it to end. A report reaches the
// fronts of the run only, never the model or the session record, so it
// is not redacted where the result of an auto-allowed command is: what
// is on the user's own screen is what their command printed.
type progressWriter struct {
	ctx context.Context

	mu    sync.Mutex
	since []byte    // the output since the last report, its last progressBytes
	last  time.Time // when the last report went, zero before the first
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.since = append(w.since, p...)
	if len(w.since) > progressBytes {
		w.since = append(w.since[:0], w.since[len(w.since)-progressBytes:]...)
	}
	var window string
	if w.last.IsZero() || time.Since(w.last) >= progressEvery {
		// Copy: the next write reuses the slice while the report
		// travels without the lock.
		window = string(w.since)
		w.since, w.last = w.since[:0], time.Now()
	}
	w.mu.Unlock()
	if window != "" {
		agenttool.Progress(w.ctx, agenttool.Result{
			Output: openresponses.FunctionCallOutputData{Text: window},
		})
	}
	return len(p), nil
}

// command is what runs a bash call, and in which environment.
//
// A call the policy allowed without asking carries the stamp of the plan
// it approved (StampArgs). It runs only if the line still analyses to
// that plan: as the plan rendered, every word quoted and --no-ext-diff
// --no-textconv added to git diff, log and show, in an environment with
// autoEnv added. If the line has changed since, because a file is
// different or the repository's git config now names a program, the
// call fails with errChanged, and the original line is never run
// instead. A call without a stamp, one a person approved or one run with
// no policy, is bash -c exactly as given, in the workspace's
// environment, the user's minus credentials: their hooks, their
// sshCommand and their GIT_CONFIG_* are theirs.
func command(ctx context.Context, an *Analyzer, in BashArgs) (workspace.Command, error) {
	if in.Stamp != "" {
		c := an.Check(ctx, in.Command)
		if !c.Auto || !hmac.Equal([]byte(stampOf(c.Render())), []byte(in.Stamp)) {
			return workspace.Command{}, errChanged
		}
		return workspace.Command{Args: []string{"bash", "-c", c.Render()}, Env: autoEnv()}, nil
	}
	return workspace.Command{Args: []string{"bash", "-c", in.Command}}, nil
}

// capWriter keeps the first max bytes written to it and counts the
// rest, so the output of a command is bounded in memory.
type capWriter struct {
	redact  bool
	mu      sync.Mutex
	max     int
	buf     bytes.Buffer
	dropped int
}

func (c *capWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if room := c.max - c.buf.Len(); room > 0 {
		k := min(room, len(p))
		c.buf.Write(p[:k])
		p = p[k:]
	}
	c.dropped += len(p)
	return n, nil
}

func (c *capWriter) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.buf.String()
	if c.redact {
		s = redactUserinfo(s)
	}
	if c.dropped > 0 {
		s += fmt.Sprintf("\n... [truncated, %d bytes omitted]", c.dropped)
	}
	return s
}

var userinfo = regexp.MustCompile(`(://)[^/@\s]+@`)

// redactUserinfo replaces the user:password@ or token@ of a URL.
func redactUserinfo(s string) string { return userinfo.ReplaceAllString(s, "${1}***@") }
