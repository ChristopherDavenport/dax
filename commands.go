package dax

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/skills"
	"github.com/ChristopherDavenport/dax/internal/render"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// A line that starts with a slash is dax's, never the model's: every
// front takes it to command, the REPL from its own input and the
// terminal client through console.WithSubmit. A line that starts with
// two slashes goes to the model with one of them dropped, for a prompt
// that begins with a path.

// isCommand is whether line is for dax rather than the model: a slash
// command, or a command of the user's own to run (!cmd).
func isCommand(line string) bool {
	return strings.HasPrefix(line, "/") && !strings.HasPrefix(line, "//") ||
		strings.HasPrefix(line, "!") && !strings.HasPrefix(line, "!!")
}

// userText is what the model is sent for line: line, less the slash or
// the ! that escaped a leading one.
func userText(line string) string {
	if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "!!") {
		return line[1:]
	}
	return line
}

// userCommander is a session that runs a command of the user's own: the
// session in this process (agent.Session), never one over a wire.
type userCommander interface {
	RunUserCommand(ctx context.Context, line string) (agent.UserCommand, error)
}

// commandHelp lists the commands, for /help and an unknown command.
const commandHelp = `commands:
  /model <name>             the model from the next request
  /think on|off             reasoning from the next request
  /tools                    the tools offered and what was left out
  /session                  the session's ID and where it is kept
  /new                      a new session, keeping the model, reasoning, mode and /mcp add servers (also /clear)
  /name <text>              name this session; -list and /resume show it
  /resume [n|ID|name]       this project's sessions, newest first; with one, continue it here
  /mcp                      the MCP servers, where each runs and what it is for
  /mcp add <name> <command> start an MCP server; its tools from the next prompt
  /mcp remove <label>       stop an MCP server
  /mode [name]              the permission mode, or switch to default, acceptEdits, dontAsk or bypassPermissions
  /permissions              the rules in force, where each came from, the mode and the grants
  /skills approve <name>    let a skill the model wrote grant its allowed-tools under -trust-skills
  /compact [instructions]   fold the conversation at the next request, keeping what you say
  /port <port>              reach a port where the tools act from this machine's loopback
  /follow <text>            a follow-up, taken when the model would stop
  /abort                    abort the run
  /quit                     leave
  !<command>                run a command yourself; its output goes with the next prompt
  //text, !!text            send /text, !text to the model`

// commandResult is what a command did: out is what it says, quit
// whether the front should end.
type commandResult struct {
	out  string
	quit bool
	// send is what the front sends as a prompt, or a steer while a run
	// goes: a prompt template's message or an MCP prompt's, marked with
	// where it came from.
	send []openresponses.Item
	// open is the session the front moves to (/new): it closes the one
	// it was on.
	open *agent.Session
}

// command runs a slash line against the session. While a run goes, a
// command that changes how the session runs (the model, reasoning, MCP
// servers) is refused rather than steered into the run; the rest work
// either way. The output and the error are cleaned, since a tool's or a
// server's name, or a server's complaint, may carry what a terminal
// would act on.
func command(ctx context.Context, t agentturn.Control, ctl agent.Controls, line string, running bool) (commandResult, error) {
	r, err := runCommand(ctx, t, ctl, line, running)
	r.out = render.Clean(r.out)
	if err != nil {
		err = errors.New(render.Clean(err.Error()))
	}
	return r, err
}

