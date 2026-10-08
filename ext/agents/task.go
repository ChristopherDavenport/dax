package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
)

// taskPreamble is what a task sub-agent is told on top of the main
// agent's prompt and the project's AGENTS.md, after "You are a
// sub-agent of <program>.". It is the same for a fresh task and a fork,
// so their instructions do not differ; what a fork has more of is in
// its messages.
const taskPreamble = "The main agent gave you one task, in the last message. Carry it out " +
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

// task is the task sub-agent: a coding agent of its own, with every
// extension's tools under the parent's policy, started by the main
// agent's model for a piece of work. Each call is a fresh run, recorded
// as a child session; several calls in one batch run at once.
//
// taskCall decides each call from its arguments: the model, sub-agent
// or main, with the reasoning effort fitted to it, and the opening
// items, the brief alone or, for a fork, the main agent's conversation
// and then the brief. A fork's conversation goes in as the run's
// prompt, so the child's session records it and verifies; the main
// agent's reasoning items are left out, since another model's cannot be
// replayed and the same model's were sent to the main agent.
//
// The instructions are fixed in the configuration, so the child
// session records what the child is told: the preamble, the main
// agent's prompt without dax-agents' delegation guide (the sub-agent
// has no sub-agents), and the AGENTS.md chain as the kit renders it for
// the main agent. Skills and memory are left out: their tools are
// added through the kit, and so are not among env.Tools.
func (o Options) task(ctx context.Context, env extension.Env) agentturn.Config {
	parts := []string{"You are a sub-agent of " + env.Name() + ". " + taskPreamble, env.SystemPrompt(Name)}
	if md := env.AgentsMD(); md != "" {
		parts = append(parts, md)
	}
	model, think := o.now(env)
	return agentturn.Config{
		Name: "task",
		Description: "Start a sub-agent that carries out a coding task in this project: it reads, writes and edits " +
			"files and runs commands under the same policy, then reports what it did. By default it starts fresh, " +
			"seeing only input, and runs the sub-agent model, which suits a task you can brief completely. Several " +
			"task calls in one turn run in parallel; give parallel tasks separate files. Review what a task reports " +
			"before relying on it.",
		Model:          env.Model(),
		ModelName:      model,
		Instructions:   strings.Join(parts, "\n\n"),
		Tools:          env.Tools(),
		Reasoning:      env.Reasoning(ctx, model, think),
		MaxTurns:       taskTurns,
		Retry:          agentturn.Retry{MaxAttempts: 3},
		BeforeToolCall: env.ChildPolicy("task"),
	}
}

// taskCall is one task call's configuration and opening items, read
// against what /think and /model have set by now.
func (o Options) taskCall(env extension.Env) func(context.Context, taskArgs, agentturn.Transcript, agentturn.Config) (agentturn.Config, openresponses.Items, error) {
	return func(ctx context.Context, a taskArgs, parent agentturn.Transcript, cfg agentturn.Config) (agentturn.Config, openresponses.Items, error) {
		if strings.TrimSpace(a.Input) == "" {
			return cfg, nil, errors.New("input is required: the task for the sub-agent")
		}
		sub, think := o.now(env)
		main, _ := env.Live()
		switch a.Model {
		case "", "subagent":
			cfg = call(ctx, env, cfg, sub, think)
		case "main":
			cfg = call(ctx, env, cfg, main, think)
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
}
