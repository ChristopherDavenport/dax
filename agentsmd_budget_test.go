package dax

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsmd"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/acp"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/policy"
)

// dax's own AGENTS.md reaches the model in a session started here with
// the default budget (#190), with room for the person's own files.
func TestDaxsOwnAGENTSmdFitsTheDefaultBudget(t *testing.T) {
	fi, err := os.Stat("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > config.DefaultAgentsMDBudget*3/4 {
		t.Errorf("AGENTS.md is %d bytes, more than three quarters of the default agents_md_budget, %d: trim it, or raise the default", fi.Size(), config.DefaultAgentsMDBudget)
	}
}

func TestTheAGENTSmdNotice(t *testing.T) {
	over := func(what string, size int64) agentkit.Omission {
		return agentkit.Omission{Part: agentsmd.PartID, Source: agentkit.SourceAgentsMD, What: what, Reason: agentsmd.OverBudget.String(), Size: size}
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
	} {
		if got := agentsMDNotice(tc.info, tc.userConfig); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// overBudget writes an AGENTS.md past the default budget in o's
// directory, and turns the chain on.
func overBudget(t *testing.T, o *agent.Options) {
	t.Helper()
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.Dir, "AGENTS.md"), []byte(strings.Repeat("x", 65<<10)), 0o644); err != nil {
		t.Fatal(err)
	}
	o.AgentsMD = true
}

// The terminal client says, without -v, that an AGENTS.md was left out
// for the budget, before it takes the screen.
func TestTheTUISaysAnAGENTSmdOverBudgetWithoutVerbose(t *testing.T) {
	r := startRig(t, &steps{}, policy.Rules{}, func(o *agent.Options) { overBudget(t, o) }, func(f *tuiFront) { f.info.UserConfig = "/u/config.json" }, true)
	r.quit()
	notice := "AGENTS.md: left out " + filepath.Join(r.dir, "AGENTS.md") + " (66560 bytes), past the chain's budget of 65536 bytes; agents_md_budget in your config (/u/config.json) raises it\n"
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
	want := "AGENTS.md: left out " + filepath.Join(o.Dir, "AGENTS.md") + " (66560 bytes), past the chain's budget of 65536 bytes; agents_md_budget in your config raises it\n"
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("standard error %q lacks %q", stderr.String(), want)
	}
}

// An editor (-front acp) has no start line: the notice goes to standard
// error, and to the person as an agent message ahead of the first
// prompt's reply, once (#194).
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
	notice := "AGENTS.md: left out " + filepath.Join(o.Dir, "AGENTS.md") + " (66560 bytes), past the chain's budget of 65536 bytes; agents_md_budget in your config (/u/config.json) raises it"
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
