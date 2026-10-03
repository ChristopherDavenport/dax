package tool

import (
	"context"
	"strings"
	"testing"
)

func TestChildEnvRemovesCredentials(t *testing.T) {
	base := []string{
		"PATH=/bin", "HOME=/h", "LANG=C",
		"OPENAI_API_KEY=sk-1", "ANTHROPIC_API_KEY=sk-2", "GEMINI_API_KEY=g", "GOOGLE_API_KEY=g2",
		"GITHUB_TOKEN=t", "FOO_TOKEN=t", "MY_SECRET=s", "DB_PASSWORD=p", "AWS_SECRET_ACCESS_KEY=a", "AWS_ACCESS_KEY_ID=a",
		"lower_api_key=x", "KEYBOARD=us", "TOKENIZERS_PARALLELISM=false", "PATH_TOKEN_FILE=/x",
	}
	got := strings.Join(ChildEnv(base, nil), " ")
	for _, want := range []string{"PATH=/bin", "HOME=/h", "LANG=C", "KEYBOARD=us", "TOKENIZERS_PARALLELISM=false", "PATH_TOKEN_FILE=/x"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %s: %s", want, got)
		}
	}
	for _, bad := range []string{"sk-1", "sk-2", "GEMINI", "GOOGLE_API", "GITHUB_TOKEN", "FOO_TOKEN", "MY_SECRET", "DB_PASSWORD", "AWS_", "lower_api_key"} {
		if strings.Contains(got, bad) {
			t.Errorf("kept %s: %s", bad, got)
		}
	}
	passed := strings.Join(ChildEnv(base, []string{"GITHUB_TOKEN", "FOO_TOKEN"}), " ")
	if !strings.Contains(passed, "GITHUB_TOKEN=t") || !strings.Contains(passed, "FOO_TOKEN=t") || strings.Contains(passed, "sk-1") {
		t.Errorf("pass_env: %s", passed)
	}
}

// #9 of the review: a test or build script an approved command ran
// read OPENAI_API_KEY from the environment.
func TestBashDoesNotHandOverTheKeys(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-leak-1")
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak-2")
	t.Setenv("MY_SERVICE_TOKEN", "tok-leak-3")
	t.Setenv("HARMLESS", "visible")
	out, err := call(context.Background(), Bash(t.TempDir()), `{"command":"echo \"[$OPENAI_API_KEY|$ANTHROPIC_API_KEY|$MY_SERVICE_TOKEN|$HARMLESS]\"; env | grep -c leak"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[||||visible]") && !strings.Contains(out, "[|||visible]") {
		t.Errorf("output: %q", out)
	}
	if strings.Contains(out, "leak-") || !strings.Contains(out, "0\n") {
		t.Errorf("a key reached the command: %q", out)
	}
	// What the user names is passed.
	out, _ = call(context.Background(), Bash(t.TempDir(), WithEnv(DefaultEnv([]string{"MY_SERVICE_TOKEN"}))), `{"command":"echo $MY_SERVICE_TOKEN $OPENAI_API_KEY"}`)
	if !strings.Contains(out, "tok-leak-3") || strings.Contains(out, "sk-leak") {
		t.Errorf("pass_env: %q", out)
	}
}
