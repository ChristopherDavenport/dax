// Package config reads dax's settings. There are three layers, each
// overriding the one before: the user's file
// (~/.config/dax/config.json), the project's (.dax/config.json in the
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
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ChristopherDavenport/agentpolicy"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/tunnel"

	"github.com/ChristopherDavenport/dax/ext/a2a"
	"github.com/ChristopherDavenport/dax/internal/box"
	"github.com/ChristopherDavenport/dax/internal/cmdline"
	"github.com/ChristopherDavenport/dax/policy"
)

// Providers are the model providers dax can talk to.
var Providers = []string{"ollama", "openai", "openrouter", "openresponses", "anthropic", "gemini", "vertex"}

// Efforts are the reasoning efforts effort may name; none is
// think false.
var Efforts = []string{"minimal", "low", "medium", "high", "xhigh"}

// Config is one file's settings. A field left out of the file is left
// to the layer below.
type Config struct {
	// Provider is ollama (the default), openai, openrouter,
	// openresponses, anthropic, gemini or vertex.
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
	// APIKeyCommand is a program and its arguments that print the
	// provider's key, run again whenever the key is due to be
	// replaced; in place of the key's environment variable.
	APIKeyCommand []string `json:"api_key_command,omitempty"`
	// APIKeyLogin says how to sign in again when the key command fails
	// or its key is refused, a URL or a command to run, shown with the
	// error; dax does not open or run it.
	APIKeyLogin string `json:"api_key_login,omitempty"`
	// SessionHeader names the request header that carries the ID of
	// the session each model call is made for, for a server that
	// groups a harness's calls, or keys a prompt cache, by session.
	SessionHeader string `json:"session_header,omitempty"`
	// ClientHeader names the request header that carries dax's name
	// and version, for a server that records which client called.
	ClientHeader string `json:"client_header,omitempty"`
	// CacheTTL is how long Claude's prompt cache keeps the main
	// conversation's prefix: 5m (the default) or 1h, which costs more to
	// write and pays when you come back to a session after 5 to 60
	// minutes. Only for the anthropic and vertex providers.
	CacheTTL string `json:"cache_ttl,omitempty"`
	// SubagentCacheTTL is CacheTTL for the sub-agents' requests.
	SubagentCacheTTL string `json:"subagent_cache_ttl,omitempty"`
	// Think asks the model to reason and shows it.
	Think *bool `json:"think,omitempty"`
	// Effort is the reasoning effort Think asks for: minimal, low (the
	// default), medium, high or xhigh.
	Effort string `json:"effort,omitempty"`
	// Agents offers the explore and task sub-agents; default true.
	Agents *bool `json:"agents,omitempty"`
	// InstructionsFile is a file of your own instructions, added to
	// the system prompt after dax's and before the AGENTS.md chain.
	InstructionsFile string `json:"instructions_file,omitempty"`
	// SkillsDirs are further directories of skills, searched after
	// .dax/skills and ~/.dax/skills.
	SkillsDirs []string `json:"skills_dirs,omitempty"`
	// AgentsMDGlobal are instruction files of your own that apply to
	// every session, whatever the repository holds: read on this
	// machine after ~/.dax/AGENTS.md and before the repository's
	// AGENTS.md chain, in order, a missing one skipped.
	AgentsMDGlobal []string `json:"agents_md_global,omitempty"`
	// MemoryDir is where the model's memory is kept; "" in a file that
	// sets it turns memory off.
	MemoryDir *string `json:"memory_dir,omitempty"`
	// MCPServers are MCP servers, by name; the name is the prefix of
	// their tools, mcp__<name>__<tool>. A project's file may name them,
	// but they start only once the user trusts the project's set
	// (-trust-mcp).
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
	// PricingFile is a JSON file of model prices, for the terminal
	// client's session cost. The user sets it; a project cannot.
	PricingFile string `json:"pricing_file,omitempty"`
	// Executor runs the tools elsewhere: the command that starts `dax
	// execute` where they are to act, or the URL where it serves. The
	// user sets it; a project cannot, since it says where the session's
	// commands run.
	Executor *Executor `json:"executor,omitempty"`
	// Box runs the tools in a box: a container dax keeps for the
	// project, under gVisor by default, reached as an executor. Its
	// presence is -box's default. The user sets it; a project cannot,
	// for the reason it cannot set executor.
	Box *Box `json:"box,omitempty"`
	// Workspace runs the tools here against a workspace served
	// elsewhere (`dax workspace`): every file operation and process
	// crosses the wire, and the tools and the policy stay here. The user
	// sets it; a project cannot, since it would send the session's
	// files and commands to another machine.
	Workspace *Workspace `json:"workspace,omitempty"`
	// Peers are remote A2A agents offered to the model as tools, by
	// the tool's name. The user sets them; a project cannot, since a
	// peer is an endpoint the session's work and a token are sent to.
	Peers map[string]Peer `json:"peers,omitempty"`
}

// Peer is one remote A2A agent (dax-a2a).
type Peer struct {
	// URL is the agent's base URL, where its card is: https, or http
	// to a loopback host.
	URL string `json:"url"`
	// Relation is subsession (the default: the call's task is
	// cancelled when the call is given up) or peer (its work outlives
	// the call).
	Relation string `json:"relation,omitempty"`
	// TokenEnv names the environment variable holding the bearer
	// token sent to the agent; the token itself never goes in a config
	// file.
	TokenEnv string `json:"token_env,omitempty"`
}

// Workspace is a workspace served over HTTP (agentworkspace's remote),
// which the session's tools act in: one the session dials at URL, or
// one that dials the session's hub (Hub). Give one of the two.
type Workspace struct {
	// URL is where `dax workspace` serves: https, or http to a
	// loopback host, with no user, password, query or fragment.
	URL string `json:"url,omitempty"`
	// TokenEnv names the variable holding the bearer token sent with
	// every request; default DAX_WORKSPACE_TOKEN. The token itself never
	// goes in a config file.
	TokenEnv string `json:"token_env,omitempty"`
	// TLSCA, for an https URL, is a CA certificate (PEM) to verify the
	// server's certificate against, in place of the system's roots.
	TLSCA string `json:"tls_ca,omitempty"`
	// Hub, in place of URL, has the session host a hub that the
	// workspace dials (`dax workspace -connect`, agentworkspace's
	// tunnel), for a workspace on a host the session cannot reach.
	Hub *WorkspaceHub `json:"hub,omitempty"`
}

// WorkspaceHub is the hub a session hosts for its workspace to dial
// into: where it listens, the name the workspace is enrolled under, and
// the variable holding the one token that enrolls it.
type WorkspaceHub struct {
	// Listen is the address the hub listens at: a loopback one unless
	// TLSCert and TLSKey are given. Its upgrade path is TunnelPath.
	Listen string `json:"listen"`
	// Name is the workspace's name, a DNS label (agentworkspace's
	// tunnel.ValidName): its identity in the record, -list and memory.
	Name string `json:"name"`
	// TokenEnv names the variable holding the token that enrolls the
	// workspace as Name; default DAX_TUNNEL_TOKEN. The token itself
	// never goes in a config file.
	TokenEnv string `json:"token_env,omitempty"`
	// TLSCert and TLSKey are the certificate and key (PEM) the hub
	// serves wss:// with, both or neither.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
	// Wait is how long the session waits for the workspace to connect,
	// a Go duration ("90s", "5m"); default 60s.
	Wait string `json:"wait,omitempty"`
}

// WorkspaceTokenEnv is the variable a workspace's token is read from
// when token_env names none, the one `dax workspace` takes.
const WorkspaceTokenEnv = "DAX_WORKSPACE_TOKEN"

// TunnelTokenEnv is the variable the token that enrolls a workspace at
// a hub is read from: by `dax workspace -connect`, always, and by the
// session's hub when workspace.hub.token_env names none.
const TunnelTokenEnv = "DAX_TUNNEL_TOKEN"

// TunnelPath is the path a session's hub takes its workspace's
// upgrade at.
const TunnelPath = "/tunnel"

// DefaultHubWait is how long a session waits for its hub's workspace
// to connect when workspace.hub.wait says nothing.
const DefaultHubWait = 60 * time.Second

