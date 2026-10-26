package dax

// This file is the command line: the flags, the admin modes, the
// settings, the provider, then one front.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agenteval/price"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentturn/session"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	wsremote "github.com/ChristopherDavenport/agentworkspace/remote"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/cmdline"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/modelinfo"
	"github.com/ChristopherDavenport/dax/internal/provider"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// hint says what to do about a store error that names no remedy the
// user can act on from dax.
func hint(err error) string {
	var damage cas.LogDamage
	switch {
	case errors.Is(err, agentsession.ErrSessionLocked):
		return "close the other dax holding it"
	case errors.Is(err, cas.ErrMigrationBusy):
		return "the store must move to per-session logs, and an older dax holds a session; stop every older dax, then start this one again"
	case errors.Is(err, cas.ErrLayout):
		return "a newer dax wrote this store; use that one"
	case errors.Is(err, cas.ErrLegacyStore):
		return "the store predates per-session logs; start a session or run -gc pack once to migrate it (older dax cannot read it after)"
	case errors.Is(err, cas.ErrStopped):
		return "a write to the store failed to reach the disk, so nothing more is written; restart dax and -resume the session"
	case errors.As(err, &damage):
		return "a session's log is damaged; -repair <id> keeps what still reads"
	}
	return ""
}

// run is the program: args are the command line after the program's
// name.
func run(ctx context.Context, args []string, p program) error {
	// execute is a mode of its own with flags of its own, read before
	// the session's: it serves the tools and runs no session.
	if len(args) > 0 && args[0] == "execute" {
		return runExecute(ctx, args[1:], p)
	}
	// attach is a mode of its own too: the terminal client over a
	// session another dax serves, which reads no config and opens none.
	if len(args) > 0 && args[0] == "attach" {
		return runAttach(ctx, args[1:], p)
	}
	// workspace serves a directory for a session elsewhere to run its
	// tools in (-workspace), and runs no session.
	if len(args) > 0 && args[0] == "workspace" {
		return runWorkspace(ctx, args[1:], p)
	}
	// box keeps the boxes -box runs sessions in, and runs no session.
	if len(args) > 0 && args[0] == "box" {
		return runBox(ctx, args[1:], p, os.Stdout)
	}
	// serve builds the session as the fronts do, from the same flags,
	// and serves it in place of a front.
	serving := len(args) > 0 && args[0] == "serve"
	if serving {
		args = args[1:]
	}
	fs := flag.NewFlagSet(p.name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %[1]s [flags]   (REPL)\n       %[1]s -p \"prompt\" [flags]\n       %[1]s execute [flags]   (serve the tools over MCP, on stdio or with -listen over HTTP; %[1]s execute -h)\n       %[1]s workspace [flags]   (serve this directory over HTTP for -workspace, or with -connect through a session's workspace.hub; %[1]s workspace -h)\n       %[1]s box up|ls|down|serve   (the boxes -box runs the tools in, and the box host)\n\nflags override ~/.config/dax/config.json and .dax/config.json; see dax's README.\n", p.name)
		fs.PrintDefaults()
	}
	prov := fs.String("provider", "", "model provider: ollama (default), openai, openrouter, openresponses, anthropic, gemini or vertex")
	model := fs.String("model", "", "model name; empty takes the provider's default")
	subModel := fs.String("subagent-model", "", "the sub-agents' model; empty takes the provider's default for them, else -model")
	base := fs.String("base-url", "", "endpoint of an Open Responses server (ollama and openresponses providers)")
	keyEnv := fs.String("api-key-env", "", "environment variable holding the openresponses provider's key")
	keyCmd := fs.String("api-key-command", "", "command line whose output is the provider's key, run again as the key ages or is refused; split as a shell splits words, nothing expanded")
	keyLogin := fs.String("api-key-login", "", "how to sign in again when the key command fails or its key is refused, a URL or a command; shown with the error")
	sessionHeader := fs.String("session-header", "", "header that carries each model call's session ID, for a server that groups calls by session")
	clientHeader := fs.String("client-header", "", "header that carries dax's name and version, for a server that records which client called")
	cacheTTL := fs.String("cache-ttl", "", "how long Claude's prompt cache keeps the main conversation: 5m (the default) or 1h (anthropic and vertex)")
	subCacheTTL := fs.String("subagent-cache-ttl", "", "how long Claude's prompt cache keeps a sub-agent's requests: 5m (the default) or 1h (anthropic and vertex)")
	cfgPath := fs.String("config", "", "user config file; default ~/.config/dax/config.json")
	think := fs.Bool("think", true, "request and show reasoning")
	effort := fs.String("effort", "", "the reasoning effort -think asks for: minimal, low (default), medium, high or xhigh")
	noPolicy := fs.Bool("no-policy", false, "run every tool call without asking; the config's policy is ignored")
	frontName := fs.String("front", "", "front end: tui or repl; default tui on a terminal, repl otherwise")
	once := fs.String("p", "", "run one prompt and exit")
	root := fs.String("sessions", agent.DefaultRoot(), "session store, a content-addressed store for every project; empty disables recording")
	resume := fs.String("resume", "", "continue the session with this ID")
	list := fs.Bool("list", false, "list the recorded sessions of this project, this directory or the configured executor's or served workspace's, and exit")
	verify := fs.String("verify", "", "verify the request hashes of the session with this ID and exit")
	project := fs.String("project", "", "write the session with this ID as a JSONL file into -out and exit")
	out := fs.String("out", ".", "directory -project writes into")
	importFile := fs.String("import", "", "read a JSONL session file an earlier dax wrote into the store and exit")
	repair := fs.String("repair", "", "rewrite the damaged log of the session with this ID from what still reads, and exit")
	gc := fs.String("gc", "", "pack the store's loose objects (pack) or repack and drop what no session needs (sweep), and exit")
	syncMode := fs.String("sync", "append", "when an append is durable: every append, on a response or output (response), or at exit (never)")
	compactAt := fs.Int("compact", 0, "fold the transcript through a local summary above this many estimated tokens; default three quarters of the model's context window when the vendor reports it, 0 disables")
	mcp := fs.String("mcp", "", "command line of one more stdio MCP server, offered as mcp__cli__<tool>; split as a shell splits words, nothing expanded")
	executorCmd := fs.String("executor", "", "command line that starts dax execute where the tools are to act (docker exec -i box dax execute -root /work), split as a shell splits words with nothing expanded, or the http:// or https:// URL where dax execute -listen serves; the tools run there; \"\" clears the config's")
	wsFlag := fs.String("workspace", "", "the https:// URL, or http:// to loopback, where dax workspace serves: the tools and the policy run here and act in that workspace, its token from $"+config.WorkspaceTokenEnv+" or the config's workspace.token_env; \"\" clears the config's")
	boxFlag := fs.Bool("box", false, "run the tools in this project's box: a container a box host (dax box serve, here or over ssh) makes or reuses from its config's profile, with the project at its own path, reached as the executor; -box=false runs here despite the config's box")
	boxHost := fs.String("box-host", "", "with -box, where the box host runs, in place of box.host: local, or ssh://[user@]host[:port]")
	boxProfile := fs.String("box-profile", "", "with -box, the box host's profile, in place of box.profile")
	boxConfine := fs.String("box-confine", "", "with -box, which of the box's tools the policy takes as confined, so a bare ask rule for them does not ask: off, units (MCP units with no internet and no files of yours beyond a read-only project) or workspace (those and bash, write and edit in a box with no internet); see docs/sandbox.md")
	boxInternet := fs.String("box-internet", "", "with -box, narrow the profile's internet for this run: off, allow (its hosts, or box.tighten's) or on; HTTP and HTTPS through the box's proxy, for every session in the box")
	executorEnv := fs.String("executor-pass-env", "", "variables, separated by commas, that the -executor command alone is given, credentials included (SSH_AUTH_SOCK); overrides executor.pass_env, \"\" clears it")
	agentsFlag := fs.Bool("agents", true, "offer the sub-agents as tools: explore (read-only) and task (changes files)")
	compactServer := fs.Bool("compact-server", false, "with -compact, use the server's compaction endpoint instead of a local summary")
	agentsMD := fs.Bool("agents-md", true, "put ~/.dax/AGENTS.md, the agents_md_global files and the AGENTS.md files from the repository's root down to this directory in the instructions")
	agentsMDGlobal := fs.String("agents-md-global", "", "your own instruction files for every session, separated by "+string(filepath.ListSeparator)+" as in PATH; overrides agents_md_global, \"\" clears it")
	skillsFlag := fs.Bool("skills", true, "offer the skills in .dax/skills, ~/.dax/skills and the config's skills_dirs through the skill tool")
	trustSkills := fs.Bool("trust-skills", false, "let a skill's allowed-tools run unasked until the next message")
	trustMCP := fs.Bool("trust-mcp", false, "start the MCP servers the project's config names, and trust this set for the project from now on")
	memoryFlag := fs.String("memory", "", "memory store directory (default ~/.dax/memory, or the config's); off or empty disables memory")
	pricingFile := fs.String("pricing-file", "", "JSON file of model prices for the terminal client's session cost")
	listen := fs.String("listen", "127.0.0.1:0", "dax serve: the address to serve the session on, loopback unless -tls-cert and -tls-key are given; port 0 picks one")
	tlsCert := fs.String("tls-cert", "", "dax serve: serve HTTPS with this certificate (PEM), with -tls-key; then -listen may be any address")
	tlsKey := fs.String("tls-key", "", "dax serve: the private key (PEM) of -tls-cert")
	a2aFlag := fs.Bool("a2a", false, "dax serve: also serve the session's agent over A2A (/a2a, its card at /.well-known/agent-card.json) behind a token of its own ($"+a2aTokenEnv+" or a file beside serve's); each A2A conversation is a session of its own")
	a2aEscalate := fs.Bool("a2a-escalate", false, "dax serve -a2a: put a call the policy asks about to the A2A caller, for a host nobody attaches to; without it a view attached to the served session answers, and with none attached it is refused")
	a2aURL := fs.String("a2a-url", "", "dax serve -a2a: the base URL callers reach serve at, which the agent card names (https, or http to loopback), as behind a proxy that ends TLS; without it the card names the origin each request reached")
	verbose := fs.Bool("v", false, "terminal client: print the start lines before it and what dax noted during the run after it, not only the resume command")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	for _, name := range []string{"listen", "tls-cert", "tls-key", "a2a", "a2a-escalate", "a2a-url"} {
		if given[name] && !serving {
			return fmt.Errorf("-%s is dax serve's", name)
		}
	}
	if serving && (*once != "" || *frontName != "") {
		return errors.New("dax serve has no front of its own: drop -p and -front, and attach with dax attach")
	}
	if serving && *root == "" {
		return errors.New("dax serve needs a session store: an attached view follows the record; drop -sessions \"\"")
	}
	if serving {
		// The address and the TLS files are checked before anything
		// starts.
		if err := checkServeAddr(*listen, *tlsCert, *tlsKey); err != nil {
			return err
		}
		if *a2aEscalate && !*a2aFlag {
			return errors.New("-a2a-escalate goes with -a2a")
		}
		if given["a2a-url"] {
			if !*a2aFlag {
				return errors.New("-a2a-url goes with -a2a")
			}
			if _, err := checkA2AURL(*a2aURL); err != nil {
				return err
			}
		}
	}

	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	switch {
	case *list:
		var f config.Flags
		if given["executor"] {
			f.Executor = executorCmd
		}
		if given["box"] {
			f.Box = boxFlag
		}
		if given["workspace"] {
			f.Workspace = wsFlag
		}
		return runList(ctx, os.Stdout, *root, dir, *cfgPath, f)
	case *project != "":
		path, err := agent.Project(ctx, *root, *project, *out)
		if err == nil {
			fmt.Println(path)
		}
		return err
	case *gc != "":
		if *gc != "pack" && *gc != "sweep" {
			return fmt.Errorf("-gc %q: want pack or sweep", *gc)
		}
		n, err := agent.GC(ctx, *root, *gc == "sweep", time.Hour)
		if err == nil {
			fmt.Printf("%s: %d object(s)\n", *gc, n)
		}
		return err
	case *repair != "":
		rep, err := agent.Repair(ctx, *root, *repair)
		if err != nil {
			return err
		}
		for _, d := range rep.Damage {
			fmt.Println("damaged:", d)
		}
		for _, d := range rep.Dropped {
			fmt.Printf("dropped: %s: %v\n", d.Entry, d.Err)
		}
		fmt.Printf("kept %d entries, head %s; the damaged log is %s\n", len(rep.Kept), rep.Head, rep.DamagedLog)
		return nil
	case *importFile != "":
		id, err := agent.Import(ctx, *root, *importFile)
		if err == nil {
			fmt.Println(id)
		}
		return err
	case *verify != "":
		return runVerify(ctx, *root, *verify)
	}

	policyMode, ok := map[string]cas.SyncPolicy{"append": cas.SyncEveryAppend, "response": cas.SyncOnResponse, "never": cas.SyncNever}[*syncMode]
	if !ok {
		return fmt.Errorf("-sync %q: want append, response or never", *syncMode)
	}

	// Settings: the user's file, the project's, then the flags.
	var flags config.Flags
	str := func(name string, v *string) *string {
		if given[name] {
			return v
		}
		return nil
	}
	flags.Provider, flags.Model, flags.BaseURL = str("provider", prov), str("model", model), str("base-url", base)
	flags.APIKeyEnv = str("api-key-env", keyEnv)
	flags.APIKeyLogin = str("api-key-login", keyLogin)
	flags.SessionHeader = str("session-header", sessionHeader)
	flags.ClientHeader = str("client-header", clientHeader)
	flags.CacheTTL = str("cache-ttl", cacheTTL)
	flags.SubagentCacheTTL = str("subagent-cache-ttl", subCacheTTL)
	if given["api-key-command"] {
		if flags.APIKeyCommand, err = cmdline.Split(*keyCmd); err != nil {
			return fmt.Errorf("-api-key-command: %w", err)
		}
		if flags.APIKeyCommand == nil {
			flags.APIKeyCommand = []string{}
		}
	}
	if given["agents-md-global"] {
		// A list of paths, split as PATH is, so a path may hold a space.
		flags.AgentsMDGlobal = filepath.SplitList(*agentsMDGlobal)
		if flags.AgentsMDGlobal == nil {
			flags.AgentsMDGlobal = []string{}
		}
	}
	flags.SubagentModel = str("subagent-model", subModel)
	flags.Executor = str("executor", executorCmd)
	flags.Workspace = str("workspace", wsFlag)
	if given["box"] {
		flags.Box = boxFlag
	}
	flags.BoxHost, flags.BoxProfile, flags.BoxInternet = str("box-host", boxHost), str("box-profile", boxProfile), str("box-internet", boxInternet)
	flags.BoxConfine = str("box-confine", boxConfine)
	if given["executor-pass-env"] {
		flags.ExecutorPassEnv = []string{}
		if strings.TrimSpace(*executorEnv) != "" {
			for _, v := range strings.Split(*executorEnv, ",") {
				flags.ExecutorPassEnv = append(flags.ExecutorPassEnv, strings.TrimSpace(v))
			}
		}
	}
	flags.PricingFile = str("pricing-file", pricingFile)
	flags.Effort = str("effort", effort)
	if given["agents"] {
		flags.Agents = agentsFlag
	}
	if given["think"] {
		flags.Think = think
	}
	if given["memory"] {
		m := *memoryFlag
		if m == "off" {
			m = ""
		}
		flags.MemoryDir = &m
	}
	flags.NoPolicy = *noPolicy
	// The project's config is the repository's, so it is read through
	// a workspace over the project, as the session reads AGENTS.md and
	// the project's skills, and not from this machine's directory. The
	// session's workspace is opened below, once the settings say what
	// its processes may inherit; this one only reads, and runs nothing.
	// With an executor the project is where it runs, and its file is
	// read through it once it is connected (executorSettings); with a
	// served workspace, through that once it is dialed.
	user, err := loadUser(*cfgPath)
	if err != nil {
		return err
	}
	proj, err := workspace.NewLocal(dir, nil)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	settings, err := loadSettings(proj, user, flags)
	proj.Close()
	if err != nil {
		return err
	}
	// A served workspace's token leaves the environment before anything
	// here starts a process, the key command first among them.
	var wsToken string
	if w := settings.Workspace; w != nil && w.Hub != nil {
		if wsToken, err = takeTunnelToken("workspace.hub", w.Hub.TokenEnv); err != nil {
			return err
		}
	} else if w != nil {
		if wsToken, err = takeWorkspaceToken(w.TokenEnv); err != nil {
			return err
		}
	}
	cost, err := pricing(settings.PricingFile)
	if err != nil {
		return err
	}

	m, err := provider.New(ctx, provider.Spec{Provider: settings.Provider, Model: settings.Model, SubagentModel: settings.SubagentModel, BaseURL: settings.BaseURL, KeyEnv: settings.APIKeyEnv, KeyCommand: settings.APIKeyCommand, KeyLogin: settings.APIKeyLogin, KeyScrub: settings.Credentials(), SessionHeader: settings.SessionHeader, ClientHeader: settings.ClientHeader, CacheTTL: settings.CacheTTL, SubagentCacheTTL: settings.SubagentCacheTTL, Client: p.name + "/" + p.version})
	if err != nil {
		return err
	}
	// What the session offers the model: dax's extensions as the
	// settings say, then the program's. They are chosen before the
	// executor starts, whose command must not get their credentials;
	// what chooses them is the user's and the flags', which a project's
	// layer, read only once the executor runs, cannot set.
	exts, err := p.selected(choice{
		MaxReadBytes: settings.MaxReadBytes,
		Agents:       settings.Agents, SubagentModel: m.SubagentName,
		Skills: *skillsFlag, SkillsDirs: settings.SkillsDirs, TrustSkills: *trustSkills,
		MemoryDir: settings.MemoryDir,
		Peers:     settings.Peers,
	})
	if err != nil {
		return err
	}
	// What no child of the session gets, whatever its name: the
	// extensions' secrets, the key's variable and every MCP server's
	// token and secret variables. A project's servers name neither, so
	// the user's settings, read before the executor, hold them all.
	secrets := agent.Secrets(exts, m.KeyEnv, mcpServersOf(settings, *mcp))
	// Where the tools act: this directory, or an executor elsewhere
	// whose tools act where it runs. A container or a remote runtime
	// is another workspace.Workspace, given to the session and to the
	// extensions the same way.
	var (
		ws     workspace.Workspace
		ex     *agent.Executor
		wsRoot = dir
		from   = "-executor"
	)
	var (
		boxRequest json.RawMessage
		wsLine     string
		// hosted is the hub workspace.hub hosts, nil without one.
		hosted *hostedHub
	)
	if settings.Box != nil {
		// The box host, here or over ssh, makes the box when the session
		// connects, and relays the session to the executor in it: from
		// here it is an executor like any other.
		argv, req, err := boxLauncher(*settings.Box, dir)
		if err != nil {
			return fmt.Errorf("-box: %w", err)
		}
		settings.Executor, boxRequest, from = argv, req, "-box"
	}
	if len(settings.Executor) > 0 || settings.ExecutorURL != "" {
		xo := agent.ExecutorOptions{Name: p.name, Version: p.version, PassEnv: settings.PassEnv, KeyEnv: m.KeyEnv, Secrets: secrets, LauncherEnv: settings.ExecutorPassEnv, MaxReadBytes: settings.MaxReadBytes, Box: boxRequest}
		if settings.ExecutorURL != "" {
			// Already running where it serves; nothing here starts it,
			// so no environment goes with it.
			ex, err = agent.DialExecutorURL(ctx, settings.ExecutorURL, xo)
		} else {
			// The command that starts it gets this machine's environment
			// without credentials, the model's key's variable among them,
			// less those pass_env and executor.pass_env name; the latter
			// are that command's alone.
			ex, err = agent.DialExecutor(ctx, settings.Executor, xo)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", from, err)
		}
		defer ex.Close()
		if settings, err = executorSettings(ex.Workspace(), user, flags); err != nil {
			return err
		}
		wsRoot = ex.Workspace().Root()
	} else if settings.Workspace != nil && settings.Workspace.Hub != nil {
		// Served from a host that dials in: the session hosts the hub,
		// waits for the workspace, then acts in it as in one it dials.
		hub := *settings.Workspace.Hub
		if hosted, err = hostHub(ctx, hub, wsToken, p.name, os.Stderr); err != nil {
			return err
		}
		defer hosted.Close()
		if settings, err = projectSettings("workspace.hub", hosted.Workspace, user, flags); err != nil {
			return err
		}
		ws, wsRoot, wsLine = hosted.Workspace, hosted.Workspace.Root(), hubLine(hosted, hub.Name)
	} else if settings.Workspace != nil {
		// Served elsewhere: the tools, their facts and the policy run
		// here, and every file and process they touch is the served
		// workspace's. The project's file is read through it.
		served, err := dialWorkspace(ctx, *settings.Workspace, wsToken)
		if err != nil {
			return err
		}
		defer served.Close()
		if settings, err = projectSettings("-workspace", served, user, flags); err != nil {
			return err
		}
		ws, wsRoot, wsLine = served, served.Root(), workspaceLine(served)
	} else {
		local, err := workspace.NewLocal(dir, toolsEnv(settings, m.KeyEnv, secrets...))
		if err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
		defer local.Close()
		ws = local
	}
	opts := agent.Options{
		Extensions: exts,
		Streamer:   m.Streamer, Model: m.Name, Think: settings.Think,
		Effort: openresponses.ReasoningEffort(settings.Effort), Dir: dir, Workspace: ws, Executor: ex, Root: *root, Sync: policyMode,
		BoxConfine:     boxConfineOf(settings.Box),
		UserDir:        agent.DefaultUserDir(),
		AgentsMD:       *agentsMD,
		AgentsMDGlobal: settings.AgentsMDGlobal,
		PassEnv:        settings.PassEnv,
		KeyEnv:         m.KeyEnv,
		MaxReadBytes:   settings.MaxReadBytes,
		Compact:        *compactAt,
		CompactServer:  *compactServer,
		Log:            func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}
	if p.named {
		opts.Name, opts.Version = p.name, p.version
	}
	// Every request, the explorer's and the compaction summary's too,
	// has its reasoning effort fitted to what the vendor says the model
	// takes. The notice goes through opts.Log as the front leaves it.
	opts.Streamer = modelinfo.Wrap(m.Streamer, m.Describer, func(msg string) {
		if opts.Log != nil {
			opts.Log("%s", msg)
		}
	})
	// The fitting happens where each configuration is built, so the
	// session records the effort that is sent.
	if w := modelinfo.Of(opts.Streamer); w != nil {
		opts.Fit = w.Fit
		// The sub-agents' own streamer, when they have one, is checked
		// against what the main one knows.
		if m.SubagentStreamer != nil {
			opts.SubagentStreamer = w.Over(m.SubagentStreamer)
		}
	} else {
		opts.SubagentStreamer = m.SubagentStreamer
	}
	info, modelLine := describeModel(ctx, opts.Streamer, m)
	if !given["compact"] {
		// The model's window when the vendor reports one; otherwise
		// compaction stays off, as before.
		opts.Compact = info.CompactBudget()
	}
	if settings.InstructionsFile != "" {
		data, err := os.ReadFile(settings.InstructionsFile)
		if err != nil {
			return fmt.Errorf("instructions_file: %w", err)
		}
		opts.Instructions = string(data)
	}
	opts.MCP = mcpServersOf(settings, *mcp)
	opts.TrustMCP = *trustMCP
	if !settings.Policy.Off {
		opts.Policy = &settings.Policy
	}
	renderers, err := extension.Renderers(wsRoot, exts)
	if err != nil {
		return err
	}

	// The front is chosen here: -p prints one answer, otherwise -front
	// names the interactive one. A new front implements the front
	// interface in front.go and gets a case in selectFront.
	fi := frontInfo{
		Name: p.name, Version: p.version, Renderers: renderers,
		Provider: settings.Provider, Model: modelNames(m, hasExtension(exts, agents.Name)), ModelInfo: modelLine, Dir: wsRoot, Executor: executorLine(ex, boxConfineOf(settings.Box)), Workspace: wsLine, Think: settings.Think, Prompt: *once,
		Policy: policySummary(settings.Policy, exts), Cost: cost, Verbose: *verbose,
	}
	var f front
	if serving {
		// dax serve is in place of a front: the views attach to it.
		f = &serveFront{info: fi, addr: *listen, tlsCert: *tlsCert, tlsKey: *tlsKey, signals: true, a2a: *a2aFlag, a2aEscalate: *a2aEscalate, a2aURL: *a2aURL, sessions: *root, hostDir: dir}
	} else if f, err = selectFront(*frontName, *once, fi, processEnv(*root != "")); err != nil {
		return err
	}
	// The terminal client owns the screen, so a key command run while
	// it does keeps what it writes for the error that reports it.
	if _, ok := f.(*tuiFront); ok && m.KeyStderr != nil {
		m.KeyStderr(nil)
	}
	f.Prepare(&opts)
	sess, err := openSession(ctx, opts, *resume)
	if err != nil {
		// What the front held back, a warning about the store say, is
		// shown before the error.
		if a, ok := f.(interface{ Abandon() }); ok {
			a.Abandon()
		}
		return err
	}
	// With a hub hosted, the whole close is bounded, so a workspace that
	// went away does not hold the exit.
	defer closeSession(sess, hosted)
	// The front is the session's human plane: it drives the session as
	// agentturn's control contract, with dax's controls, as a front over
	// a wire would.
	return f.Run(ctx, sess)
}

