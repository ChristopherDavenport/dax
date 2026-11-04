package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// A process that dies while a batch runs leaves the record holding the
// result of a call that returned and no output for it. On resume that
// call is owed what it returned, never "whether it ran is not recorded",
// and is not run again (#162).
func TestACallThatReturnedBeforeAStopIsAnsweredWithItsResult(t *testing.T) {
	ctx := context.Background()
	model := &batched{calls: [][2]string{{"read", `{"path":"AGENTS.md"}`}, {"bash", `{"command":"sleep 30"}`}}}
	o := options(t, model)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	// The record as a process killed while bash runs leaves it: read
	// has returned, and bash, which runs alone, has been handed over.
	crashed := filepath.Join(t.TempDir(), "sessions")
	var once sync.Once
	s.ag.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		if d, ok := ev.(*agentturn.ToolDispatch); ok && d.Name == "bash" {
			once.Do(func() {
				if err := os.CopyFS(crashed, os.DirFS(o.Root)); err != nil {
					t.Error(err)
				}
				go s.ag.Abort()
			})
		}
		return nil
	})
	if _, err := promptOn(ctx, s, "look", nil); err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	o.Root = crashed
	r, err := Resume(ctx, o, id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var read *agentturn.PendingCall
	for _, p := range r.State().Pending {
		if p.Call.Name == "read" {
			read = &p
		}
	}
	if read == nil || read.Reason != agentturn.PendingReturned || read.Ran == nil {
		t.Fatalf("resumed pending = %+v, want read returned with its result", r.State().Pending)
	}
	if _, err := promptOn(ctx, r, "what happened?", nil); err != nil {
		t.Fatal(err)
	}
	var out string
	for _, it := range r.ag.State().Transcript {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok && o.CallID == read.Call.CallID {
			out = o.Output.Text
		}
	}
	if out != read.Ran.Output.Text || !strings.Contains(out, "Run go test before saying done.") {
		t.Errorf("the model was told %q, want read's own result %q", out, read.Ran.Output.Text)
	}
}
