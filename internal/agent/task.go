package agent

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/ChristopherDavenport/agentpolicy"
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
// Its instructions are fixed here, in the configuration, so the child
// session records what the child is told: the preamble, dex's prompt
// with the user's instructions, and agentsText, the AGENTS.md chain as
// the kit renders it for the main agent. Skills and memory are left
// out: the sub-agent has neither tool.
func (o Options) task(ctx context.Context, model openresponses.Streamer, ws *tool.Workspace, env []string, eng *atomic.Pointer[agentpolicy.Engine], agentsText string) agentturn.Config {
	parts := []string{taskPreamble, prompt.Build(o.Dir, o.Instructions)}
	if agentsText != "" {
		parts = append(parts, agentsText)
	}
	cfg := agentturn.Config{
		Name: "task",
		Description: "Start a sub-agent that carries out a self-contained coding task in this project: it reads, " +
			"writes and edits files and runs commands under the same policy, then reports what it did. It does not " +
			"see this conversation, so give it everything it needs: the goal, the files involved, the conventions to " +
			"follow and how to check the result. Several task calls in one turn run in parallel; give parallel tasks " +
			"separate files. Review what a task reports before relying on it.",
		Model:        model,
		ModelName:    o.subagentModel(),
		Instructions: strings.Join(parts, "\n\n"),
		Tools:        tool.Builtins(ws, o.MaxReadBytes, tool.WithEnv(env)),
		Reasoning:    o.reasoningFor(ctx, o.subagentModel()),
		MaxTurns:     taskTurns,
		Retry:        agentturn.Retry{MaxAttempts: 3},
	}
	if o.Policy != nil {
		cfg.BeforeToolCall = o.childPolicy("task", eng, &tool.Analyzer{Dir: o.Dir, MaxFile: o.MaxReadBytes})
	}
	return cfg
}
