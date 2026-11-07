//go:build unix

package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
)

// fakeEnv is the session as dax-hooks reads it: its directory, the
// environment a child gets, and the user's log.
type fakeEnv struct {
	extension.Env
	dir  string
	env  []string
	mu   sync.Mutex
	logs []string
}

func (e *fakeEnv) Dir() string                { return e.dir }
func (e *fakeEnv) HostEnv() []string          { return e.env }
func (e *fakeEnv) ToolEnv() extension.ToolEnv { return extension.ToolEnv{} }
func (e *fakeEnv) Log(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logs = append(e.logs, fmt.Sprintf(format, args...))
}

func (e *fakeEnv) said() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.logs, "\n")
}

func newEnv(t *testing.T) *fakeEnv {
	t.Helper()
	home := t.TempDir()
	return &fakeEnv{dir: t.TempDir(), env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}}
}

// guard is the PreToolUse hooks of c over env, as the engine calls them.
func guard(t *testing.T, env *fakeEnv, c Command) extension.PolicyHooks {
	t.Helper()
	ext := New(Options{PreToolUse: []Matcher{{Commands: []Command{c}}}})
	ph, err := ext.PolicyHooks(env)
	if err != nil {
		t.Fatal(err)
	}
	return ph
}

func callInfo(id, args string) agentturn.ToolCallInfo {
	call := &openresponses.FunctionCall{Name: "bash", Arguments: args, CallID: id}
	return agentturn.ToolCallInfo{Call: call, Args: json.RawMessage(args)}
}

// A string command is a shell's line, as Claude Code runs it: a pipe,
// && and ~ work, so a hook copied from Claude Code's settings guards
// here too (review M3).
func TestAStringHookRunsThroughTheShell(t *testing.T) {
	env := newEnv(t)
	home := strings.TrimPrefix(env.env[1], "HOME=")
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif grep -q 'push --force'; then echo \"no force pushes in $CLAUDE_PROJECT_DIR\" >&2; exit 2; fi\n"
	if err := os.WriteFile(filepath.Join(home, "bin", "no-force-push"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"~/bin/no-force-push",
		"cat | grep -q 'push --force' && { echo \"no force pushes in $CLAUDE_PROJECT_DIR\" >&2; exit 2; }; exit 0",
	} {
		ph := guard(t, env, Command{Shell: line})
		d, err := ph.Decide(context.Background(), callInfo("c1", `{"command":"git push --force"}`))
		if err != nil || d == nil || d.Action != agentturn.Block || !strings.Contains(d.Reason, "no force pushes in "+env.dir) {
			t.Errorf("%q: decision %+v (%v), want refused with the hook's stderr naming the project", line, d, err)
		}
		d, err = ph.Decide(context.Background(), callInfo("c2", `{"command":"git push"}`))
		if err != nil || d != nil {
			t.Errorf("%q on a plain push: decision %+v (%v), want none", line, d, err)
		}
	}
}

// A guard that cannot answer refuses the call: one that does not start,
// that the shell cannot find, that dies by a signal or that runs past
// its timeout. A guard failing open would let through the calls it is
// there to stop (review M3).
func TestAGuardThatCannotAnswerRefusesTheCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Command
	}{
		{"no such program", Command{Argv: []string{filepath.Join(t.TempDir(), "missing")}}},
		{"the shell finds no program", Command{Shell: "dax-no-such-hook-program --check"}},
		{"not executable", Command{Shell: "/dev/null"}},
		{"killed by a signal", Command{Shell: "kill -9 $$"}},
		{"past its timeout", Command{Shell: "sleep 5", Timeout: 200 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			ph := guard(t, env, tc.c)
			start := time.Now()
			d, err := ph.Decide(context.Background(), callInfo("c1", `{"command":"ls"}`))
			if err != nil || d == nil || d.Action != agentturn.Block || !strings.Contains(d.Reason, "refused") {
				t.Fatalf("decision %+v (%v), want the call refused", d, err)
			}
			if time.Since(start) > 4*time.Second {
				t.Errorf("took %s", time.Since(start))
			}
			if !strings.Contains(env.said(), "refused") {
				t.Errorf("the user was told %q, want the refusal", env.said())
			}
		})
	}
}