// runList prints the sessions of the project here: this directory's,
// or with an executor or a served workspace configured (the -executor
// or -workspace flag or the user's file; a project's can name neither),
// the project of the workspace the latest session reached at that
// address recorded. Nothing is started or dialed. A session whose tools
// ran elsewhere than this machine says where at the end of its line.
func runList(ctx context.Context, w io.Writer, root, dir, cfgPath string, f config.Flags) error {
	// Where the tools act is resolved as a run resolves it (config's
	// Resolve: the executor, the workspace, then the box, from your file
	// and the flags), with the same refusals of two together, so -list
	// lists the project a run with these flags would act in.
	user, err := loadUser(cfgPath)
	if err != nil {
		return err
	}
	argv, url, err := config.ExecutorOf(user, f)
	if err != nil {
		return err
	}
	served, err := config.WorkspaceOf(user, f)
	if err != nil {
		return err
	}
	// A box's address is its box host's command, which the settings and
	// the directory alone make: the one a session in it was reached by.
	if bs, err := config.BoxOf(user, f); err != nil {
		return err
	} else if bs != nil {
		if argv, _, err = boxLauncher(*bs, dir); err != nil {
			return err
		}
	}
	var (
		sums []agentsession.Summary
		ws   = workspace.Descriptor{Kind: workspace.KindLocal, Root: dir}
	)
	switch {
	case served != nil:
		// A hub's workspace is listed by the ref its tunnel records,
		// and nothing listens.
		ws, sums, err = agent.ListWorkspaceURL(ctx, root, served.Ref())
	case url != "":
		ws, sums, err = agent.ListExecutorURL(ctx, root, url)
	case len(argv) > 0:
		ws, sums, err = agent.ListExecutor(ctx, root, argv)
	default:
		sums, err = agent.List(ctx, root, dir)
	}
	at := ""
	if ws.Kind != workspace.KindLocal || ws.Ref != "" {
		at = "  " + ws.Kind
		if ws.Ref != "" {
			at += " " + ws.Ref
		}
		at += ":" + ws.Root
	}
	for _, s := range sums {
		fmt.Fprintf(w, "%s  %s  %7d B  %s%s\n", s.Header.CreatedAt.Local().Format("2006-01-02 15:04"), s.Header.ID, s.Size, s.Path, at)
	}
	return err
}