func runCommand(ctx context.Context, t agentturn.Control, ctl agent.Controls, line string, running bool) (commandResult, error) {
	if cmd, ok := strings.CutPrefix(line, "!"); ok {
		return userCommand(ctx, ctl, cmd)
	}
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	idle := func() error {
		if running {
			return fmt.Errorf("%s waits for the run to end", name)
		}
		return nil
	}
	switch name {
	case "/help":
		return commandResult{out: commandHelp + templateHelp(ctx, ctl)}, nil
	case "/quit", "/exit":
		return commandResult{quit: true}, nil
	case "/abort":
		if !running {
			return commandResult{}, errors.New("no run to abort")
		}
		t.Abort()
		return commandResult{}, nil
	case "/follow":
		if arg == "" {
			return commandResult{}, errors.New("usage: /follow <text>")
		}
		if err := t.Queue(ctx, agentturn.QueueFollowUp, openresponses.UserText(arg)); err != nil {
			return commandResult{}, err
		}
		if running {
			return commandResult{out: "[queued as follow-up]"}, nil
		}
		return commandResult{out: "[queued as follow-up for the next run]"}, nil
	case "/tools":
		return commandResult{out: strings.Join(assemblyLines(ctl.Info()), "\n")}, nil
	case "/session":
		info := ctl.Info()
		return commandResult{out: strings.TrimSpace(info.ID + " " + info.Path)}, nil
	case "/new", "/clear":
		if arg != "" {
			return commandResult{}, fmt.Errorf("usage: %s, with nothing after it", name)
		}
		if err := idle(); err != nil {
			return commandResult{}, err
		}
		return reopen(ctx, ctl, "")
	case "/name":
		return nameCommand(ctx, ctl, arg, running)
	case "/resume":
		return resumeCommand(ctx, ctl, arg, running)
	case "/model":
		if arg == "" {
			return commandResult{}, errors.New("usage: /model <name>")
		}
		if err := idle(); err != nil {
			return commandResult{}, err
		}
		if err := ctl.SetModel(arg); err != nil {
			return commandResult{}, err
		}
		return commandResult{out: "[model " + arg + " from the next request]"}, nil
	case "/think":
		if arg != "on" && arg != "off" {
			return commandResult{}, errors.New("usage: /think on|off")
		}
		if err := idle(); err != nil {
			return commandResult{}, err
		}
		if err := ctl.SetThink(arg == "on"); err != nil {
			return commandResult{}, err
		}
		return commandResult{out: "[reasoning " + arg + " from the next request]"}, nil
	case "/permissions":
		// The listing changes nothing, so it works during a run.
		if arg != "" {
			return commandResult{}, errors.New("usage: /permissions, with nothing after it")
		}
		return commandResult{out: strings.Join(permissionLines(ctl.Info()), "\n")}, nil
	case "/mode":
		// A switch during a run waits for its next turn, so it works
		// during one.
		return modeCommand(ctx, ctl, arg)
	case "/skills":
		// An approval changes what the next read of the skill grants,
		// not the run, so it works during one.
		return skillsCommand(ctl, arg)
	case "/port":
		// A forward changes nothing the run does, so it works during one.
		port, err := strconv.Atoi(arg)
		if err != nil || port < 1 || port > 65535 {
			return commandResult{}, errors.New("usage: /port <port>, a port on the workspace's loopback (1 to 65535)")
		}
		f, ok := ctl.(forwarder)
		if !ok {
			return commandResult{}, errors.New("/port forwards to the session's own machine; it is not offered over dax attach")
		}
		url, err := f.Forward(ctx, port)
		if err != nil {
			return commandResult{}, err
		}
		return commandResult{out: fmt.Sprintf("port %d: %s", port, url)}, nil
	case "/compact":
		if err := idle(); err != nil {
			return commandResult{}, err
		}
		c, ok := ctl.(compacter)
		if !ok {
			return commandResult{}, errors.New("/compact is not offered by this session")
		}
		if err := c.Compact(ctx, arg); err != nil {
			return commandResult{}, err
		}
		if arg != "" {
			return commandResult{out: "[the next request folds the conversation, with your instructions]"}, nil
		}
		return commandResult{out: "[the next request folds the conversation]"}, nil
	case "/mcp":
		// The listing changes nothing, so it works during a run.
		if sub, rest, _ := strings.Cut(arg, " "); sub == "" || sub == "list" {
			if strings.TrimSpace(rest) != "" {
				return commandResult{}, errors.New("usage: /mcp list, with nothing after it")
			}
			return commandResult{out: strings.Join(mcpLines(ctl.Info()), "\n")}, nil
		}
		if err := idle(); err != nil {
			return commandResult{}, err
		}
		return mcpCommand(ctx, ctl, arg)
	}
	if tp, ok := ctl.(templater); ok && name != "/" {
		if slices.ContainsFunc(tp.Templates(), func(p agent.PromptTemplate) bool { return "/"+p.Name == name }) {
			item, err := tp.Template(strings.TrimPrefix(name, "/"), arg)
			if err != nil {
				return commandResult{}, err
			}
			return commandResult{send: []openresponses.Item{item}}, nil
		}
	}
	if mp, ok := ctl.(mcpPrompter); ok && strings.HasPrefix(name, "/mcp__") {
		items, err := mp.MCPPrompt(ctx, strings.TrimPrefix(name, "/"), arg)
		if err != nil {
			return commandResult{}, err
		}
		return commandResult{send: items}, nil
	}
	return commandResult{}, fmt.Errorf("unknown command %s\n%s%s", name, commandHelp, templateHelp(ctx, ctl))
}

