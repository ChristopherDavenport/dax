// Package config reads dex's settings. There are three layers, each
// overriding the one before: the user's file
// (~/.config/dex/config.json), the project's (.dex/config.json in the
// working directory) and the command line. The files are JSON, read
// with the standard library, and strict: an unknown field is an error
// naming the file, so a typo is not a silently ignored setting.
//
// The project's file comes from a repository, which is not the user,
// so it is held to less: it may not start programs (mcp_servers), may
// not point an API key at another host (base_url, except for the
// local Ollama, which takes no key), and the rules in its policy can
// ask or deny but not allow.
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
var Providers = []string{"ollama", "openai", "anthropic", "gemini"}

// Config is one file's settings. A field left out of the file is left
// to the layer below.
type Config struct {
	// Provider is ollama (the default), openai, anthropic or gemini.
	Provider string `json:"provider,omitempty"`
	// Model is the provider's model name; empty takes the provider's
	// default.
	Model string `json:"model,omitempty"`
	// BaseURL is the endpoint of an OpenAI-compatible server, for the
	// ollama and openai providers.
	BaseURL string `json:"base_url,omitempty"`
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

var mcpName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func (l *Layer) validate() error {
	c := &l.Config
	if c.Provider != "" && !slices.Contains(Providers, c.Provider) {
		return fmt.Errorf(`provider %q: want one of %s`, c.Provider, strings.Join(Providers, ", "))
	}
	if c.BaseURL != "" {
		if err := checkBaseURL(c.BaseURL); err != nil {
			return err
		}
	}
	if l.Project && len(c.MCPServers) > 0 {
		return errors.New("mcp_servers: a project file may not start programs; put the server in your own config")
	}
	for name, s := range c.MCPServers {
		if !mcpName.MatchString(name) {
			return fmt.Errorf("mcp_servers: name %q: use letters, digits, - and _", name)
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
			return errors.New(`policy.fallback "allow": a project file may not allow everything`)
		}
		if l.Project && p.Builtin != nil && !*p.Builtin {
			return errors.New("policy.builtin: a project file may not drop the built-in rules")
		}
		for list, rules := range map[string][]string{"allow": p.Allow, "ask": p.Ask, "deny": p.Deny} {
			for _, r := range rules {
				if _, err := agentpolicy.ParseRules(r); err != nil {
					return fmt.Errorf("policy.%s: %w", list, err)
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
	Provider, Model, BaseURL, MemoryDir *string
	Think                               *bool
	// NoPolicy turns the policy off: every call runs.
	NoPolicy bool
}

// Settings is what dex runs with: the layers folded, the flags laid
// over them, validated as a whole.
type Settings struct {
	Provider         string
	Model            string // empty: the provider's default
	BaseURL          string // empty: the provider's default
	Think            bool
	InstructionsFile string
	SkillsDirs       []string
	MemoryDir        string // empty: off; Resolve fills the default in
	MCP              []MCP
	Policy           PolicySettings
	// Sources says which layer set each of provider, model, base_url:
	// "default", the file's path, or "flag".
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
		Sources: map[string]string{"provider": "default", "model": "default", "base_url": "default"},
	}
	servers := map[string]MCP{}
	baseFrom := Layer{}
	for _, l := range layers {
		if l.Provider != "" {
			s.Provider, s.Sources["provider"] = l.Provider, l.Path
		}
		if l.Model != "" {
			s.Model, s.Sources["model"] = l.Model, l.Path
		}
		if l.BaseURL != "" {
			s.BaseURL, s.Sources["base_url"], baseFrom = l.BaseURL, l.Path, l
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
		for n, m := range l.MCPServers {
			servers[n] = MCP{Name: n, Command: m.Command}
		}
		if p := l.Policy; p != nil {
			if p.Builtin != nil {
				s.Policy.Builtin = *p.Builtin
			}
			if p.Fallback != "" {
				s.Policy.Fallback = p.Fallback
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
		s.Model, s.Sources["model"] = *f.Model, "flag"
	}
	if f.BaseURL != nil {
		s.BaseURL, s.Sources["base_url"], baseFrom = *f.BaseURL, "flag", Layer{}
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
		if s.Provider != "ollama" && s.Provider != "openai" {
			return s, fmt.Errorf("base_url is for the ollama and openai providers, not %s", s.Provider)
		}
		if baseFrom.Project && s.Provider != "ollama" {
			return s, fmt.Errorf("config %s: base_url would receive your %s API key; a project file may set it only for ollama", baseFrom.Path, s.Provider)
		}
	}
	return s, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
