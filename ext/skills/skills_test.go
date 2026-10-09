package skills_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/ext/skills"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/workspace"
)

// scripted makes the calls in order, one per model call, then answers.
type scripted struct{ calls [][2]string }

func (m *scripted) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	step := 0
	for _, it := range req.Input {
		switch v := it.(type) {
		case *openresponses.Message:
			if v.Role == openresponses.RoleUser {
				step = 0
			}
		case *openresponses.FunctionCallOutput:
			step++
		}
	}
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if step < len(m.calls) {
		call, err := em.FunctionCall("", m.calls[step][0])
		if err != nil {
			return err
		}
		if err := call.Arguments(m.calls[step][1]); err != nil {
			return err
		}
		if err := call.Close(); err != nil {
			return err
		}
		return em.Complete()
	}
	msg, err := em.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if err := msg.Text("done"); err != nil {
		return err
	}
	if err := msg.Close(); err != nil {
		return err
	}
	return em.Complete()
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const greet = "---\nname: greet\ndescription: How to greet the user.\nallowed-tools: Bash(echo:*)\n---\nSay hello.\n"

// session is dax-skills, after the extensions in before, over model,
// under the policy's default of asking, in a fresh project and user
// directory, the user's greet skill installed.
func session(t *testing.T, model openresponses.Streamer, so skills.Options, before ...func(dir string) extension.Extension) agent.Options {
	t.Helper()
	base := t.TempDir()
	o := agent.Options{
		Model: "scripted", Streamer: model,
		Dir: filepath.Join(base, "project"), UserDir: filepath.Join(base, "user"),
		Policy: &policy.Settings{Builtin: true, Fallback: "ask"},
	}
	must(t, os.MkdirAll(o.Dir, 0o755))
	write(t, filepath.Join(o.UserDir, "skills", "greet", "SKILL.md"), greet)
	for _, b := range before {
		o.Extensions = append(o.Extensions, b(o.Dir))
	}
	o.Extensions = append(o.Extensions, skills.New(so))
	return o
}

func TestTheExtension(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    skills.Options
	}{{"default", skills.Options{}}, {"trusted with dirs", skills.Options{Dirs: []string{"/x"}, Trust: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			e := skills.New(tc.o)
			if e.Name != "dax-skills" || !slices.Equal(e.Owns, []string{"skill"}) || !slices.Equal(e.Policy.Allow, []string{"skill"}) ||
				len(e.Policy.Ask)+len(e.Policy.Deny) != 0 || e.Tools != nil || e.Kit == nil {
				t.Errorf("extension %+v", e)
			}
		})
	}
}

// The skill tool is offered, the user's skill is in the catalogue, and
// reading a skill runs unasked.
func TestASkillIsOfferedAndReadUnasked(t *testing.T) {
	var approve func(*openresponses.FunctionCall, string) bool
	ctx := context.Background()
	o := session(t, &scripted{calls: [][2]string{{"skill", `{"name":"greet"}`}}}, skills.Options{})
	approve = func(c *openresponses.FunctionCall, reason string) bool {
		t.Errorf("asked about %s: %s", c.Name, reason)
		return false
	}
	s, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var names []string
	for _, tl := range s.Tools() {
		names = append(names, tl.Name)
	}
	if !slices.Contains(names, "skill") {
		t.Errorf("tools %v lack skill", names)
	}
	if !strings.Contains(s.Agent().Config().Instructions, "greet") {
		t.Errorf("the user's skill is not in the catalogue:\n%s", s.Agent().Config().Instructions)
	}
	if _, err := prompt(ctx, s, "greet me", approve); err != nil {
		t.Fatal(err)
	}
	var out string
	for _, it := range s.Agent().State().Transcript {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok {
			out = o.Output.String() // the skill tool answers in parts
		}
	}
	if !strings.Contains(out, "Say hello.") {
		t.Errorf("the skill's body was not returned: %q", out)
	}
}

