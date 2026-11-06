package skills_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/ext/skills"
	"github.com/ChristopherDavenport/dax/extension"
)

// requests is scripted, keeping every request.
type requests struct {
	scripted
	mu   sync.Mutex
	reqs []openresponses.Request
}

func (m *requests) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	m.mu.Unlock()
	return m.scripted.CreateStream(ctx, req, sink)
}

// learnt is a skill_manage call that writes the skill fix-migration.
const learnt = `{"action":"create","name":"fix-migration","description":"How to rerun a flaky migration.","instructions":"Drop the lock table, then rerun.","allowed_tools":"Bash(echo:*)"}`

// The model writes a skill with skill_manage, which asks, into
// ~/.dax/learned; the next turn's catalogue lists it and the skill
// tool serves it, in the same session. Its allowed-tools grant nothing
// until the person approves its frontmatter's digest, even under
// -trust-skills (#159).
func TestTheModelWritesASkillAndUsesIt(t *testing.T) {
	ctx := context.Background()
	model := &requests{scripted: scripted{calls: [][2]string{
		{skills.ManageTool, learnt},
		{"skill", `{"name":"fix-migration"}`},
		{"bash", `{"command":"echo granted"}`},
	}}}
	o := session(t, model, skills.Options{Trust: true}, func(string) extension.Extension { return coding.New(0) })
	s, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var asked []string
	if _, err := prompt(ctx, s, "save how you did that as a skill", func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Name)
		return c.Name == skills.ManageTool
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{skills.ManageTool, "bash"}) {
		t.Errorf("asked about %v; want the write, and bash, which an unapproved skill does not grant", asked)
	}
	data, err := os.ReadFile(filepath.Join(o.UserDir, "learned", "fix-migration", "SKILL.md"))
	if err != nil || !strings.Contains(string(data), "Drop the lock table") {
		t.Fatalf("the skill was not written: %q, %v", data, err)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if strings.Contains(model.reqs[0].Instructions, "fix-migration") {
		t.Error("the skill was listed before it was written")
	}
	if last := model.reqs[len(model.reqs)-1]; !strings.Contains(last.Instructions, "fix-migration") {
		t.Errorf("the next turn's catalogue does not list the skill:\n%s", last.Instructions)
	}
	var outs []string
	for _, it := range s.Agent().State().Transcript {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok {
			outs = append(outs, o.Output.String())
		}
	}
	if len(outs) < 2 || !strings.Contains(outs[1], "Drop the lock table") {
		t.Errorf("the skill tool did not serve the new skill: %q", outs)
	}
}

// A learned skill's allowed-tools grant under -trust-skills once the
// person approved its frontmatter's digest, and stop when it changes.
func TestAnApprovedLearnedSkillGrants(t *testing.T) {
	ctx := context.Background()
	o := session(t, nil, skills.Options{Trust: true}, func(string) extension.Extension { return coding.New(0) })
	dir := filepath.Join(o.UserDir, "learned", "fix-migration")
	write(t, filepath.Join(dir, "SKILL.md"), "---\nname: fix-migration\ndescription: How to rerun it.\nallowed-tools: Bash(echo:*)\n---\nRerun.\n")
	if _, err := skills.Approve(o.UserDir, "fix-migration"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../skills/greet", "greet", "missing"} {
		if _, err := skills.Approve(o.UserDir, name); err == nil {
			t.Errorf("approved %q, which is no learned skill", name)
		}
	}
	o.Streamer = &scripted{calls: [][2]string{{"skill", `{"name":"fix-migration"}`}, {"bash", `{"command":"echo granted"}`}}}
	s, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var asked []string
	if _, err := prompt(ctx, s, "rerun it", func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Name)
		return false
	}); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Errorf("asked about %v; the approved skill grants echo", asked)
	}
}

// skill_manage refuses a name the project's or the user's skills hold,
// a skill that does not validate, and allowed-tools that do not parse,
// and writes nothing for any of them.
func TestSkillManageRefuses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, args, want string }{
		{"the user's skill's name", `{"action":"create","name":"greet","description":"x","instructions":"y"}`, "greet"},
		{"a name out of the grammar", `{"action":"create","name":"../x","description":"x","instructions":"y"}`, "name"},
		{"no description", `{"action":"create","name":"ok-name","instructions":"y"}`, "description"},
		{"allowed-tools that do not parse", `{"action":"create","name":"ok-name","description":"x","instructions":"y","allowed_tools":"Bash(echo"}`, "allowed"},
		{"an update of nothing", `{"action":"update","name":"ok-name","description":"x","instructions":"y"}`, "no learned skill"},
		{"a delete of nothing", `{"action":"delete","name":"ok-name"}`, "no learned skill"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := session(t, &scripted{calls: [][2]string{{skills.ManageTool, tc.args}}}, skills.Options{})
			s, err := agent.New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := prompt(ctx, s, "save it", func(*openresponses.FunctionCall, string) bool { return true }); err != nil {
				t.Fatal(err)
			}
			var out string
			for _, it := range s.Agent().State().Transcript {
				if o, ok := it.(*openresponses.FunctionCallOutput); ok {
					out = o.Output.String()
				}
			}
			if !strings.Contains(strings.ToLower(out), tc.want) || !strings.Contains(out, "Error") {
				t.Errorf("output %q, want an error naming %q", out, tc.want)
			}
			es, _ := os.ReadDir(filepath.Join(o.UserDir, "learned"))
			if len(es) != 0 {
				t.Errorf("learned holds %v", es)
			}
		})
	}
}

// After a reload the skill tool still claims a read of a project
// skill's file as a read of it (agentkit#96, WithFileClaim).
func TestAReloadKeepsTheProjectsFileClaim(t *testing.T) {
	ctx := context.Background()
	o := session(t, &scripted{calls: [][2]string{{skills.ManageTool, learnt}, {"skill", `{"name":"fix-migration"}`}}}, skills.Options{})
	write(t, filepath.Join(o.Dir, ".dax", "skills", "deploy", "SKILL.md"), "---\nname: deploy\ndescription: Deploy.\n---\nShip it.\n")
	s, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := prompt(ctx, s, "save it", func(*openresponses.FunctionCall, string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	tl, ok := s.Kit.LookupTool("skill")
	if !ok {
		t.Fatal("no skill tool")
	}
	if !strings.Contains(s.Agent().Config().Instructions, "fix-migration") && !strings.Contains(fmtCatalog(s), "fix-migration") {
		t.Fatal("the reload did not happen")
	}
	f, ok := tl.(agenttool.Factual)
	if !ok {
		t.Fatal("the skill tool makes no facts claim")
	}
	facts, err := f.Facts(ctx, json.RawMessage(`{"name":"deploy","path":"SKILL.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(facts)
	if !strings.Contains(string(b), `"read"`) || !strings.Contains(string(b), ".dax/skills/deploy/SKILL.md") {
		t.Errorf("the claim after the reload: %s", b)
	}
}

// fmtCatalog is the names the kit's catalogue lists.
func fmtCatalog(s *agent.Session) string {
	if c := s.Kit.Catalog(); c != nil {
		return strings.Join(c.Names(), ",")
	}
	return ""
}
