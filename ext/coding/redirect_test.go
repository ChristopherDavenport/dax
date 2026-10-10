package coding

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
)

// #47: a redirect's target is decided as the path it writes, the way a
// file tool's path is: normalised from the directory the line is in at
// that point (after a cd), and through the links on its way. A deny of
// write(.env) reaches `echo x > notes` when notes is a link to .env,
// whatever the fallback; before, the target was its raw text, so the
// deny was an ask, and an allow fallback wrote .env.
func TestARedirectIsDecidedAsTheFileItWrites(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{".env": "SECRET=1\n", "README.md": "# hi\n", "sub/a.txt": "a\n"} {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	for link, target := range map[string]string{"notes": ".env", "sub/notes": "../.env", "docs": "sub", "readme": "README.md"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, fallback := range []string{"ask", "allow"} {
		s := defaults
		s.Fallback = fallback
		s.User = Rules{Deny: []string{"write(.env)"}}
		for _, tc := range []struct {
			cmd  string
			want agentturn.ToolAction
		}{
			{"echo x > notes", agentturn.Block},
			{"echo x >> notes", agentturn.Block},
			{"echo x >| notes", agentturn.Block},
			{"echo x &> notes", agentturn.Block},
			{"echo x 2> notes", agentturn.Block},
			{"echo x > ./notes", agentturn.Block},
			{"echo x > sub/../notes", agentturn.Block},
			{"echo x > " + filepath.Join(dir, "notes"), agentturn.Block},
			{"echo x > sub/notes", agentturn.Block},
			{"echo x > docs/notes", agentturn.Block},
			{"cd sub && echo x > ../notes", agentturn.Block},
			{"cd sub && echo x > notes", agentturn.Block},
			{"cd sub && echo x > ../.env", agentturn.Block},
			{"cd docs && echo x >> ../notes", agentturn.Block},
			{"echo x > .env", agentturn.Block},
			// What leads elsewhere is decided as that, by the fallback.
			{"echo x > readme", fallbackAction(fallback)},
			{"echo x > sub/new.txt", fallbackAction(fallback)},
			{"cd sub && echo x > a.txt", fallbackAction(fallback)},
			{"echo x > /dev/null; true", fallbackAction(fallback)},
		} {
			if got, reason := decideIn(t, dir, s, "bash", bash(tc.cmd)); got != tc.want {
				t.Errorf("fallback %s: %q = %v (%s), want %v", fallback, tc.cmd, got, reason, tc.want)
			}
		}
	}
}

func fallbackAction(f string) agentturn.ToolAction {
	if f == "allow" {
		return agentturn.Allow
	}
	return agentturn.Defer
}