// reopener is what /new asks of the session's controls: the session's
// own (agent.Session.Reopen). Over dax attach every view acts on the
// one conversation the host serves, so the attached controls offer
// none: a view cannot move the host to another session.
type reopener interface {
	Reopen(ctx context.Context, id string) (*agent.Session, error)
}

// namer is what /name asks of the session's controls: the session's
// own (agent.Session); dax attach offers none.
type namer interface {
	SetName(ctx context.Context, name string) error
	Name() string
}

// nameCommand is /name: with no text the session's name, with text the
// session named so.
func nameCommand(ctx context.Context, ctl agent.Controls, arg string, running bool) (commandResult, error) {
	n, ok := ctl.(namer)
	if !ok {
		return commandResult{}, errors.New("/name is not offered over dax attach")
	}
	if arg == "" {
		if name := n.Name(); name != "" {
			return commandResult{out: "session " + strconv.Quote(name)}, nil
		}
		return commandResult{}, errors.New("usage: /name <text>; this session has no name")
	}
	if running {
		return commandResult{}, errors.New("/name waits for the run to end")
	}
	if err := n.SetName(ctx, arg); err != nil {
		return commandResult{}, err
	}
	return commandResult{out: "[session named " + strconv.Quote(strings.TrimSpace(arg)) + "]"}, nil
}

// lister is what /resume asks of the session's controls to list the
// project's sessions: the session's own (agent.Session).
type lister interface {
	Sessions(ctx context.Context) ([]agentsession.Summary, error)
}

// maxListed is how many sessions /resume lists.
const maxListed = 20

// resumeCommand is /resume: with nothing, this project's sessions,
// newest first and numbered, the current one marked; with a number from
// that list, an ID, the start of one, or a name (the whole name, or a
// part of only one), the front moves to that session over the live
// settings (agent.Session.Reopen).
func resumeCommand(ctx context.Context, ctl agent.Controls, arg string, running bool) (commandResult, error) {
	l, ok := ctl.(lister)
	if !ok {
		return commandResult{}, errors.New("a served session stays the one conversation every attached view acts on: /new and /resume are not offered over dax attach")
	}
	sums, err := l.Sessions(ctx)
	if err != nil {
		return commandResult{}, err
	}
	current := ctl.Info().ID
	if arg == "" {
		if len(sums) == 0 {
			return commandResult{out: "no sessions in this project"}, nil
		}
		var b strings.Builder
		for i, s := range sums {
			if i == maxListed {
				fmt.Fprintf(&b, "\n  (%d more: /resume ID)", len(sums)-i)
				break
			}
			mark := " "
			if s.Header.ID == current {
				mark = "*"
			}
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "%s%3d  %s  %s", mark, i+1, s.Header.CreatedAt.Local().Format("2006-01-02 15:04"), s.Header.ID)
			if s.Name != "" {
				b.WriteString("  " + strconv.Quote(oneLine(s.Name, maxField)))
			}
		}
		return commandResult{out: b.String()}, nil
	}
	if running {
		return commandResult{}, errors.New("/resume waits for the run to end")
	}
	id, err := pickSession(sums, arg)
	if err != nil {
		return commandResult{}, err
	}
	if id == current {
		return commandResult{}, errors.New("that is this session")
	}
	return reopen(ctx, ctl, id)
}

