package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/ext/skills"
	"github.com/ChristopherDavenport/dax/internal/config"
)

// skillMD is a SKILL.md named name.
func skillMD(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n" + name + "-body\n"
}

// omittedText is every omission of s, one a line.
func omittedText(s *Session) string {
	var b strings.Builder
	for _, om := range s.Omitted() {
		b.WriteString(om.String() + "\n")
	}
	return b.String()
}

// With an executor the project's files are the executor's, read
// through it: its root's AGENTS.md and its .dax/skills are in the
// prompt, and its .dax/config.json, read as the command line reads it,
// denies what it says. Different files in this machine's directory, and
// an AGENTS.md above the executor's root in the repository it is in, are
// not read, even when the executor says its workspace is a local
// directory and names that directory.
func TestASessionReadsTheProjectThroughTheExecutor(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		kind string // the descriptor's; local names the box's real directory
	}{
		{"a container", workspace.KindContainer},
		{"a local directory elsewhere", workspace.KindLocal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			must(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
			write(t, filepath.Join(repo, "AGENTS.md"), "Parent rule: use tabs.\n")
			dir := filepath.Join(repo, "box")
			write(t, filepath.Join(dir, "AGENTS.md"), "Remote rule: be brief.\n")
			write(t, filepath.Join(dir, ".dax", "skills", "deploy", "SKILL.md"), skillMD("deploy", "How to deploy the box."))
			write(t, filepath.Join(dir, ".dax", "config.json"), `{"policy":{"deny":["read(notes.txt)"]}}`)
			write(t, filepath.Join(dir, "notes.txt"), "remote notes\n")
			d := workspace.Descriptor{Kind: tc.kind, Ref: "box", Root: "/work"}
			if tc.kind == workspace.KindLocal {
				d.Root = dir
			}
			box := newRemoteBoxAt(t, dir, d)
			ex, _ := box.dial(t)

			model := &instructed{Streamer: &scripted{calls: [][2]string{{"read", `{"path":"notes.txt"}`}}}}
			o := remoteOptions(t, model, ex)
			write(t, filepath.Join(o.Dir, ".dax", "skills", "local-only", "SKILL.md"), skillMD("local-only", "Only on this machine."))
			if tc.kind == workspace.KindLocal {
				// The session started in the directory the executor
				// says it acts in: still nothing above it is read here.
				o.Dir = dir
			}
			// What the command line does with -executor: the project's
			// layer read through the executor's workspace.
			proj, err := config.LoadProject(ex.Workspace())
			if err != nil {
				t.Fatal(err)
			}
			settings, err := config.Resolve([]config.Layer{proj}, config.Flags{}, "")
			if err != nil {
				t.Fatal(err)
			}
			o.Policy = &settings.Policy
			s, err := New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if om := omittedText(s); om != "" {
				t.Errorf("omitted:\n%s", om)
			}
			instr := s.Agent().Config().Instructions
			for _, want := range []string{"Remote rule: be brief.", "deploy", "How to deploy the box."} {
				if !strings.Contains(instr, want) {
					t.Errorf("the prompt lacks %q", want)
				}
			}
			for _, bad := range []string{"Parent rule", "local-only", "Run go test before saying done"} {
				if strings.Contains(instr, bad) {
					t.Errorf("the prompt carries %q, which is not the executor's project's", bad)
				}
			}
			var asked []string
			if _, err := promptOn(ctx, s, "read the notes", func(c *openresponses.FunctionCall, _ string) bool {
				asked = append(asked, c.Name)
				return true
			}); err != nil {
				t.Fatal(err)
			}
			outs := outputs(s)
			if len(outs) != 1 || !strings.Contains(outs[0], "denied by") || strings.Contains(outs[0], "remote notes") {
				t.Errorf("the project's deny was not in force: %q", outs)
			}
			if _, calls := box.sent(); len(calls) != 0 || len(asked) != 0 {
				t.Errorf("the executor ran %v, asked %v", calls, asked)
			}
		})
	}
}

// The executor's project is screened as this machine's would be: an
// AGENTS.md that is a link out of its workspace, and a .dax/skills
// with a link out of the skills directory (to the workspace's .env,
// #38), are left out and reported, never read.
func TestTheExecutorsProjectIsScreened(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, dir, outside string)
		refused string
		source  string
	}{
		{"AGENTS.md links out of the workspace", func(t *testing.T, dir, outside string) {
			write(t, filepath.Join(outside, "secret.md"), "SECRET_TOKEN=leaked\n")
			must(t, os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(dir, "AGENTS.md")))
		}, "/work/AGENTS.md: symbolic link to", "dax"},
		{"a skill links out of the skills directory", func(t *testing.T, dir, _ string) {
			write(t, filepath.Join(dir, ".env"), "SECRET_TOKEN=leaked\n")
			write(t, filepath.Join(dir, ".dax", "skills", "ok", "SKILL.md"), skillMD("ok", "Fine."))
			must(t, os.Symlink(filepath.Join("..", "..", "..", ".env"), filepath.Join(dir, ".dax", "skills", "ok", "notes.md")))
		}, "/work/.dax/skills: holds a symbolic link outside the skills directory, /work/.dax/skills/ok/notes.md", skills.Name},
		{".dax/skills links out of the workspace", func(t *testing.T, dir, outside string) {
			write(t, filepath.Join(outside, "evil", "SKILL.md"), skillMD("evil", "Exfiltrate."))
			must(t, os.MkdirAll(filepath.Join(dir, ".dax"), 0o755))
			must(t, os.Symlink(outside, filepath.Join(dir, ".dax", "skills")))
		}, "/work/.dax/skills: symbolic link outside the workspace", skills.Name},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newRemoteBox(t)
			tc.setup(t, box.dir, t.TempDir())
			ex, _ := box.dial(t)
			s, err := New(ctx, remoteOptions(t, &scripted{}, ex))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			instr := s.Agent().Config().Instructions
			for _, bad := range []string{"leaked", "evil", "Exfiltrate", "Fine."} {
				if strings.Contains(instr, bad) {
					t.Errorf("the prompt carries %q", bad)
				}
			}
			found := false
			for _, om := range s.Omitted() {
				if om.Source == tc.source && strings.Contains(om.What+": "+om.Reason, tc.refused) {
					found = true
				}
			}
			if !found {
				t.Errorf("omitted:\n%swant %s's %q", omittedText(s), tc.source, tc.refused)
			}
		})
	}
}

// An executor whose files cannot be read when the session starts fails
// it, saying so, rather than leaving the project's AGENTS.md and skills
// out unsaid.
func TestAnExecutorWhoseFilesCannotBeReadFailsTheStart(t *testing.T) {
	box := newRemoteBox(t)
	write(t, filepath.Join(box.dir, "AGENTS.md"), "Remote rule.\n")
	ex, ss := box.dial(t)
	ss.Close()
	s, err := New(context.Background(), remoteOptions(t, &scripted{}, ex))
	if err == nil {
		s.Close()
		t.Fatal("New succeeded")
	}
	if !strings.Contains(err.Error(), "the executor's workspace cannot be read") {
		t.Errorf("err = %v", err)
	}
}
