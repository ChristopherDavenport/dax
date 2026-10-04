// Package config reads dex's settings. There are three layers, each
// overriding the one before: the user's file
// (~/.config/dex/config.json), the project's (.dex/config.json in the
// working directory) and the command line. The files are JSON, read
// with the standard library, and strict: an unknown field is an error
// naming the file, so a typo is not a silently ignored setting.
//
// The project's file comes from a repository, which is not the user,
// so it can only tighten: it may add ask and deny rules, drop the
// built-in allow list and make the fallback stricter, and nothing
// else. Where the model runs, what it is told, what it remembers and
// what programs it starts are the user's to say.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"
)

// Providers are the model providers dex can talk to.
var Providers = []string{"ollama", "openai", "openrouter", "openresponses", "anthropic", "gemini"}

// Config is one file's settings. A field left out of the file is left
// to the layer below.
type Config struct {
	// Provider is ollama (the default), openai, openrouter,
	// openresponses, anthropic or gemini.
	Provider string `json:"provider,omitempty"`
	// Model is the provider's model name; empty takes the provider's
	// default.
	Model string `json:"model,omitempty"`
	// SubagentModel is the model the sub-agents run; empty takes the
	// provider's default for them, else the main model.
	SubagentModel string `json:"subagent_model,omitempty"`
	// BaseURL is the endpoint of an Open Responses server: another
	// Ollama host, or the server the openresponses provider talks to.
	BaseURL string `json:"base_url,omitempty"`
	// APIKeyEnv names the environment variable that holds the
	// openresponses provider's key; the key itself never goes in a
	// config file.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// Think asks the model to reason and shows it.
	Think *bool `json:"think,omitempty"`
	// InstructionsFile is a file of your own instructions, added to
	// the system prompt after dex's and before the AGENTS.md chain.
	InstructionsFile string `json:"instructions_file,omitempty"`
	// SkillsDirs are further directories of skills, searched after
	// .dex/skills and ~/.dex/skills.
	SkillsDirs []string `json:"skills_dirs,omitempty"`
	// MemoryDir is where the model's memory is kept; "" in a file that
	// sets it turns memory off.
	MemoryDir *string `json:"memory_dir,omitempty"`
	// MCPServers are stdio MCP servers, by name; the name is the
	// prefix of their tools, mcp__<name>__<tool>.
	MCPServers map[string]MCPServer `json:"mcp_servers,omitempty"`
	// MaxReadBytes is the most bytes of a file the read tool scans and
	// the edit tool will rewrite; default 2 MiB.
	MaxReadBytes int64 `json:"max_read_bytes,omitempty"`
	// PassEnv names environment variables that look like credentials
	// (*_API_KEY, *_TOKEN, *_SECRET) which bash commands and MCP
	// servers are nevertheless given. By default they get none.
	PassEnv []string `json:"pass_env,omitempty"`
	// Policy decides which tool calls run, ask or are refused.
	Policy *Policy `json:"policy,omitempty"`
}

// MCPServer is how to start one MCP server.
type MCPServer struct {
	// Command is the command line, split on spaces and honouring quotes.
	Command string `json:"command"`
}

// Policy holds the rules of the policy layer. A rule is a tool name,
// or a name with a specifier: "read", "bash(go test:*)", "write(docs/**)".
type Policy struct {
	// Builtin keeps dex's own allow list: the read-only tools and a
	// few safe commands. Default true.
	Builtin *bool `json:"builtin,omitempty"`
	// Fallback is what a call no rule names does: ask (the default),
	// allow or deny.
	Fallback string   `json:"fallback,omitempty"`
	Allow    []string `json:"allow,omitempty"`
	Ask      []string `json:"ask,omitempty"`
	Deny     []string `json:"deny,omitempty"`
}

// Path is where the user's file is: $XDG_CONFIG_HOME/dex/config.json,
// else ~/.config/dex/config.json.
func Path() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "dex", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "dex", "config.json")
	}
	return filepath.Join(home, ".config", "dex", "config.json")
}

// ProjectPath is the project's file for a working directory.
func ProjectPath(dir string) string { return filepath.Join(dir, ".dex", "config.json") }

// Layer is a Config and where it came from.
type Layer struct {
	Config
	Path    string
	Project bool
}