// #7 of the review: .dax/skills -> anywhere offered those skills. A
// project's skills directory that is, or holds, a link out of the
// workspace is left out whole and reported; one inside it is read.
func TestAProjectsSkillsLinkedOutAreLeftOut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, dir, outside string)
		refused string // in the omission's reason; empty: none
		offered string // in the instructions
	}{
		{"the directory is a link out", func(t *testing.T, dir, outside string) {
			write(t, filepath.Join(outside, "evil", "SKILL.md"), "---\nname: evil\ndescription: Exfiltrate.\n---\nsecret-skill-body\n")
			must(t, os.MkdirAll(filepath.Join(dir, ".dax"), 0o755))
			must(t, os.Symlink(outside, filepath.Join(dir, ".dax", "skills")))
		}, "symbolic link outside the workspace", ""},
		{"a file in it is a link out", func(t *testing.T, dir, outside string) {
			write(t, filepath.Join(outside, "x.md"), "leaked\n")
			write(t, filepath.Join(dir, ".dax", "skills", "ok", "SKILL.md"), "---\nname: ok\ndescription: Fine.\n---\nbody\n")
			must(t, os.Symlink(filepath.Join(outside, "x.md"), filepath.Join(dir, ".dax", "skills", "ok", "ref.md")))
		}, "ref.md", ""},
		{".dax is a link out", func(t *testing.T, dir, outside string) {
			write(t, filepath.Join(outside, "skills", "evil", "SKILL.md"), "---\nname: evil\ndescription: Exfiltrate.\n---\nsecret-skill-body\n")
			must(t, os.Symlink(outside, filepath.Join(dir, ".dax")))
		}, "symbolic link outside the workspace", ""},
		{"a link to a file of the workspace outside the skills directory", func(t *testing.T, dir, _ string) {
			write(t, filepath.Join(dir, ".env"), "SECRET_TOKEN=leaked\n")
			write(t, filepath.Join(dir, ".dax", "skills", "ok", "SKILL.md"), "---\nname: ok\ndescription: Fine.\n---\nbody\n")
			must(t, os.Symlink(filepath.Join("..", "..", "..", ".env"), filepath.Join(dir, ".dax", "skills", "ok", "notes.md")))
		}, "outside the skills directory", ""},
		{"a SKILL.md that is such a link", func(t *testing.T, dir, _ string) {
			write(t, filepath.Join(dir, "docs", "evil.md"), "---\nname: evil\ndescription: Exfiltrate.\n---\nsecret-skill-body\n")
			must(t, os.MkdirAll(filepath.Join(dir, ".dax", "skills", "evil"), 0o755))
			must(t, os.Symlink(filepath.Join("..", "..", "..", "docs", "evil.md"), filepath.Join(dir, ".dax", "skills", "evil", "SKILL.md")))
		}, "outside the skills directory", ""},
		{"an absolute link to the workspace outside the skills directory", func(t *testing.T, dir, _ string) {
			write(t, filepath.Join(dir, ".env"), "SECRET_TOKEN=leaked\n")
			write(t, filepath.Join(dir, ".dax", "skills", "ok", "SKILL.md"), "---\nname: ok\ndescription: Fine.\n---\nbody\n")
			must(t, os.Symlink(filepath.Join(dir, ".env"), filepath.Join(dir, ".dax", "skills", "ok", "notes.md")))
		}, "outside the skills directory", ""},
		{"a chain of links that leaves the skills directory", func(t *testing.T, dir, _ string) {
			write(t, filepath.Join(dir, ".env"), "SECRET_TOKEN=leaked\n")
			write(t, filepath.Join(dir, ".dax", "skills", "ok", "SKILL.md"), "---\nname: ok\ndescription: Fine.\n---\nbody\n")
			must(t, os.Symlink(filepath.Join("..", "..", ".."), filepath.Join(dir, ".dax", "skills", "ok", "up")))
			must(t, os.Symlink(filepath.Join("up", ".env"), filepath.Join(dir, ".dax", "skills", "ok", "notes.md")))
		}, "outside the skills directory", ""},
		{"a link between two skills is read", func(t *testing.T, dir, _ string) {
			write(t, filepath.Join(dir, ".dax", "skills", "greet2", "SKILL.md"), "---\nname: greet2\ndescription: Another.\n---\nHi.\n")
			write(t, filepath.Join(dir, ".dax", "skills", "wave", "SKILL.md"), "---\nname: wave\ndescription: Waves.\n---\nHi.\n")
			must(t, os.Symlink(filepath.Join("..", "greet2", "SKILL.md"), filepath.Join(dir, ".dax", "skills", "wave", "shared.md")))
		}, "", "greet2"},
		{"a link that stays inside is read", func(t *testing.T, dir, _ string) {
			write(t, filepath.Join(dir, "real", "greet2", "SKILL.md"), "---\nname: greet2\ndescription: Another.\n---\nHi.\n")
			must(t, os.MkdirAll(filepath.Join(dir, ".dax"), 0o755))
			must(t, os.Symlink(filepath.Join("..", "real"), filepath.Join(dir, ".dax", "skills")))
		}, "", "greet2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := session(t, &scripted{}, skills.Options{})
			tc.setup(t, o.Dir, t.TempDir())
			s, err := agent.New(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			instr := s.Agent().Config().Instructions
			for _, bad := range []string{"evil", "secret-skill-body", "Fine.", "leaked"} {
				if strings.Contains(instr, bad) {
					t.Errorf("instructions contain %q", bad)
				}
			}
			if !strings.Contains(instr, "greet") || tc.offered != "" && !strings.Contains(instr, tc.offered) {
				t.Errorf("instructions lack the user's skill or %q:\n%s", tc.offered, instr)
			}
			var ours []string
			for _, om := range s.Omitted() {
				if om.Source == skills.Name {
					ours = append(ours, om.What+" "+om.Reason)
				}
			}
			switch {
			case tc.refused == "" && len(ours) > 0:
				t.Errorf("unexpected refusal: %v", ours)
			case tc.refused != "" && (len(ours) != 1 || !strings.Contains(ours[0], filepath.Join(o.Dir, ".dax", "skills")) || !strings.Contains(ours[0], tc.refused)):
				t.Errorf("omitted %v, want the skills directory refused for %q", ours, tc.refused)
			}
		})
	}
}

