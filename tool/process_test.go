package tool

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
)

// The background tool starts a command that outlives the call, lists it,
// reads what it printed since the last read, and stops it; closing
// the tool stops what is still running. The same on this machine,
// served over remote and through a tunnel (#153).
func TestTheProcessToolStartsListsReadsAndStops(t *testing.T) {
	dir := t.TempDir()
	local, err := workspace.NewLocal(dir, DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	for _, tc := range []struct {
		name string
		ws   workspace.Workspace
	}{{"local", local}, {"remote", served(t, local)}, {"tunnel", tunneled(t, local)}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pt := Process(NewFiles(tc.ws), 0)
			out, err := call(ctx, pt, `{"action":"start","command":"echo up; while :; do sleep 0.05; done"}`)
			if err != nil || !strings.Contains(out, "[1] running") || !strings.Contains(out, "up") {
				t.Fatalf("start = %q, %v", out, err)
			}
			if out, err := call(ctx, pt, `{"action":"list"}`); err != nil || !strings.Contains(out, "[1] running") {
				t.Errorf("list = %q, %v", out, err)
			}
			// Read once, it says so the next time.
			if out, err := call(ctx, pt, `{"action":"output","id":1}`); err != nil || !strings.Contains(out, "[no new output]") {
				t.Errorf("output = %q, %v", out, err)
			}
			if out, err := call(ctx, pt, `{"action":"stop","id":1}`); err != nil || !strings.Contains(out, "[1] exited") {
				t.Errorf("stop = %q, %v", out, err)
			}
			if _, err := call(ctx, pt, `{"action":"output","id":9}`); err == nil {
				t.Error("output of no process")
			}
			// One left running ends with the tool.
			if _, err := call(ctx, pt, `{"action":"start","command":"while :; do sleep 0.05; done"}`); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { pt.(interface{ Close() error }).Close(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("closing the tool did not end its process")
			}
			if out, _ := call(ctx, pt, `{"action":"list"}`); !strings.Contains(out, "[2] exited") {
				t.Errorf("after close: %q", out)
			}
		})
	}
}

// stuck is a workspace whose processes never say they ended.
type stuck struct{ *workspace.Local }

func (s stuck) Start(ctx context.Context, cmd workspace.Command) (workspace.Process, error) {
	p, err := s.Local.Start(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return stuckProc{Process: p, never: make(chan struct{})}, nil
}

type stuckProc struct {
	workspace.Process
	never chan struct{}
}

func (p stuckProc) Wait() (int, error) { <-p.never; return 0, nil }

// stop is bounded, as the tool's close is: a process whose end the
// workspace never reports does not hold the call (review, #153 low).
func TestStopIsBounded(t *testing.T) {
	defer func(d time.Duration) { stopWait = d }(stopWait)
	stopWait = 300 * time.Millisecond
	local, err := workspace.NewLocal(t.TempDir(), DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	pt := Process(NewFiles(stuck{local}), 0)
	if _, err := call(context.Background(), pt, `{"action":"start","command":"sleep 30"}`); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { out, _ := call(context.Background(), pt, `{"action":"stop","id":1}`); done <- out }()
	select {
	case out := <-done:
		if !strings.Contains(out, "did not say it ended") {
			t.Errorf("stop = %q", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stop waited on a process whose end never came")
	}
}

// An auto-allowed start's output loses a URL's credentials, as bash's
// does (review, #153 low).
func TestAnAutoAllowedStartsOutputIsRedacted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://user:ghp_SECRETTOKEN@github.com/o/r.git"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	pt := Process(newWS(t, dir), 0)
	out, err := call(context.Background(), pt, stampedBy(t, pt, `{"action":"start","command":"git remote -v"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SECRETTOKEN") || !strings.Contains(out, "https://***@github.com/o/r.git") {
		t.Errorf("start = %q", out)
	}
}
