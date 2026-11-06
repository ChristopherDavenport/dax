package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"

	"github.com/ChristopherDavenport/dax/extension"
)

// A file of the project's chain that agents_md_global also names is the
// project's, not the person's own: a caller's requests keep it. The
// person's own file is left out of what Env.AgentsMD gives on a
// caller's context, and kept on the person's (round 2, R2-3b, R2-3d).
func TestACallerKeepsTheProjectsAgentsMDAGlobalNames(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{})
	repo := t.TempDir()
	o.Dir = filepath.Join(repo, "sub")
	o.AgentsMD = true
	for p, text := range map[string]string{
		filepath.Join(repo, ".git", "HEAD"):   "ref: refs/heads/main\n",
		filepath.Join(repo, "AGENTS.md"):      "REPO-RULE-otter\n",
		filepath.Join(o.Dir, "AGENTS.md"):     "SUB-RULE-badger\n",
		filepath.Join(o.UserDir, "AGENTS.md"): "PERSONAL-RULE-ostrich\n",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	o.AgentsMDGlobal = []string{filepath.Join(repo, "AGENTS.md"), filepath.Join(o.Dir, "AGENTS.md")}
	var env extension.Env
	o.Extensions = append(o.Extensions, extension.Extension{Name: "seer", Kit: func(e extension.Env) ([]agentkit.Option, error) {
		env = e
		return nil, nil
	}})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	caller := context.WithValue(ctx, peerKey{}, "c1")
	got, err := s.callerMD.instructions("prompt" + agentkit.Separator + s.callerMD.all)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"REPO-RULE-otter", "SUB-RULE-badger"} {
		if !strings.Contains(got, want) {
			t.Errorf("a caller's instructions lack %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "PERSONAL-RULE-ostrich") {
		t.Errorf("a caller's instructions carry the person's own:\n%s", got)
	}
	if md := env.AgentsMD(caller); strings.Contains(md, "PERSONAL-RULE-ostrich") || !strings.Contains(md, "REPO-RULE-otter") {
		t.Errorf("Env.AgentsMD on a caller's context:\n%s", md)
	}
	if md := env.AgentsMD(ctx); !strings.Contains(md, "PERSONAL-RULE-ostrich") || !strings.Contains(md, "REPO-RULE-otter") {
		t.Errorf("Env.AgentsMD on the person's context:\n%s", md)
	}
}