// loadUser reads the user's file: the one named, which must exist, or
// the default, which need not.
func loadUser(userPath string) (config.Layer, error) {
	explicit := userPath != ""
	if !explicit {
		userPath = config.Path()
	}
	return config.Load(userPath, false, explicit)
}

// loadSettings folds the user's layer, the project's read through the
// workspace the project is in, and the flags. With an executor or a
// served workspace, which only the user's layer and the flags can name,
// the project is there and not this directory, so project is not read:
// projectSettings reads it through the executor once it is connected,
// or through the workspace once it is dialed.
func loadSettings(project workspace.Workspace, user config.Layer, flags config.Flags) (config.Settings, error) {
	var proj config.Layer
	if !config.HasExecutor(user, flags) && !config.HasWorkspace(user, flags) {
		var err error
		if proj, err = config.LoadProject(project); err != nil {
			return config.Settings{}, err
		}
	}
	return config.Resolve([]config.Layer{user, proj}, flags, filepath.Join(agent.DefaultUserDir(), "memory"))
}

// executorSettings are the settings with the project's .dax/config.json
// read through the executor's workspace, view, folded in. The project's
// layer is validated as any project's: it can only tighten the policy
// and cannot name an executor, a provider or anything else that
// started the executor, so what started it stands. A file
// that cannot be read (a link out of the workspace, too large, the
// executor gone or too slow) fails the session's start: leaving it out
// would drop rules that only tighten.
func executorSettings(view workspace.Workspace, user config.Layer, flags config.Flags) (config.Settings, error) {
	return projectSettings("-executor", view, user, flags)
}

