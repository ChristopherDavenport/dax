package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/policy"
)

// The human plane is agentturn's control contract: a controller that
// holds nothing but it, in a goroutine of its own and with no front
// attached, drives a session end to end. It answers by rule the
// permission a run stops on and the question a sub-agent's call asks
// while it runs, and the work both were about is done.
func TestAControllerHoldingOnlyTheControlDrivesASession(t *testing.T) {
	ctx := context.Background()
	model := &twoModels{
		parent: scripted{calls: [][2]string{
			{"bash", `{"command":"touch by-parent"}`},
			{"explore", `{"input":"look around"}`},
		}},
		child: scripted{calls: [][2]string{{"bash", `{"command":"touch by-child"}`}}},
	}
	o := withAgents(options(t, model), "")
	o.Policy = &policy.Settings{Builtin: true, Fallback: "ask"}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var turn agentturn.Control = s
	var mu sync.Mutex
	var permitted, replied []string
	type result struct {
		end *agentturn.RunEnd
		err error
	}
	done := make(chan result, 1)
	go func() {
		// Every question asked while a call runs is allowed.
		defer s.Answering()()
		defer turn.Subscribe(func(_ context.Context, ev agentturn.Event) error {
			q, ok := ev.(*agentturn.Question)
			if !ok {
				return nil
			}
			go func() {
				mu.Lock()
				replied = append(replied, q.Call.Name+" "+string(q.Call.Args))
				mu.Unlock()
				turn.Reply(ctx, q.ID, agenttool.Answer{Action: agenttool.ActionAccept})
			}()
			return nil
		})()
		// Every call the policy asks about is allowed.
		end, err := turn.Prompt(ctx, openresponses.UserText("go"))
		for err == nil && end.Reason == agentturn.ReasonInputRequired {
			var answers []agentturn.Answer
			for _, p := range PermissionsOf(turn, end) {
				mu.Lock()
				permitted = append(permitted, p.Call.Name+" "+p.Call.Arguments)
				mu.Unlock()
				answers = append(answers, agentturn.Approve(p.Call.CallID))
			}
			end, err = turn.Resume(ctx, answers...)
		}
		done <- result{end, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the controller did not finish")
	}
	if r.err != nil || r.end.Reason != agentturn.ReasonDone {
		t.Fatalf("the session ended %v, %v; want done", r.end, r.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(permitted) != 1 || !strings.Contains(permitted[0], "by-parent") {
		t.Errorf("permitted %q, want the parent's bash call", permitted)
	}
	if len(replied) != 1 || !strings.Contains(replied[0], "by-child") {
		t.Errorf("replied to %q, want the explore child's bash call", replied)
	}
	for _, name := range []string{"by-parent", "by-child"} {
		if _, err := os.Stat(filepath.Join(o.Dir, name)); err != nil {
			t.Errorf("%s was not made: %v", name, err)
		}
	}
}
