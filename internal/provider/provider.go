// Package provider turns the provider setting into a model: the
// openresponses.Streamer the agent loop calls. A provider is a vendor,
// not a protocol: Ollama, OpenAI, OpenRouter and any other server that
// speaks Open Responses (the openresponses provider) all go through the
// openresponses client, each with its own defaults; Anthropic and
// Gemini go through their adapter modules, and so does vertex, which is
// Claude and Gemini on Google Vertex AI through those same two adapters.
// Keys come from the environment, or from a command the user names
// whose output is the key, and no error or log here carries one; vertex
// takes no key but Google's Application Default Credentials.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"cloud.google.com/go/auth/oauth2adapt"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"golang.org/x/oauth2/google"
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

// The vertex provider's project and location come from Google Cloud's
// own variables, the ones its SDKs and genai read, not from any vendor's:
// the project from GOOGLE_CLOUD_PROJECT, else the credentials' own
// project, and the location from GOOGLE_CLOUD_LOCATION, else
// GOOGLE_CLOUD_REGION.
const (
	ProjectEnv  = "GOOGLE_CLOUD_PROJECT"
	LocationEnv = "GOOGLE_CLOUD_LOCATION"
	RegionEnv   = "GOOGLE_CLOUD_REGION"
)

// cloudPlatform is the OAuth scope Vertex AI takes.
const cloudPlatform = "https://www.googleapis.com/auth/cloud-platform"

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
	case "anthropic", "vertex":
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
	// KeyCommand is a program and its arguments whose standard output
	// is the key, in place of the environment variable. It is run when
	// the model is built, again once the key is KeyTTL old, and again
	// when the server answers 401, so a key that expires is replaced
	// without a restart. It is for the providers that take a key.
	KeyCommand []string
	// KeyLogin says how to sign in again, a URL or a command, for the
	// error when KeyCommand fails or its key is refused; empty leaves
	// the error a general word about signing in.
	KeyLogin string
	// SessionHeader names the header each request carries the ID of
	// its run's session in, for a server that groups calls or keys a
	// prompt cache by session; empty sends none. A sub-agent's
	// requests carry the sub-agent's session.
	SessionHeader string
	// ClientHeader names the header each request carries Client in,
	// for a server that records which client called; empty sends none.
	ClientHeader string
	// Client is what ClientHeader carries, such as "dax/v0.0.5".
	Client string
	// Getenv reads the environment; nil is os.Getenv.
	Getenv func(string) string
	// Credentials finds the vertex provider's Google credentials; nil
	// is Application Default Credentials, which reads
	// GOOGLE_APPLICATION_CREDENTIALS or the file that
	// `gcloud auth application-default login` writes.
	Credentials func(context.Context) (*google.Credentials, error)
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
	// KeyStderr sends the key command's standard error to w as it runs,
	// os.Stderr until it is called; nil keeps it only for the error, for
	// a front that owns the screen. It is nil without a key command.
	KeyStderr func(w io.Writer)
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
	case "ollama", "openai", "openrouter", "anthropic", "gemini", "vertex":
		if spec.KeyEnv != "" {
			return m, fmt.Errorf("%s: api_key_env is for the openresponses provider", spec.Provider)
		}
		if spec.BaseURL != "" && spec.Provider != "ollama" {
			return m, fmt.Errorf("%s: base_url is not supported; use the openresponses provider for another server", spec.Provider)
		}
		m.KeyEnv = KeyEnv(spec.Provider)
		if spec.Provider == "vertex" {
			if spec.SessionHeader != "" || spec.ClientHeader != "" {
				return m, errors.New("vertex: session_header and client_header are not supported; Google's clients send their own")
			}
			if vertexLocation(getenv) == "" {
				return m, fmt.Errorf("vertex: set %s (or %s) in the environment", LocationEnv, RegionEnv)
			}
			for _, name := range []string{m.Name, m.SubagentName} {
				if vertexFamily(name) == "" {
					return m, errVertexModel(name)
				}
			}
		}
	default:
		return m, fmt.Errorf("unknown provider %q", spec.Provider)
	}
	var key string
	var keys *keySource
	switch {
	case len(spec.KeyCommand) > 0:
		switch spec.Provider {
		case "ollama", "vertex":
			return m, fmt.Errorf("%s: api_key_command is for a provider that takes a key", spec.Provider)
		}
		if spec.KeyEnv != "" {
			return m, fmt.Errorf("%s: api_key_command and api_key_env are two sources for one key; set one", spec.Provider)
		}
		// The key comes from the command, so there is no variable for
		// the children's environment to be kept free of.
		m.KeyEnv = ""
		keys = newKeySource(spec.KeyCommand, spec.KeyLogin)
		m.KeyStderr = keys.setEcho
		// Run it now, so a command that cannot print a key is an error
		// before any request, as a missing variable is.
		if _, err := keys.Key(ctx); err != nil {
			return m, fmt.Errorf("%s: %w", spec.Provider, err)
		}
	case spec.KeyLogin != "":
		return m, fmt.Errorf("%s: api_key_login is for api_key_command, which is not set", spec.Provider)
	case m.KeyEnv != "":
		if key = strings.TrimSpace(getenv(m.KeyEnv)); key == "" {
			return m, fmt.Errorf("%s: %w: set %s in the environment (dax does not read keys from its config files)", spec.Provider, ErrNoKey, m.KeyEnv)
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
		if keys != nil || spec.SessionHeader != "" || spec.ClientHeader != "" {
			opts = append(opts, openresponses.WithMiddleware(func(next http.RoundTripper) http.RoundTripper {
				return transport(spec, keys, bearer, next)
			}))
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
		client := sdk.NewClient(anthropicOptions(key, keys, transport(spec, keys, xAPIKey, nil))...)
		m.Streamer = anthropic.New(client.Messages)
		m.Endpoint = "api.anthropic.com"
		m.Describer = modelinfo.Anthropic(&client.Models)
	case "vertex":
		find := spec.Credentials
		if find == nil {
			find = func(ctx context.Context) (*google.Credentials, error) {
				return google.FindDefaultCredentials(ctx, cloudPlatform)
			}
		}
		creds, err := find(ctx)
		if err != nil {
			return m, fmt.Errorf("vertex: no Google credentials (run gcloud auth application-default login): %w", err)
		}
		project := strings.TrimSpace(getenv(ProjectEnv))
		if project == "" {
			project = creds.ProjectID
		}
		if project == "" {
			return m, fmt.Errorf("vertex: no Google Cloud project: set %s in the environment", ProjectEnv)
		}
		location := vertexLocation(getenv)
		claude := sdk.NewClient(vertex.WithCredentials(ctx, location, project, creds))
		gc, err := genai.NewClient(ctx, &genai.ClientConfig{
			Backend:     genai.BackendVertexAI,
			Project:     project,
			Location:    location,
			Credentials: oauth2adapt.AuthCredentialsFromOauth2Credentials(creds),
		})
		if err != nil {
			return m, fmt.Errorf("vertex: %w", err)
		}
		m.Streamer = vertexModels{claude: anthropic.New(claude.Messages), gemini: gemini.New(gc)}
		m.Endpoint = "vertex ai " + project + "/" + location
		m.Describer = vertexDescriber{}
	case "gemini":
		client, err := genai.NewClient(ctx, geminiConfig(key, keys, transport(spec, keys, googAPIKey, nil)))
		if err != nil {
			return m, fmt.Errorf("gemini: %w", err)
		}
		m.Streamer = gemini.New(client)
		m.Endpoint = "generativelanguage.googleapis.com"
		m.Describer = modelinfo.Gemini(client.Models)
	}
	if keys != nil {
		m.Streamer = withKeyErrors(m.Streamer, keys)
	}
	return m, nil
}

// vertexLocation is the Vertex AI location the environment names, empty
// for none.
func vertexLocation(getenv func(string) string) string {
	if l := strings.TrimSpace(getenv(LocationEnv)); l != "" {
		return l
	}
	return strings.TrimSpace(getenv(RegionEnv))
}

// anthropicOptions authenticates the Anthropic client with key, or with
// the command's key when keys is set, and sends through rt when it is
// not nil. The command's path drops the SDK's environment defaults, so
// no ANTHROPIC_* variable adds a credential beside the command's.
func anthropicOptions(key string, keys *keySource, rt http.RoundTripper) []option.RequestOption {
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if keys != nil {
		opts = []option.RequestOption{option.WithoutEnvironmentDefaults()}
	}
	if rt != nil {
		opts = append(opts, option.WithHTTPClient(&http.Client{Transport: rt}))
	}
	return opts
}

// geminiConfig is the Gemini API client's config for key, or for the
// command's key when keys is set, sending through rt when it is not
// nil. genai insists on a key for this backend and sends it on every
// request; the transport overwrites it with the command's, so the
// placeholder never leaves the process.
func geminiConfig(key string, keys *keySource, rt http.RoundTripper) *genai.ClientConfig {
	cc := &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI}
	if keys != nil {
		cc.APIKey = "from-api-key-command"
	}
	if rt != nil {
		cc.HTTPClient = &http.Client{Transport: rt}
	}
	return cc
}