// projectSettings are the settings with the project's .dax/config.json
// read through view, the workspace the project is in elsewhere, folded
// in, as executorSettings says; from names the flag that put it there.
func projectSettings(from string, view workspace.Workspace, user config.Layer, flags config.Flags) (config.Settings, error) {
	proj, err := config.LoadProject(view)
	if errors.Is(err, wsremote.ErrForbidden) {
		if !config.ServersOnly(view) {
			// The session would act there under none of the project's
			// rules, which only tighten.
			return config.Settings{}, fmt.Errorf("%s: the workspace lets the session act in it but not read its project's config, whose rules would then not apply; grant read too (dax workspace refuses write or exec without it): %w", from, err)
		}
		// A served workspace granted its servers alone: the session
		// reads no file there and its tools touch none for a rule to
		// tighten; the session says the config was left out.
		proj, err = config.Layer{Project: true}, nil
	}
	if err != nil {
		return config.Settings{}, fmt.Errorf("%s: %w", from, err)
	}
	s, err := config.Resolve([]config.Layer{user, proj}, flags, filepath.Join(agent.DefaultUserDir(), "memory"))
	if err != nil {
		return config.Settings{}, fmt.Errorf("%s: %w", from, err)
	}
	return s, nil
}

// toolsEnv is the environment the tools and MCP servers of a session
// on this machine get: this machine's, less its credentials but those
// pass_env names, and less the model's key's variable, keyEnv, and the
// extensions' secrets (a peer's token_env), whatever their names.
// executor.pass_env is the executor's launcher's alone and is not here.
// mcpServersOf are the settings' MCP servers and the -mcp flag's, as
// the session takes them.
func mcpServersOf(s config.Settings, flag string) []agent.MCPServer {
	var out []agent.MCPServer
	for _, m := range s.MCP {
		out = append(out, agent.MCPServer{
			Name: m.Name, Command: m.Command, Host: m.In == config.MCPInHost,
			URL: m.URL, TokenEnv: m.TokenEnv, Project: m.Project,
			Description: m.Description, SecretEnv: m.SecretEnv,
		})
	}
	if flag != "" {
		out = append(out, agent.MCPServer{Name: config.MCPFlagName, Command: flag})
	}
	return out
}