// -trust-skills trusts the user's skills and never the repository's.
func TestTrustSkillsNeverTrustsARepositorysSkill(t *testing.T) {
	var approve func(*openresponses.FunctionCall, string) bool
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		dir       func(o agent.Options, configured string) string // where the skill is written
		wantAsked int
	}{
		{"the user's own skills", func(o agent.Options, _ string) string { return filepath.Join(o.UserDir, "skills") }, 0},
		{"a configured skills_dirs", func(_ agent.Options, c string) string { return c }, 0},
		{"the repository's .dax/skills", func(o agent.Options, _ string) string { return filepath.Join(o.Dir, ".dax", "skills") }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &scripted{calls: [][2]string{
				{"skill", `{"name":"pgreet"}`},
				{"bash", `{"command":"echo hello"}`},
			}}
			configured := filepath.Join(t.TempDir(), "configured")
			must(t, os.MkdirAll(configured, 0o755))
			// The skill grants Bash(echo:*), dax-coding's alias.
			o := session(t, model, skills.Options{Dirs: []string{configured}, Trust: true}, func(dir string) extension.Extension { return coding.New(0) })
			write(t, filepath.Join(tc.dir(o, configured), "pgreet", "SKILL.md"),
				"---\nname: pgreet\ndescription: Greets.\nallowed-tools: Bash(echo:*)\n---\nSay hello.\n")
			var asked []string
			approve = func(c *openresponses.FunctionCall, _ string) bool {
				asked = append(asked, c.Arguments)
				return true
			}
			s, err := agent.New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := prompt(ctx, s, "greet me", approve); err != nil {
				t.Fatal(err)
			}
			if len(asked) != tc.wantAsked {
				t.Fatalf("asked %v, want %d question(s)", asked, tc.wantAsked)
			}
		})
	}
}

// A configured skills directory that does not exist is an error, unlike
// the project's and the user's.
func TestAMissingConfiguredDirectoryIsAnError(t *testing.T) {
	o := session(t, &scripted{}, skills.Options{Dirs: []string{filepath.Join(t.TempDir(), "nope")}})
	if s, err := agent.New(context.Background(), o); err == nil {
		s.Close()
		t.Error("a missing skills_dirs entry was accepted")
	}
}

// The project's skills come first, then the user's, then the config's:
// a skill of one name in the project shadows the user's, as it did
// when the kit read both directories, and the user's shadows a
// configured one.
func TestTheProjectsSkillShadowsTheUsers(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "configured")
	write(t, filepath.Join(configured, "greet", "SKILL.md"), "---\nname: greet\ndescription: Configured greet.\n---\nHi.\n")
	write(t, filepath.Join(configured, "wave", "SKILL.md"), "---\nname: wave\ndescription: Configured wave.\n---\nHi.\n")
	o := session(t, &scripted{}, skills.Options{Dirs: []string{configured}})
	write(t, filepath.Join(o.Dir, ".dax", "skills", "greet", "SKILL.md"), "---\nname: greet\ndescription: Project greet.\n---\nHi.\n")
	s, err := agent.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	instr := s.Agent().Config().Instructions
	for _, w := range []string{"Project greet.", "Configured wave.", filepath.Join(o.Dir, ".dax", "skills", "greet", "SKILL.md")} {
		if !strings.Contains(instr, w) {
			t.Errorf("instructions lack %q:\n%s", w, instr)
		}
	}
	for _, n := range []string{"How to greet the user.", "Configured greet."} {
		if strings.Contains(instr, n) {
			t.Errorf("instructions hold the shadowed %q", n)
		}
	}
}

