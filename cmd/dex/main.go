// Command dex is a minimal coding agent: a REPL over an Open Responses
// model with read, write, edit and bash tools, recording every session.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dex/internal/agent"
	"github.com/ChristopherDavenport/dex/internal/render"
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
	base := flag.String("base", "http://localhost:11434/v1", "Open Responses base URL")
	key := flag.String("key", os.Getenv("DEX_API_KEY"), "API key, if the server needs one")
	model := flag.String("model", "qwen3.5:9b", "model name")
	think := flag.Bool("think", true, "request and show reasoning")
	once := flag.String("p", "", "run one prompt and exit")
	root := flag.String("sessions", agent.DefaultRoot(), "session store, a content-addressed store for every project; empty disables recording")
	resume := flag.String("resume", "", "continue the session with this ID")
	list := flag.Bool("list", false, "list recorded sessions for this directory and exit")
	verify := flag.String("verify", "", "verify the request hashes of the session with this ID and exit")
	project := flag.String("project", "", "write the session with this ID as a JSONL file into -out and exit")
	out := flag.String("out", ".", "directory -project writes into")
	importFile := flag.String("import", "", "read a JSONL session file an earlier dex wrote into the store and exit")
	repair := flag.String("repair", "", "rewrite the damaged log of the session with this ID from what still reads, and exit")
	gc := flag.String("gc", "", "pack the store's loose objects (pack) or repack and drop what no session needs (sweep), and exit")
	sync := flag.String("sync", "append", "when an append is durable: every append, on a response or output (response), or at exit (never)")
	compactAt := flag.Int("compact", 0, "fold the transcript through a local summary above this many estimated tokens; 0 disables")
	confirm := flag.Bool("confirm", false, "ask before write, edit and bash calls run")
	mcp := flag.String("mcp", "", "command line of a stdio MCP server whose tools are offered as mcp__<name>")
	agents := flag.Bool("agents", false, "offer the explore sub-agent as a tool")
	compactServer := flag.Bool("compact-server", false, "with -compact, use the server's compaction endpoint instead of a local summary")
	agentsMD := flag.Bool("agents-md", true, "put ~/.dex/AGENTS.md and the AGENTS.md files from / down to this directory in the instructions")
	skills := flag.Bool("skills", true, "offer the skills in .dex/skills and ~/.dex/skills through the skill tool")
	trustSkills := flag.Bool("trust-skills", false, "with -confirm, let a skill's allowed-tools run unasked until the next message")
	memory := flag.String("memory", filepath.Join(agent.DefaultUserDir(), "memory"), "memory store directory, with a user scope and one for this directory; empty disables memory")
	flag.Parse()

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
		n, failed, err := agent.Verify(ctx, *root, *verify)
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
			causes, err := agent.Unhashed(ctx, *root, *verify)
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

	policy, ok := map[string]cas.SyncPolicy{"append": cas.SyncEveryAppend, "response": cas.SyncOnResponse, "never": cas.SyncNever}[*sync]
	if !ok {
		return fmt.Errorf("-sync %q: want append, response or never", *sync)
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	opts := agent.Options{
		BaseURL: *base, APIKey: *key, Model: *model, Think: *think, Dir: dir, Root: *root, Sync: policy,
		UserDir:       agent.DefaultUserDir(),
		AgentsMD:      *agentsMD,
		Skills:        *skills,
		TrustSkills:   *trustSkills,
		MemoryDir:     *memory,
		MCP:           *mcp,
		Compact:       *compactAt,
		CompactServer: *compactServer,
		Confirm:       *confirm,
		Agents:        *agents,
		Log:           func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}

	if *once != "" {
		// One prompt: approvals read stdin directly.
		opts.Approve = func(c *openresponses.FunctionCall, reason string) bool {
			fmt.Print(question(c, reason))
			if !in.Scan() {
				fmt.Println()
				return false
			}
			return yes(in.Text())
		}
		opts.Elicit = elicitor(func(q string) bool {
			fmt.Print(q)
			return in.Scan() && yes(in.Text())
		})
		sess, err := openSession(ctx, opts, *resume)
		if err != nil {
			return err
		}
		defer sess.Close()
		sess.Agent.Subscribe((&render.Printer{W: os.Stdout, Think: *think}).Handle)
		abortOnInterrupt(sess)
		showAssembly(sess)
		showPending(sess, "resumed")
		return turn(ctx, sess, *once)
	}

	// REPL: one goroutine reads stdin so a line typed during a run can
	// steer it, follow it up, or answer an approval.
	asks := make(chan *ask)
	opts.Approve = func(c *openresponses.FunctionCall, reason string) bool {
		a := &ask{q: question(c, reason), reply: make(chan bool, 1)}
		asks <- a
		return <-a.reply
	}
	opts.Elicit = elicitor(func(q string) bool {
		a := &ask{q: q, reply: make(chan bool, 1)}
		asks <- a
		return <-a.reply
	})
	sess, err := openSession(ctx, opts, *resume)
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Agent.Subscribe((&render.Printer{W: os.Stdout, Think: *think}).Handle)
	abortOnInterrupt(sess)

	fmt.Printf("dex · %s · %s\n", *model, dir)
	if id := sess.ID(); id != "" {
		fmt.Printf("session %s\n", id)
	}
	showAssembly(sess)
	showPending(sess, "resumed")
	lines := make(chan string)
	go func() {
		defer close(lines)
		for in.Scan() {
			lines <- in.Text()
		}
	}()
	done := make(chan error, 1)
	running := false
	var cur *ask
	prompt := func() {
		if !running {
			fmt.Print("\n> ")
		}
	}
	prompt()
	for {
		select {
		case a := <-asks:
			cur = a
			fmt.Print(a.q)
		case err := <-done:
			running = false
			if err != nil {
				fmt.Fprintln(os.Stderr, "\nerror:", err)
				if h := hint(err); h != "" {
					fmt.Fprintln(os.Stderr, h)
				}
			}
			prompt()
		case line, ok := <-lines:
			if !ok {
				if running {
					sess.Agent.Abort()
					<-done
				}
				fmt.Println()
				return nil
			}
			line = strings.TrimSpace(line)
			if cur != nil {
				cur.reply <- yes(line)
				cur = nil
				continue
			}
			if line == "" {
				prompt()
				continue
			}
			if running {
				switch {
				case strings.HasPrefix(line, "/follow "):
					sess.FollowUp(strings.TrimPrefix(line, "/follow "))
					fmt.Println("[queued as follow-up]")
				case line == "/abort":
					sess.Agent.Abort()
				default:
					sess.Steer(line)
					fmt.Println("[queued as steering]")
				}
				continue
			}
			if handled, err := command(ctx, sess, line); handled {
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				}
				if line == "/quit" || line == "/exit" {
					return nil
				}
				prompt()
				continue
			}
			running = true
			go func(text string) { done <- turn(ctx, sess, text) }(line)
		}
	}
}

