package agent

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dex/internal/prompt"
	"github.com/ChristopherDavenport/dex/internal/tool"
)

// taskPreamble is what a task sub-agent is told on top of dex's own
// prompt and the project's AGENTS.md.
const taskPreamble = "You are a sub-agent of dex. The main agent gave you one task; you do not see its conversation, " +
	"so everything you know about the task is in the first message. Carry it out with the tools, then end with a short " +
	"report the main agent can act on: what you changed (files and what in them), what you ran and its result, and " +
	"anything left undone or uncertain. Other sub-agents may be working in the same project at the same time; " +
	"change only the files your task is about."

// taskTurns bounds a task's run; a coding task takes more turns than
// an exploration.
const taskTurns = 40

// task is the configuration of the task sub-agent: a coding agent of
// its own on the sub-agent model, with the file tools and bash under
// the parent's policy, started by the main agent's model for a piece
// of work it can describe completely. Each call is a fresh run on a
// fresh transcript, recorded as a child session; several calls in one
// batch run at once.
//
// Its instructions are the main agent's own parts, dex's prompt with
// the user's instructions and the AGENTS.md chain, read from the kit
// at each request, since the kit that assembles them is built after
// this configuration is fixed. Skills and memory are left out: the
// sub-agent has neither tool.
func (o Options) task(model openresponses.Streamer, ws *tool.Workspace, env []string, eng *atomic.Pointer[agentpolicy.Engine], kit *atomic.Pointer[agentkit.Kit]) agentturn.Config {
	fallback := prompt.Build(o.Dir, o.Instructions)
	cfg := agentturn.Config{
		Name: "task",
		Description: "Start a sub-agent that carries out a self-contained coding task in this project: it reads, " +
			"writes and edits files and runs commands under the same policy, then reports what it did. It does not " +
			"see this conversation, so give it everything it needs: the goal, the files involved, the conventions to " +
			"follow and how to check the result. Several task calls in one turn run in parallel; give parallel tasks " +
			"separate files. Review what a task reports before relying on it.",
		Model: &taskInstructions{Streamer: model, instructions: func() string {
			return taskPrompt(kit.Load(), fallback)
		}},
		ModelName:    o.subagentModel(),
		Instructions: taskPreamble + "\n\n" + fallback,
		Tools:        tool.Builtins(ws, o.MaxReadBytes, tool.WithEnv(env)),
		Reasoning:    o.reasoning(),
		MaxTurns:     taskTurns,
		Retry:        agentturn.Retry{MaxAttempts: 3},
	}
	if o.Policy != nil {
		cfg.BeforeToolCall = o.childPolicy("task", eng, &tool.Analyzer{Dir: o.Dir, MaxFile: o.MaxReadBytes})
	}
	return cfg
}

// taskPrompt is the preamble and the main agent's product and
// AGENTS.md parts, or the preamble and fallback before there is a kit.
func taskPrompt(k *agentkit.Kit, fallback string) string {
	parts := []string{taskPreamble}
	found := false
	if k != nil {
		for _, p := range k.Parts() {
			if p.ID == agentkit.PartProduct || p.ID == agentsmd.PartID {
				parts = append(parts, p.Text)
				found = found || p.ID == agentkit.PartProduct
			}
		}
	}
	if !found {
		parts = append(parts, fallback)
	}
	return strings.Join(parts, "\n\n")
}

// taskInstructions sets the instructions of every request it sends.
// It offers only CreateStream: the sub-agent's loop is given no
// compaction, so nothing asks it for Compact.
type taskInstructions struct {
	openresponses.Streamer
	instructions func() string
}

func (t *taskInstructions) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	req.Instructions = t.instructions()
	return t.Streamer.CreateStream(ctx, req, sink)
}
