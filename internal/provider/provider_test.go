package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/providers/anthropic"
	"github.com/ChristopherDavenport/openresponses/providers/gemini"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestSelection(t *testing.T) {
	keys := map[string]string{"OPENAI_API_KEY": "sk-secret-1", "ANTHROPIC_API_KEY": "sk-secret-2", "GEMINI_API_KEY": "secret-3", "OPENROUTER_API_KEY": "sk-or-4", "LITELLM_MASTER": "secret-5"}
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
		{Spec{Provider: "openrouter"}, "client", "anthropic/claude-sonnet-5.5", OpenRouterURL, "OPENROUTER_API_KEY"},
		{Spec{Provider: "openrouter", Model: "openai/gpt-5"}, "client", "openai/gpt-5", OpenRouterURL, "OPENROUTER_API_KEY"},
		{Spec{Provider: "openresponses", Model: "m", BaseURL: "http://vllm:8000/v1"}, "client", "m", "http://vllm:8000/v1", ""},
		{Spec{Provider: "openresponses", Model: "m", BaseURL: "https://llm.example/v1", KeyEnv: "LITELLM_MASTER"}, "client", "m", "https://llm.example/v1", "LITELLM_MASTER"},
		{Spec{Provider: "anthropic"}, "anthropic", "claude-sonnet-5-5", "", "ANTHROPIC_API_KEY"},
		{Spec{Provider: "anthropic", Model: "claude-x"}, "anthropic", "claude-x", "", "ANTHROPIC_API_KEY"},
		{Spec{Provider: "gemini"}, "gemini", "gemini-2.5-pro", "", "GEMINI_API_KEY"},
	}
	for _, tc := range tests {
		t.Run(tc.spec.Provider+"/"+tc.spec.Model, func(t *testing.T) {
			tc.spec.Getenv = env(keys)
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
