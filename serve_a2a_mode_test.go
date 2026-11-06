package dax

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/a2aproject/a2a-go/a2a"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/policy"
)

// An A2A caller's calls are decided as the default mode decides them,
// whatever mode the person put the served session in, at start or
// since: under bypassPermissions or acceptEdits a peer's bash, write
// and its sub-agent's write are still put to a view, and refused with
// none attached.
func TestAnA2ACallerIsNotUnderTheUsersMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start string // the mode the session starts in
		set   string // the mode switched to before the call
		m     func() *steps
		file  string
	}{
		{"bash under /mode bypassPermissions", "", policy.ModeBypass, func() *steps { return &steps{parent: [][2]string{{"bash", `{"command":"touch PEER"}`}}} }, "PEER"},
		{"bash started in bypassPermissions (-no-policy)", policy.ModeBypass, "", func() *steps { return &steps{parent: [][2]string{{"bash", `{"command":"touch PEER"}`}}} }, "PEER"},
		{"write under /mode acceptEdits", "", policy.ModeAcceptEdits, func() *steps {
			return &steps{parent: [][2]string{{"write", `{"path":"PEER","content":"x"}`}}}
		}, "PEER"},
		{"a sub-agent's write under /mode bypassPermissions", "", policy.ModeBypass, func() *steps {
			return &steps{parent: [][2]string{{"task", `{"input":"write the file"}`}}, child: [][2]string{{"write", `{"path":"NOTED","content":"x"}`}}}
		}, "NOTED"},
	} {
		for _, view := range []bool{true, false} {
			name := tc.name + ", no view"
			if view {
				name = tc.name + ", a view declines"
			}
			t.Run(name, func(t *testing.T) {
				rig := startRig(t, tc.m(), policy.Rules{Allow: []string{"task"}}, func(o *agent.Options) {
					if tc.start != "" {
						o.Policy.Mode = tc.start
					}
				}, nil, false)
				if tc.set != "" {
					if _, err := rig.sess.SetMode(t.Context(), tc.set); err != nil {
						t.Fatal(err)
					}
				}
				s := serveA2A(t, rig, false, t.TempDir())
				v := &answeringView{}
				if view {
					v.attach(t, s.rig.sess, agenttool.ActionDecline)
				}
				task, err := send(t, s.client(aliceToken), say("c1", "make it"))
				if err != nil {
					t.Fatal(err)
				}
				if task.Status.State != a2a.TaskStateCompleted {
					t.Fatalf("task %s, want completed", task.Status.State)
				}
				if _, err := os.Stat(filepath.Join(s.rig.dir, tc.file)); err == nil {
					t.Errorf("%s was made: the caller's call ran under the person's mode", tc.file)
				}
				if asked := v.questions(); view && len(asked) != 1 {
					t.Errorf("the view was asked %q, want the call", asked)
				}
			})
		}
	}
}

// A mode switch while an A2A caller's run goes leaves its calls where
// they were: the switch to bypassPermissions made while the peer's
// first call is asked about does not let its second run unasked.
func TestAModeSwitchDuringAPeersRunDoesNotReachIt(t *testing.T) {
	rig := startRig(t, &steps{parent: [][2]string{{"bash", `{"command":"touch A"}`}, {"bash", `{"command":"touch B"}`}}}, policy.Rules{}, nil, nil, false)
	s := serveA2A(t, rig, false, t.TempDir())
	var (
		mu    sync.Mutex
		asked []string
		once  sync.Once
		serr  error
	)
	defer viewed{s.rig.sess}.Subscribe(func(ctx context.Context, ev agentturn.Event) error {
		if q, ok := ev.(*agentturn.Question); ok {
			mu.Lock()
			asked = append(asked, q.Elicitation.Message)
			mu.Unlock()
			once.Do(func() { _, serr = s.rig.sess.SetMode(context.WithoutCancel(ctx), policy.ModeBypass) })
			go s.rig.sess.Reply(context.WithoutCancel(ctx), q.ID, agenttool.Answer{Action: agenttool.ActionAccept})
		}
		return nil
	})()
	if _, err := send(t, s.client(aliceToken), say("c1", "make them")); err != nil {
		t.Fatal(err)
	}
	if serr != nil {
		t.Fatal(serr)
	}
	if mode, _ := s.rig.sess.Mode(); mode != policy.ModeBypass {
		t.Fatalf("the served session is in %q", mode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 || !strings.Contains(asked[1], "touch B") {
		t.Errorf("asked %q, want both calls, the second after the switch", asked)
	}
}
