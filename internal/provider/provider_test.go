package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/providers/anthropic"
	"github.com/ChristopherDavenport/openresponses/providers/gemini"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// fakeCredentials stands in for Application Default Credentials, so
// no test reads the machine's gcloud login.
func fakeCredentials(context.Context) (*google.Credentials, error) {
	return &google.Credentials{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t"})}, nil
}

func TestSelection(t *testing.T) {
	keys := map[string]string{"OPENAI_API_KEY": "sk-secret-1", "ANTHROPIC_API_KEY": "sk-secret-2", "GEMINI_API_KEY": "secret-3", "OPENROUTER_API_KEY": "sk-or-4", "LITELLM_MASTER": "secret-5"}
	// The vertex project and region are settings, not keys, so the
	// endpoint may name them.
	vertexVars := map[string]string{ProjectEnv: "my-project", LocationEnv: "global"}
	tests := []struct {
		spec       Spec
		wantType   string
		wantModel  string
		wantURL    string
		wantKeyEnv string
	}{
		{Spec{Provider: "ollama"}, "client", "qwen3.5:9b", OllamaURL, ""},
		{Spec{Provider: "ollama", Model: "qwen3:1.7b", BaseURL: "http://gpu:11434/v1"}, "client", "qwen3:1.7b", "http://gpu:11434/v1", ""},
		{Spec{Provider: "openai"}, "client", "gpt-5", OpenAIURL, "OPENAI_API_KEY"},
		{Spec{Provider: "openrouter"}, "client", "deepseek/deepseek-v4-pro-0813", OpenRouterURL, "OPENROUTER_API_KEY"},
		{Spec{Provider: "openrouter", Model: "openai/gpt-5"}, "client", "openai/gpt-5", OpenRouterURL, "OPENROUTER_API_KEY"},
		{Spec{Provider: "openresponses", Model: "m", BaseURL: "http://vllm:8000/v1"}, "client", "m", "http://vllm:8000/v1", ""},
		{Spec{Provider: "openresponses", Model: "m", BaseURL: "https://llm.example/v1", KeyEnv: "LITELLM_MASTER"}, "client", "m", "https://llm.example/v1", "LITELLM_MASTER"},
		{Spec{Provider: "anthropic"}, "anthropic", "claude-sonnet-5-5", "", "ANTHROPIC_API_KEY"},
		{Spec{Provider: "anthropic", Model: "claude-x"}, "anthropic", "claude-x", "", "ANTHROPIC_API_KEY"},
		{Spec{Provider: "gemini"}, "gemini", "gemini-2.5-pro", "", "GEMINI_API_KEY"},
		{Spec{Provider: "vertex"}, "vertex", "claude-sonnet-5-5", "vertex ai my-project/global", ""},
		{Spec{Provider: "vertex", Model: "claude-opus-5-5"}, "vertex", "claude-opus-5-5", "vertex ai my-project/global", ""},
		{Spec{Provider: "vertex", Model: "gemini-3.5-flash"}, "vertex", "gemini-3.5-flash", "vertex ai my-project/global", ""},
	}
	for _, tc := range tests {
		t.Run(tc.spec.Provider+"/"+tc.spec.Model, func(t *testing.T) {
			tc.spec.Getenv = func(k string) string { return keys[k] + vertexVars[k] }
			tc.spec.Credentials = fakeCredentials
			m, err := New(context.Background(), tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			var got string
			switch m.Streamer.(type) {
			case *openresponses.ClientAdapter:
				got = "client"
			case *anthropic.Adapter:
				got = "anthropic"
			case *gemini.Adapter:
				got = "gemini"
			case vertexModels:
				got = "vertex"
			}
			if got != tc.wantType {
				t.Errorf("streamer is %T, want %s", m.Streamer, tc.wantType)
			}
			if m.Name != tc.wantModel {
				t.Errorf("model = %q, want %q", m.Name, tc.wantModel)
			}
			if tc.wantURL != "" && m.Endpoint != tc.wantURL {
				t.Errorf("endpoint = %q, want %q", m.Endpoint, tc.wantURL)
			}
			if m.KeyEnv != tc.wantKeyEnv {
				t.Errorf("key variable = %q, want %q", m.KeyEnv, tc.wantKeyEnv)
			}
			// Every vendor that publishes model metadata is asked;
			// OpenAI and a generic server publish none.
			if wantDescriber := tc.spec.Provider != "openai" && tc.spec.Provider != "openresponses"; (m.Describer != nil) != wantDescriber {
				t.Errorf("describer = %T, want one: %v", m.Describer, wantDescriber)
			}
			for _, k := range keys {
				if strings.Contains(m.Endpoint, k) {
					t.Errorf("endpoint carries a key: %q", m.Endpoint)
				}
			}
		})
	}
}

func TestAMissingKeyIsNamedAndNeverGuessed(t *testing.T) {
	for provider, variable := range map[string]string{"openai": "OPENAI_API_KEY", "openrouter": "OPENROUTER_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY"} {
		for name, getenv := range map[string]func(string) string{
			"unset":      env(nil),
			"empty":      env(map[string]string{variable: ""}),
			"whitespace": env(map[string]string{variable: "  \n"}),
			// Another provider's key is not this one's.
			"other key": env(map[string]string{"OPENAI_API_KEY": "x", "OPENROUTER_API_KEY": "x", "ANTHROPIC_API_KEY": "x", "GEMINI_API_KEY": "x", variable: ""}),
		} {
			_, err := New(context.Background(), Spec{Provider: provider, Getenv: getenv})
			if !errors.Is(err, ErrNoKey) || !strings.Contains(err.Error(), variable) || !strings.HasPrefix(err.Error(), provider+":") {
				t.Errorf("%s/%s: err = %v, want ErrNoKey naming %s", provider, name, err, variable)
			}
		}
	}
	// Ollama needs no key, nor does an openresponses server unless the
	// user names its variable, and then it must be set.
	if _, err := New(context.Background(), Spec{Provider: "ollama", Getenv: env(nil)}); err != nil {
		t.Errorf("ollama without a key: %v", err)
	}
	open := Spec{Provider: "openresponses", Model: "m", BaseURL: "http://x/v1", Getenv: env(map[string]string{"OPENAI_API_KEY": "x"})}
	if _, err := New(context.Background(), open); err != nil {
		t.Errorf("openresponses without a key: %v", err)
	}
	open.KeyEnv = "MY_SERVER_KEY"
	if _, err := New(context.Background(), open); !errors.Is(err, ErrNoKey) || !strings.Contains(err.Error(), "MY_SERVER_KEY") {
		t.Errorf("openresponses with an unset key variable: err = %v", err)
	}
}

func TestSettingsErrorsComeBeforeTheKey(t *testing.T) {
	for _, tc := range []struct {
		spec Spec
		want string
	}{
		{Spec{Provider: "openai", BaseURL: "https://openrouter.ai/api/v1"}, "openai: base_url is not supported; use the openresponses provider"},
		{Spec{Provider: "openrouter", BaseURL: "https://x"}, "openrouter: base_url is not supported"},
		{Spec{Provider: "openai", KeyEnv: "OTHER"}, "openai: api_key_env is for the openresponses provider"},
		{Spec{Provider: "ollama", KeyEnv: "OTHER"}, "ollama: api_key_env"},
		{Spec{Provider: "openresponses", Model: "m"}, "openresponses: base_url is required"},
		{Spec{Provider: "openresponses", BaseURL: "http://x/v1"}, "openresponses: model is required"},
	} {
		tc.spec.Getenv = env(nil)
		_, err := New(context.Background(), tc.spec)
		if err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, ErrNoKey) {
			t.Errorf("%+v: err = %v, want %q", tc.spec, err, tc.want)
		}
	}
}

func TestVertexReadsGoogleCloudSettings(t *testing.T) {
	withProject := func(project string) func(context.Context) (*google.Credentials, error) {
		return func(ctx context.Context) (*google.Credentials, error) {
			c, _ := fakeCredentials(ctx)
			c.ProjectID = project
			return c, nil
		}
	}
	for _, tc := range []struct {
		name  string
		vars  map[string]string
		creds string
		want  string
	}{
		{"both set", map[string]string{ProjectEnv: "p", LocationEnv: "us-east5"}, "adc", "vertex ai p/us-east5"},
		{"region for location", map[string]string{ProjectEnv: "p", RegionEnv: "europe-west1"}, "", "vertex ai p/europe-west1"},
		{"location over region", map[string]string{ProjectEnv: "p", LocationEnv: "global", RegionEnv: "us-east5"}, "", "vertex ai p/global"},
		{"project from credentials", map[string]string{LocationEnv: "global"}, "adc", "vertex ai adc/global"},
		// Claude Code's own variables are not read.
		{"not anthropic's", map[string]string{ProjectEnv: "p", LocationEnv: "global", "ANTHROPIC_VERTEX_PROJECT_ID": "other", "CLOUD_ML_REGION": "us"}, "", "vertex ai p/global"},
	} {
		m, err := New(context.Background(), Spec{Provider: "vertex", Getenv: env(tc.vars), Credentials: withProject(tc.creds)})
		if err != nil || m.Endpoint != tc.want || m.KeyEnv != "" {
			t.Errorf("%s: endpoint %q, key variable %q, err %v; want %q", tc.name, m.Endpoint, m.KeyEnv, err, tc.want)
		}
	}
	if _, err := New(context.Background(), Spec{Provider: "vertex", Getenv: env(map[string]string{LocationEnv: "global"}), Credentials: withProject("")}); err == nil || !strings.Contains(err.Error(), "set "+ProjectEnv) {
		t.Errorf("no project anywhere: err = %v", err)
	}
}

func TestVertexSettingsErrorsComeBeforeCredentials(t *testing.T) {
	asked := false
	spy := func(ctx context.Context) (*google.Credentials, error) { asked = true; return fakeCredentials(ctx) }
	full := map[string]string{ProjectEnv: "p", LocationEnv: "global"}
	for _, spec := range []Spec{
		{Provider: "vertex", Getenv: env(map[string]string{ProjectEnv: "p", LocationEnv: " "})},
		{Provider: "vertex", Getenv: env(full), BaseURL: "https://x"},
		{Provider: "vertex", Getenv: env(full), KeyEnv: "K"},
	} {
		spec.Credentials = spy
		if _, err := New(context.Background(), spec); err == nil || asked {
			t.Errorf("%+v: err = %v, credentials looked for: %v", spec, err, asked)
		}
	}
	if _, err := New(context.Background(), Spec{Provider: "vertex", Getenv: env(nil), Credentials: spy}); err == nil || !strings.Contains(err.Error(), LocationEnv) {
		t.Errorf("no location: err = %v", err)
	}
	none := func(context.Context) (*google.Credentials, error) {
		return nil, errors.New("could not find default credentials")
	}
	_, err := New(context.Background(), Spec{Provider: "vertex", Getenv: env(full), Credentials: none})
	if err == nil || !strings.Contains(err.Error(), "gcloud auth application-default login") || !strings.Contains(err.Error(), "could not find") {
		t.Errorf("without credentials: err = %v", err)
	}
}

func TestErrorsNeverCarryTheKey(t *testing.T) {
	const secret = "sk-very-secret-value"
	getenv := env(map[string]string{"ANTHROPIC_API_KEY": secret, "OPENAI_API_KEY": secret, "GEMINI_API_KEY": secret, "OPENROUTER_API_KEY": secret})
	for _, spec := range []Spec{
		{Provider: "anthropic", BaseURL: "https://x"},
		{Provider: "gemini", BaseURL: "https://x"},
		{Provider: "openrouter", BaseURL: "https://x"},
		{Provider: "openresponses", Model: "m", BaseURL: "http://x/v1", KeyEnv: "UNSET_KEY"},
		{Provider: "bogus"},
	} {
		spec.Getenv = getenv
		_, err := New(context.Background(), spec)
		if err == nil {
			t.Errorf("%+v: want an error", spec)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("error carries the key: %v", err)
		}
	}
}

func TestProviderTable(t *testing.T) {
	if KeyEnv("ollama") != "" || KeyEnv("openai") != "OPENAI_API_KEY" || KeyEnv("openrouter") != "OPENROUTER_API_KEY" || KeyEnv("openresponses") != "" || KeyEnv("anthropic") != "ANTHROPIC_API_KEY" || KeyEnv("gemini") != "GEMINI_API_KEY" {
		t.Error("key variables")
	}
}

func TestSubagentModel(t *testing.T) {
	getenv := env(map[string]string{"OPENROUTER_API_KEY": "k", "ANTHROPIC_API_KEY": "k", ProjectEnv: "p", LocationEnv: "global"})
	for _, tc := range []struct {
		spec Spec
		want string
	}{
		{Spec{Provider: "openrouter"}, "deepseek/deepseek-v4.1-flash"},
		// The provider's default for sub-agents holds whatever the main
		// model is.
		{Spec{Provider: "openrouter", Model: "openai/gpt-5"}, "deepseek/deepseek-v4.1-flash"},
		{Spec{Provider: "openrouter", SubagentModel: "x/y"}, "x/y"},
		// A provider with no default for them runs the main model.
		{Spec{Provider: "anthropic"}, "claude-sonnet-5-5"},
		{Spec{Provider: "vertex", Model: "claude-opus-5-5", SubagentModel: "claude-sonnet-5-5"}, "claude-sonnet-5-5"},
		{Spec{Provider: "ollama", Model: "qwen3:1.7b"}, "qwen3:1.7b"},
		{Spec{Provider: "ollama", Model: "qwen3:1.7b", SubagentModel: "qwen3:0.6b"}, "qwen3:0.6b"},
	} {
		tc.spec.Getenv, tc.spec.Credentials = getenv, fakeCredentials
		m, err := New(context.Background(), tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		if m.SubagentName != tc.want {
			t.Errorf("%+v: subagent model %q, want %q", tc.spec, m.SubagentName, tc.want)
		}
	}
}
