package dax

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/filestore"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/skills"
	"github.com/ChristopherDavenport/dax/policy"
)

// secret is what the person's own memory and learned skill hold, which
// no caller may read.
const secret = "the launch code is zebra"

// withThePersonsOwn gives the rig dax-skills, a user memory entry and a
// learned skill holding secret, and an installed skill of the user's.
func withThePersonsOwn(t *testing.T) func(*agent.Options) {
	return func(o *agent.Options) {
		o.Extensions = append(o.Extensions, skills.New(skills.Options{}))
		mem, err := filestore.Open(filepath.Join(o.UserDir, "memory"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mem.Put(t.Context(), agentmemory.Entry{Scope: "user", Name: "launch-code", Content: secret}); err != nil {
			t.Fatal(err)
		}
		for dir, body := range map[string]string{
			filepath.Join(skills.LearnedDir(o.UserDir), "secret-recipe"): secret,
			filepath.Join(o.UserDir, "skills", "house-style"):            "Write short sentences.",
		} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			md := "---\nname: " + filepath.Base(dir) + "\ndescription: A skill.\n---\n" + body + "\n"
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// outputs are the tool outputs a session recorded, joined.
func outputs(t *testing.T, s *a2aServed, id string) string {
	t.Helper()
	var outs []string
	for _, e := range s.session(id).Entries() {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if o, ok := it.Item.(*openresponses.FunctionCallOutput); ok {
				// Text, or the content parts a skill's read returns.
				data, err := json.Marshal(o.Output)
				if err != nil {
					t.Fatal(err)
				}
				outs = append(outs, o.Output.Text, string(data))
			}
		}
	}
	return strings.Join(outs, "\n")
}

// An A2A caller is refused, outright and asking no one, every tool that
// reads or changes the person's own memory or learned skills, and so is
// its sub-agent: a view attached that would approve anything is asked
// nothing, the memory and the learned directory are as they were, and
// nothing the caller is told holds the person's memory (#157's review,
// H1).
func TestAnA2ACallerCannotUseThePersonsMemoryOrSkills(t *testing.T) {
	for _, tc := range []struct {
		tool, args string
		child      bool // called by the caller's explore sub-agent
	}{
		{"memory_search", `{"query":"zebra"}`, false},
		{"memory_save", `{"scope":"user","name":"planted","content":"planted"}`, false},
		{"memory_patch", `{"scope":"user","name":"launch-code","old_text":"zebra","new_text":"planted"}`, false},
		{"memory_forget", `{"scope":"user","name":"launch-code"}`, false},
		{"memory_insert", `{"scope":"user-archive","content":"planted"}`, false},
		{"skill_manage", `{"action":"create","name":"planted","description":"A plant.","instructions":"planted"}`, false},
		{"memory_search", `{"query":"zebra"}`, true},
	} {
		name := tc.tool
		if tc.child {
			name = "a sub-agent's " + tc.tool
		}
		t.Run(name, func(t *testing.T) {
			m := &steps{parent: [][2]string{{tc.tool, tc.args}}}
			if tc.child {
				m = &steps{parent: [][2]string{{"explore", `{"input":"look"}`}}, child: [][2]string{{tc.tool, tc.args}}}
			}
			rig := startRig(t, m, policy.Rules{Allow: []string{"explore"}}, withThePersonsOwn(t), nil, false)
			s := serveA2A(t, rig, false, t.TempDir())
			v := &answeringView{}
			v.attach(t, s.rig.sess, agenttool.ActionAccept)
			if _, err := send(t, s.client(aliceToken), say("c1", "use it")); err != nil {
				t.Fatal(err)
			}
			got := outputs(t, s, "c1")
			if tc.child {
				got = m.sawInChild()
			}
			if !strings.Contains(got, tc.tool+" "+agent.PersonOnlyNote) || strings.Contains(got, "zebra") {
				t.Errorf("the caller was told %q; want %s refused as the person's own", got, tc.tool)
			}
			if asked := v.questions(); len(asked) != 0 {
				t.Errorf("the view was asked %q; want the call refused unasked", asked)
			}
			mem, err := filestore.Open(filepath.Join(rig.o.UserDir, "memory"))
			if err != nil {
				t.Fatal(err)
			}
			for _, sc := range []agentmemory.Scope{"user", "user-archive"} {
				es, err := mem.List(t.Context(), sc)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range es {
					if e.Name != "launch-code" || e.Content != secret {
						t.Errorf("the person's memory holds %s/%s %q", sc, e.Name, e.Content)
					}
				}
				if sc == "user" && len(es) != 1 {
					t.Errorf("the person's memory holds %d entries, want the one", len(es))
				}
			}
			if _, err := os.Stat(filepath.Join(skills.LearnedDir(rig.o.UserDir), "planted")); err == nil {
				t.Error("the caller wrote a learned skill")
			}
		})
	}
}

// A caller reads the skills the person installed, and not the ones the
// model wrote for the person (#159's review, L1): the skill tool serves
// it the installed skill and refuses the learned one.
func TestAnA2ACallerReadsNoLearnedSkill(t *testing.T) {
	m := &steps{parent: [][2]string{{"skill", `{"name":"house-style"}`}, {"skill", `{"name":"secret-recipe"}`}}}
	rig := startRig(t, m, policy.Rules{}, withThePersonsOwn(t), nil, false)
	s := serveA2A(t, rig, false, t.TempDir())
	if _, err := send(t, s.client(aliceToken), say("c1", "read them")); err != nil {
		t.Fatal(err)
	}
	got := outputs(t, s, "c1")
	if !strings.Contains(got, "Write short sentences.") || strings.Contains(got, "zebra") || !strings.Contains(got, "secret-recipe") {
		t.Errorf("the caller was told %q; want the installed skill and the learned one refused", got)
	}
}

// instructionsSeen is a model that keeps the instructions of every
// request it is sent.
type instructionsSeen struct {
	*steps
	mu  sync.Mutex
	got []string
}

func (m *instructionsSeen) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	m.got = append(m.got, req.Instructions)
	m.mu.Unlock()
	return m.steps.CreateStream(ctx, req, sink)
}

// seen are the instructions sent since the last call, and forgets them.
func (m *instructionsSeen) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.got
	m.got = nil
	return out
}

// A caller's requests, its own and its sub-agents', carry no memory
// block: the person's memory, their user scope across every project
// included, is shown to the person's own requests alone, and the
// caller's conversation records none of it (#157's review, H1).
func TestAnA2ACallersRequestsCarryNoMemory(t *testing.T) {
	m := &instructionsSeen{steps: &steps{parent: [][2]string{{"explore", `{"input":"look"}`}}}}
	rig := startRig(t, m.steps, policy.Rules{Allow: []string{"explore"}}, func(o *agent.Options) {
		withThePersonsOwn(t)(o)
		o.Streamer = m
	}, nil, false)
	if _, err := agent.Drive(t.Context(), rig.sess, agent.Rules{}, openresponses.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	if mine := strings.Join(m.seen(), "\n"); !strings.Contains(mine, "zebra") {
		t.Fatalf("the person's own requests carry no memory block:\n%s", mine)
	}
	s := serveA2A(t, rig, false, t.TempDir())
	if _, err := send(t, s.client(aliceToken), say("c1", "look around")); err != nil {
		t.Fatal(err)
	}
	theirs := m.seen()
	if len(theirs) < 3 {
		t.Fatalf("the caller's run made %d requests, want its own two and its sub-agent's", len(theirs))
	}
	for _, in := range theirs {
		if strings.Contains(in, "zebra") || strings.Contains(in, "launch-code") {
			t.Errorf("a caller's request carries the person's memory:\n%s", in)
		}
	}
	data, err := json.Marshal(s.session("c1").Entries())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "zebra") {
		t.Error("the caller's conversation records the person's memory")
	}
	// The person's next request still carries it.
	if _, err := agent.Drive(t.Context(), rig.sess, agent.Rules{}, openresponses.UserText("again")); err != nil {
		t.Fatal(err)
	}
	if mine := strings.Join(m.seen(), "\n"); !strings.Contains(mine, "zebra") {
		t.Errorf("after a caller's run, the person's requests carry no memory block:\n%s", mine)
	}
}
