package dax

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentsmd"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/acp"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/policy"
)

// dax's own AGENTS.md reaches the model in a session started here with
// the default budget (#190), and is held to 16 KiB, a quarter of it, so
// the person's own files and nested ones have room (#193): what a
// session needs only for some work goes in a skill.
func TestDaxsOwnAGENTSmdFitsTheDefaultBudget(t *testing.T) {
	const most = 16 << 10
	fi, err := os.Stat("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > most || most > config.DefaultAgentsMDBudget/4 {
		t.Errorf("AGENTS.md is %d bytes, more than %d (the default agents_md_budget is %d): move what only some work needs into a skill under .dax/skills", fi.Size(), most, config.DefaultAgentsMDBudget)
	}
}

// The release procedure AGENTS.md points to is a skill the session
// offers: .dax/skills/release, which loads without problems (#193).
func TestTheReleaseSkillLoads(t *testing.T) {
	md, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), ".dax/skills/release/SKILL.md") {
		t.Error("AGENTS.md does not point to .dax/skills/release/SKILL.md")
	}
	cat, err := agentskill.DiscoverDirs(".dax/skills")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Skills) != 1 || cat.Skills[0].Name != "release" || len(cat.Problems) != 0 {
		t.Errorf("skills %d, problems %v; want release alone, without problems", len(cat.Skills), cat.Problems)
	}
}