// TunnelRef is the ref a workspace reached through a hub under name is
// recorded by: agentworkspace's tunnel's, a host that resolves nowhere,
// which no workspace.url may name (it is http to a host that is not
// loopback).
func TunnelRef(name string) string { return "http://" + name + ".tunnel.invalid" }

// WorkspaceSettings is the served workspace the session's tools act
// in: Workspace with its defaults filled in. With Hub, URL is empty.
type WorkspaceSettings struct {
	URL, TokenEnv, TLSCA string
	Hub                  *HubSettings
}

// Ref is the identity the workspace is recorded and listed by: the
// URL's, or a hub's TunnelRef.
func (w WorkspaceSettings) Ref() string {
	if w.Hub != nil {
		return TunnelRef(w.Hub.Name)
	}
	return WorkspaceRef(w.URL)
}

// HubSettings is workspace.hub with its defaults filled in.
type HubSettings struct {
	Listen, Name, TokenEnv, TLSCert, TLSKey string
	Wait                                    time.Duration
}

// Box runs the tools in a box a box host makes: `dax box serve`, here
// or on another machine over ssh. What the box is (its image, runtime,
// limits and internet) is the box host's config's; the session names a
// profile there and may only narrow it.
type Box struct {
	// Host is where the box host runs: local (the default), or
	// ssh://[user@]host[:port].
	Host string `json:"host,omitempty"`
	// Profile is one of the box host's profiles; empty is its default.
	Profile string `json:"profile,omitempty"`
	// MCP narrows which of the profile's MCP units the session gets,
	// each run by the box host in a container of its own; empty is all
	// of them. A unit the profile has not is refused by the box host.
	MCP []string `json:"mcp,omitempty"`
	// Tighten narrows the profile: less memory, CPUs or processes, less
	// internet, fewer hosts. The box host refuses a widening.
	Tighten *box.Tighten `json:"tighten,omitempty"`
	// HostConfig, with host local, is the box host config the local box
	// host reads in place of ~/.config/dax/box-host.json.
	HostConfig string `json:"host_config,omitempty"`
	// Project, with an ssh host, is the project's path on that host;
	// default the same path as here.
	Project string `json:"project,omitempty"`
	// Checkout, with an ssh host and in place of Project, is a
	// repository the box host clones and keeps current, which is then
	// the project; the box host's checkouts.allow decides which.
	Checkout *BoxCheckout `json:"checkout,omitempty"`
	// RemoteDax, with an ssh host, is dax there; default dax on the
	// login's PATH.
	RemoteDax string `json:"remote_dax,omitempty"`
	// SSH, with an ssh host, are more arguments for ssh, before the
	// host: ["-F", "/home/me/.ssh/build.conf"].
	SSH []string `json:"ssh,omitempty"`
	// Confine says which of the box's tools the session's policy takes
	// as confined, so a bare ask rule for them does not ask: off (the
	// default), units (the MCP units the box host holds to no files
	// beyond their own, read-only project and no internet), or
	// workspace (those and the workspace's bash, write and edit, when
	// its internet is off). See docs/sandbox.md, "Fewer questions in a
	// box".
	Confine string `json:"confine,omitempty"`
}

// The values of Box.Confine.
const (
	ConfineOff       = "off"
	ConfineUnits     = "units"
	ConfineWorkspace = "workspace"
)

// CheckConfine refuses a value Box.Confine does not take.
func CheckConfine(field, v string) error {
	switch v {
	case "", ConfineOff, ConfineUnits, ConfineWorkspace:
		return nil
	}
	return fmt.Errorf("%s %q: want off, units or workspace", field, v)
}

// BoxCheckout is a repository for the box host to clone and keep: its
// URL (https://, ssh://, git@host:path or file://, with no credentials
// in it) and a branch or tag, the remote's default branch when empty.
type BoxCheckout struct {
	URL string `json:"url"`
	Ref string `json:"ref,omitempty"`
}

// BoxLocal is a box host on this machine.
const BoxLocal = "local"

// BoxSettings is the box a session runs its tools in: where its box
// host is, how to start it, and what the session asks of it.
type BoxSettings struct {
	// Host is BoxLocal or the ssh URL.
	Host string
	// SSH is Host parsed, nil when local.
	SSH *SSHTarget
	// Request is what the session asks of the box host.
	Request box.Request
	// HostConfig, Project, RemoteDax and SSHArgs are Box's.
	HostConfig, Project, RemoteDax string
	SSHArgs                        []string
	// Checkout is Box's, its URL as box.NormalizeURL writes it; nil for
	// none.
	Checkout *BoxCheckout
	// Confine is Box's, ConfineOff when it says nothing.
	Confine string
}

// SSHTarget is an ssh box host.
type SSHTarget struct{ User, Host, Port string }

// ParseBoxHost is host as Box.Host takes it: BoxLocal, or an ssh URL
// with a user and a port at most. A host or a user ssh would read as
// an option (one starting with -) is refused.
func ParseBoxHost(field, host string) (*SSHTarget, error) {
	if host == "" || host == BoxLocal {
		return nil, nil
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "ssh" || u.Opaque != "" {
		return nil, fmt.Errorf("%s %q: want local or ssh://[user@]host[:port]", field, host)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s %q: an ssh host takes no path, query or fragment; give the project's path as box.project", field, host)
	}
	t := &SSHTarget{Host: u.Hostname(), Port: u.Port()}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return nil, fmt.Errorf("%s %q: a password; ssh's keys or agent sign in", field, host)
		}
		t.User = u.User.Username()
		if !sshWord.MatchString(t.User) {
			return nil, fmt.Errorf("%s %q: user %q", field, host, t.User)
		}
	}
	if !sshHost.MatchString(t.Host) {
		return nil, fmt.Errorf("%s %q: want a host name or address", field, host)
	}
	if t.Port != "" {
		if n, err := strconv.Atoi(t.Port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%s %q: port %q", field, host, t.Port)
		}
	}
	return t, nil
}

var (
	sshWord = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	sshHost = regexp.MustCompile(`^[A-Za-z0-9_:][A-Za-z0-9._:%-]*$`)
)

// Executor is how to start or reach the executor.
type Executor struct {
	// Command starts `dax execute` and speaks MCP on its standard input
	// and output: a command line, "docker exec -i box dax execute -root
	// /work", or the program and its arguments as an array, used
	// exactly.
	Command Command `json:"command"`
	// URL is where `dax execute -listen` already serves, http or https:
	// "http://127.0.0.1:7777". Nothing here starts it, so it takes no
	// PassEnv. Give it or Command, not both.
	URL string `json:"url,omitempty"`
	// PassEnv names variables of this machine's environment, credentials
	// included, that the command which starts the executor is given and
	// nothing else is: ssh's SSH_AUTH_SOCK, the token a kubectl
	// credential plugin reads. The tools and MCP servers never see
	// them, here or in the executor. The model's key may not be one.
	PassEnv []string `json:"pass_env,omitempty"`
}

// Command is a command as a config file gives it: a string, a command
// line split as a shell splits words with nothing expanded
// (cmdline.Split), or an array of the program and its arguments, used
// exactly. No shell runs either.
type Command struct {
	line  string
	argv  []string
	array bool
	given bool
}

// UnmarshalJSON takes a string or an array of strings.
func (c *Command) UnmarshalJSON(b []byte) error {
	*c = Command{given: true}
	var err error
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '[' {
		c.array = true
		err = json.Unmarshal(t, &c.argv)
	} else {
		err = json.Unmarshal(b, &c.line)
	}
	if err != nil {
		return errors.New(`command: want a command line ("docker exec -i box dax execute") or the program and its arguments (["docker", "exec", ...])`)
	}
	return nil
}

// Given reports whether the file set the command.
func (c Command) Given() bool { return c.given }

// Argv is the program and its arguments.
func (c Command) Argv() ([]string, error) {
	if c.array {
		return c.argv, nil
	}
	return cmdline.Split(c.line)
}

// Line is the command as the file wrote it, for an error to quote.
func (c Command) Line() string {
	if c.array {
		return strings.Join(c.argv, " ")
	}
	return c.line
}

