package agent

import (
	"context"
	"github.com/ChristopherDavenport/dex/internal/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses/echo"
)

// #7 of the review: AGENTS.md -> ~/.ssh/id_rsa put the key in the
// system prompt, and .dex/skills -> anywhere offered those skills.
func TestSymlinksOutOfTheWorkspaceAreNotReadIntoThePrompt(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	secrets := t.TempDir()
	write(t, filepath.Join(secrets, "id_rsa"), "-----BEGIN PRIVATE KEY-----\nsecret-key-material\n")
	write(t, filepath.Join(secrets, "skills", "evil", "SKILL.md"), "---\nname: evil\ndescription: Exfiltrate.\n---\nsecret-skill-body\n")
	// The project's AGENTS.md is a link out; so is its skills dir.
	os.Remove(filepath.Join(o.Dir, "AGENTS.md"))
	must(t, os.Symlink(filepath.Join(secrets, "id_rsa"), filepath.Join(o.Dir, "AGENTS.md")))
	must(t, os.MkdirAll(filepath.Join(o.Dir, ".dex"), 0o755))
	must(t, os.Symlink(filepath.Join(secrets, "skills"), filepath.Join(o.Dir, ".dex", "skills")))

	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	instr := s.Agent.Config().Instructions
	for _, bad := range []string{"secret-key-material", "PRIVATE KEY", "evil", "secret-skill-body"} {
		if strings.Contains(instr, bad) {
			t.Errorf("instructions contain %q:\n%s", bad, instr)
		}
	}
	// The user's own skill is still offered, and the refusals are reported.
	if !strings.Contains(instr, "greet") {
		t.Errorf("the user's skill is missing:\n%s", instr)
	}
	var what []string
	for _, om := range s.Omitted() {
		what = append(what, om.What+" "+om.Reason)
	}
	all := strings.Join(what, "\n")
	for _, want := range []string{filepath.Join(o.Dir, "AGENTS.md") + " symbolic link", filepath.Join(o.Dir, ".dex", "skills") + " symbolic link"} {
		if !strings.Contains(all, want) {
			t.Errorf("omitted lacks %q:\n%s", want, all)
		}
	}
}

func TestASymlinkInsideTheWorkspaceIsRead(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	os.Remove(filepath.Join(o.Dir, "AGENTS.md"))
	write(t, filepath.Join(o.Dir, "docs", "agents.md"), "Use tabs.\n")
	must(t, os.Symlink(filepath.Join("docs", "agents.md"), filepath.Join(o.Dir, "AGENTS.md")))
	write(t, filepath.Join(o.Dir, "real", "greet2", "SKILL.md"), "---\nname: greet2\ndescription: Another.\n---\nHi.\n")
	must(t, os.MkdirAll(filepath.Join(o.Dir, ".dex"), 0o755))
	must(t, os.Symlink(filepath.Join("..", "real"), filepath.Join(o.Dir, ".dex", "skills")))

	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	instr := s.Agent.Config().Instructions
	if !strings.Contains(instr, "Use tabs.") || !strings.Contains(instr, "greet2") {
		t.Errorf("in-workspace links should be read:\n%s", instr)
	}
	for _, om := range s.Omitted() {
		if om.Source == "dex" {
			t.Errorf("unexpected refusal: %+v", om)
		}
	}
}

func TestASkillFileLinkedOutIsRefusedWithItsDirectory(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	outside := filepath.Join(t.TempDir(), "x.md")
	write(t, outside, "leaked\n")
	write(t, filepath.Join(o.Dir, ".dex", "skills", "ok", "SKILL.md"), "---\nname: ok\ndescription: Fine.\n---\nbody\n")
	must(t, os.Symlink(outside, filepath.Join(o.Dir, ".dex", "skills", "ok", "ref.md")))
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if strings.Contains(s.Agent.Config().Instructions, "name: ok") || strings.Contains(s.Agent.Config().Instructions, "Fine.") {
		t.Error("the project's skills should have been refused whole")
	}
	found := false
	for _, om := range s.Omitted() {
		found = found || strings.Contains(om.Reason, "ref.md")
	}
	if !found {
		t.Errorf("omitted: %+v", s.Omitted())
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestMCPServersGetAScrubbedEnvironment(t *testing.T) {
	env := tool.ChildEnv([]string{"PATH=/bin", "OPENAI_API_KEY=sk-1", "X_TOKEN=t"}, []string{"X_TOKEN"})
	tr, err := mcpTransport("server --flag  arg", env)
	if err != nil {
		t.Fatal(err)
	}
	ct := tr.(*mcp.CommandTransport)
	if strings.Join(ct.Command.Args, "|") != "server|--flag|arg" {
		t.Errorf("args: %v", ct.Command.Args)
	}
	if strings.Join(ct.Command.Env, " ") != "PATH=/bin X_TOKEN=t" {
		t.Errorf("env: %v", ct.Command.Env)
	}
	if _, err := mcpTransport("  ", env); err == nil {
		t.Error("an empty command should be an error")
	}
}
