package dax

// This file is the command line: the flags, the admin modes, the
// settings, the provider, then one front.

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/extension"
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
	fs := flag.NewFlagSet(p.name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %[1]s [flags]   (REPL)\n       %[1]s -p \"prompt\" [flags]\n       %[1]s execute [flags]   (serve the tools over stdio MCP; %[1]s execute -h)\n\nflags override ~/.config/dax/config.json and .dax/config.json; see dax's README.\n", p.name)
		fs.PrintDefaults()
	}
	prov := fs.String("provider", "", "model provider: ollama (default), openai, openrouter, openresponses, anthropic, gemini or vertex")
	model := fs.String("model", "", "model name; empty takes the provider's default")
	subModel := fs.String("subagent-model", "", "the sub-agents' model; empty takes the provider's default for them, else -model")
	base := fs.String("base-url", "", "endpoint of an Open Responses server (ollama and openresponses providers)")
	keyEnv := fs.String("api-key-env", "", "environment variable holding the openresponses provider's key")
	keyCmd := fs.String("api-key-command", "", "command line whose output is the provider's key, run again as the key ages or is refused; split on spaces")
	keyLogin := fs.String("api-key-login", "", "how to sign in again when the key command fails or its key is refused, a URL or a command; shown with the error")
	sessionHeader := fs.String("session-header", "", "header that carries each model call's session ID, for a server that groups calls by session")
	clientHeader := fs.String("client-header", "", "header that carries dax's name and version, for a server that records which client called")
	cfgPath := fs.String("config", "", "user config file; default ~/.config/dax/config.json")
	think := fs.Bool("think", true, "request and show reasoning")
	effort := fs.String("effort", "", "the reasoning effort -think asks for: minimal, low (default), medium, high or xhigh")
	noPolicy := fs.Bool("no-policy", false, "run every tool call without asking; the config's policy is ignored")
	frontName := fs.String("front", "", "front end: tui or repl; default tui on a terminal, repl otherwise")
	once := fs.String("p", "", "run one prompt and exit")
	root := fs.String("sessions", agent.DefaultRoot(), "session store, a content-addressed store for every project; empty disables recording")
	resume := fs.String("resume", "", "continue the session with this ID")
	list := fs.Bool("list", false, "list recorded sessions for this directory and exit")
	verify := fs.String("verify", "", "verify the request hashes of the session with this ID and exit")
	project := fs.String("project", "", "write the session with this ID as a JSONL file into -out and exit")
	out := fs.String("out", ".", "directory -project writes into")
	importFile := fs.String("import", "", "read a JSONL session file an earlier dax wrote into the store and exit")
	repair := fs.String("repair", "", "rewrite the damaged log of the session with this ID from what still reads, and exit")
	gc := fs.String("gc", "", "pack the store's loose objects (pack) or repack and drop what no session needs (sweep), and exit")
	syncMode := fs.String("sync", "append", "when an append is durable: every append, on a response or output (response), or at exit (never)")
	compactAt := fs.Int("compact", 0, "fold the transcript through a local summary above this many estimated tokens; default three quarters of the model's context window when the vendor reports it, 0 disables")
	mcp := fs.String("mcp", "", "command line of one more stdio MCP server, offered as mcp__cli__<tool>")
	executorCmd := fs.String("executor", "", "command line that starts dax execute where the tools are to act (docker exec -i box dax execute -root /work), split on spaces; the tools run there; \"\" clears the config's")
	agentsFlag := fs.Bool("agents", true, "offer the sub-agents as tools: explore (read-only) and task (changes files)")
	compactServer := fs.Bool("compact-server", false, "with -compact, use the server's compaction endpoint instead of a local summary")
	agentsMD := fs.Bool("agents-md", true, "put ~/.dax/AGENTS.md, the agents_md_global files and the AGENTS.md files from the repository's root down to this directory in the instructions")
	agentsMDGlobal := fs.String("agents-md-global", "", "your own instruction files for every session, separated by "+string(filepath.ListSeparator)+" as in PATH; overrides agents_md_global, \"\" clears it")
	skillsFlag := fs.Bool("skills", true, "offer the skills in .dax/skills, ~/.dax/skills and the config's skills_dirs through the skill tool")
	trustSkills := fs.Bool("trust-skills", false, "let a skill's allowed-tools run unasked until the next message")
	memoryFlag := fs.String("memory", "", "memory store directory (default ~/.dax/memory, or the config's); off or empty disables memory")
	pricingFile := fs.String("pricing-file", "", "JSON file of model prices for the terminal client's session cost")
	verbose := fs.Bool("v", false, "terminal client: print the start lines before it and what dax noted during the run after it, not only the resume command")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	switch {
	case *list:
		sums, err := agent.List(ctx, *root, dir)
		for _, s := range sums {
			fmt.Printf("%s  %s  %7d B  %s\n", s.Header.CreatedAt.Local().Format("2006-01-02 15:04"), s.Header.ID, s.Size, s.Path)
		}
		return err
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
	if given["api-key-command"] {
		flags.APIKeyCommand = strings.Fields(*keyCmd)
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
	proj, err := workspace.NewLocal(dir, nil)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	settings, err := loadSettings(proj, *cfgPath, flags)
	proj.Close()
	if err != nil {
		return err
	}
	cost, err := pricing(settings.PricingFile)
	if err != nil {
		return err
	}

	m, err := provider.New(ctx, provider.Spec{Provider: settings.Provider, Model: settings.Model, SubagentModel: settings.SubagentModel, BaseURL: settings.BaseURL, KeyEnv: settings.APIKeyEnv, KeyCommand: settings.APIKeyCommand, KeyLogin: settings.APIKeyLogin, SessionHeader: settings.SessionHeader, ClientHeader: settings.ClientHeader, Client: p.name + "/" + p.version})
	if err != nil {
		return err
	}
	// Where the tools act: this directory, or an executor elsewhere
	// whose tools act where it runs. A container or a remote runtime
	// is another workspace.Workspace, given to the session and to the
	// extensions the same way.
	var (
		ws     workspace.Workspace
		ex     *agent.Executor
		wsRoot = dir
	)
	if settings.Executor != "" {
		if len(settings.MCP) > 0 || *mcp != "" {
			return errors.New("-executor: MCP servers cannot run with an executor yet, since they would run on this machine; leave out mcp_servers and -mcp, or clear the executor with -executor=''")
		}
		// The command that starts it gets this machine's environment
		// without credentials, the model's key's variable among them.
		ex, err = agent.DialExecutor(ctx, settings.Executor, agent.ExecutorOptions{Name: p.name, Version: p.version, PassEnv: settings.PassEnv, KeyEnv: m.KeyEnv})
		if err != nil {
			return fmt.Errorf("-executor: %w", err)
		}
		defer ex.Close()
		wsRoot = ex.Workspace().Root()
	} else {
		local, err := workspace.NewLocal(dir, tool.DefaultEnv(settings.PassEnv, m.KeyEnv))
		if err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
		defer local.Close()
		ws = local
	}
	// What the session offers the model: dax's extensions as the
	// settings say, then the program's.
	exts, err := p.selected(choice{
		MaxReadBytes: settings.MaxReadBytes,
		Agents:       settings.Agents, SubagentModel: m.SubagentName,
		Skills: *skillsFlag, SkillsDirs: settings.SkillsDirs, TrustSkills: *trustSkills,
		MemoryDir: settings.MemoryDir,
	})
	if err != nil {
		return err
	}
	opts := agent.Options{
		Extensions: exts,
		Streamer:   m.Streamer, Model: m.Name, Think: settings.Think,
		Effort: openresponses.ReasoningEffort(settings.Effort), Dir: dir, Workspace: ws, Executor: ex, Root: *root, Sync: policyMode,
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
	for _, s := range settings.MCP {
		opts.MCP = append(opts.MCP, agent.MCPServer{Name: s.Name, Command: s.Command})
	}
	if *mcp != "" {
		opts.MCP = append(opts.MCP, agent.MCPServer{Name: "cli", Command: *mcp})
	}
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
	f, err := selectFront(*frontName, *once, frontInfo{
		Name: p.name, Renderers: renderers,
		Provider: settings.Provider, Model: modelNames(m, hasExtension(exts, agents.Name)), ModelInfo: modelLine, Dir: wsRoot, Executor: executorLine(ex), Think: settings.Think, Prompt: *once,
		Policy: policySummary(settings.Policy, exts), Cost: cost, Verbose: *verbose,
	}, processEnv(*root != ""))
	if err != nil {
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
	defer sess.Close()
	// The front is the session's human plane: it drives the session's
	// Turn and dax's controls, as a front over a wire would.
	return f.Run(ctx, sess)
}

// loadSettings reads the user's file and the project's, through the
// workspace the project is in, and folds them with the flags.
func loadSettings(project workspace.Workspace, userPath string, flags config.Flags) (config.Settings, error) {
	explicit := userPath != ""
	if !explicit {
		userPath = config.Path()
	}
	user, err := config.Load(userPath, false, explicit)
	if err != nil {
		return config.Settings{}, err
	}
	// With an executor the project is where it runs, not this
	// directory, and its .dax/config.json is not read yet.
	var proj config.Layer
	if strings.TrimSpace(config.ExecutorOf(user, flags)) == "" {
		if proj, err = config.LoadProject(project); err != nil {
			return config.Settings{}, err
		}
	}
	return config.Resolve([]config.Layer{user, proj}, flags, filepath.Join(agent.DefaultUserDir(), "memory"))
}

// executorLine is the banner's line about the executor, "" for none.
func executorLine(ex *agent.Executor) string {
	if ex == nil {
		return ""
	}
	return ex.String()
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
