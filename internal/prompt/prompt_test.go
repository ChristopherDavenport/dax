package prompt

import (
	"strings"
	"testing"
)

func TestBuildIncludesToolUseGuidance(t *testing.T) {
	got := Build("/code/project", "")

	for _, want := range []string{
		"do not guess at file contents.",
		"Prefer glob, grep and ls over shell commands",
		"Never run a broad, unbounded shell search such as find /",
		"Before edit, confirm the target string appears exactly once",
		"After changing Go files, run go build ./..., then go vet, then the relevant tests",
		"Prefer explore for broad read-only investigation",
		"Read before you edit.",
		"Keep replies short and state what you changed.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt does not contain %q:\n%s", want, got)
		}
	}
}

func TestBuildKeepsDirAndExtra(t *testing.T) {
	got := Build("/code/project", "use tabs")
	if !strings.Contains(got, "use tabs") {
		t.Errorf("prompt is missing extra instructions:\n%s", got)
	}
	if !strings.Contains(got, "Current working directory: /code/project") {
		t.Errorf("prompt is missing the working directory:\n%s", got)
	}
}