func TestTheAGENTSmdNotice(t *testing.T) {
	over := func(what string, size int64) agentkit.Omission {
		return agentkit.Omission{Part: agentsmd.PartID, Source: agentkit.SourceAgentsMD, What: what, Reason: agentsmd.OverBudget.String(), Size: size}
	}
	cut := func(what string, size int64) agentkit.Omission {
		return agentkit.Omission{Part: agentsmd.PartID, Source: agentkit.SourceAgentsMD, What: what, Reason: agentsmd.Truncated.String(), Size: size}
	}
	for _, tc := range []struct {
		name       string
		info       agent.Info
		userConfig string
		want       string
	}{
		{"nothing left out", agent.Info{AgentsMDBudget: 65536}, "/u/config.json", ""},
		{"another kind left out", agent.Info{AgentsMDBudget: 65536, Omitted: []agentkit.Omission{
			{Part: agentsmd.PartID, Source: "dax", What: "/w/AGENTS.md", Reason: "a link out of the workspace"},
			{Part: agentsmd.PartID, Source: agentkit.SourceAgentsMD, What: "/w/agents.md", Reason: agentsmd.Shadowed.String(), By: "/w/AGENTS.md"},
		}}, "/u/config.json", ""},
		{"one file", agent.Info{AgentsMDBudget: 65536, Omitted: []agentkit.Omission{over("/w/AGENTS.md", 70000)}}, "/u/config.json",
			"AGENTS.md: left out /w/AGENTS.md (70000 bytes), past the chain's budget of 65536 bytes; agents_md_budget in your config (/u/config.json) raises it"},
		{"files after it", agent.Info{AgentsMDBudget: 65536, Omitted: []agentkit.Omission{over("/w/AGENTS.md", 70000), over("/w/a/AGENTS.md", 10), over("/w/a/b/AGENTS.md", 10)}}, "",
			"AGENTS.md: left out /w/AGENTS.md (70000 bytes) and 2 files after it, past the chain's budget of 65536 bytes; agents_md_budget in your config raises it"},
		{"the project's bound", agent.Info{AgentsMDBudget: 6144, AgentsMDProject: 4096, Omitted: []agentkit.Omission{over("/w/AGENTS.md", 5000), over("/w/a/AGENTS.md", 10)}}, "/u/config.json",
			"AGENTS.md: left out /w/AGENTS.md (5000 bytes) and 1 file after it, past the 4096 bytes the project's .dax/config.json allows its files (agents_md_budget)"},
		{"a cut", agent.Info{AgentsMDBudget: 65536, AgentsMDKept: 64512, Omitted: []agentkit.Omission{cut("/w/AGENTS.md", 5488)}}, "/u/config.json",
			"AGENTS.md: cut /w/AGENTS.md at 64512 bytes, 5488 bytes left out, past the chain's budget of 65536 bytes; agents_md_budget in your config (/u/config.json) raises it"},
		{"a cut and a file after it", agent.Info{AgentsMDBudget: 65536, AgentsMDKept: 64512, Omitted: []agentkit.Omission{cut("/w/AGENTS.md", 5488), over("/w/a/AGENTS.md", 10)}}, "",
			"AGENTS.md: cut /w/AGENTS.md at 64512 bytes, 5488 bytes left out, and left out 1 file after it, past the chain's budget of 65536 bytes; agents_md_budget in your config raises it"},
		{"a cut and files after it", agent.Info{AgentsMDBudget: 65536, AgentsMDKept: 64512, Omitted: []agentkit.Omission{cut("/w/AGENTS.md", 5488), over("/w/a/AGENTS.md", 10), over("/w/a/b/AGENTS.md", 10)}}, "",
			"AGENTS.md: cut /w/AGENTS.md at 64512 bytes, 5488 bytes left out, and left out 2 files after it, past the chain's budget of 65536 bytes; agents_md_budget in your config raises it"},
		{"a cut at the project's bound", agent.Info{AgentsMDBudget: 6144, AgentsMDProject: 4096, AgentsMDKept: 3000, Omitted: []agentkit.Omission{cut("/w/AGENTS.md", 2000), over("/w/a/AGENTS.md", 10)}}, "/u/config.json",
			"AGENTS.md: cut /w/AGENTS.md at 3000 bytes, 2000 bytes left out, and left out 1 file after it, past the 4096 bytes the project's .dax/config.json allows its files (agents_md_budget)"},
		{"a cut to the marker alone", agent.Info{AgentsMDBudget: 1024, Omitted: []agentkit.Omission{cut("/w/AGENTS.md", 70000)}}, "",
			"AGENTS.md: cut /w/AGENTS.md at 0 bytes, 70000 bytes left out, past the chain's budget of 1024 bytes; agents_md_budget in your config raises it"},
	} {
		if got := agentsMDNotice(tc.info, tc.userConfig); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// overBudget writes an AGENTS.md past the default budget in o's
// directory, 65 lines of 1 KiB, and turns the chain on. The chain cuts
// it after 63 lines (64512 bytes), the most that fit 65536 with
// agentsmd's marker, and 2048 bytes are left out.
func overBudget(t *testing.T, o *agent.Options) {
	t.Helper()
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.Dir, "AGENTS.md"), []byte(strings.Repeat(strings.Repeat("x", 1023)+"\n", 65)), 0o644); err != nil {
		t.Fatal(err)
	}
	o.AgentsMD = true
}

// The terminal client says, without -v, that an AGENTS.md was cut for
// the budget, before it takes the screen.
func TestTheTUISaysAnAGENTSmdOverBudgetWithoutVerbose(t *testing.T) {
	r := startRig(t, &steps{}, policy.Rules{}, func(o *agent.Options) { overBudget(t, o) }, func(f *tuiFront) { f.info.UserConfig = "/u/config.json" }, true)
	r.quit()
	notice := "AGENTS.md: cut " + filepath.Join(r.dir, "AGENTS.md") + " at 64512 bytes, 2048 bytes left out, past the chain's budget of 65536 bytes; agents_md_budget in your config (/u/config.json) raises it\n"
	if want, pre := notice+"To resume this session: dax -resume "+r.sess.ID()+"\n", r.pre.String(); pre != want {
		t.Errorf("the output is %q, want %q", pre, want)
	}
}

