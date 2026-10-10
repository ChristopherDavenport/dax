package dax

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/render"
)

// A line that starts with a slash is dax's, never the model's: every
// front takes it to command, the REPL from its own input and the
// terminal client through console.WithSubmit. A line that starts with
// two slashes goes to the model with one of them dropped, for a prompt
// that begins with a path.

// isCommand is whether line is for dax rather than the model.
func isCommand(line string) bool {
	return strings.HasPrefix(line, "/") && !strings.HasPrefix(line, "//")
}

// userText is what the model is sent for line: line, less the slash
// that escaped a leading one.
func userText(line string) string {
	if strings.HasPrefix(line, "//") {
		return line[1:]
	}
	return line
}

// commandHelp lists the commands, for /help and an unknown command.
const commandHelp = `commands:
  /model <name>             the model from the next request
  /think on|off             reasoning from the next request
  /tools                    the tools offered and what was left out
  /session                  the session's ID and where it is kept
  /mcp add <name> <command> start an MCP server; its tools from the next prompt
  /mcp remove <label>       stop an MCP server
  /follow <text>            a follow-up, taken when the model would stop
  /abort                    abort the run
  /quit                     leave
  //text                    send /text to the model`

// commandResult is what a command did: out is what it says, quit
// whether the front should end.
type commandResult struct {
	out  string
	quit bool
}

// command runs a slash line against the session. While a run goes, a
// command that changes how the session runs (the model, reasoning, MCP
// servers) is refused rather than steered into the run; the rest work
// either way. The output and the error are cleaned, since a tool's or a
// server's name, or a server's complaint, may carry what a terminal
// would act on.
func command(ctx context.Context, t agent.Turn, ctl agent.Controls, line string, running bool) (commandResult, error) {
	r, err := runCommand(ctx, t, ctl, line, running)
	r.out = render.Clean(r.out)
	if err != nil {
		err = errors.New(render.Clean(err.Error()))
	}
	return r, err
}

func runCommand(ctx context.Context, t agent.Turn, ctl agent.Controls, line string, running bool) (commandResult, error) {
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
		return commandResult{out: commandHelp}, nil
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
		if err := t.FollowUp(ctx, openresponses.UserText(arg)); err != nil {
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
	case "/mcp":
		if err := idle(); err != nil {
			return commandResult{}, err
		}
		return mcpCommand(ctx, ctl, arg)
	}
	return commandResult{}, fmt.Errorf("unknown command %s\n%s", name, commandHelp)
}

// mcpCommand is /mcp add and /mcp remove.
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
	return commandResult{}, errors.New("usage: /mcp add <name> <command line>, or /mcp remove <label>")
}
