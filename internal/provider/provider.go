// Package provider turns the provider setting into a model: the
// openresponses.Streamer the agent loop calls. A provider is a vendor,
// not a protocol: Ollama, OpenAI, OpenRouter and any other server that
// speaks Open Responses (the openresponses provider) all go through the
// openresponses client, each with its own defaults; Anthropic and
// Gemini go through their adapter modules. Keys come from the
// environment and from nowhere else, and no error or log here carries
// one.
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

	"github.com/ChristopherDavenport/dax/internal/modelinfo"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/providers/anthropic"
	"github.com/ChristopherDavenport/openresponses/providers/gemini"
)

// Defaults for each provider, used when neither a file nor a flag says
// otherwise.
const (
	OllamaURL     = "http://localhost:11434/v1"
	OpenAIURL     = "https://api.openai.com/v1"
	OpenRouterURL = "https://openrouter.ai/api/v1"
)

// DefaultModel is the model each provider runs when none is named.
// The openresponses provider has none: the server decides what exists.
func DefaultModel(provider string) string {
	switch provider {
	case "openai":
		return "gpt-5"
	case "openrouter":
		return "deepseek/deepseek-v4-pro-0813"
	case "openresponses":
		return ""
	case "anthropic":
		return "claude-sonnet-5-5"
	case "gemini":
		return "gemini-2.5-pro"
	}
	return "qwen3.5:9b"
}

// DefaultSubagentModel is the model a provider's sub-agents run when
// none is named; empty means the main model.
func DefaultSubagentModel(provider string) string {
	if provider == "openrouter" {
		return "deepseek/deepseek-v4.1-flash"
	}
	return ""
}

// KeyEnv is the environment variable that holds the provider's API
// key, empty for a provider that takes none. The openresponses
// provider's is whatever Spec.KeyEnv names.
func KeyEnv(provider string) string {
	switch provider {
	case "openai":
		return "OPENAI_API_KEY"
	case "openrouter":
		return "OPENROUTER_API_KEY"
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
	// SubagentModel is the sub-agents' model name; empty takes
	// DefaultSubagentModel, and failing that the main model.
	SubagentModel string
	// BaseURL is for ollama, where empty takes the default, and for
	// openresponses, where it is required.
	BaseURL string
	// KeyEnv names the variable holding the openresponses provider's
	// key; empty sends none. The other providers' are fixed.
	KeyEnv string
	// Getenv reads the environment; nil is os.Getenv.
	Getenv func(string) string
}

// Model is a provider's streamer and the model name it will be asked
// for.
type Model struct {
	Streamer openresponses.Streamer
	Name     string
	// SubagentName is the model the sub-agents are asked for.
	SubagentName string
	// Endpoint says where requests go, for the banner; it holds no key.
	Endpoint string
	// KeyEnv is the variable the key was read from, empty for none, so
	// that the child processes' environment can be kept free of it.
	KeyEnv string
	// Describer asks the vendor what a model supports; nil for a vendor
	// that publishes nothing (OpenAI, an openresponses server).
	Describer modelinfo.Describer
}

// New builds the model for spec. No request is made.
func New(ctx context.Context, spec Spec) (Model, error) {
	getenv := spec.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	m := Model{Name: spec.Model, SubagentName: spec.SubagentModel}
	if m.Name == "" {
		m.Name = DefaultModel(spec.Provider)
	}
	if m.SubagentName == "" {
		m.SubagentName = DefaultSubagentModel(spec.Provider)
	}
	if m.SubagentName == "" {
		m.SubagentName = m.Name
	}
	// Settings errors come before a missing key, so a key is never
	// asked for on behalf of a setting that cannot work.
	switch spec.Provider {
	case "openresponses":
		if spec.BaseURL == "" {
			return m, errors.New("openresponses: base_url is required")
		}
		if m.Name == "" {
			return m, errors.New("openresponses: model is required")
		}
		m.KeyEnv = spec.KeyEnv
	case "ollama", "openai", "openrouter", "anthropic", "gemini":
		if spec.KeyEnv != "" {
			return m, fmt.Errorf("%s: api_key_env is for the openresponses provider", spec.Provider)
		}
		if spec.BaseURL != "" && spec.Provider != "ollama" {
			return m, fmt.Errorf("%s: base_url is not supported; use the openresponses provider for another server", spec.Provider)
		}
		m.KeyEnv = KeyEnv(spec.Provider)
	default:
		return m, fmt.Errorf("unknown provider %q", spec.Provider)
	}
	var key string
	if env := m.KeyEnv; env != "" {
		if key = strings.TrimSpace(getenv(env)); key == "" {
			return m, fmt.Errorf("%s: %w: set %s in the environment (dax does not read keys from its config files)", spec.Provider, ErrNoKey, env)
		}
	}
	switch spec.Provider {
	case "ollama", "openai", "openrouter", "openresponses":
		base := spec.BaseURL
		if base == "" {
			base = map[string]string{"ollama": OllamaURL, "openai": OpenAIURL, "openrouter": OpenRouterURL}[spec.Provider]
		}
		var opts []openresponses.ClientOption
		if key != "" {
			opts = append(opts, openresponses.WithAPIKey(key))
		}
		m.Streamer = openresponses.NewClient(base, opts...).AsAdapter()
		m.Endpoint = base
		switch spec.Provider {
		case "ollama":
			m.Describer = modelinfo.Ollama(base, nil)
		case "openrouter":
			m.Describer = modelinfo.OpenRouter(base, nil)
		}
	case "anthropic":
		client := sdk.NewClient(option.WithAPIKey(key))
		m.Streamer = anthropic.New(client.Messages)
		m.Endpoint = "api.anthropic.com"
		m.Describer = modelinfo.Anthropic(&client.Models)
	case "gemini":
		client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI})
		if err != nil {
			return m, fmt.Errorf("gemini: %w", err)
		}
		m.Streamer = gemini.New(client)
		m.Endpoint = "generativelanguage.googleapis.com"
		m.Describer = modelinfo.Gemini(client.Models)
	}
	return m, nil
}