// pickSession is the ID of the session ref names among sums: its number
// in /resume's list, its ID or a start of one, its name, or a part of
// one name alone, case aside.
func pickSession(sums []agentsession.Summary, ref string) (string, error) {
	if n, err := strconv.Atoi(ref); err == nil {
		if n < 1 || n > len(sums) || n > maxListed {
			return "", fmt.Errorf("no session %d in the list; /resume lists them", n)
		}
		return sums[n-1].Header.ID, nil
	}
	one := func(match func(agentsession.Summary) bool, what string) (string, bool, error) {
		var found []agentsession.Summary
		for _, s := range sums {
			if match(s) {
				found = append(found, s)
			}
		}
		switch len(found) {
		case 0:
			return "", false, nil
		case 1:
			return found[0].Header.ID, true, nil
		}
		var ids []string
		for _, s := range found {
			ids = append(ids, s.Header.ID)
		}
		return "", true, fmt.Errorf("%q is %s of %d sessions: %s", ref, what, len(found), strings.Join(ids, ", "))
	}
	lower := strings.ToLower(ref)
	for _, m := range []struct {
		match func(agentsession.Summary) bool
		what  string
	}{
		{func(s agentsession.Summary) bool { return s.Header.ID == ref }, "the ID"},
		{func(s agentsession.Summary) bool { return strings.EqualFold(s.Name, ref) }, "the name"},
		{func(s agentsession.Summary) bool { return len(ref) >= 4 && strings.HasPrefix(s.Header.ID, ref) }, "the start of the ID"},
		{func(s agentsession.Summary) bool {
			return s.Name != "" && strings.Contains(strings.ToLower(s.Name), lower)
		}, "part of the name"},
	} {
		if id, ok, err := one(m.match, m.what); ok || err != nil {
			return id, err
		}
	}
	return "", fmt.Errorf("no session in this project is %q; /resume lists them", ref)
}

// reopen is the session the front moves to: a new one for id "", or
// the recorded session id, over the live settings.
func reopen(ctx context.Context, ctl agent.Controls, id string) (commandResult, error) {
	r, ok := ctl.(reopener)
	if !ok {
		return commandResult{}, errors.New("a served session stays the one conversation every attached view acts on: /new and /resume are not offered over dax attach")
	}
	n, err := r.Reopen(ctx, id)
	if err != nil {
		return commandResult{}, err
	}
	return commandResult{open: n}, nil
}

// mcpPrompter is what an MCP prompt's command asks of the session's
// controls: the session's own (agent.Session.MCPPrompts and
// MCPPrompt), which hold its servers; dax attach offers none.
type mcpPrompter interface {
	MCPPrompts(ctx context.Context) []agent.MCPPrompt
	MCPPrompt(ctx context.Context, command, args string) (openresponses.Items, error)
}

// imager is what an @path.png in a typed line asks of the session's
// controls: the session's own (agent.Session.Image), which reads it on
// this machine; dax attach offers none, and the line goes as typed.
type imager interface {
	Image(ctx context.Context, path string) (*openresponses.InputImage, error)
}

// mentioner is what an @ mention asks of the session's controls: the
// session's own (agent.Session.MCPMentions and MCPResources), which hold
// its servers; dax attach offers none, and its lines go as typed.
type mentioner interface {
	MCPMentions(ctx context.Context, text string) (openresponses.Items, error)
	MCPResources(ctx context.Context) []agent.MCPResource
}

// mentionNamer says whether a word is a mention of one of the session's
// MCP servers' resources (agent.Session.IsMCPMention).
type mentionNamer interface {
	IsMCPMention(word string) bool
}

// typedItems is what a line the person typed sends: the line, their
// words, and after it each MCP resource it mentions (@server:uri),
// attached once, marked as the server's text.
func typedItems(ctx context.Context, ctl agent.Controls, text string) ([]openresponses.Item, error) {
	msg := openresponses.UserText(text)
	// The images it names (@path.png, the workspace's files), each once,
	// attached to the person's message: theirs, as the line is.
	// A word naming a server's resource (@server:x.png) is the server's.
	if im, ok := ctl.(imager); ok {
		mn, _ := ctl.(mentionNamer)
		seen := map[string]bool{}
		for _, word := range strings.Fields(text) {
			p, ok := strings.CutPrefix(strings.TrimRight(word, ".,;!?"), "@")
			if !ok || seen[p] || tool.ImageType(p, nil) == "" || mn != nil && mn.IsMCPMention("@"+p) {
				continue
			}
			seen[p] = true
			img, err := im.Image(ctx, p)
			if err != nil {
				return nil, errors.New(render.Clean("@" + p + ": " + err.Error()))
			}
			msg.Content = append(msg.Content, img)
		}
	}
	items := []openresponses.Item{msg}
	m, ok := ctl.(mentioner)
	if !ok {
		return items, nil
	}
	more, err := m.MCPMentions(ctx, text)
	if err != nil {
		return nil, errors.New(render.Clean(err.Error()))
	}
	return append(items, more...), nil
}

// templater is what a prompt template asks of the session's controls:
// the session's own (agent.Session.Templates and Template), which read
// the templates where the session runs; dax attach offers none.
type templater interface {
	Templates() []agent.PromptTemplate
	Template(name, args string) (openresponses.Item, error)
}

