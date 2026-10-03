package agent

import (
	"context"
	"github.com/ChristopherDavenport/dex/internal/tool"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// #13 of the review: the store root was created 0755.
func TestTheStoreAndMemoryAreCreatedPrivate(t *testing.T) {
	var warned strings.Builder
	stderr = &warned
	defer func() { stderr = os.Stderr }()
	o := options(t, &echo.Adapter{})
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, d := range []string{o.Root, o.MemoryDir} {
		if m := mode(t, d); m != 0o700 {
			t.Errorf("%s is %04o, want 0700", d, m)
		}
	}
	if warned.Len() != 0 {
		t.Errorf("a fresh directory is not warned about: %q", warned.String())
	}
}

func TestAnExistingWorldReadableStoreIsMadePrivateWithAWarning(t *testing.T) {
	var warned strings.Builder
	stderr = &warned
	defer func() { stderr = os.Stderr }()
	o := options(t, &echo.Adapter{})
	must(t, os.MkdirAll(o.Root, 0o755))
	must(t, os.Chmod(o.Root, 0o755))
	must(t, os.MkdirAll(o.MemoryDir, 0o750))
	must(t, os.Chmod(o.MemoryDir, 0o750))
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if m := mode(t, o.Root); m != 0o700 {
		t.Errorf("root is %04o", m)
	}
	if m := mode(t, o.MemoryDir); m != 0o700 {
		t.Errorf("memory is %04o", m)
	}
	for _, want := range []string{o.Root + " was readable by other users (mode 0755)", o.MemoryDir + " was readable by other users (mode 0750)"} {
		if !strings.Contains(warned.String(), want) {
			t.Errorf("warning lacks %q:\n%s", want, warned.String())
		}
	}
	// Opened again, it is quiet.
	warned.Reset()
	s, _ = New(context.Background(), o)
	s.Close()
	if warned.Len() != 0 {
		t.Errorf("second open warned: %q", warned.String())
	}
	// Import and GC go through the same door.
	must(t, os.Chmod(o.Root, 0o755))
	if _, err := GC(context.Background(), o.Root, false, time.Hour); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, o.Root); m != 0o700 {
		t.Errorf("after gc root is %04o", m)
	}
}

// -trust-skills trusts the user's skills and never the repository's.
func TestTrustSkillsNeverTrustsARepositorysSkill(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		dir       func(o Options) string // where the skill is written
		wantAsked int
	}{
		{"the user's own skills", func(o Options) string { return filepath.Join(o.UserDir, "skills") }, 0},
		{"a configured skills_dirs", func(o Options) string { return o.SkillsDirs[0] }, 0},
		{"the repository's .dex/skills", func(o Options) string { return filepath.Join(o.Dir, ".dex", "skills") }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &scripted{calls: [][2]string{
				{"skill", `{"name":"pgreet"}`},
				{"bash", `{"command":"echo hello"}`},
			}}
			o := options(t, model)
			o.SkillsDirs = []string{filepath.Join(t.TempDir(), "configured")}
			must(t, os.MkdirAll(o.SkillsDirs[0], 0o755))
			write(t, filepath.Join(tc.dir(o), "pgreet", "SKILL.md"),
				"---\nname: pgreet\ndescription: Greets.\nallowed-tools: Bash(echo:*)\n---\nSay hello.\n")
			o.Policy, o.TrustSkills = confirmPolicy(t), true
			var asked []string
			o.Approve = func(c *openresponses.FunctionCall, _ string) bool {
				asked = append(asked, c.Arguments)
				return true
			}
			s, err := New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := s.Prompt(ctx, "greet me"); err != nil {
				t.Fatal(err)
			}
			if len(asked) != tc.wantAsked {
				t.Fatalf("asked %v, want %d question(s)", asked, tc.wantAsked)
			}
		})
	}
}

// The hook that stamps an auto-allowed bash call, end to end: the
// model's own stamp is removed, an allowed call is stamped and runs.
func TestAnAutoAllowedBashCallRunsStampedThroughASession(t *testing.T) {
	ctx := context.Background()
	model := &scripted{calls: [][2]string{
		{"bash", `{"command":"pwd","dex_stamp":"forged"}`},
		{"bash", `{"command":"touch PWN","dex_stamp":"forged"}`},
	}}
	o := options(t, model)
	o.Policy = confirmPolicy(t)
	var asked []string
	o.Approve = func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Arguments)
		return false
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Prompt(ctx, "go"); err != nil {
		t.Fatal(err)
	}
	var outputs []string
	for _, it := range s.Agent.State().Transcript {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok {
			outputs = append(outputs, o.Output.Text)
		}
	}
	if len(outputs) < 1 || !strings.Contains(outputs[0], "[exit 0]") || !strings.Contains(outputs[0], o.Dir) {
		t.Errorf("pwd, allowed without asking, did not run: %q", outputs)
	}
	if len(asked) != 1 || strings.Contains(asked[0], "dex_stamp") && !strings.Contains(asked[0], "forged") {
		t.Errorf("questions: %v", asked)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "PWN")); err == nil {
		t.Error("touch ran")
	}
}