// noLinks is a workspace whose file system cannot tell a link from
// what it leads to, as one over a plain file API may not.
type noLinks struct{ *workspace.Local }

func (n noLinks) FS() fs.FS { return struct{ fs.FS }{n.Local.FS()} }

// Where the workspace cannot say where a link leads, a skills directory
// with a link below it is refused, failing toward refusing; one with no
// link is offered.
func TestAWorkspaceThatCannotReadLinksRefusesALinkedSkill(t *testing.T) {
	for _, tc := range []struct {
		name    string
		link    bool
		refused string
	}{
		{"no link", false, ""},
		{"a link between two skills", true, "holds a symbolic link the workspace cannot follow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := session(t, &scripted{}, skills.Options{})
			write(t, filepath.Join(o.Dir, ".dax", "skills", "greet2", "SKILL.md"), "---\nname: greet2\ndescription: Another.\n---\nHi.\n")
			if tc.link {
				must(t, os.Symlink("SKILL.md", filepath.Join(o.Dir, ".dax", "skills", "greet2", "again.md")))
			}
			local, err := workspace.NewLocal(o.Dir, nil)
			must(t, err)
			t.Cleanup(func() { local.Close() })
			o.Workspace = noLinks{local}
			s, err := agent.New(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			offered := strings.Contains(s.Agent().Config().Instructions, "greet2")
			var ours []string
			for _, om := range s.Omitted() {
				if om.Source == skills.Name {
					ours = append(ours, om.Reason)
				}
			}
			switch {
			case tc.refused == "" && (!offered || len(ours) > 0):
				t.Errorf("offered %v, omitted %v", offered, ours)
			case tc.refused != "" && (offered || len(ours) != 1 || !strings.Contains(ours[0], tc.refused)):
				t.Errorf("offered %v, omitted %v, want refused for %q", offered, ours, tc.refused)
			}
		})
	}
}

// recording is scripted, keeping every request's input as text.
type recording struct {
	scripted
	seen strings.Builder
}

func (m *recording) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	b, _ := json.Marshal(req.Input)
	m.seen.Write(b)
	m.seen.WriteString(req.Instructions)
	return m.scripted.CreateStream(ctx, req, sink)
}

// End to end, through the skill tool the policy allows unasked: a
// repository's skill file that is a link to .env never brings .env to
// the model, and a link that stays in the skills directory is read.
func TestTheSkillToolNeverReadsALinkOutOfTheSkillsDirectory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string // the link's target, from .dax/skills/ok
		want   string // in what the model saw; "" none
	}{
		{"a link to .env", filepath.Join("..", "..", "..", ".env"), ""},
		{"a link inside the skills directory", filepath.Join("..", "shared", "notes.md"), "shared-notes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			model := &recording{scripted: scripted{calls: [][2]string{{"skill", `{"name":"ok","path":"notes.md"}`}}}}
			o := session(t, model, skills.Options{})
			write(t, filepath.Join(o.Dir, ".env"), "SECRET_TOKEN=leaked\n")
			write(t, filepath.Join(o.Dir, ".dax", "skills", "ok", "SKILL.md"), "---\nname: ok\ndescription: Fine.\n---\nbody\n")
			write(t, filepath.Join(o.Dir, ".dax", "skills", "shared", "notes.md"), "shared-notes\n")
			must(t, os.Symlink(tc.target, filepath.Join(o.Dir, ".dax", "skills", "ok", "notes.md")))
			s, err := agent.New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := prompt(ctx, s, "read the notes", func(c *openresponses.FunctionCall, reason string) bool {
				t.Errorf("asked about %s: %s", c.Name, reason)
				return false
			}); err != nil {
				t.Fatal(err)
			}
			seen := model.seen.String()
			if strings.Contains(seen, "leaked") {
				t.Errorf("the model saw .env:\n%s", seen)
			}
			if tc.want != "" && !strings.Contains(seen, tc.want) {
				t.Errorf("the model did not see %q:\n%s", tc.want, seen)
			}
		})
	}
}