// templateHelp lists the session's prompt templates for /help, the
// project's marked, since its text is the repository's.
func templateHelp(ctx context.Context, ctl agent.Controls) string {
	var b strings.Builder
	if tp, ok := ctl.(templater); ok && len(tp.Templates()) > 0 {
		b.WriteString("\nprompt templates ($ARGUMENTS, $1…$9 filled in):")
		for _, p := range tp.Templates() {
			fmt.Fprintf(&b, "\n  /%-24s %s", p.Name+" [args]", p.Path)
			if p.Project {
				b.WriteString(" (the project's: its text is the repository's)")
			}
		}
	}
	if mp, ok := ctl.(mcpPrompter); ok {
		if prompts := mp.MCPPrompts(ctx); len(prompts) > 0 {
			b.WriteString("\nMCP prompts (a server's text, sent as its own):")
			for _, p := range prompts {
				fmt.Fprintf(&b, "\n  /%s", p.Command)
				for _, a := range p.Arguments {
					b.WriteString(" <" + oneLine(a, maxField) + ">")
				}
				if p.Description != "" {
					b.WriteString("  " + oneLine(p.Description, maxDescription))
				}
			}
		}
	}
	return b.String()
}

// permissionLines are /permissions' lines: the mode, the default, each
// source with the file it came from and its rules, the box's
// confinement claim, and the grant sets in force with their scope.
// Every text is cleaned, since a rule or a path may hold what a
// terminal would act on.
func permissionLines(info agent.Info) []string {
	p := info.Policy
	if p == nil {
		return []string{"no policy: every call runs"}
	}
	lines := []string{modeLine(p.Mode, p.Pending), "default: " + p.Default}
	list := func(indent string, allow, ask, deny []string) {
		for _, l := range []struct {
			what  string
			rules []string
		}{{"deny", deny}, {"ask", ask}, {"allow", allow}} {
			for _, r := range l.rules {
				lines = append(lines, indent+l.what+" "+r)
			}
		}
	}
	for _, s := range p.Sources {
		var about []string
		if s.Path != "" {
			about = append(about, s.Path)
		}
		if !s.Trusted {
			about = append(about, "untrusted: no allow rules")
		}
		if len(s.Allow)+len(s.Ask)+len(s.Deny) == 0 {
			about = append(about, "no rules")
		}
		head := s.Name
		if len(about) > 0 {
			head += " (" + strings.Join(about, "; ") + ")"
		}
		lines = append(lines, head)
		list("  ", s.Allow, s.Ask, s.Deny)
	}
	if p.Confined != "" {
		lines = append(lines, p.Confined)
	}
	for _, g := range p.Grants {
		scope := "every conversation"
		if g.Scope != "" {
			scope = "scope " + g.Scope
		}
		lines = append(lines, "granted "+g.Source+" ("+scope+")")
		list("  ", g.Allow, g.Ask, g.Deny)
	}
	for i, l := range lines {
		lines[i] = render.Clean(l)
	}
	return lines
}

// moder is what /mode and Shift-Tab ask of the session's controls: the
// session's own (agent.Session) or a served one's command.
type moder interface {
	Mode() (mode, pending string)
	SetMode(ctx context.Context, name string) (pending bool, err error)
	NextMode(ctx context.Context) (string, error)
}

// modeCommand is /mode: with no name the mode in force, the one waiting
// for the run's next turn and the modes there are; with one, the
// switch.
func modeCommand(ctx context.Context, ctl agent.Controls, arg string) (commandResult, error) {
	m, ok := ctl.(moder)
	if !ok {
		return commandResult{}, errors.New("/mode is not offered by this session")
	}
	if arg == "" {
		return commandResult{out: modeLine(m.Mode()) + "\nmodes: " + strings.Join(policy.Modes, ", ")}, nil
	}
	pending, err := m.SetMode(ctx, arg)
	if err != nil {
		return commandResult{}, err
	}
	mode, wait := m.Mode()
	if pending {
		return commandResult{out: "[mode " + wait + " from the run's next turn]"}, nil
	}
	return commandResult{out: "[mode " + mode + "]"}, nil
}

// modeLine says the mode in force, and the one waiting for the next
// turn.
func modeLine(mode, pending string) string {
	if pending != "" {
		return "mode " + mode + " (" + pending + " from the run's next turn)"
	}
	return "mode " + mode
}