// Another exit status is Claude Code's non-blocking error: the user is
// told, and the call is decided as if the hook had said nothing.
func TestAHookThatExitsOneDoesNotBlock(t *testing.T) {
	env := newEnv(t)
	ph := guard(t, env, Command{Shell: "echo oops >&2; exit 1"})
	d, err := ph.Decide(context.Background(), callInfo("c1", `{"command":"ls"}`))
	if err != nil || d != nil {
		t.Fatalf("decision %+v (%v), want none", d, err)
	}
	if !strings.Contains(env.said(), "oops") {
		t.Errorf("the user was told %q, want the hook's stderr", env.said())
	}
}

// A PostToolUse hook that cannot run leaves the result as it was: the
// call has happened.
func TestAPostToolUseHookThatFailsLeavesTheResult(t *testing.T) {
	env := newEnv(t)
	r := &runner{env: env, matchers: []Matcher{{Commands: []Command{{Shell: "dax-no-such-hook-program"}}}}}
	call := &openresponses.FunctionCall{Name: "bash", Arguments: `{}`, CallID: "c1"}
	ov, err := r.after(context.Background(), agentturn.ToolResultInfo{Call: call, Result: agenttool.Text("done")})
	if err != nil || ov != nil {
		t.Fatalf("override %+v (%v), want none", ov, err)
	}
	if !strings.Contains(env.said(), "goes on") {
		t.Errorf("the user was told %q", env.said())
	}
}

// A run cancelled while a guard runs kills the hook and every process
// it started at once, not at its timeout (review low).
func TestACancelledRunKillsTheHooksProcessGroup(t *testing.T) {
	env := newEnv(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	ph := guard(t, env, Command{Shell: "sleep 30 & echo $! > '" + pidFile + "'; wait", Timeout: 60 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *agentturn.ToolDecision, 1)
	go func() {
		d, _ := ph.Decide(ctx, callInfo("c1", `{"command":"ls"}`))
		done <- d
	}()
	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if pid == 0 {
		t.Fatal("the hook did not start its child")
	}
	cancel()
	select {
	case d := <-done:
		if d == nil || d.Action != agentturn.Block {
			t.Errorf("decision %+v, want refused", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled run waited for its hook")
	}
	gone := false
	for deadline := time.Now().Add(5 * time.Second); !gone && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		gone = syscall.Kill(pid, 0) != nil
	}
	if !gone {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Error("the hook's child outlived the cancelled run")
	}
}

// A call whose arguments are not JSON is refused by a guard that would
// read it, never skipped; a call with no arguments is given {} (review
// low).
func TestAGuardRefusesArgumentsItCannotRead(t *testing.T) {
	env := newEnv(t)
	got := filepath.Join(t.TempDir(), "in")
	ph := guard(t, env, Command{Shell: "cat > '" + got + "'"})
	d, err := ph.Decide(context.Background(), callInfo("c1", `{"command":`))
	if err != nil || d == nil || d.Action != agentturn.Block {
		t.Fatalf("arguments not JSON: decision %+v (%v), want refused", d, err)
	}
	if _, err := os.Stat(got); err == nil {
		t.Error("the hook ran on arguments that are not JSON")
	}
	d, err = ph.Decide(context.Background(), callInfo("c2", ``))
	if err != nil || d != nil {
		t.Fatalf("no arguments: decision %+v (%v), want none", d, err)
	}
	var in struct {
		ToolInput json.RawMessage `json:"tool_input"`
	}
	b, _ := os.ReadFile(got)
	if err := json.Unmarshal(b, &in); err != nil || string(in.ToolInput) != `{}` {
		t.Errorf("the hook read %s (%v), want tool_input {}", b, err)
	}
}