// Load reads one file. A file that is not there is an empty layer
// unless must is set, as it is for a path the user named.
func Load(path string, project, must bool) (Layer, error) {
	l := Layer{Path: path, Project: project}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && !must {
		return l, nil
	}
	if err != nil {
		return l, fmt.Errorf("config: %w", err)
	}
	return Parse(data, path, project)
}

// Parse reads a file's contents; path is for the error messages and
// for resolving the file's relative paths.
func Parse(data []byte, path string, project bool) (Layer, error) {
	l := Layer{Path: path, Project: project}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l.Config); err != nil {
		return l, fmt.Errorf("config %s: %w", path, err)
	}
	if dec.More() {
		return l, fmt.Errorf("config %s: trailing data after the object", path)
	}
	if err := l.validate(); err != nil {
		return l, fmt.Errorf("config %s: %w", path, err)
	}
	l.resolvePaths()
	return l, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var mcpName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// CheckMCPName reports whether name can name an MCP server: letters,
// digits, - and _, starting with a letter or digit, and no double
// underscore, which is what joins the server's name to a tool's in
// mcp__<server>__<tool>: a server a__b would otherwise be
// indistinguishable from server a with a tool b__x.
func CheckMCPName(name string) error {
	switch {
	case !mcpName.MatchString(name):
		return fmt.Errorf("MCP server name %q: use letters, digits, - and _", name)
	case strings.Contains(name, "__"):
		return fmt.Errorf("MCP server name %q: a double underscore separates the server from the tool", name)
	}
	return nil
}

func (l *Layer) validate() error {
	c := &l.Config
	if c.Provider != "" && !slices.Contains(Providers, c.Provider) {
		return fmt.Errorf(`provider %q: want one of %s`, c.Provider, strings.Join(Providers, ", "))
	}
	for _, v := range c.PassEnv {
		if !envName.MatchString(v) {
			return fmt.Errorf("pass_env: %q is not a variable name", v)
		}
	}
	if c.MaxReadBytes < 0 {
		return errors.New("max_read_bytes: want a positive number of bytes")
	}
	if c.BaseURL != "" {
		if err := checkBaseURL(c.BaseURL); err != nil {
			return err
		}
	}
	if c.APIKeyEnv != "" && !envName.MatchString(c.APIKeyEnv) {
		return fmt.Errorf("api_key_env: %q is not a variable name", c.APIKeyEnv)
	}
	if l.Project {
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"provider", c.Provider != ""}, {"model", c.Model != ""}, {"subagent_model", c.SubagentModel != ""}, {"base_url", c.BaseURL != ""},
			{"api_key_env", c.APIKeyEnv != ""},
			{"think", c.Think != nil}, {"instructions_file", c.InstructionsFile != ""},
			{"skills_dirs", len(c.SkillsDirs) > 0}, {"memory_dir", c.MemoryDir != nil},
			{"mcp_servers", len(c.MCPServers) > 0},
			{"pass_env", len(c.PassEnv) > 0}, {"max_read_bytes", c.MaxReadBytes != 0},
		} {
			if f.set {
				return fmt.Errorf("%s: a project file may only tighten the policy; put %s in your own config (%s)", f.name, f.name, Path())
			}
		}
	}
	for name, s := range c.MCPServers {
		if err := CheckMCPName(name); err != nil {
			return fmt.Errorf("mcp_servers: %w", err)
		}
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("mcp_servers.%s: command is required", name)
		}
	}
	if p := c.Policy; p != nil {
		switch p.Fallback {
		case "", "ask", "allow", "deny":
		default:
			return fmt.Errorf(`policy.fallback %q: want ask, allow or deny`, p.Fallback)
		}
		if l.Project && p.Fallback == "allow" {
			return errors.New(`policy.fallback "allow": a project file may only make the fallback stricter`)
		}
		if l.Project && len(p.Allow) > 0 {
			return fmt.Errorf("policy.allow: a project file may not allow anything; put allow rules in your own config (%s)", Path())
		}
		for list, rules := range map[string][]string{"allow": p.Allow, "ask": p.Ask, "deny": p.Deny} {
			for _, r := range rules {
				parsed, err := agentpolicy.ParseRules(r)
				if err != nil {
					return fmt.Errorf("policy.%s: %w", list, err)
				}
				for _, pr := range parsed {
					if l.Project && strings.HasPrefix(pr.Spec, "!") {
						return fmt.Errorf("policy.%s: %q: a project file may not carve an exception out of a rule", list, r)
					}
				}
			}
		}
	}
	return nil
}

func checkBaseURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("base_url %q: want an http:// or https:// URL", s)
	}
	return nil
}

// resolvePaths makes the file's paths absolute: ~ is the home
// directory and the rest are relative to the file's directory.
func (l *Layer) resolvePaths() {
	base := filepath.Dir(l.Path)
	if l.Project {
		// .dex/config.json: relative paths are the project's.
		base = filepath.Dir(base)
	}
	fix := func(p string) string {
		if p == "" {
			return p
		}
		if p == "~" || strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, strings.TrimPrefix(p, "~"))
			}
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		return filepath.Clean(p)
	}
	l.InstructionsFile = fix(l.InstructionsFile)
	for i, d := range l.SkillsDirs {
		l.SkillsDirs[i] = fix(d)
	}
	if l.MemoryDir != nil && *l.MemoryDir != "" {
		m := fix(*l.MemoryDir)
		l.MemoryDir = &m
	}
}

// Flags are the settings the command line can override. A nil field
// was not given.
type Flags struct {
	Provider, Model, SubagentModel, BaseURL, APIKeyEnv, MemoryDir *string
	Think                                                         *bool
	// NoPolicy turns the policy off: every call runs.
	NoPolicy bool
}

// Settings is what dex runs with: the layers folded, the flags laid
// over them, validated as a whole.
type Settings struct {
	Provider         string
	Model            string // empty: the provider's default
	SubagentModel    string // empty: the provider's default, else Model
	BaseURL          string // empty: the provider's default
	APIKeyEnv        string // empty: the openresponses provider sends no key
	Think            bool
	InstructionsFile string
	SkillsDirs       []string
	MemoryDir        string // empty: off; Resolve fills the default in
	MCP              []MCP
	PassEnv          []string
	MaxReadBytes     int64
	Policy           PolicySettings
	// Sources says which layer set each of provider, model, base_url
	// and api_key_env: "default", the file's path, or "flag".
	Sources map[string]string
}

// MCP is one server to start.
type MCP struct{ Name, Command string }

// PolicySettings is the policy folded across layers. The user's rules
// and the project's are kept apart because the project's are not
// trusted.
type PolicySettings struct {
	Off      bool
	Builtin  bool
	Fallback string
	User     Rules
	Project  Rules
}

// Rules are rule lists in the policy grammar, one string per entry.
type Rules struct{ Allow, Ask, Deny []string }