// forwarder is what /port asks of the session's controls: the
// session's own (agent.Session.Forward). Over dax attach the forward
// would listen on the serving machine, not the attaching one, so the
// attached controls offer none.
type forwarder interface {
	Forward(ctx context.Context, port int) (string, error)
}

// compacter is what /compact asks of the session's controls: the
// session's own (agent.Session.Compact) or a served one's command.
type compacter interface {
	Compact(ctx context.Context, instructions string) error
}

// mcpCommand is /mcp add and /mcp remove; runCommand answers /mcp and
// /mcp list.
func mcpCommand(ctx context.Context, ctl agent.Controls, arg string) (commandResult, error) {
	sub, rest, _ := strings.Cut(arg, " ")
	rest = strings.TrimSpace(rest)
	switch sub {
	case "add":
		// /mcp add <name> <command line>
		prefix, cmd, ok := strings.Cut(rest, " ")
		if !ok {
			return commandResult{}, errors.New("usage: /mcp add <name> <command line>")
		}
		label, err := ctl.AddMCP(ctx, prefix, strings.TrimSpace(cmd))
		if err != nil {
			return commandResult{}, err
		}
		lines := append([]string{"[" + label + " added; its tools are offered from the next prompt]"}, assemblyLines(ctl.Info())...)
		return commandResult{out: strings.Join(lines, "\n")}, nil
	case "remove":
		if rest == "" {
			return commandResult{}, errors.New("usage: /mcp remove <label>")
		}
		if err := ctl.RemoveMCP(rest); err != nil {
			return commandResult{}, err
		}
		return commandResult{out: strings.Join(assemblyLines(ctl.Info()), "\n")}, nil
	}
	return commandResult{}, errors.New("usage: /mcp, /mcp add <name> <command line>, or /mcp remove <label>")
}

// userCommand runs cmd, a "!" line, as the user's own: in the session's
// workspace and not through the policy, its output, line and exit
// status queued as the user's message for the next prompt, or as a
// follow-up while a run goes. Only the session's own front runs one:
// over dax attach a token holder is not the person at the host, and the
// policy is the only check on that path.
func userCommand(ctx context.Context, ctl agent.Controls, cmd string) (commandResult, error) {
	if strings.TrimSpace(cmd) == "" {
		return commandResult{}, errors.New("usage: !<command>, run in the workspace by you, its output sent with the next prompt")
	}
	r, ok := ctl.(userCommander)
	if !ok {
		return commandResult{}, errors.New("! runs a command on the session's own machine; it is not offered over dax attach")
	}
	c, err := r.RunUserCommand(ctx, cmd)
	if err != nil {
		return commandResult{}, err
	}
	out := strings.TrimRight(c.Output, "\n")
	switch {
	case c.TimedOut:
		out += "\n[killed; sent with the next prompt]"
	default:
		out += fmt.Sprintf("\n[exit %d; sent with the next prompt]", c.ExitCode)
	}
	if c.Cut > 0 {
		out += fmt.Sprintf(" [%d bytes cut]", c.Cut)
	}
	return commandResult{out: strings.TrimLeft(out, "\n")}, nil
}

// userDirer is a session whose user directory a command acts on: the
// session in this process (agent.Session), never one over a wire.
type userDirer interface{ UserDir() string }

// skillsCommand is /skills approve NAME: the person approves a skill
// the model wrote (dax-skills' skill_manage), as its frontmatter is now,
// so its allowed-tools grant under -trust-skills.
func skillsCommand(ctl agent.Controls, arg string) (commandResult, error) {
	sub, name, _ := strings.Cut(arg, " ")
	name = strings.TrimSpace(name)
	if sub != "approve" || name == "" || strings.Contains(name, " ") {
		return commandResult{}, errors.New("usage: /skills approve <name>, a skill the model wrote")
	}
	u, ok := ctl.(userDirer)
	if !ok || u.UserDir() == "" {
		return commandResult{}, errors.New("/skills approve acts on this machine's ~/.dax; it is not offered over dax attach or without a user directory")
	}
	a, err := skills.Approve(u.UserDir(), name)
	if err != nil {
		return commandResult{}, err
	}
	tools := a.AllowedTools
	if tools == "" {
		tools = "none"
	}
	return commandResult{out: fmt.Sprintf("[approved the skill %s as it is now (%s); allowed-tools: %s, granted under -trust-skills at its next read; a change to its frontmatter needs approving again]", name, a.Digest, tools)}, nil
}