// MCPServer is how to reach one MCP server: a command started in the
// workspace or on this machine, or a URL. Exactly one of Command and
// URL is given.
type MCPServer struct {
	// Command is the command line of a stdio server, split as a shell
	// splits words, with nothing expanded (cmdline.Split).
	Command string `json:"command,omitempty"`
	// In says where Command runs: MCPInWorkspace (the default), in the
	// workspace when it can start a process and here when it cannot,
	// with an executor in the executor or not at all; or MCPInHost, on
	// this machine whatever the workspace.
	In string `json:"in,omitempty"`
	// URL is a streamable HTTP server called from this machine: https,
	// or http to a loopback host.
	URL string `json:"url,omitempty"`
	// TokenEnv names the variable holding the bearer token sent to URL's
	// origin only; empty sends none. Refused in a project's file.
	TokenEnv string `json:"token_env,omitempty"`
}

// Where a command server runs (MCPServer.In).
const (
	MCPInWorkspace = "workspace"
	MCPInHost      = "host"
)

// Policy holds the rules of the policy layer. A rule is a tool name,
// or a name with a specifier: "read", "bash(go test:*)", "write(docs/**)".
type Policy struct {
	// Builtin keeps dax's own allow list: the read-only tools and a
	// few safe commands. Default true.
	Builtin *bool `json:"builtin,omitempty"`
	// Fallback is what a call no rule names does: ask (the default),
	// allow or deny.
	Fallback string   `json:"fallback,omitempty"`
	Allow    []string `json:"allow,omitempty"`
	Ask      []string `json:"ask,omitempty"`
	Deny     []string `json:"deny,omitempty"`
}

// Path is where the user's file is: $XDG_CONFIG_HOME/dax/config.json,
// else ~/.config/dax/config.json.
func Path() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "dax", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "dax", "config.json")
	}
	return filepath.Join(home, ".config", "dax", "config.json")
}

// ProjectName is the project's file, as a name in its workspace.
const ProjectName = ".dax/config.json"

// MaxProjectBytes bounds the project's file, which a repository writes.
const MaxProjectBytes = 1 << 20

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

// LoadProject reads the project's file through the workspace the
// session's tools act in, w, so it is read where the project is and a
// link out of the workspace is refused as a tool's read of it would be:
// the file is a repository's, and dax reads no repository file from
// this machine's directory. A file that is not there is an empty
// layer; one that is a link out, a FIFO, a device or over
// MaxProjectBytes is an error, since leaving it out would drop rules
// that only tighten. Its errors name it as w.Root() joined with
// ProjectName.
func LoadProject(w workspace.Workspace) (Layer, error) {
	shown := path.Join(w.Root(), ProjectName)
	l := Layer{Path: shown, Project: true}
	data, err := workspace.Read(w, ProjectName, MaxProjectBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, fmt.Errorf("config %s: %w", shown, err)
	}
	return Parse(data, shown, true)
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
// MCPFlagName is the name of the server the -mcp flag starts, a name of
// the user's like any in their config.
const MCPFlagName = "cli"

// MCPNamesCollide reports whether servers called a and b would offer
// tools under one prefix, mcp__<name>__<tool>: one name is the other,
// or the other and one underscore, whose tools mcp__<b>___<tool> start
// as b's do. No name has a double underscore, so no other pair does.
func MCPNamesCollide(a, b string) bool { return a == b || a == b+"_" || b == a+"_" }

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
	if err := checkPaths("agents_md_global", c.AgentsMDGlobal); err != nil {
		return err
	}
	if c.Effort != "" && !slices.Contains(Efforts, c.Effort) {
		return fmt.Errorf(`effort %q: want one of %s`, c.Effort, strings.Join(Efforts, ", "))
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
	if err := checkKeyCommand(c.APIKeyCommand); err != nil {
		return err
	}
	if err := checkKeyLogin(c.APIKeyLogin); err != nil {
		return err
	}
	if err := checkHeaderName("session_header", c.SessionHeader); err != nil {
		return err
	}
	if err := checkHeaderName("client_header", c.ClientHeader); err != nil {
		return err
	}
	if err := checkCacheTTL("cache_ttl", c.CacheTTL); err != nil {
		return err
	}
	if err := checkCacheTTL("subagent_cache_ttl", c.SubagentCacheTTL); err != nil {
		return err
	}
	if l.Project {
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"provider", c.Provider != ""}, {"model", c.Model != ""}, {"subagent_model", c.SubagentModel != ""}, {"base_url", c.BaseURL != ""},
			{"api_key_env", c.APIKeyEnv != ""}, {"api_key_command", len(c.APIKeyCommand) > 0},
			{"api_key_login", c.APIKeyLogin != ""}, {"session_header", c.SessionHeader != ""}, {"client_header", c.ClientHeader != ""},
			{"cache_ttl", c.CacheTTL != ""}, {"subagent_cache_ttl", c.SubagentCacheTTL != ""},
			{"think", c.Think != nil}, {"effort", c.Effort != ""}, {"agents", c.Agents != nil}, {"instructions_file", c.InstructionsFile != ""},
			{"skills_dirs", len(c.SkillsDirs) > 0}, {"agents_md_global", len(c.AgentsMDGlobal) > 0}, {"memory_dir", c.MemoryDir != nil},
			{"pass_env", len(c.PassEnv) > 0}, {"max_read_bytes", c.MaxReadBytes != 0},
			{"pricing_file", c.PricingFile != ""}, {"executor", c.Executor != nil}, {"box", c.Box != nil}, {"workspace", c.Workspace != nil}, {"peers", len(c.Peers) > 0},
		} {
			if f.set {
				return fmt.Errorf("%s: a project file may only tighten the policy; put %s in your own config (%s)", f.name, f.name, Path())
			}
		}
	}
	if x := c.Executor; x != nil {
		if !x.Command.Given() && x.URL == "" && len(x.PassEnv) == 0 {
			return errors.New("executor.command or executor.url is required")
		}
		if x.URL != "" {
			if x.Command.Given() {
				return errors.New("executor: give command or url, not both")
			}
			if len(x.PassEnv) > 0 {
				return errors.New("executor.pass_env is for executor.command: an executor at a URL is started by no command here")
			}
			if err := checkExecutorURL("executor.url", x.URL); err != nil {
				return err
			}
		}
		if x.Command.Given() {
			argv, err := x.Command.Argv()
			if err != nil {
				return fmt.Errorf("executor.command: %w", err)
			}
			if err := checkExecutor("executor.command", x.Command.Line(), argv); err != nil {
				return err
			}
		}
		if err := checkExecutorEnv("executor.pass_env", x.PassEnv, c.APIKeyEnv); err != nil {
			return err
		}
	}
	if b := c.Box; b != nil {
		if x := c.Executor; x != nil && (x.Command.Given() || x.URL != "") {
			return errors.New("box and executor both say where the tools run; give one (executor.pass_env alone may stay: it is the box's docker command's)")
		}
		if err := b.check("box"); err != nil {
			return err
		}
	}
	if w := c.Workspace; w != nil {
		if x := c.Executor; x != nil && (x.Command.Given() || x.URL != "") {
			return errors.New("workspace and executor both say where the tools act; give one")
		}
		if c.Box != nil {
			return errors.New("workspace and box both say where the tools act; give one")
		}
		if err := w.check("workspace"); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(c.Peers) {
		if err := c.Peers[name].remote(name).Check(); err != nil {
			return fmt.Errorf("peers: %w", err)
		}
	}
	for name, s := range c.MCPServers {
		if err := CheckMCPName(name); err != nil {
			return fmt.Errorf("mcp_servers: %w", err)
		}
		if err := s.check(name, l.Project); err != nil {
			return err
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

// remote is p as dax-a2a takes it.
func (p Peer) remote(name string) a2a.Remote {
	return a2a.Remote{Name: name, URL: p.URL, Relation: p.Relation, TokenEnv: p.TokenEnv}
}

// check refuses a workspace setting that is not one.
func (w Workspace) check(field string) error {
	if w.URL != "" && w.Hub != nil {
		return fmt.Errorf("%s: give url (dax dials the workspace) or hub (the workspace dials dax), not both", field)
	}
	if w.TokenEnv != "" && !envName.MatchString(w.TokenEnv) {
		return fmt.Errorf("%s.token_env: %q is not a variable name", field, w.TokenEnv)
	}
	if w.Hub != nil {
		if w.TLSCA != "" {
			return fmt.Errorf("%s.tls_ca is for an https url; a hub serves with tls_cert and tls_key", field)
		}
		_, err := w.Hub.settings(field + ".hub")
		return err
	}
	if w.URL == "" {
		return fmt.Errorf("%s.url is required: where dax workspace serves (or hub, for one that dials in)", field)
	}
	if err := CheckWorkspaceURL(field+".url", w.URL); err != nil {
		return err
	}
	if w.TLSCA != "" && !strings.HasPrefix(strings.ToLower(w.URL), "https://") {
		return fmt.Errorf("%s.tls_ca is for an https url", field)
	}
	return nil
}

// settings is h checked, with its defaults filled in. Without a
// certificate the hub listens on a loopback address alone, as dax serve
// does, since the workspace's every file and command would cross the
// network in the clear.
func (h WorkspaceHub) settings(field string) (*HubSettings, error) {
	s := &HubSettings{Listen: strings.TrimSpace(h.Listen), Name: h.Name, TokenEnv: h.TokenEnv, TLSCert: h.TLSCert, TLSKey: h.TLSKey, Wait: DefaultHubWait}
	if s.TokenEnv == "" {
		s.TokenEnv = TunnelTokenEnv
	}
	if s.Listen == "" {
		return nil, fmt.Errorf("%s.listen is required: the address the workspace dials", field)
	}
	host, _, err := net.SplitHostPort(s.Listen)
	if err != nil {
		return nil, fmt.Errorf("%s.listen %q: %w", field, s.Listen, err)
	}
	if (s.TLSCert == "") != (s.TLSKey == "") {
		return nil, fmt.Errorf("%s: tls_cert and tls_key go together", field)
	}
	if s.TLSCert == "" && !loopbackHost(host) {
		return nil, fmt.Errorf("%s.listen %q: without tls_cert and tls_key the hub listens on loopback only (127.0.0.1, ::1 or localhost), since the workspace's files and commands would cross the network in the clear", field, s.Listen)
	}
	if !tunnel.ValidName(h.Name) {
		return nil, fmt.Errorf("%s.name %q: want a DNS label, 1 to %d lower-case letters, digits and hyphens, not starting or ending with a hyphen", field, h.Name, tunnel.MaxNameBytes)
	}
	if !envName.MatchString(s.TokenEnv) {
		return nil, fmt.Errorf("%s.token_env: %q is not a variable name", field, s.TokenEnv)
	}
	if h.Wait != "" {
		d, err := time.ParseDuration(h.Wait)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("%s.wait %q: want a positive duration such as \"60s\" or \"5m\"", field, h.Wait)
		}
		s.Wait = d
	}
	return s, nil
}

// CheckWorkspaceURL refuses a URL a workspace's token and the
// session's files should not go to: one that is not https, unless it
// is http to this machine, and one carrying credentials, a query or a
// fragment, which the workspace's descriptor would otherwise hold.
func CheckWorkspaceURL(field, s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%s %q: want the https:// or http:// URL dax workspace serves at", field, s)
	}
	if u.User != nil {
		return fmt.Errorf("%s %q carries credentials; the token goes in the variable token_env names", field, u.Redacted())
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("%s %q: a query or fragment; give the workspace's address alone", field, s)
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return fmt.Errorf("%s %q: http only to a loopback host, since the token and the session's files would cross the network in the clear; serve with -tls-cert and -tls-key, or tunnel (ssh -L) and give 127.0.0.1", field, s)
	}
	return nil
}

// WorkspaceRef is the identity of the workspace served at baseURL, the
// ref dax records it by and lists, keys memory and trusts its MCP
// servers under: the URL with its scheme and host in lower case, no
// default port (80 for http, 443 for https), no user, query or
// fragment, and no trailing slash on its path. Spellings of one address
// are one workspace; two machines reached in turn at one URL are one
// too.
func WorkspaceRef(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return baseURL
	}
	u.User = nil
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	u.Path, u.RawPath = strings.TrimSuffix(u.Path, "/"), ""
	u.Scheme = strings.ToLower(u.Scheme)
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	u.Host = host
	return u.String()
}

