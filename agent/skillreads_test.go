package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/policy"
)

// #39: a project's .dax/skills that is a link elsewhere in the
// workspace offered that directory to the skill tool, which the policy
// allows unasked, so .dax/skills -> .. read the workspace's secrets
// without read(.env)'s question. A read of a file of a project's skill,
// its instructions included, is now held to read's asks and denies, on
// the file's name in the workspace and where its links lead, while
// read's allows never open it. A skill the user installed is the
// user's, and is not held. Each case runs on this machine and on an
// executor, whose files the claim reads through it.
func TestAProjectsSkillFilesAreHeldToReadsRules(t *testing.T) {
	const leaked = "SECRET_TOKEN=leaked"
	linkedUp := func(t *testing.T, dir string) {
		// .dax/skills -> ..: every directory of the workspace is a
		// skill, and a link to the root's .env stays inside it.
		write(t, filepath.Join(dir, ".env"), leaked+"\n")
		write(t, filepath.Join(dir, "app", "SKILL.md"), skillMD("app", "The app."))
		write(t, filepath.Join(dir, "app", ".env"), leaked+"\n")
		must(t, os.Symlink(filepath.Join("..", ".env"), filepath.Join(dir, "app", "notes.md")))
		must(t, os.MkdirAll(filepath.Join(dir, ".dax"), 0o755))
		must(t, os.Symlink("..", filepath.Join(dir, ".dax", "skills")))
	}
	linkedConfig := func(t *testing.T, dir string) {
		write(t, filepath.Join(dir, "config", "x", "SKILL.md"), skillMD("x", "Config."))
		write(t, filepath.Join(dir, "config", "x", ".env"), leaked+"\n")
		must(t, os.MkdirAll(filepath.Join(dir, ".dax"), 0o755))
		must(t, os.Symlink(filepath.Join("..", "config"), filepath.Join(dir, ".dax", "skills")))
	}
	linkedClaude := func(t *testing.T, dir string) {
		write(t, filepath.Join(dir, ".claude", "skills", "x", "SKILL.md"), skillMD("x", "Shared."))
		write(t, filepath.Join(dir, ".claude", "skills", "x", "ref.md"), "shared-ref\n")
		must(t, os.MkdirAll(filepath.Join(dir, ".dax"), 0o755))
		must(t, os.Symlink(filepath.Join("..", ".claude", "skills"), filepath.Join(dir, ".dax", "skills")))
	}
	userSkill := func(t *testing.T, o Options) {
		write(t, filepath.Join(o.UserDir, "skills", "mine", "SKILL.md"), skillMD("mine", "The user's."))
		write(t, filepath.Join(o.UserDir, "skills", "mine", ".env"), "USER_TOKEN=mine\n")
	}
	for _, tc := range []struct {
		name    string
		project func(t *testing.T, dir string)
		args    string
		user    policy.Rules
		builtin bool // false drops the extensions' allows, dax-skills' allow of skill among them
		asked   bool
		denied  string // in the call's output when the policy refused it
		served  string // in the call's output when it ran
	}{
		{".. : the instructions", linkedUp, `{"name":"app"}`, policy.Rules{}, true, false, "", "app-body"},
		{".. : a .env of the skill", linkedUp, `{"name":"app","path":".env"}`, policy.Rules{}, true, true, "", ""},
		{".. : a link to the root's .env", linkedUp, `{"name":"app","path":"notes.md"}`, policy.Rules{}, true, true, "", ""},
		{"../config: a deny of read refuses the instructions", linkedConfig, `{"name":"x"}`,
			policy.Rules{Deny: []string{"read(config/**)"}}, true, false, "config/x/SKILL.md", ""},
		{"../config: an allow of read does not lift a deny", linkedConfig, `{"name":"x","path":".env"}`,
			policy.Rules{Allow: []string{"read(config/x/.env)"}, Deny: []string{"read(config/**)"}}, true, false, "config/x/.env", ""},
		{"../config: an allow of read does not allow the call", linkedConfig, `{"name":"x","path":".env"}`,
			policy.Rules{Allow: []string{"read(config/x/.env)"}}, false, true, "", ""},
		{"../.claude/skills: the instructions", linkedClaude, `{"name":"x"}`, policy.Rules{}, true, false, "", "x-body"},
		{"../.claude/skills: a file", linkedClaude, `{"name":"x","path":"ref.md"}`, policy.Rules{}, true, false, "", "shared-ref"},
		{"a user's skill is not held", nil, `{"name":"mine","path":".env"}`, policy.Rules{}, true, false, "", "USER_TOKEN=mine"},
	} {
		for _, where := range []string{"local", "executor"} {
			t.Run(where+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				model := &scripted{calls: [][2]string{{"skill", tc.args}}}
				var o Options
				if where == "local" {
					o = options(t, model)
					o.Policy = confirmPolicy(t)
					if tc.project != nil {
						tc.project(t, o.Dir)
					}
				} else {
					box := newRemoteBox(t)
					if tc.project != nil {
						tc.project(t, box.dir)
					}
					ex, _ := box.dial(t)
					o = remoteOptions(t, model, ex)
				}
				userSkill(t, o)
				o.Policy.User, o.Policy.Builtin = tc.user, tc.builtin
				s, err := New(ctx, o)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if om := omittedText(s); om != "" {
					t.Fatalf("omitted:\n%s", om)
				}
				asked := false
				if _, err := promptOn(ctx, s, "use the skill", func(c *openresponses.FunctionCall, reason string) bool {
					asked = asked || c.Name == "skill"
					return false
				}); err != nil {
					t.Fatal(err)
				}
				var out string
				for _, it := range s.Agent().State().Transcript {
					if fo, ok := it.(*openresponses.FunctionCallOutput); ok {
						out += fo.Output.String()
					}
				}
				switch {
				case asked != tc.asked:
					t.Errorf("asked = %v, want %v; output %q", asked, tc.asked, out)
				case tc.denied != "" && (!strings.Contains(out, "denied by") || !strings.Contains(out, tc.denied)):
					t.Errorf("output %q, want the call denied on %s", out, tc.denied)
				case tc.served != "" && !strings.Contains(out, tc.served):
					t.Errorf("output %q, want %q served", out, tc.served)
				}
				if strings.Contains(out, "leaked") {
					t.Errorf("the model was given a secret: %q", out)
				}
			})
		}
	}
}
