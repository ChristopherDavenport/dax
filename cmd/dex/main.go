// Command dex is a coding agent: a REPL, or one prompt with -p, over
// an Open Responses model, with read, write, edit, glob, grep, ls and
// bash tools under a policy, recording every session.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dex/internal/agent"
	"github.com/ChristopherDavenport/dex/internal/config"
	"github.com/ChristopherDavenport/dex/internal/modelinfo"
	"github.com/ChristopherDavenport/dex/internal/policy"
	"github.com/ChristopherDavenport/dex/internal/provider"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dex:", err)
		if h := hint(err); h != "" {
			fmt.Fprintln(os.Stderr, "dex:", h)
		}
		os.Exit(1)
	}
}

// hint says what to do about a store error that names no remedy the
// user can act on from dex.
func hint(err error) string {
	var damage cas.LogDamage
	switch {
	case errors.Is(err, agentsession.ErrSessionLocked):
		return "close the other dex holding it"
	case errors.Is(err, cas.ErrMigrationBusy):
		return "the store must move to per-session logs, and an older dex holds a session; stop every older dex, then start this one again"
	case errors.Is(err, cas.ErrLayout):
		return "a newer dex wrote this store; use that one"
	case errors.Is(err, cas.ErrLegacyStore):
		return "the store predates per-session logs; start a session or run -gc pack once to migrate it (older dex cannot read it after)"
	case errors.Is(err, cas.ErrStopped):
		return "a write to the store failed to reach the disk, so nothing more is written; restart dex and -resume the session"
	case errors.As(err, &damage):
		return "a session's log is damaged; -repair <id> keeps what still reads"
	}
	return ""
}

func run() error {
	fs := flag.NewFlagSet("dex", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: dex [flags]   (REPL)\n       dex -p \"prompt\" [flags]\n\nflags override ~/.config/dex/config.json and .dex/config.json; see the README.")
		fs.PrintDefaults()
	}
	prov := fs.String("provider", "", "model provider: ollama (default), openai, openrouter, openresponses, anthropic or gemini")
	model := fs.String("model", "", "model name; empty takes the provider's default")
	subModel := fs.String("subagent-model", "", "the sub-agents' model; empty takes the provider's default for them, else -model")
	base := fs.String("base-url", "", "endpoint of an Open Responses server (ollama and openresponses providers)")
	keyEnv := fs.String("api-key-env", "", "environment variable holding the openresponses provider's key")
	cfgPath := fs.String("config", "", "user config file; default ~/.config/dex/config.json")
	think := fs.Bool("think", true, "request and show reasoning")
	noPolicy := fs.Bool("no-policy", false, "run every tool call without asking; the config's policy is ignored")
	frontName := fs.String("front", "", "front end: tui or repl; default tui on a terminal, repl otherwise")
	once := fs.String("p", "", "run one prompt and exit")
	root := fs.String("sessions", agent.DefaultRoot(), "session store, a content-addressed store for every project; empty disables recording")
	resume := fs.String("resume", "", "continue the session with this ID")
	list := fs.Bool("list", false, "list recorded sessions for this directory and exit")
	verify := fs.String("verify", "", "verify the request hashes of the session with this ID and exit")
	project := fs.String("project", "", "write the session with this ID as a JSONL file into -out and exit")
	out := fs.String("out", ".", "directory -project writes into")
	importFile := fs.String("import", "", "read a JSONL session file an earlier dex wrote into the store and exit")
	repair := fs.String("repair", "", "rewrite the damaged log of the session with this ID from what still reads, and exit")
	gc := fs.String("gc", "", "pack the store's loose objects (pack) or repack and drop what no session needs (sweep), and exit")
	syncMode := fs.String("sync", "append", "when an append is durable: every append, on a response or output (response), or at exit (never)")
	compactAt := fs.Int("compact", 0, "fold the transcript through a local summary above this many estimated tokens; 0 disables")
	mcp := fs.String("mcp", "", "command line of one more stdio MCP server, offered as mcp__cli__<tool>")
	agents := fs.Bool("agents", false, "offer the explore sub-agent as a tool")
	compactServer := fs.Bool("compact-server", false, "with -compact, use the server's compaction endpoint instead of a local summary")
	agentsMD := fs.Bool("agents-md", true, "put ~/.dex/AGENTS.md and the AGENTS.md files from / down to this directory in the instructions")
	skills := fs.Bool("skills", true, "offer the skills in .dex/skills, ~/.dex/skills and the config's skills_dirs through the skill tool")
	trustSkills := fs.Bool("trust-skills", false, "let a skill's allowed-tools run unasked until the next message")
	memory := fs.String("memory", "", "memory store directory (default ~/.dex/memory, or the config's); off or empty disables memory")
	if err := fs.Parse(os.Args[1:]); err != nil {
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
	ctx := context.Background()
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
	flags.SubagentModel = str("subagent-model", subModel)
	if given["think"] {
		flags.Think = think
	}
	if given["memory"] {
		m := *memory
		if m == "off" {
			m = ""
		}
		flags.MemoryDir = &m
	}
	flags.NoPolicy = *noPolicy
	settings, err := loadSettings(dir, *cfgPath, flags)
	if err != nil {
		return err
	}

	m, err := provider.New(ctx, provider.Spec{Provider: settings.Provider, Model: settings.Model, SubagentModel: settings.SubagentModel, BaseURL: settings.BaseURL, KeyEnv: settings.APIKeyEnv})
	if err != nil {
		return err
	}
	opts := agent.Options{
		Streamer: m.Streamer, Model: m.Name, SubagentModel: m.SubagentName, Think: settings.Think,
		Dir: dir, Root: *root, Sync: policyMode,
		UserDir:       agent.DefaultUserDir(),
		AgentsMD:      *agentsMD,
		Skills:        *skills,
		SkillsDirs:    settings.SkillsDirs,
		PassEnv:       settings.PassEnv,
		KeyEnv:        m.KeyEnv,
		MaxReadBytes:  settings.MaxReadBytes,
		TrustSkills:   *trustSkills,
		MemoryDir:     settings.MemoryDir,
		Compact:       *compactAt,
		CompactServer: *compactServer,
		Agents:        *agents,
		Log:           func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}
	// Every request, the explorer's and the compaction summary's too,
	// has its reasoning effort fitted to what the vendor says the model
	// takes. The notice goes through opts.Log as the front leaves it.
	opts.Streamer = modelinfo.Wrap(m.Streamer, m.Describer, func(msg string) {
		if opts.Log != nil {
			opts.Log("%s", msg)
		}
	})
	modelLine := describeModel(ctx, opts.Streamer, m)
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
		p, err := policy.Build(settings.Policy)
		if err != nil {
			return err
		}
		opts.Policy = &p
	}

	// The front is chosen here: -p prints one answer, otherwise -front
	// names the interactive one. A new front implements the front
	// interface in front.go and gets a case in selectFront.
	f, err := selectFront(*frontName, *once, frontInfo{
		Provider: settings.Provider, Model: modelNames(m, *agents), ModelInfo: modelLine, Dir: dir, Think: settings.Think, Prompt: *once,
		Policy: policySummary(settings.Policy),
	}, processEnv(*root != ""))
	if err != nil {
		return err
	}
	opts.Approve, opts.Elicit = f.Hooks()
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
	// The opener is how a front drops the current context and starts a
	// fresh session with the same options. It never resumes: /clear means
	// from now, not from the leaf the session was opened at.
	f.setOpen(func() (*agent.Session, error) {
		return agent.New(ctx, opts)
	})
	defer func() {
		// A front that swaps the session in place closes the one it leaves
		// running; the others are closed here.
		if _, own := f.(closesOwnSessions); !own {
			sess.Close()
		}
	}()
	return f.Run(ctx, sess)
}

