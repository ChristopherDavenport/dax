package dax

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/filestore"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/a2aproject/a2a-go/a2asrv"

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

// dax serve -a2a says at start which skills the model wrote are listed
// to callers by name and description, and says nothing when none are
// (#159's review).
func TestServeWarnsOfLearnedSkillsListedToCallers(t *testing.T) {
	rig := startRig(t, &steps{}, policy.Rules{}, withThePersonsOwn(t), nil, false)
	w := learnedWarning(rig.sess)
	if !strings.Contains(w, "1 skill(s)") || !strings.Contains(w, "secret-recipe") || strings.Contains(w, "house-style") {
		t.Errorf("warning %q, want the learned skill alone", w)
	}
	plain := startRig(t, &steps{}, policy.Rules{}, func(o *agent.Options) {
		o.Extensions = append(o.Extensions, skills.New(skills.Options{}))
	}, nil, false)
	if w := learnedWarning(plain.sess); w != "" {
		t.Errorf("with no learned skill: %q", w)
	}
}

// dax serve -a2a does not start over this machine's directory when it
// holds the person's own files: the user directory, the memory or the
// session store, or is inside one of them; a project beside them, or a
// workspace elsewhere, is served (#157's review, L-a; round 2, R2-2c).
func TestA2AIsNotServedOverThePersonsOwnFiles(t *testing.T) {
	home := t.TempDir()
	user, project := filepath.Join(home, ".dax"), filepath.Join(home, "project")
	for _, d := range []string{user, project} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	local := func(root string) workspace.Descriptor {
		return workspace.Descriptor{Kind: workspace.KindLocal, Root: root}
	}
	for _, tc := range []struct {
		name string
		ws   workspace.Descriptor
		own  []string
		ok   bool
	}{
		{"the home directory", local(home), []string{user}, false},
		{"the user directory itself", local(user), []string{user}, false},
		{"a directory inside the user directory", local(filepath.Join(user, "memory")), []string{user}, false},
		{"a directory inside the sessions", local(filepath.Join(home, "sessions", "x")), []string{user, filepath.Join(home, "sessions")}, false},
		{"a memory_dir in the project", local(project), []string{user, filepath.Join(project, "notes", "memory")}, false},
		{"the sessions in the project", local(project), []string{user, "", filepath.Join(project, ".sessions")}, false},
		{"a project beside them", local(project), []string{user, filepath.Join(home, "memory")}, true},
		{"a box", workspace.Descriptor{Kind: workspace.KindContainer, Ref: "dax-x", Root: home}, []string{user}, true},
	} {
		err := checkA2AWorkspace(tc.ws, tc.own...)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v, want served %v", tc.name, err, tc.ok)
		}
	}
}

