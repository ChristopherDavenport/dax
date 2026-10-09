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
		// The second review's list.
		"GITHUB_PAT=pat1", "SSH_AUTH_SOCK=/tmp/agent.1", "PGPASSWORD=pg1", "MYSQL_PWD=my1", "DATABASE_URL=postgres://db1",
		"STRIPE_KEY=sk_live_1", "SECRET_KEY=sec1", "CI_JOB_JWT=jwt1", "PASSWORD=pw1", "TOKEN=tk1", "API_KEY=ak1",
		"MY_CREDENTIALS=cr1", "SERVICE_AUTH=au1", "DB_PASSWORD_PROD=p2", "APP_SECRET_KEY_BASE=s2",
		"CACHE_URL=redis://user:hunter2@host:6379/0", "PLAIN_URL=https://example.com/x", "REGISTRY=https://user@host/x",
		// Paths to credential files pass through.
		"GOOGLE_APPLICATION_CREDENTIALS=/home/u/gcp.json", "KUBECONFIG=/home/u/.kube/config", "DOCKER_CONFIG=/home/u/.docker",
		"NETRC=/home/u/.netrc", "AWS_SHARED_CREDENTIALS_FILE=/home/u/.aws/credentials",
	}
	got := strings.Join(ChildEnv(base, nil), " ")
	for _, want := range []string{"PATH=/bin", "HOME=/h", "LANG=C", "KEYBOARD=us", "TOKENIZERS_PARALLELISM=false", "PATH_TOKEN_FILE=/x",
		"PLAIN_URL=https://example.com/x", "REGISTRY=https://user@host/x",
		"GOOGLE_APPLICATION_CREDENTIALS=", "KUBECONFIG=", "DOCKER_CONFIG=", "NETRC=", "AWS_SHARED_CREDENTIALS_FILE="} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %s: %s", want, got)
		}
	}
	for _, bad := range []string{"sk-1", "sk-2", "GEMINI", "GOOGLE_API", "GITHUB_TOKEN", "FOO_TOKEN", "MY_SECRET", "DB_PASSWORD", "AWS_ACCESS", "AWS_SECRET", "lower_api_key",
		"pat1", "agent.1", "pg1", "my1", "db1", "sk_live_1", "sec1", "jwt1", "pw1", "tk1", "ak1", "cr1", "au1", "p2", "s2", "hunter2"} {
		if strings.Contains(got, bad) {
			t.Errorf("kept %s: %s", bad, got)
		}
	}
	// A key variable the user named, whatever it is called, is removed
	// too; pass_env still wins.
	named := strings.Join(ChildEnv(append(base, "LITELLM_MASTER=lm1", "OPENROUTER_API_KEY=or1"), nil, "LITELLM_MASTER"), " ")
	if strings.Contains(named, "lm1") || strings.Contains(named, "or1") || !strings.Contains(named, "PATH=/bin") {
		t.Errorf("named key: %s", named)
	}
	if named := strings.Join(ChildEnv([]string{"LITELLM_MASTER=lm1"}, []string{"LITELLM_MASTER"}, "LITELLM_MASTER"), " "); named != "LITELLM_MASTER=lm1" {
		t.Errorf("pass_env over a named key: %q", named)
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
	out, err := call(context.Background(), Bash(newWS(t, t.TempDir())), `{"command":"echo \"[$OPENAI_API_KEY|$ANTHROPIC_API_KEY|$MY_SERVICE_TOKEN|$HARMLESS]\"; env | grep -c leak"}`)
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
	out, _ = call(context.Background(), Bash(newWSEnv(t, t.TempDir(), DefaultEnv([]string{"MY_SERVICE_TOKEN"}))), `{"command":"echo $MY_SERVICE_TOKEN $OPENAI_API_KEY"}`)
	if !strings.Contains(out, "tok-leak-3") || strings.Contains(out, "sk-leak") {
		t.Errorf("pass_env: %q", out)
	}
}