// Resolve folds the layers in order, user first, then the flags.
// defaultMemory is the memory directory when no layer names one.
func Resolve(layers []Layer, f Flags, defaultMemory string) (Settings, error) {
	s := Settings{
		Provider: "ollama", Think: true, MemoryDir: defaultMemory,
		Policy:  PolicySettings{Builtin: true, Fallback: "ask"},
		Sources: map[string]string{"provider": "default", "model": "default", "subagent_model": "default", "base_url": "default", "api_key_env": "default"},
	}
	servers := map[string]MCP{}
	// model, subagent_model, base_url and api_key_env belong to the
	// provider in force
	// where they were set; setFor records which one that was.
	setFor := map[string]string{}
	for _, l := range layers {
		if l.Provider != "" {
			s.Provider, s.Sources["provider"] = l.Provider, l.Path
		}
		if l.Model != "" {
			s.Model, s.Sources["model"], setFor["model"] = l.Model, l.Path, s.Provider
		}
		if l.SubagentModel != "" {
			s.SubagentModel, s.Sources["subagent_model"], setFor["subagent_model"] = l.SubagentModel, l.Path, s.Provider
		}
		if l.BaseURL != "" {
			s.BaseURL, s.Sources["base_url"], setFor["base_url"] = l.BaseURL, l.Path, s.Provider
		}
		if l.APIKeyEnv != "" {
			s.APIKeyEnv, s.Sources["api_key_env"], setFor["api_key_env"] = l.APIKeyEnv, l.Path, s.Provider
		}
		if l.Think != nil {
			s.Think = *l.Think
		}
		if l.InstructionsFile != "" {
			s.InstructionsFile = l.InstructionsFile
		}
		for _, d := range l.SkillsDirs {
			if !slices.Contains(s.SkillsDirs, d) {
				s.SkillsDirs = append(s.SkillsDirs, d)
			}
		}
		if l.MemoryDir != nil {
			s.MemoryDir = *l.MemoryDir
		}
		if l.MaxReadBytes != 0 {
			s.MaxReadBytes = l.MaxReadBytes
		}
		for _, v := range l.PassEnv {
			if !slices.Contains(s.PassEnv, v) {
				s.PassEnv = append(s.PassEnv, v)
			}
		}
		for n, m := range l.MCPServers {
			servers[n] = MCP{Name: n, Command: m.Command}
		}
		if p := l.Policy; p != nil {
			if p.Builtin != nil {
				if l.Project {
					// Tighten only: a project can drop the allow list,
					// never bring back what the user dropped.
					s.Policy.Builtin = s.Policy.Builtin && *p.Builtin
				} else {
					s.Policy.Builtin = *p.Builtin
				}
			}
			if p.Fallback != "" {
				if !l.Project || strictness[p.Fallback] > strictness[s.Policy.Fallback] {
					s.Policy.Fallback = p.Fallback
				}
			}
			dst := &s.Policy.User
			if l.Project {
				dst = &s.Policy.Project
			}
			dst.Allow = append(dst.Allow, p.Allow...)
			dst.Ask = append(dst.Ask, p.Ask...)
			dst.Deny = append(dst.Deny, p.Deny...)
		}
	}
	if f.Provider != nil {
		s.Provider, s.Sources["provider"] = *f.Provider, "flag"
	}
	if f.Model != nil {
		s.Model, s.Sources["model"], setFor["model"] = *f.Model, "flag", s.Provider
	}
	if f.SubagentModel != nil {
		s.SubagentModel, s.Sources["subagent_model"], setFor["subagent_model"] = *f.SubagentModel, "flag", s.Provider
	}
	if f.BaseURL != nil {
		s.BaseURL, s.Sources["base_url"], setFor["base_url"] = *f.BaseURL, "flag", s.Provider
	}
	if f.APIKeyEnv != nil {
		s.APIKeyEnv, s.Sources["api_key_env"], setFor["api_key_env"] = *f.APIKeyEnv, "flag", s.Provider
	}
	// A later layer that switched the provider leaves the earlier
	// provider's model, endpoint and key variable behind: the user's
	// "model": "qwen3-coder:30b" is Ollama's, and -provider openrouter
	// takes OpenRouter's default instead.
	for name, p := range setFor {
		if p == s.Provider {
			continue
		}
		switch name {
		case "model":
			s.Model = ""
		case "subagent_model":
			s.SubagentModel = ""
		case "base_url":
			s.BaseURL = ""
		case "api_key_env":
			s.APIKeyEnv = ""
		}
		s.Sources[name] = "default"
	}
	if f.Think != nil {
		s.Think = *f.Think
	}
	if f.MemoryDir != nil {
		s.MemoryDir = *f.MemoryDir
	}
	s.Policy.Off = f.NoPolicy
	for _, n := range sortedKeys(servers) {
		s.MCP = append(s.MCP, servers[n])
	}
	if !slices.Contains(Providers, s.Provider) {
		return s, fmt.Errorf("provider %q: want one of %s", s.Provider, strings.Join(Providers, ", "))
	}
	if s.BaseURL != "" {
		if err := checkBaseURL(s.BaseURL); err != nil {
			return s, err
		}
		if s.Provider != "ollama" && s.Provider != "openresponses" {
			return s, fmt.Errorf("base_url is for the ollama and openresponses providers, not %s (%s); use provider openresponses for another server", s.Provider, s.Sources["base_url"])
		}
	}
	if s.Provider == "openresponses" && s.BaseURL == "" {
		return s, errors.New("provider openresponses needs a base_url")
	}
	if s.APIKeyEnv != "" {
		if !envName.MatchString(s.APIKeyEnv) {
			return s, fmt.Errorf("api_key_env: %q is not a variable name", s.APIKeyEnv)
		}
		if s.Provider != "openresponses" {
			return s, fmt.Errorf("api_key_env is for the openresponses provider, not %s (%s)", s.Provider, s.Sources["api_key_env"])
		}
	}
	return s, nil
}

// strictness orders the fallbacks; a project's may only raise it.
var strictness = map[string]int{"allow": 0, "ask": 1, "deny": 2}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
