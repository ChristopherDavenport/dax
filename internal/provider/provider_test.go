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
	keys := map[string]string{"OPENAI_API_KEY": "sk-secret-1", "ANTHROPIC_API_KEY": "sk-secret-2", "GEMINI_API_KEY": "secret-3"}
	tests := []struct {
		spec      Spec
		wantType  string
		wantModel string
		wantURL   string
	}{
		{Spec{Provider: "ollama"}, "client", "qwen3.5:9b", OllamaURL},
		{Spec{Provider: "ollama", Model: "qwen3:1.7b", BaseURL: "http://gpu:11434/v1"}, "client", "qwen3:1.7b", "http://gpu:11434/v1"},
		{Spec{Provider: "openai"}, "client", "gpt-5", OpenAIURL},
		{Spec{Provider: "openai", Model: "m", BaseURL: "https://openrouter.ai/api/v1"}, "client", "m", "https://openrouter.ai/api/v1"},
		{Spec{Provider: "anthropic"}, "anthropic", "claude-sonnet-5-5", ""},
		{Spec{Provider: "anthropic", Model: "claude-x"}, "anthropic", "claude-x", ""},
		{Spec{Provider: "gemini"}, "gemini", "gemini-2.5-pro", ""},
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
			for _, k := range keys {
				if strings.Contains(m.Endpoint, k) {
					t.Errorf("endpoint carries a key: %q", m.Endpoint)
				}
			}
		})
	}
}

func TestAMissingKeyIsNamedAndNeverGuessed(t *testing.T) {
	for provider, variable := range map[string]string{"openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY"} {
		for name, getenv := range map[string]func(string) string{
			"unset":      env(nil),
			"empty":      env(map[string]string{variable: ""}),
			"whitespace": env(map[string]string{variable: "  \n"}),
			// Another provider's key is not this one's.
			"other key": env(map[string]string{"OPENAI_API_KEY": "x", "ANTHROPIC_API_KEY": "x", "GEMINI_API_KEY": "x", variable: ""}),
		} {
			_, err := New(context.Background(), Spec{Provider: provider, Getenv: getenv})
			if !errors.Is(err, ErrNoKey) || !strings.Contains(err.Error(), variable) || !strings.HasPrefix(err.Error(), provider+":") {
				t.Errorf("%s/%s: err = %v, want ErrNoKey naming %s", provider, name, err, variable)
			}
		}
	}
	// Ollama needs no key.
	if _, err := New(context.Background(), Spec{Provider: "ollama", Getenv: env(nil)}); err != nil {
		t.Errorf("ollama without a key: %v", err)
	}
}

func TestErrorsNeverCarryTheKey(t *testing.T) {
	const secret = "sk-very-secret-value"
	getenv := env(map[string]string{"ANTHROPIC_API_KEY": secret, "OPENAI_API_KEY": secret, "GEMINI_API_KEY": secret})
	for _, spec := range []Spec{
		{Provider: "anthropic", BaseURL: "https://x"},
		{Provider: "gemini", BaseURL: "https://x"},
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
	if KeyEnv("ollama") != "" || KeyEnv("openai") != "OPENAI_API_KEY" || KeyEnv("anthropic") != "ANTHROPIC_API_KEY" || KeyEnv("gemini") != "GEMINI_API_KEY" {
		t.Error("key variables")
	}
}