// -p says it on standard error, before the run.
func TestPrintSaysAnAGENTSmdOverBudgetOnStderr(t *testing.T) {
	base := t.TempDir()
	o := agent.Options{
		Model: "scripted", Streamer: &steps{}, Dir: filepath.Join(base, "project"), Root: filepath.Join(base, "sessions"),
		UserDir:    filepath.Join(base, "user"),
		Extensions: []extension.Extension{coding.New(0)},
		Policy:     &policy.Settings{Builtin: true, Fallback: "allow"},
	}
	overBudget(t, &o)
	sess, err := agent.New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	var stderr strings.Builder
	f := &printFront{info: frontInfo{Name: "dax", Prompt: "go"}, in: bufio.NewScanner(strings.NewReader("")), stderr: &stderr}
	if err := f.run(t.Context(), sess, sess); err != nil {
		t.Fatal(err)
	}
	want := "AGENTS.md: cut " + filepath.Join(o.Dir, "AGENTS.md") + " at 64512 bytes, 2048 bytes left out, past the chain's budget of 65536 bytes; agents_md_budget in your config raises it\n"
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("standard error %q lacks %q", stderr.String(), want)
	}
}

// An editor (-front acp) has no start line: the notice goes to standard
// error, and to the person as an agent message ahead of the first
// prompt's reply, once (#194), with the same words for a cut file
// (#197).
func TestAnEditorIsToldOfAnAGENTSmdOverBudget(t *testing.T) {
	base := t.TempDir()
	o := agent.Options{
		Model: "scripted", Streamer: &steps{}, Dir: filepath.Join(base, "project"), Root: filepath.Join(base, "sessions"),
		UserDir:    filepath.Join(base, "user"),
		Extensions: []extension.Extension{coding.New(0)},
		Policy:     &policy.Settings{Builtin: true, Fallback: "ask"},
	}
	overBudget(t, &o)
	sess, err := agent.New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	var stderr syncBuf
	e := startACPFront(t, sess, &acpFront{info: frontInfo{Dir: o.Dir, UserConfig: "/u/config.json"}, errw: &stderr})
	defer e.stop()
	e.call(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: 1})
	var created acp.NewSessionResponse
	json.Unmarshal(e.call(acp.MethodSessionNew, acp.NewSessionRequest{CWD: o.Dir}).Result, &created)
	notice := "AGENTS.md: cut " + filepath.Join(o.Dir, "AGENTS.md") + " at 64512 bytes, 2048 bytes left out, past the chain's budget of 65536 bytes; agents_md_budget in your config (/u/config.json) raises it"
	if got := stderr.String(); got != "dax: "+notice+"\n" {
		t.Errorf("standard error %q, want the notice", got)
	}
	for i, want := range []bool{true, false} {
		e.call(acp.MethodSessionPrompt, acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{acp.Text("hi")}})
		var texts []string
		for _, u := range e.updates() {
			if u.SessionUpdate == acp.UpdateAgentMessage && u.Content != nil {
				texts = append(texts, u.Content.Text)
			}
		}
		told := len(texts) > 0 && texts[0] == "[dax] "+notice+"\n\n"
		if told != want || strings.Count(strings.Join(texts, ""), notice) != map[bool]int{true: 1, false: 0}[want] {
			t.Errorf("prompt %d: agent messages %q; want the notice first: %v", i+1, texts, want)
		}
	}
}

// /tools and -v list a cut file among what was left out, with the
// bytes sent and the bytes left out, since its first part is in the
// instructions; a file left out whole is listed as before.
func TestTheAssemblyListsACutAGENTSmd(t *testing.T) {
	info := agent.Info{AgentsMDKept: 64512, Omitted: []agentkit.Omission{
		{Part: agentsmd.PartID, Source: agentkit.SourceAgentsMD, What: "/w/AGENTS.md", Reason: agentsmd.Truncated.String(), Size: 2048},
		{Part: agentsmd.PartID, Source: agentkit.SourceAgentsMD, What: "/w/a/AGENTS.md", Reason: agentsmd.OverBudget.String(), Size: 10},
	}}
	got := assemblyLines(info)[1:]
	want := []string{
		"omitted: agentsmd: /w/AGENTS.md (truncated): its first 64512 bytes sent, 2048 bytes left out",
		"omitted: agentsmd: /w/a/AGENTS.md (over budget)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
