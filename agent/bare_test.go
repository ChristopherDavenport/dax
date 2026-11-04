package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

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
