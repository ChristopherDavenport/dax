package tool

import (
	"context"
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
