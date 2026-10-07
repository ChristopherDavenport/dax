package prompt

import (
	"strings"
	"testing"
)

func TestBuild(t *testing.T) {
	for _, tc := range []struct {
		name   string
		extra  string
		agents bool
		want   []string
		not    []string
	}{
		{
			name: "without sub-agents",
			want: []string{"You are dax", "Read before you edit.", "Current working directory: /p"},
			not:  []string{delegation, "explore:", "task:"},
		},
		{
			name:   "with sub-agents",
			agents: true,
			want:   []string{"You are dax", delegation},
		},
		{
			name:  "the user's instructions",
			extra: "  Use tabs.\n",
			want:  []string{"Use tabs.\n\nCurrent working directory: /p"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Build("/p", tc.extra, tc.agents)
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
		})
	}
}
