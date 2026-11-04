package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// A bare run leaves the user's own AGENTS.md out, keeps the
// repository's, and says in its header what it left out, so two
// sessions read as the configurations they were (#141).
func TestABareSessionLeavesTheUsersAgentsMDOutAndSaysSo(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	write(t, filepath.Join(o.UserDir, "AGENTS.md"), "Always answer in French.\n")
	o.NoUserAgentsMD = true
	o.Bare = []string{"config", "agents_md_home", "skills", "memory"}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	instr := s.ag.Config().Instructions
	if strings.Contains(instr, "Always answer in French.") {
		t.Error("a bare session read the user's AGENTS.md")
	}
	if !strings.Contains(instr, "Run go test before saying done.") {
		t.Error("a bare session left the repository's AGENTS.md out")
	}
	h, _ := headerOf(t, o.Root, s)
	var p struct {
		Bare    bool     `json:"bare"`
		LeftOut []string `json:"left_out"`
	}
	if err := json.Unmarshal(h.Extra[headerProfile], &p); err != nil || !p.Bare || strings.Join(p.LeftOut, ",") != "config,agents_md_home,skills,memory" {
		t.Errorf("%s = %s (%v), want bare and what it left out", headerProfile, h.Extra[headerProfile], err)
	}

	// Without it, the user's file is read and the header says nothing.
	o = options(t, &echo.Adapter{})
	write(t, filepath.Join(o.UserDir, "AGENTS.md"), "Always answer in French.\n")
	s, err = New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.ag.Config().Instructions, "Always answer in French.") {
		t.Error("the user's AGENTS.md is left out of a session that is not bare")
	}
	if h, _ := headerOf(t, o.Root, s); h.Extra[headerProfile] != nil {
		t.Errorf("a session that is not bare records %s", h.Extra[headerProfile])
	}
}

// A bare run neither reads nor writes the MCP trust file in ~/.dax: a
// project's servers start only with -trust-mcp, for that session alone,
// whatever the file trusts (#141).
func TestABareSessionIgnoresTheMCPTrustFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mcp-trust.json")
	d := workspace.Descriptor{Kind: "local", Root: "/p"}
	servers := []MCPServer{{Name: "mine", Command: "a"}, {Name: "theirs", Command: "b", Project: true}}
	key := recordedWorkspace{Kind: d.Kind, Ref: d.Ref, Root: d.Root}
	if err := recordTrust(file, key, serverSetHash(servers[1:], false)); err != nil {
		t.Fatal(err)
	}
	if got := trusted(servers, d, false, file, false); len(got) != 2 {
		t.Fatalf("the trust file does not trust the set: %v", got)
	}
	o := Options{MCPTrustFile: file, NoMCPTrustFile: true}
	if f := o.trustFile(); f != "" {
		t.Fatalf("a bare session's trust file is %q", f)
	}
	var warned strings.Builder
	restore := CaptureWarnings(&warned)
	got := trusted(servers, d, false, o.trustFile(), false)
	restore()
	if len(got) != 1 || got[0].Name != "mine" {
		t.Errorf("a bare session started %v, want yours alone", got)
	}
	if strings.Contains(warned.String(), "trust file") {
		t.Errorf("the trust file was read: %s", warned.String())
	}
	before, _ := os.ReadFile(file)
	if got := trusted(servers, d, false, o.trustFile(), true); len(got) != 2 {
		t.Errorf("-trust-mcp under -bare started %v", got)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Error("-trust-mcp under -bare wrote the trust file")
	}
}
