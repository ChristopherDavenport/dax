package prompt

import (
	"strings"
	"testing"
)

func TestBuild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		who   string
		extra []string
		want  []string
		not   []string
	}{
		{
			name: "the role line alone",
			want: []string{"You are dax, a coding agent", "Keep replies short", "Current working directory: /p"},
		},
		{
			name:  "the user's instructions",
			extra: []string{"  Use tabs.\n"},
			want:  []string{"Use tabs.\n\nCurrent working directory: /p"},
		},
		{
			name:  "the extensions' instructions in order, then the user's, empties left out",
			extra: []string{"Prefer glob.", "", "  ", "Use deploy to ship.", "Use tabs."},
			want:  []string{"found.\n\nPrefer glob.\n\nUse deploy to ship.\n\nUse tabs.\n\nCurrent working directory: /p"},
		},
		{
			name: "a program built on dax names itself",
			who:  "acme",
			want: []string{"You are acme, a coding agent"},
			not:  []string{"You are dax"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Build(tc.who, "/p", tc.extra...)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("prompt lacks %q:\n%s", w, got)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("prompt carries %q:\n%s", n, got)
				}
			}
			if !strings.HasSuffix(got, "Current working directory: /p") {
				t.Errorf("prompt does not end with the working directory:\n%s", got)
			}
			if strings.Contains(got, "\n\n\n") {
				t.Errorf("an empty part left a gap:\n%q", got)
			}
		})
	}
}