// loadSettings reads the user's and the project's files and folds
// them with the flags.
func loadSettings(dir, userPath string, flags config.Flags) (config.Settings, error) {
	explicit := userPath != ""
	if !explicit {
		userPath = config.Path()
	}
	user, err := config.Load(userPath, false, explicit)
	if err != nil {
		return config.Settings{}, err
	}
	proj, err := config.Load(config.ProjectPath(dir), true, false)
	if err != nil {
		return config.Settings{}, err
	}
	return config.Resolve([]config.Layer{user, proj}, flags, filepath.Join(agent.DefaultUserDir(), "memory"))
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

// policySummary says in a line what policy is in force.
func policySummary(p config.PolicySettings) string {
	if p.Off {
		return "off (-no-policy): every call runs"
	}
	var parts []string
	if p.Builtin {
		parts = append(parts, "built-in allow list and secret-path asks")
	} else {
		parts = append(parts, "no built-in allow list")
	}
	if n := len(p.User.Allow) + len(p.User.Ask) + len(p.User.Deny); n > 0 {
		parts = append(parts, fmt.Sprintf("%d rule(s) of yours", n))
	}
	if n := len(p.Project.Ask) + len(p.Project.Deny); n > 0 {
		parts = append(parts, fmt.Sprintf("%d rule(s) from the project", n))
	}
	return strings.Join(parts, ", ") + "; anything else: " + p.Fallback
}

// describeModel is the banner's line about the model: what its vendor
// says it takes, or why that could not be asked. It is empty for a
// vendor that publishes nothing. The answer is kept, so the first
// request does not ask again.
func describeModel(ctx context.Context, s openresponses.Streamer, m provider.Model) string {
	w := modelinfo.Of(s)
	if w == nil || m.Describer == nil {
		return ""
	}
	info, err := w.Describe(ctx, m.Name)
	if err != nil {
		return "unknown (" + err.Error() + "); reasoning is sent as asked"
	}
	return info.String()
}

// modelNames is the model for the banner, and the sub-agents' when they
// are offered and run another.
func modelNames(m provider.Model, agents bool) string {
	if agents && m.SubagentName != m.Name {
		return m.Name + " (explore: " + m.SubagentName + ")"
	}
	return m.Name
}