// checkExecutor refuses an executor command, written as line and split
// into argv, that names no program, or that is an address rather than
// a command: a URL, which goes in executor.url, or a unix socket, which
// is not supported yet.
func checkExecutor(field, line string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("%s is required: the command that starts dax execute", field)
	}
	if strings.TrimSpace(argv[0]) == "" {
		return fmt.Errorf("%s %q: an empty program", field, line)
	}
	c := strings.ToLower(argv[0])
	if isExecutorURL(c) {
		return fmt.Errorf("%s %q: a URL, not a command; give it as executor.url", field, line)
	}
	if strings.HasPrefix(c, "unix:") {
		return fmt.Errorf("%s %q: an executor over unix is not supported yet; give the command that starts dax execute (docker exec -i box dax execute, ssh host dax execute) or the URL it serves at (http://127.0.0.1:7777)", field, line)
	}
	return nil
}

// isExecutorURL reports whether an executor is given as the URL where
// `dax execute -listen` serves rather than as a command.
func isExecutorURL(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// checkExecutorURL refuses an executor URL that is not http or https
// with a host, or that carries credentials: none are sent yet, and one
// written into a URL would be in a config file.
func checkExecutorURL(field, s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%s %q: %w", field, s, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s %q: want an http:// or https:// URL", field, s)
	}
	if u.Host == "" {
		return fmt.Errorf("%s %q: no host", field, s)
	}
	if u.User != nil {
		return fmt.Errorf("%s %q: a URL with a user or password; an executor takes no credentials yet", field, s)
	}
	if u.Fragment != "" || u.RawQuery != "" {
		return fmt.Errorf("%s %q: a query or fragment; give the executor's address alone", field, s)
	}
	return nil
}

// check reports whether s can be reached, as the server called name in
// a project's file when project.
func (s MCPServer) check(name string, project bool) error {
	command, url := strings.TrimSpace(s.Command) != "", s.URL != ""
	switch {
	case command && url:
		return fmt.Errorf("mcp_servers.%s: give command or url, not both", name)
	case !command && !url:
		return fmt.Errorf("mcp_servers.%s: command or url is required", name)
	}
	if command {
		if _, err := cmdline.Split(s.Command); err != nil {
			return fmt.Errorf("mcp_servers.%s: command: %w", name, err)
		}
		switch s.In {
		case "", MCPInWorkspace, MCPInHost:
		default:
			return fmt.Errorf("mcp_servers.%s: in %q: want %s or %s", name, s.In, MCPInWorkspace, MCPInHost)
		}
		if s.TokenEnv != "" {
			return fmt.Errorf("mcp_servers.%s: token_env is for a url; a command's server gets its environment", name)
		}
		return nil
	}
	if s.In != "" {
		return fmt.Errorf("mcp_servers.%s: in is for a command; a url is called from this machine", name)
	}
	if err := checkMCPURL(s.URL); err != nil {
		return fmt.Errorf("mcp_servers.%s: url: %w", name, err)
	}
	if s.TokenEnv != "" {
		if project {
			return fmt.Errorf("mcp_servers.%s: token_env: a project file may not name a token, which would send your credential where the repository says; put the server in your own config (%s)", name, Path())
		}
		if !envName.MatchString(s.TokenEnv) {
			return fmt.Errorf("mcp_servers.%s: token_env: %q is not a variable name", name, s.TokenEnv)
		}
	}
	return nil
}

// checkMCPURL refuses a URL a server's requests, and a token, should
// not go to: one that is not https, unless it is http to this machine,
// and one carrying credentials or a fragment.
func checkMCPURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%q: want an https:// URL", s)
	}
	if u.User != nil {
		return fmt.Errorf("%q carries credentials; name the variable that holds the token in token_env", u.Redacted())
	}
	if u.Fragment != "" {
		return fmt.Errorf("%q: a fragment; give the server's endpoint", s)
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return fmt.Errorf("%q: http only to a loopback host, since its requests would cross the network in the clear", s)
	}
	return nil
}