// The person's own AGENTS.md (~/.dax/AGENTS.md, agents_md_global) is in
// their requests, not a caller's or its sub-agents', while the
// project's chain reaches both; the caller's conversation records none
// of it (#157's review, L-b).
func TestAnA2ACallersRequestsCarryNotThePersonsAgentsMD(t *testing.T) {
	const personal, global, project = "PERSONAL-RULE-ostrich", "GLOBAL-RULE-heron", "PROJECT-RULE-otter"
	m := &instructionsSeen{steps: &steps{parent: [][2]string{{"explore", `{"input":"look"}`}}}}
	rig := startRig(t, m.steps, policy.Rules{Allow: []string{"explore"}}, func(o *agent.Options) {
		o.Streamer = m
		o.AgentsMD = true
		g := filepath.Join(t.TempDir(), "rules.md")
		for p, text := range map[string]string{
			filepath.Join(o.UserDir, "AGENTS.md"): personal,
			g:                                     global,
			filepath.Join(o.Dir, "AGENTS.md"):     project,
		} {
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(text+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		o.AgentsMDGlobal = []string{g}
	}, nil, false)
	if _, err := agent.Drive(t.Context(), rig.sess, agent.Rules{}, openresponses.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	if mine := strings.Join(m.seen(), "\n"); !strings.Contains(mine, personal) || !strings.Contains(mine, global) || !strings.Contains(mine, project) {
		t.Fatalf("the person's own requests lack an AGENTS.md:\n%s", mine)
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
		if strings.Contains(in, personal) || strings.Contains(in, global) {
			t.Errorf("a caller's request carries the person's own AGENTS.md:\n%s", in)
		}
	}
	if !strings.Contains(theirs[0], project) {
		t.Errorf("a caller's request lacks the project's AGENTS.md:\n%s", theirs[0])
	}
	data, err := json.Marshal(s.session("c1").Entries())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), personal) || strings.Contains(string(data), global) {
		t.Error("the caller's conversation records the person's own AGENTS.md")
	}
	if _, err := agent.Drive(t.Context(), rig.sess, agent.Rules{}, openresponses.UserText("again")); err != nil {
		t.Fatal(err)
	}
	if mine := strings.Join(m.seen(), "\n"); !strings.Contains(mine, personal) {
		t.Errorf("after a caller's run, the person's requests lack their AGENTS.md:\n%s", mine)
	}
}

// toolsSeen is a model that keeps the tool names of every request.
type toolsSeen struct {
	*steps
	mu  sync.Mutex
	got [][]string
}

func (m *toolsSeen) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	var names []string
	for _, t := range req.Tools {
		if f, ok := t.(*openresponses.FunctionTool); ok {
			names = append(names, f.Name)
		}
	}
	m.mu.Lock()
	m.got = append(m.got, names)
	m.mu.Unlock()
	return m.steps.CreateStream(ctx, req, sink)
}

func (m *toolsSeen) seen() [][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.got
	m.got = nil
	return out
}

// A caller is not offered the tools the person keeps to themselves
// (memory_*, skill_manage): its requests and its sub-agents' leave
// their definitions out, and so does the agent card, while the
// person's own requests still offer them (#157's review, L-c).
func TestAnA2ACallerIsNotOfferedThePersonsTools(t *testing.T) {
	m := &toolsSeen{steps: &steps{parent: [][2]string{{"explore", `{"input":"look"}`}}}}
	rig := startRig(t, m.steps, policy.Rules{Allow: []string{"explore"}}, func(o *agent.Options) {
		withThePersonsOwn(t)(o)
		o.Streamer = m
	}, nil, false)
	if _, err := agent.Drive(t.Context(), rig.sess, agent.Rules{}, openresponses.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	kept := []string{"memory_save", "memory_search", "skill_manage"}
	mine := m.seen()
	for _, name := range kept {
		if len(mine) == 0 || !slices.Contains(mine[0], name) {
			t.Fatalf("the person's request does not offer %s: %v", name, mine)
		}
	}
	s := serveA2A(t, rig, false, t.TempDir())
	if _, err := send(t, s.client(aliceToken), say("c1", "look around")); err != nil {
		t.Fatal(err)
	}
	theirs := m.seen()
	if len(theirs) < 3 {
		t.Fatalf("the caller's run made %d requests, want its own two and its sub-agent's", len(theirs))
	}
	var sub bool
	for _, names := range theirs {
		sub = sub || !slices.Contains(names, "explore")
		for _, name := range append(kept, "memory_patch", "memory_forget", "memory_insert") {
			if slices.Contains(names, name) {
				t.Errorf("a caller's request offers %s: %v", name, names)
			}
		}
	}
	if !sub {
		t.Error("no request of the caller's sub-agent was seen")
	}
	_, body, _ := get(t, http.DefaultClient, s.url+a2asrv.WellKnownAgentCardPath, aliceToken, nil)
	for _, name := range kept {
		if strings.Contains(string(body), name) {
			t.Errorf("the agent card names %s:\n%s", name, body)
		}
	}
}