type ask struct {
	q     string
	reply chan bool
}

// question is what the user is asked about a call the policy asked
// about.
func question(c *openresponses.FunctionCall, reason string) string {
	if reason != "" {
		reason = " (" + reason + ")"
	}
	return fmt.Sprintf("? allow %s %s%s [y/N] ", c.Name, c.Arguments, reason)
}

// elicitor puts a tool's mid-call question to the user as a yes or no.
// dex has no form to fill in, so a question asking for one is
// cancelled, as is one answered out of band at a URL, which is shown.
// Questions asked together are put one at a time.
func elicitor(confirm func(q string) bool) agenttool.Elicitor {
	var mu sync.Mutex
	return func(_ context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
		mu.Lock()
		defer mu.Unlock()
		if q.URL != "" {
			fmt.Printf("[a tool asks you to visit %s: %s]\n", q.URL, q.Message)
			return agenttool.Answer{Action: agenttool.ActionCancel}, nil
		}
		if hasFields(q.Schema) {
			fmt.Printf("[a tool asks for a form dex cannot show: %s]\n", q.Message)
			return agenttool.Answer{Action: agenttool.ActionCancel}, nil
		}
		if confirm("? " + q.Message + " [y/N] ") {
			return agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{}`)}, nil
		}
		return agenttool.Answer{Action: agenttool.ActionDecline}, nil
	}
}

// hasFields reports whether a form's schema asks for any property.
func hasFields(schema json.RawMessage) bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	return len(schema) > 0 && json.Unmarshal(schema, &s) == nil && len(s.Properties) > 0
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

func yes(s string) bool {
	a := strings.ToLower(strings.TrimSpace(s))
	return a == "y" || a == "yes"
}

func openSession(ctx context.Context, opts agent.Options, resume string) (*agent.Session, error) {
	if resume != "" {
		return agent.Resume(ctx, opts, resume)
	}
	return agent.New(ctx, opts)
}

// abortOnInterrupt makes Ctrl-C abort the run in flight; a second one,
// or one while idle, exits.
func abortOnInterrupt(sess *agent.Session) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range sigs {
			if !sess.Agent.State().Running {
				fmt.Println()
				sess.Close()
				os.Exit(130)
			}
			sess.Agent.Abort()
		}
	}()
}

// command handles a slash command while idle; handled is false for a
// prompt.
func command(ctx context.Context, sess *agent.Session, line string) (handled bool, err error) {
	switch {
	case strings.HasPrefix(line, "/mcp add "):
		// /mcp add <prefix> <command line>
		prefix, cmd, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "/mcp add ")), " ")
		if !ok {
			return true, errors.New("usage: /mcp add <prefix> <command line>")
		}
		label, err := sess.AddMCP(ctx, prefix, strings.TrimSpace(cmd))
		if err == nil {
			fmt.Printf("[%s added; its tools are offered from the next prompt]\n", label)
			showAssembly(sess)
		}
		return true, err
	case strings.HasPrefix(line, "/mcp remove "):
		err := sess.RemoveMCP(strings.TrimSpace(strings.TrimPrefix(line, "/mcp remove ")))
		if err == nil {
			showAssembly(sess)
		}
		return true, err
	case line == "/tools":
		showAssembly(sess)
		return true, nil
	case line == "/quit", line == "/exit":
		return true, nil
	case line == "/session":
		fmt.Println(sess.ID(), sess.Path())
		return true, nil
	case strings.HasPrefix(line, "/model "):
		return true, sess.SetModel(strings.TrimSpace(strings.TrimPrefix(line, "/model ")))
	case line == "/think on":
		return true, sess.SetThink(true)
	case line == "/think off":
		return true, sess.SetThink(false)
	case strings.HasPrefix(line, "/follow "):
		sess.FollowUp(strings.TrimPrefix(line, "/follow "))
		fmt.Println("[queued as follow-up for the next run]")
		return true, nil
	}
	return false, nil
}

func turn(ctx context.Context, sess *agent.Session, text string) error {
	end, err := sess.Prompt(ctx, text)
	if err != nil {
		return err
	}
	switch end.Reason {
	case agentturn.ReasonAborted:
		showPending(sess, "aborted")
	case agentturn.ReasonInputRequired:
		for _, p := range end.Pending {
			fmt.Printf("[input required: %s %s]\n", p.Call.Name, p.Call.Arguments)
		}
	case agentturn.ReasonStopped:
		fmt.Printf("[stopped: %s]\n", end.Cause)
	}
	if h := hint(end.Err); end.Err != nil && h != "" {
		fmt.Printf("[%s]\n", h)
	}
	return nil
}

// showAssembly prints the tools the kit assembled, with the source of
// each one that is not dex's own, and what the instruction layers left
// out, so the user knows what the model was not given.
func showAssembly(sess *agent.Session) {
	var names []string
	for _, t := range sess.Tools() {
		if t.Source == "WithTools" {
			names = append(names, t.Name)
		} else {
			names = append(names, t.Name+" ["+t.Source+"]")
		}
	}
	fmt.Printf("tools: %s\n", strings.Join(names, ", "))
	for _, o := range sess.Omitted() {
		fmt.Printf("omitted: %s\n", o)
	}
}

// showPending tells the user which calls are unanswered and why, since
// the next prompt answers them in those terms.
func showPending(sess *agent.Session, why string) {
	pending := sess.Pending()
	if len(pending) == 0 {
		return
	}
	fmt.Printf("[%s; %d tool call(s) unanswered, answered on the next prompt]\n", why, len(pending))
	for _, p := range pending {
		fmt.Printf("  ⏸ %s\n", agent.Describe(p))
	}
}