// loopbackHost reports whether host is this machine's.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkExecutorEnv refuses a launcher's pass list that names something
// other than a variable, or names keyEnv, the variable of the model's
// key, which never leaves this process.
func checkExecutorEnv(field string, names []string, keyEnv string) error {
	for _, v := range names {
		if !envName.MatchString(v) {
			return fmt.Errorf("%s: %q is not a variable name", field, v)
		}
		if keyEnv != "" && v == keyEnv {
			return fmt.Errorf("%s: %s holds the model's key (api_key_env), which never reaches the executor", field, v)
		}
	}
	return nil
}

// HasWorkspace reports whether the user's layer and the flags set a
// served workspace: what decides, as HasExecutor does, whether the
// project is this machine's directory. A project's layer cannot set
// one.
func HasWorkspace(user Layer, f Flags) bool {
	if f.Workspace != nil {
		return strings.TrimSpace(*f.Workspace) != ""
	}
	return user.Workspace != nil
}

// WorkspaceOf is the served workspace the user's layer and the flags
// name, nil for none: the flag's URL in place of the config's, which
// keeps its token_env and tls_ca, and "" clears it. A workspace with an
// executor or a box is refused, whichever layer or flag names each,
// since each says where the tools act.
func WorkspaceOf(user Layer, f Flags) (*WorkspaceSettings, error) {
	if !HasWorkspace(user, f) {
		return nil, nil
	}
	var w Workspace
	if user.Workspace != nil {
		w = *user.Workspace
	}
	field := "workspace"
	if w.Hub != nil {
		field = "workspace.hub"
	}
	if f.Workspace != nil {
		// The flag's URL is dialed, in place of the config's url or hub.
		w.URL, w.Hub, field = strings.TrimSpace(*f.Workspace), nil, "-workspace"
		if err := CheckWorkspaceURL("-workspace", w.URL); err != nil {
			return nil, err
		}
		if w.TLSCA != "" && !strings.HasPrefix(strings.ToLower(w.URL), "https://") {
			return nil, errors.New("-workspace: your config's workspace.tls_ca is for an https URL")
		}
	}
	argv, url, err := ExecutorOf(user, f)
	if err != nil {
		return nil, err
	}
	// Each pair says which to clear: the flag given wins over the
	// config's, and two flags are refused together.
	switch {
	case BoxOn(user, f) && f.Box != nil && f.Workspace != nil:
		return nil, errors.New("-box and -workspace both say where the tools act; give one")
	case BoxOn(user, f) && f.Box != nil:
		return nil, errors.New(`-box: your config's workspace says where the tools act; give -workspace "" to use a box instead`)
	case BoxOn(user, f):
		return nil, fmt.Errorf("%s: your config's box says where the tools run; give -box=false to use this workspace instead", field)
	case (len(argv) > 0 || url != "") && f.Executor != nil && f.Workspace != nil:
		return nil, errors.New("-executor and -workspace both say where the tools act; give one")
	case (len(argv) > 0 || url != "") && f.Executor != nil:
		return nil, errors.New(`-executor: your config's workspace says where the tools act; give -workspace "" to use this executor instead`)
	case len(argv) > 0 || url != "":
		return nil, fmt.Errorf(`%s: your config's executor says where the tools run; give -executor "" to use this workspace instead`, field)
	}
	s := &WorkspaceSettings{URL: w.URL, TokenEnv: w.TokenEnv, TLSCA: w.TLSCA}
	if s.TokenEnv == "" {
		s.TokenEnv = WorkspaceTokenEnv
	}
	if w.Hub != nil {
		if s.Hub, err = w.Hub.settings("workspace.hub"); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// HasExecutor reports whether the user's layer and the flags set an
// executor, a box's among them: what decides, before the project's
// layer is read, whether the project is this machine's directory at
// all. A project's layer cannot set one.
func HasExecutor(user Layer, f Flags) bool {
	if BoxOn(user, f) {
		return true
	}
	if f.Executor != nil {
		return strings.TrimSpace(*f.Executor) != ""
	}
	return user.Executor != nil && (user.Executor.Command.Given() || user.Executor.URL != "")
}

// BoxOn reports whether the tools run in a box: -box when given, else
// whether the user's layer has a box.
func BoxOn(user Layer, f Flags) bool {
	if f.Box != nil {
		return *f.Box
	}
	return user.Box != nil
}

// BoxOf is the box the user's layer and the flags say the tools run
// in; nil when they run elsewhere. A box with an executor is refused,
// whichever layer or flag names each, and so are -box-host,
// -box-profile and -box-internet without a box.
func BoxOf(user Layer, f Flags) (*BoxSettings, error) {
	if !BoxOn(user, f) {
		for _, x := range []struct {
			name string
			v    *string
		}{{"-box-host", f.BoxHost}, {"-box-profile", f.BoxProfile}, {"-box-internet", f.BoxInternet}, {"-box-confine", f.BoxConfine}} {
			if x.v != nil {
				return nil, fmt.Errorf("%s goes with -box or a box in your config", x.name)
			}
		}
		return nil, nil
	}
	argv, url, err := ExecutorOf(user, f)
	if err != nil {
		return nil, err
	}
	if len(argv) > 0 || url != "" {
		if f.Executor != nil {
			return nil, errors.New(`-executor: the tools run in a box (-box or your config's box); give -box=false to use this executor instead`)
		}
		return nil, errors.New(`-box: your config's executor says where the tools run; give -executor "" to run them in a box instead`)
	}
	var b Box
	if user.Box != nil {
		b = *user.Box
	}
	if f.BoxHost != nil {
		b.Host = *f.BoxHost
	}
	if f.BoxProfile != nil {
		if err := box.CheckName("-box-profile", *f.BoxProfile); err != nil {
			return nil, err
		}
		b.Profile = *f.BoxProfile
	}
	if f.BoxInternet != nil {
		t := box.Tighten{}
		if b.Tighten != nil {
			t = *b.Tighten
		}
		switch *f.BoxInternet {
		case box.InternetOff, box.InternetOn:
			// A mode for this run in place of the config's: its hosts go
			// with allow alone.
			t.Internet, t.Allow = *f.BoxInternet, nil
		case box.InternetAllow:
			t.Internet = box.InternetAllow
		default:
			return nil, fmt.Errorf("-box-internet %q: want off, on or allow", *f.BoxInternet)
		}
		b.Tighten = &t
	}
	if f.BoxConfine != nil {
		if err := CheckConfine("-box-confine", *f.BoxConfine); err != nil {
			return nil, err
		}
		b.Confine = *f.BoxConfine
	}
	if err := b.check("box"); err != nil {
		if f.BoxHost != nil {
			return nil, fmt.Errorf("-box-host: %w", err)
		}
		return nil, err
	}
	s := &BoxSettings{Host: BoxLocal, HostConfig: b.HostConfig, Project: b.Project, RemoteDax: b.RemoteDax, SSHArgs: b.SSH}
	if c := b.Checkout; c != nil {
		norm, _ := box.NormalizeURL(c.URL) // checked above
		s.Checkout = &BoxCheckout{URL: norm, Ref: c.Ref}
	}
	if s.SSH, err = ParseBoxHost("box.host", b.Host); err != nil {
		return nil, err
	}
	if s.SSH != nil {
		s.Host = b.Host
	}
	s.Request = box.Request{Profile: b.Profile, MCP: b.MCP}
	s.Confine = b.Confine
	if s.Confine == "" {
		s.Confine = ConfineOff
	}
	// The workspace's claim needs its git guard: the box host holds the
	// repository's .git read-only, and says so.
	s.Request.Guard = s.Confine == ConfineWorkspace
	if b.Tighten != nil {
		s.Request.Tighten = *b.Tighten
	}
	return s, nil
}

// check refuses a box setting that is not one, or that goes with
// another host than the one named.
func (b Box) check(field string) error {
	t, err := ParseBoxHost(field+".host", b.Host)
	if err != nil {
		return err
	}
	if b.Profile != "" {
		if err := box.CheckName(field+".profile", b.Profile); err != nil {
			return err
		}
	}
	for _, name := range b.MCP {
		if err := CheckMCPName(name); err != nil {
			return fmt.Errorf("%s.mcp: %w", field, err)
		}
	}
	if b.Tighten != nil {
		if err := box.CheckTighten(field+".tighten", *b.Tighten); err != nil {
			return err
		}
	}
	if err := CheckConfine(field+".confine", b.Confine); err != nil {
		return err
	}
	local := t == nil
	switch {
	case local && b.Project != "":
		return fmt.Errorf("%s.project: with a local box host the project is the directory dax starts in", field)
	case local && b.RemoteDax != "":
		return fmt.Errorf("%s.remote_dax goes with an ssh box.host", field)
	case local && len(b.SSH) > 0:
		return fmt.Errorf("%s.ssh goes with an ssh box.host", field)
	case !local && b.HostConfig != "":
		return fmt.Errorf("%s.host_config: an ssh box host reads its own config, on its machine", field)
	case b.HostConfig != "" && !filepath.IsAbs(b.HostConfig):
		return fmt.Errorf("%s.host_config %q: want an absolute path", field, b.HostConfig)
	case b.Project != "" && !filepath.IsAbs(b.Project):
		return fmt.Errorf("%s.project %q: want an absolute path on the box host", field, b.Project)
	case b.RemoteDax != "" && (strings.TrimSpace(b.RemoteDax) == "" || strings.ContainsAny(b.RemoteDax, "\x00\n\r")):
		return fmt.Errorf("%s.remote_dax %q: want a program's name or path", field, b.RemoteDax)
	case local && b.Checkout != nil:
		return fmt.Errorf("%s.checkout goes with an ssh box.host: a local box host's project is the directory dax starts in", field)
	case b.Checkout != nil && b.Project != "":
		return fmt.Errorf("%s.checkout and %s.project both name the project; give one", field, field)
	}
	if c := b.Checkout; c != nil {
		if _, err := box.NormalizeURL(c.URL); err != nil {
			return fmt.Errorf("%s.checkout.url: %w", field, err)
		}
		if c.Ref != "" {
			if err := box.CheckRef(field+".checkout.ref", c.Ref); err != nil {
				return err
			}
		}
	}
	for _, a := range b.SSH {
		if a == "" || strings.ContainsAny(a, "\x00\n\r") {
			return fmt.Errorf("%s.ssh %q: want ssh's arguments, one per string", field, a)
		}
	}
	return nil
}

// ExecutorOf is the executor the user's layer and the flags name, as
// Resolve takes it: the program and its arguments of the command that
// starts it, or the URL where it serves; neither when the tools run in
// this process. It reads only what names the executor, so a mode that
// must know where the tools would act without starting anything (-list)
// is not failed by the rest of the settings.
func ExecutorOf(user Layer, f Flags) (argv []string, url string, err error) {
	return executorOf([]Layer{user}, f)
}

// executorOf folds the executor across the layers, a later one's
// replacing an earlier one's, then the flag, which replaces them all
// and clears them when it is "".
func executorOf(layers []Layer, f Flags) (argv []string, url string, err error) {
	for _, l := range layers {
		x := l.Executor
		if x == nil {
			continue
		}
		if x.Command.Given() {
			// Validated when the file was read.
			argv, _ = x.Command.Argv()
			url = ""
		}
		if x.URL != "" {
			argv, url = nil, x.URL
		}
	}
	if f.Executor == nil {
		return argv, url, nil
	}
	line := strings.TrimSpace(*f.Executor)
	switch {
	case isExecutorURL(line):
		if err := checkExecutorURL("-executor", line); err != nil {
			return nil, "", err
		}
		return nil, line, nil
	case line != "":
		argv, err := cmdline.Split(*f.Executor)
		if err != nil {
			return nil, "", fmt.Errorf("-executor: %w", err)
		}
		if err := checkExecutor("-executor", *f.Executor, argv); err != nil {
			return nil, "", err
		}
		return argv, "", nil
	}
	return nil, "", nil
}

// checkKeyCommand refuses a key command with no program or an empty
// word; the command is run without a shell, so there is nothing to
// quote.
func checkKeyCommand(argv []string) error {
	if argv == nil {
		return nil
	}
	if len(argv) == 0 {
		return errors.New("api_key_command: want a program and its arguments, [\"program\", \"arg\", ...]")
	}
	for _, a := range argv {
		if strings.TrimSpace(a) == "" {
			return errors.New("api_key_command: an empty word; give the program and each argument as its own string")
		}
	}
	return nil
}

// checkKeyLogin refuses a sign-in hint that would not print as one
// line of an error.
func checkKeyLogin(s string) error {
	if len(s) > 500 {
		return errors.New("api_key_login: want a URL or a command, at most 500 bytes")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("api_key_login: want one line of text, a URL or a command")
		}
	}
	return nil
}

// headerName is a header name dax may set: a token of letters, digits
// and hyphens, at most 64 bytes.
var headerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// reservedHeaders are the headers a client sets itself: the key, the
// request's framing and the vendors' own. A session or client header
// under one of these names would replace what the client sent.
var reservedHeaders = []string{
	"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie",
	"Host", "Content-Type", "Content-Length", "Content-Encoding", "Transfer-Encoding",
	"Connection", "Accept", "Accept-Encoding", "Anthropic-Version", "Anthropic-Beta", "Openai-Beta",
}

// CacheTTLs are the lifetimes Claude's prompt cache offers.
var CacheTTLs = []string{"5m", "1h"}

// checkCacheTTL refuses a cache_ttl or subagent_cache_ttl that is not
// one of CacheTTLs; empty is unset.
func checkCacheTTL(field, ttl string) error {
	if ttl != "" && !slices.Contains(CacheTTLs, ttl) {
		return fmt.Errorf("%s %q: want one of %s", field, ttl, strings.Join(CacheTTLs, ", "))
	}
	return nil
}

// checkHeaderName refuses a session_header or client_header that is not
// a header name, or that names a header the client sets itself.
func checkHeaderName(field, name string) error {
	if name == "" {
		return nil
	}
	if !headerName.MatchString(name) {
		return fmt.Errorf("%s: %q is not a header name", field, name)
	}
	for _, r := range reservedHeaders {
		if strings.EqualFold(name, r) {
			return fmt.Errorf("%s: %s is a header the client sets itself; name another", field, r)
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
		// .dax/config.json: relative paths are the project's.
		base = filepath.Dir(base)
	}
	fix := func(p string) string { return resolvePath(p, base) }
	l.InstructionsFile = fix(l.InstructionsFile)
	for i, d := range l.SkillsDirs {
		l.SkillsDirs[i] = fix(d)
	}
	for i, f := range l.AgentsMDGlobal {
		l.AgentsMDGlobal[i] = fix(f)
	}
	if l.MemoryDir != nil && *l.MemoryDir != "" {
		m := fix(*l.MemoryDir)
		l.MemoryDir = &m
	}
	l.PricingFile = fix(l.PricingFile)
	if l.Workspace != nil {
		l.Workspace.TLSCA = fix(l.Workspace.TLSCA)
		if h := l.Workspace.Hub; h != nil {
			h.TLSCert, h.TLSKey = fix(h.TLSCert), fix(h.TLSKey)
		}
	}
}

// resolvePath expands a leading ~ to the home directory and makes p
// absolute against base. "" stays "".
func resolvePath(p, base string) string {
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

// checkPaths refuses an empty entry in a list of paths, which would
// otherwise name the directory the list is relative to.
func checkPaths(field string, paths []string) error {
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("%s: an empty path", field)
		}
	}
	return nil
}

// Flags are the settings the command line can override. A nil field
// was not given.
type Flags struct {
	Provider, Model, SubagentModel, BaseURL, APIKeyEnv, MemoryDir *string
	// APIKeyCommand is the program and arguments; nil was not given.
	APIKeyCommand                    []string
	APIKeyLogin, PricingFile, Effort *string
	SessionHeader, ClientHeader      *string
	CacheTTL, SubagentCacheTTL       *string
	// Executor replaces the config's executor: a command line split as
	// a shell splits words, or an http or https URL where it serves; ""
	// clears it.
	Executor *string
	// ExecutorPassEnv replaces executor.pass_env; nil was not given and
	// empty clears it.
	ExecutorPassEnv []string
	// Box replaces whether the config's box is used: -box, -box=false.
	// BoxHost and BoxProfile replace its host and profile, and
	// BoxInternet its tightened internet, for the run.
	Box                              *bool
	BoxHost, BoxProfile, BoxInternet *string
	// BoxConfine replaces box.confine for the run.
	BoxConfine *string
	// Workspace replaces workspace.url: the URL where dax workspace
	// serves; "" clears the config's workspace.
	Workspace     *string
	Think, Agents *bool
	// AgentsMDGlobal replaces the config's agents_md_global; nil was
	// not given and empty clears it. A relative path is the working
	// directory's.
	AgentsMDGlobal []string
	// NoPolicy turns the policy off: every call runs.
	NoPolicy bool
}

// Settings is what dax runs with: the layers folded, the flags laid
// over them, validated as a whole.
type Settings struct {
	Provider         string
	Model            string   // empty: the provider's default
	SubagentModel    string   // empty: the provider's default, else Model
	BaseURL          string   // empty: the provider's default
	APIKeyEnv        string   // empty: the openresponses provider sends no key
	APIKeyCommand    []string // empty: the key comes from the environment
	APIKeyLogin      string   // empty: no sign-in hint with a key error
	SessionHeader    string   // empty: no session header on model calls
	ClientHeader     string   // empty: no client header on model calls
	CacheTTL         string   // empty: Claude's default, 5m
	SubagentCacheTTL string   // empty: Claude's default, 5m
	Think            bool
	Effort           string // the effort Think asks for
	Agents           bool
	InstructionsFile string
	SkillsDirs       []string
	AgentsMDGlobal   []string // the user's own instruction files, absolute
	MemoryDir        string   // empty: off; Resolve fills the default in
	PricingFile      string   // empty: no terminal-client cost
	Executor         []string // the executor's program and arguments; empty: ExecutorURL, or the tools run in this process
	ExecutorURL      string   // where the executor serves, when it is not started here
	ExecutorPassEnv  []string // what only the executor's command is given
	// Box is the box the tools run in, nil for none; the caller starts
	// its box host and sets Executor to that command, since nothing here
	// runs one.
	Box *BoxSettings
	// Workspace is the served workspace the tools act in, nil for none;
	// the caller dials it, and reads the project's layer through it.
	Workspace    *WorkspaceSettings
	MCP          []MCP
	Peers        []a2a.Remote // the user's, sorted by name
	PassEnv      []string
	MaxReadBytes int64
	Policy       PolicySettings
	// Sources says which layer set each of provider, model, base_url,
	// api_key_env, api_key_command, api_key_login, session_header,
	// client_header, cache_ttl and subagent_cache_ttl: "default", the
	// file's path, or "flag".
	Sources map[string]string
}

// MCP is one server to reach, as MCPServer gives it, and whether a
// project's file named it.
type MCP struct {
	Name, Command, In, URL, TokenEnv string
	Project                          bool
}

// PolicySettings is the policy folded across layers; policy.Build
// reads it.
type PolicySettings = policy.Settings

// Rules are rule lists in the policy grammar, one string per entry.
type Rules = policy.Rules

// Resolve folds the layers in order, user first, then the flags.
// defaultMemory is the memory directory when no layer names one.
func Resolve(layers []Layer, f Flags, defaultMemory string) (Settings, error) {
	s := Settings{
		Provider: "ollama", Think: true, Effort: "low", Agents: true, MemoryDir: defaultMemory,
		Policy:  PolicySettings{Builtin: true, Fallback: "ask"},
		Sources: map[string]string{"provider": "default", "model": "default", "subagent_model": "default", "base_url": "default", "api_key_env": "default", "api_key_command": "default", "api_key_login": "default", "session_header": "default", "client_header": "default", "cache_ttl": "default", "subagent_cache_ttl": "default"},
	}
	servers := map[string]MCP{}
	peers := map[string]a2a.Remote{}
	// model, subagent_model, base_url, api_key_env, api_key_command,
	// api_key_login, session_header, client_header, cache_ttl and
	// subagent_cache_ttl belong to the provider in force where they were
	// set; setFor records which one that was.
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
		if len(l.APIKeyCommand) > 0 {
			s.APIKeyCommand, s.Sources["api_key_command"], setFor["api_key_command"] = l.APIKeyCommand, l.Path, s.Provider
		}
		if l.APIKeyLogin != "" {
			s.APIKeyLogin, s.Sources["api_key_login"], setFor["api_key_login"] = l.APIKeyLogin, l.Path, s.Provider
		}
		if l.SessionHeader != "" {
			s.SessionHeader, s.Sources["session_header"], setFor["session_header"] = l.SessionHeader, l.Path, s.Provider
		}
		if l.ClientHeader != "" {
			s.ClientHeader, s.Sources["client_header"], setFor["client_header"] = l.ClientHeader, l.Path, s.Provider
		}
		if l.CacheTTL != "" {
			s.CacheTTL, s.Sources["cache_ttl"], setFor["cache_ttl"] = l.CacheTTL, l.Path, s.Provider
		}
		if l.SubagentCacheTTL != "" {
			s.SubagentCacheTTL, s.Sources["subagent_cache_ttl"], setFor["subagent_cache_ttl"] = l.SubagentCacheTTL, l.Path, s.Provider
		}
		if l.Think != nil {
			s.Think = *l.Think
		}
		if l.Effort != "" {
			s.Effort = l.Effort
		}
		if l.Agents != nil {
			s.Agents = *l.Agents
		}
		if l.InstructionsFile != "" {
			s.InstructionsFile = l.InstructionsFile
		}
		for _, d := range l.SkillsDirs {
			if !slices.Contains(s.SkillsDirs, d) {
				s.SkillsDirs = append(s.SkillsDirs, d)
			}
		}
		for _, f := range l.AgentsMDGlobal {
			if !slices.Contains(s.AgentsMDGlobal, f) {
				s.AgentsMDGlobal = append(s.AgentsMDGlobal, f)
			}
		}
		if l.MemoryDir != nil {
			s.MemoryDir = *l.MemoryDir
		}
		if l.PricingFile != "" {
			s.PricingFile = l.PricingFile
		}
		if x := l.Executor; x != nil && x.PassEnv != nil {
			s.ExecutorPassEnv = x.PassEnv
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
			if l.Project {
				for _, had := range servers {
					if !had.Project && MCPNamesCollide(n, had.Name) {
						return Settings{}, fmt.Errorf("mcp_servers.%s: your config names a server %s, and the project's (%s) one whose tools would be named as its are; a project may not stand in for one of yours, so rename one", n, had.Name, l.Path)
					}
				}
			}
			servers[n] = MCP{Name: n, Command: m.Command, In: m.In, URL: m.URL, TokenEnv: m.TokenEnv, Project: l.Project}
		}
		if !l.Project {
			// Refused in a project's file when it was read; a layer
			// built some other way adds none either.
			for n, p := range l.Peers {
				peers[n] = p.remote(n)
			}
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
	if f.APIKeyCommand != nil {
		s.APIKeyCommand, s.Sources["api_key_command"], setFor["api_key_command"] = f.APIKeyCommand, "flag", s.Provider
		if len(f.APIKeyCommand) == 0 {
			// Turning the command off turns its sign-in hint off too.
			s.APIKeyLogin, s.Sources["api_key_login"] = "", "default"
			delete(setFor, "api_key_login")
		}
	}
	if f.APIKeyLogin != nil {
		s.APIKeyLogin, s.Sources["api_key_login"], setFor["api_key_login"] = *f.APIKeyLogin, "flag", s.Provider
	}
	if f.SessionHeader != nil {
		s.SessionHeader, s.Sources["session_header"], setFor["session_header"] = *f.SessionHeader, "flag", s.Provider
	}
	if f.ClientHeader != nil {
		s.ClientHeader, s.Sources["client_header"], setFor["client_header"] = *f.ClientHeader, "flag", s.Provider
	}
	if f.CacheTTL != nil {
		s.CacheTTL, s.Sources["cache_ttl"], setFor["cache_ttl"] = *f.CacheTTL, "flag", s.Provider
	}
	if f.SubagentCacheTTL != nil {
		s.SubagentCacheTTL, s.Sources["subagent_cache_ttl"], setFor["subagent_cache_ttl"] = *f.SubagentCacheTTL, "flag", s.Provider
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
		case "api_key_command":
			s.APIKeyCommand = nil
		case "api_key_login":
			s.APIKeyLogin = ""
		case "session_header":
			s.SessionHeader = ""
		case "client_header":
			s.ClientHeader = ""
		case "cache_ttl":
			s.CacheTTL = ""
		case "subagent_cache_ttl":
			s.SubagentCacheTTL = ""
		}
		s.Sources[name] = "default"
	}
	if f.Think != nil {
		s.Think = *f.Think
	}
	if f.Effort != nil {
		s.Effort = *f.Effort
	}
	if f.Agents != nil {
		s.Agents = *f.Agents
	}
	if f.MemoryDir != nil {
		s.MemoryDir = *f.MemoryDir
	}
	if f.PricingFile != nil {
		s.PricingFile = *f.PricingFile
	}
	var err error
	if s.Executor, s.ExecutorURL, err = executorOf(layers, f); err != nil {
		return s, err
	}
	// A project's layer can name neither a box, an executor nor a
	// workspace, so the others', the last of each, decide.
	var user Layer
	for _, l := range layers {
		if !l.Project && l.Box != nil {
			user.Box = l.Box
		}
		if !l.Project && l.Executor != nil {
			user.Executor = l.Executor
		}
		if !l.Project && l.Workspace != nil {
			user.Workspace = l.Workspace
		}
	}
	if s.Workspace, err = WorkspaceOf(user, f); err != nil {
		return s, err
	}
	if s.Box, err = BoxOf(user, f); err != nil {
		return s, err
	}
	if f.ExecutorPassEnv != nil {
		if err := checkExecutorEnv("-executor-pass-env", f.ExecutorPassEnv, ""); err != nil {
			return s, err
		}
		s.ExecutorPassEnv = f.ExecutorPassEnv
		if s.ExecutorURL != "" && len(f.ExecutorPassEnv) > 0 {
			return s, errors.New("-executor-pass-env: an executor at a URL is started by no command here, so nothing is given them")
		}
	}
	if f.AgentsMDGlobal != nil {
		if err := checkPaths("-agents-md-global", f.AgentsMDGlobal); err != nil {
			return s, err
		}
		wd, err := os.Getwd()
		if err != nil {
			return s, err
		}
		s.AgentsMDGlobal = []string{}
		for _, p := range f.AgentsMDGlobal {
			if p = resolvePath(p, wd); !slices.Contains(s.AgentsMDGlobal, p) {
				s.AgentsMDGlobal = append(s.AgentsMDGlobal, p)
			}
		}
	}
	s.Policy.Off = f.NoPolicy
	for _, n := range sortedKeys(servers) {
		s.MCP = append(s.MCP, servers[n])
	}
	for _, n := range sortedKeys(peers) {
		s.Peers = append(s.Peers, peers[n])
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
	if !slices.Contains(Efforts, s.Effort) {
		return s, fmt.Errorf("effort %q: want one of %s", s.Effort, strings.Join(Efforts, ", "))
	}
	if s.Provider == "openresponses" && s.BaseURL == "" {
		return s, errors.New("provider openresponses needs a base_url")
	}
	if err := checkExecutorEnv("executor.pass_env", s.ExecutorPassEnv, s.APIKeyEnv); err != nil {
		return s, err
	}
	// The workspace's token is sent to it alone, and a hub's is
	// compared with the one its workspace sends: no command, server or
	// launcher here may be handed either.
	if w := s.Workspace; w != nil {
		field, env, what := "workspace.token_env", w.TokenEnv, "the workspace's token, which only the workspace is sent"
		if w.Hub != nil {
			field, env, what = "workspace.hub.token_env", w.Hub.TokenEnv, "the token that enrolls the workspace at the hub"
		}
		for _, list := range []struct {
			field string
			names []string
		}{{"pass_env", s.PassEnv}, {"executor.pass_env", s.ExecutorPassEnv}} {
			if slices.Contains(list.names, env) {
				return s, fmt.Errorf("%s: %s holds %s; take it out of %s", list.field, env, what, list.field)
			}
		}
		// One variable for two credentials would send one where the
		// other goes, and dax takes the workspace's out of its
		// environment, so the other would find none.
		if env == s.APIKeyEnv {
			return s, fmt.Errorf("%s: %s is api_key_env too, the model's key; name a variable of the workspace's own", field, env)
		}
		for _, m := range s.MCP {
			if m.TokenEnv == env {
				return s, fmt.Errorf("%s: %s is MCP server %s's token_env too; name a variable of the workspace's own", field, env, m.Name)
			}
		}
		for _, p := range s.Peers {
			if p.TokenEnv == env {
				return s, fmt.Errorf("%s: %s is peer %s's token_env too; name a variable of the workspace's own", field, env, p.Name)
			}
		}
		if w.Hub != nil && env == w.TokenEnv {
			return s, fmt.Errorf("%s: %s is workspace.token_env too, a served workspace's token; name a variable of the hub's own", field, env)
		}
	}
	// A peer's token is the peer's alone: no command, server or
	// launcher may be handed it.
	for _, p := range s.Peers {
		for _, list := range []struct {
			field string
			names []string
		}{{"pass_env", s.PassEnv}, {"executor.pass_env", s.ExecutorPassEnv}} {
			if p.TokenEnv != "" && slices.Contains(list.names, p.TokenEnv) {
				return s, fmt.Errorf("%s: %s holds peer %s's token (token_env), which only that peer is sent; take it out of %s", list.field, p.TokenEnv, p.Name, list.field)
			}
		}
	}
	if s.APIKeyEnv != "" {
		if !envName.MatchString(s.APIKeyEnv) {
			return s, fmt.Errorf("api_key_env: %q is not a variable name", s.APIKeyEnv)
		}
		if s.Provider != "openresponses" {
			return s, fmt.Errorf("api_key_env is for the openresponses provider, not %s (%s)", s.Provider, s.Sources["api_key_env"])
		}
	}
	if len(s.APIKeyCommand) > 0 {
		if err := checkKeyCommand(s.APIKeyCommand); err != nil {
			return s, err
		}
		if s.Provider == "ollama" || s.Provider == "vertex" {
			return s, fmt.Errorf("api_key_command is for a provider that takes a key, not %s (%s)", s.Provider, s.Sources["api_key_command"])
		}
		if s.APIKeyEnv != "" {
			return s, fmt.Errorf("api_key_command (%s) and api_key_env (%s) are two sources for one key; set one", s.Sources["api_key_command"], s.Sources["api_key_env"])
		}
	}
	for _, h := range []struct{ field, name string }{{"session_header", s.SessionHeader}, {"client_header", s.ClientHeader}} {
		if h.name == "" {
			continue
		}
		if err := checkHeaderName(h.field, h.name); err != nil {
			return s, err
		}
		if s.Provider == "vertex" {
			return s, fmt.Errorf("%s is not supported by the vertex provider (%s)", h.field, s.Sources[h.field])
		}
	}
	if s.SessionHeader != "" && strings.EqualFold(s.SessionHeader, s.ClientHeader) {
		return s, fmt.Errorf("session_header and client_header both name %s; name two headers", s.SessionHeader)
	}
	for _, t := range []struct{ field, ttl string }{{"cache_ttl", s.CacheTTL}, {"subagent_cache_ttl", s.SubagentCacheTTL}} {
		if t.ttl == "" {
			continue
		}
		if err := checkCacheTTL(t.field, t.ttl); err != nil {
			return s, err
		}
		if s.Provider != "anthropic" && s.Provider != "vertex" {
			return s, fmt.Errorf("%s is for Claude's prompt cache, on the anthropic or vertex provider, not %s (%s)", t.field, s.Provider, s.Sources[t.field])
		}
	}
	if s.APIKeyLogin != "" {
		if err := checkKeyLogin(s.APIKeyLogin); err != nil {
			return s, err
		}
		if len(s.APIKeyCommand) == 0 {
			return s, fmt.Errorf("api_key_login (%s) is for api_key_command, which is not set", s.Sources["api_key_login"])
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