func toolsEnv(s config.Settings, keyEnv string, secrets ...string) []string {
	return tool.DefaultEnv(s.PassEnv, append([]string{keyEnv}, secrets...)...)
}

// executorLine is the banner's line about the executor, "" for none.
func executorLine(ex *agent.Executor, confine string) string {
	if ex == nil {
		return ""
	}
	line := ex.String()
	if confine == config.ConfineUnits || confine == config.ConfineWorkspace {
		// What the user's box.confine asks of the policy, beside what
		// the box host made.
		line += " · confine " + confine
	}
	return line
}

// pricing loads the terminal client's price source. It is empty without
// one, and the client then shows token usage but no cost.
func pricing(path string) (client.Cost, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pricing_file: %w", err)
	}
	table, err := price.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("pricing_file: %w", err)
	}
	return price.Hook(table), nil
}

func runVerify(ctx context.Context, root, id string) error {
	n, failed, err := agent.Verify(ctx, root, id)
	if err != nil {
		return err
	}
	// A response recorded with no request hash is one the recorder
	// could not stand behind, not one that failed its check.
	unhashed, bad := 0, 0
	for _, f := range failed {
		if errors.Is(f, agentsession.ErrNoHash) {
			unhashed++
			continue
		}
		fmt.Println(f)
		bad++
	}
	fmt.Printf("%d response(s), %d failed, %d without a request hash\n", n, bad, unhashed)
	if unhashed > 0 {
		causes, err := agent.Unhashed(ctx, root, id)
		if err != nil {
			return err
		}
		told := 0
		for _, c := range causes {
			fmt.Printf("  %d unhashed from %s: %s\n", c.Responses, c.Entry, describeUnhashed(c))
			told += c.Responses
		}
		if told < unhashed {
			fmt.Printf("  %d unhashed with no cause recorded (a recorder before agentturn v0.0.15 wrote none)\n", unhashed-told)
		}
	}
	if bad > 0 {
		os.Exit(1)
	}
	return nil
}

