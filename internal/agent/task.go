package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentturn"
	childagent "github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dex/internal/prompt"
	"github.com/ChristopherDavenport/dex/internal/tool"
)

// taskPreamble is what a task sub-agent is told on top of dex's own
// prompt and the project's AGENTS.md. It is the same for a fresh task
// and a fork, so their instructions do not differ; what a fork has
// more of is in its messages.
const taskPreamble = "You are a sub-agent of dex. The main agent gave you one task, in the last message. Carry it out " +
	"with the tools, then end with a short report the main agent can act on: what you changed (files and what in " +
	"them), what you ran and its result, and anything left undone or uncertain. Other sub-agents may be working in " +
	"the same project at the same time; change only the files your task is about."

// forkDirective opens the last message of a fork, after the main
// agent's conversation.
const forkDirective = "Everything above is the main agent's conversation, up to the task call that started you; " +
	"use it as context, and do not continue it. Your task, and only it:"

// taskTurns bounds a task's run; a coding task takes more turns than
// an exploration.
const taskTurns = 40

// taskArgs are the task tool's arguments. Context and Model are enums
// of roles rather than model names, so the schema is the same whatever
// the configuration names, and a call cannot name a model the user did
// not configure.
type taskArgs struct {
	Input   string `json:"input" desc:"The task. For a fresh context, the complete brief: the goal, the files involved, the conventions to follow and how to check the result"`
	Context string `json:"context,omitempty" enum:"fresh,fork" desc:"fresh (default): the sub-agent sees only input. fork: it also sees this conversation so far; use it when the task depends on what was said or decided here and a brief would leave that out"`
	Model   string `json:"model,omitempty" enum:"subagent,main" desc:"subagent (default): the faster, cheaper sub-agent model. main: the model running this conversation, for a task that needs it"`
}

// task is the task sub-agent: a coding agent of its own, with the file
// tools and bash under the parent's policy, started by the main
// agent's model for a piece of work. Each call is a fresh run, recorded
// as a child session; several calls in one batch run at once.
//
// Session.taskCall decides each call from its arguments: the model,
// sub-agent or main, with the reasoning effort fitted to it, and the
// opening items, the brief alone or, for a fork, the main agent's
// conversation and then the brief. A fork's conversation goes in as the
// run's prompt, so the child's session records it and verifies; the
// main agent's reasoning items are left out, since another model's
// cannot be replayed and the same model's were sent to the main agent.
//
// The instructions are fixed in the configuration, so the child
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
		Description: "Start a sub-agent that carries out a coding task in this project: it reads, writes and edits " +
			"files and runs commands under the same policy, then reports what it did. By default it starts fresh, " +
			"seeing only input, and runs the sub-agent model, which suits a task you can brief completely. Several " +
			"task calls in one turn run in parallel; give parallel tasks separate files. Review what a task reports " +
			"before relying on it.",
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

// subagentCall is cfg for one sub-agent call on model, with the effort
// /think sets now fitted to it.
func (s *Session) subagentCall(ctx context.Context, cfg agentturn.Config, model string, think bool) agentturn.Config {
	o := s.opts
	o.Think = think
	cfg.ModelName = model
	cfg.Reasoning = o.reasoningFor(ctx, model)
	return cfg
}

// exploreCall is one explore call: the sub-agent model and the effort
// in force now, and the question.
func (s *Session) exploreCall(ctx context.Context, in childagent.Input, _ agentturn.Transcript, cfg agentturn.Config) (agentturn.Config, openresponses.Items, error) {
	think, _, sub := s.now()
	return s.subagentCall(ctx, cfg, sub, think), openresponses.Items{openresponses.UserText(in.Input)}, nil
}

// taskCall is one task call's configuration and opening items, read
// against what /think and /model have set by now.
func (s *Session) taskCall(ctx context.Context, a taskArgs, parent agentturn.Transcript, cfg agentturn.Config) (agentturn.Config, openresponses.Items, error) {
	if strings.TrimSpace(a.Input) == "" {
		return cfg, nil, errors.New("input is required: the task for the sub-agent")
	}
	think, main, sub := s.now()
	switch a.Model {
	case "", "subagent":
		cfg = s.subagentCall(ctx, cfg, sub, think)
	case "main":
		cfg = s.subagentCall(ctx, cfg, main, think)
	default:
		return cfg, nil, fmt.Errorf("model %q: want subagent or main", a.Model)
	}
	switch a.Context {
	case "", "fresh":
		return cfg, openresponses.Items{openresponses.UserText(a.Input)}, nil
	case "fork":
		var items openresponses.Items
		for _, it := range parent {
			if _, ok := it.(*openresponses.ReasoningItem); ok {
				continue
			}
			items = append(items, it)
		}
		return cfg, append(items, openresponses.UserText(forkDirective+"\n\n"+a.Input)), nil
	default:
		return cfg, nil, fmt.Errorf("context %q: want fresh or fork", a.Context)
	}
}
