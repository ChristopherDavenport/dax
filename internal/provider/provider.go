// Package provider turns the provider setting into a model: the
// openresponses.Streamer the agent loop calls. Ollama and any
// OpenAI-compatible server go through the openresponses client;
// Anthropic and Gemini through their adapter modules. Keys come from
// the environment and from nowhere else, and no error or log here
// carries one.
package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"google.golang.org/genai"

	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/providers/anthropic"
	"github.com/ChristopherDavenport/openresponses/providers/gemini"
)

// Defaults for each provider, used when neither a file nor a flag says
// otherwise.
const (
	OllamaURL = "http://localhost:11434/v1"
	OpenAIURL = "https://api.openai.com/v1"
)

// DefaultModel is the model each provider runs when none is named.
func DefaultModel(provider string) string {
	switch provider {
	case "openai":
		return "gpt-5"
	case "anthropic":
		return "claude-sonnet-5-5"
	case "gemini":
		return "gemini-2.5-pro"
	}
	return "qwen3.5:9b"
}

// KeyEnv is the environment variable that holds the provider's API
// key, empty for a provider that takes none.
func KeyEnv(provider string) string {
	switch provider {
	case "openai":
		return "OPENAI_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "gemini":
		return "GEMINI_API_KEY"
	}
	return ""
}

// ErrNoKey is the error for a provider whose key variable is empty.
var ErrNoKey = errors.New("no API key")

// Spec says which model to build.
type Spec struct {
	Provider string
	// Model is the model name; empty takes DefaultModel.
	Model string
	// BaseURL is for ollama and openai; empty takes the default.
	BaseURL string
	// Getenv reads the environment; nil is os.Getenv.
	Getenv func(string) string
}

// Model is a provider's streamer and the model name it will be asked
// for.
type Model struct {
	Streamer openresponses.Streamer
	Name     string
	// Endpoint says where requests go, for the banner; it holds no key.
	Endpoint string
}

// New builds the model for spec. No request is made.
func New(ctx context.Context, spec Spec) (Model, error) {
	getenv := spec.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	m := Model{Name: spec.Model}
	if m.Name == "" {
		m.Name = DefaultModel(spec.Provider)
	}
	var key string
	if env := KeyEnv(spec.Provider); env != "" {
		if key = strings.TrimSpace(getenv(env)); key == "" {
			return m, fmt.Errorf("%s: %w: set %s in the environment (dex does not read keys from its config files)", spec.Provider, ErrNoKey, env)
		}
	}
	switch spec.Provider {
	case "ollama", "openai":
		base := spec.BaseURL
		if base == "" {
			base = OllamaURL
			if spec.Provider == "openai" {
				base = OpenAIURL
			}
		}
		var opts []openresponses.ClientOption
		if key != "" {
			opts = append(opts, openresponses.WithAPIKey(key))
		}
		m.Streamer = openresponses.NewClient(base, opts...).AsAdapter()
		m.Endpoint = base
	case "anthropic":
		if spec.BaseURL != "" {
			return m, errors.New("anthropic: base_url is not supported")
		}
		client := sdk.NewClient(option.WithAPIKey(key))
		m.Streamer = anthropic.New(client.Messages)
		m.Endpoint = "api.anthropic.com"
	case "gemini":
		if spec.BaseURL != "" {
			return m, errors.New("gemini: base_url is not supported")
		}
		client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI})
		if err != nil {
			return m, fmt.Errorf("gemini: %w", err)
		}
		m.Streamer = gemini.New(client)
		m.Endpoint = "generativelanguage.googleapis.com"
	default:
		return m, fmt.Errorf("unknown provider %q", spec.Provider)
	}
	return m, nil
}