// describeUnhashed says where a request's input and the recorded path
// parted.
func describeUnhashed(c agent.UnhashedCause) string {
	item := func(i *session.UnhashedItem) string {
		if i == nil {
			return "nothing"
		}
		return strings.TrimSpace(strings.Join([]string{i.Type, i.ID, i.CallID}, " "))
	}
	return fmt.Sprintf("%s; at input %d sent %s, recorded %s", c.Reason, c.Index, item(c.Sent), item(c.Recorded))
}

// policySummary says in a line what policy is in force: the rules each
// extension ships, the user's and the project's.
func policySummary(p config.PolicySettings, exts []extension.Extension) string {
	if p.Off {
		return "off (-no-policy): every call runs"
	}
	var parts []string
	for _, e := range exts {
		allow, rest := len(e.Policy.Allow), len(e.Policy.Ask)+len(e.Policy.Deny)
		if !p.Builtin {
			allow = 0
		}
		if allow+rest > 0 {
			parts = append(parts, fmt.Sprintf("%d rule(s) from %s", allow+rest, policy.SourceExtension(e.Name)))
		}
	}
	if !p.Builtin {
		parts = append(parts, "no shipped allow rules")
	}
	if n := len(p.User.Allow) + len(p.User.Ask) + len(p.User.Deny); n > 0 {
		parts = append(parts, fmt.Sprintf("%d rule(s) of yours", n))
	}
	if n := len(p.Project.Ask) + len(p.Project.Deny); n > 0 {
		parts = append(parts, fmt.Sprintf("%d rule(s) from the project", n))
	}
	return strings.Join(parts, ", ") + "; anything else: " + p.Fallback
}

// describeModel is what the model's vendor says about it, and the
// banner's line about the model: what it takes, or why that could not
// be asked. Both are empty for a vendor that publishes nothing. The answer is kept, so the first
// request does not ask again.
func describeModel(ctx context.Context, s openresponses.Streamer, m provider.Model) (modelinfo.Info, string) {
	w := modelinfo.Of(s)
	if w == nil || m.Describer == nil {
		return modelinfo.Info{}, ""
	}
	info, err := w.Describe(ctx, m.Name)
	if err != nil {
		return modelinfo.Info{}, "unknown (" + err.Error() + "); reasoning is sent as asked"
	}
	return info, info.String()
}

// modelNames is the model for the banner, and the sub-agents' when they
// are offered and run another.
func modelNames(m provider.Model, withAgents bool) string {
	if withAgents && m.SubagentName != m.Name {
		return m.Name + " (explore: " + m.SubagentName + ")"
	}
	return m.Name
}

// buildVersion is the main module's version, as `go install` stamps
// it, else fallback.
func buildVersion(fallback string) string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return fallback
}
