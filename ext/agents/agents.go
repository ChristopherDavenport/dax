// Package agents is dax-agents, the extension that offers the main
// agent two sub-agents as tools: explore, a read-only investigator, and
// task, a coding agent of its own for a piece of work. Each runs on a
// fresh transcript, recorded as a child session, under the parent's
// policy, and its final answer comes back as the tool's output.
//
// It is built only from what package extension offers any extension:
// the sub-agents' tools are every extension's (env.Tools, and the
// read-only ones, env.ReadOnlyTools, for explore), so a tool another
// extension adds reaches them too, whichever order the extensions come
// in.
package agents

import (
	"context"
	"strings"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentturn"
	childagent "github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/toolrender"
)

// Name is the extension's name, and so its rules' source:
// extension:dax-agents.
const Name = "dax-agents"

// Options configure dax-agents.
type Options struct {
	// Model is the model name the sub-agents' requests carry; empty is
	// the main agent's model in force at each call, which /model
	// changes.
	Model string
}

// delegation is the main agent's guide to its sub-agents: when to hand
// work to explore or task, when to keep it, and what to do with what
// comes back. The tools' own descriptions say how each one works; this
// says how to divide the work between them and the main agent, whose
// context holds everything it reads to the end of the session. A
// front shows the user only the start of a sub-agent's report, so the
// main agent's reply is where its findings reach the user.
const delegation = "Your context lasts the whole session; spend it on decisions, and let sub-agents do the reading and the routine work.\n" +
	"- explore: a question whose answer means reading or searching many files, such as where something is used or how a feature works. " +
	"Ask one focused question and ask for the conclusion with paths and lines, not file contents. Split a broad sweep into several explores.\n" +
	"- task: a change you can brief completely, giving the goal, the files, the conventions and how to check it. " +
	"A fresh task sees only your brief. Use context fork only when the brief would leave out what was decided here.\n" +
	"- Calls in one turn run in parallel; give parallel tasks separate files.\n" +
	"- Work directly when you know the file or symbol, when the change is small, or when you need to see the code to decide.\n" +
	"- Do not repeat a search you delegated. Before you build on a sub-agent's report, read the lines it depends on.\n" +
	"- In your reply, pass on what a sub-agent found, since the user sees only the start of its report; " +
	"do not repeat file contents or command output."

// New is dax-agents. Its rules allow starting either sub-agent, since
// starting one does nothing of itself: every call the sub-agent makes
// is decided by the session's policy, so a task's writes and commands
// ask as the main agent's do.
func New(o Options) extension.Extension {
	return extension.Extension{
		Name:         Name,
		Owns:         []string{"explore", "task"},
		Policy:       policy.Rules{Allow: []string{"explore", "task"}},
		Instructions: delegation,
		Kit: func(env extension.Env) ([]agentkit.Option, error) {
			ctx := context.Background()
			return []agentkit.Option{
				agentkit.WithChildAgent(o.explore(ctx, env), childagent.WithCallConfig(o.exploreCall(env))),
				agentkit.WithChildAgent(o.task(ctx, env), childagent.WithCallConfig(o.taskCall(env))),
			}, nil
		},
		Renderers: func(string) toolview.Renderers { return toolrender.SubAgents() },
	}
}

// now is the sub-agent model and the reasoning setting in force: the
// configured model, or the main agent's when none is.
func (o Options) now(env extension.Env) (model string, think bool) {
	main, think := env.Live()
	if o.Model != "" {
		return o.Model, think
	}
	return main, think
}

// call is cfg for one sub-agent call on model, with the effort /think
// sets now fitted to it.
func call(ctx context.Context, env extension.Env, cfg agentturn.Config, model string, think bool) agentturn.Config {
	cfg.ModelName = model
	cfg.Reasoning = env.Reasoning(ctx, model, think)
	return cfg
}

// explore is the read-only investigator: the extensions' read-only
// tools, governed by the parent's policy (env.ChildPolicy): the user's
// denies, the extensions' asks, the path rules. A call the policy asks
// about is put to the user from inside the child's run.
func (o Options) explore(ctx context.Context, env extension.Env) agentturn.Config {
	tools := env.ReadOnlyTools()
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name()
	}
	model, think := o.now(env)
	return agentturn.Config{
		Name: "explore",
		Description: "Delegate a read-only investigation of the project to a sub-agent. " +
			"Give it one clear question; it reads files and runs read-only commands and returns a written answer. " +
			"Use it for broad searches so their output stays out of this conversation.",
		Model:     env.Model(),
		ModelName: model,
		Instructions: "You are a read-only explorer working in " + env.Dir() + ". Answer the question using the " + list(names) + " tools; " +
			"never modify files. End with a concise written answer that stands on its own.",
		Tools:          tools,
		Reasoning:      env.Reasoning(ctx, model, think),
		MaxTurns:       10,
		Retry:          agentturn.Retry{MaxAttempts: 3},
		BeforeToolCall: env.ChildPolicy("explore"),
	}
}

// exploreCall is one explore call: the sub-agent model and the effort
// in force now, and the question.
func (o Options) exploreCall(env extension.Env) func(context.Context, childagent.Input, agentturn.Transcript, agentturn.Config) (agentturn.Config, openresponses.Items, error) {
	return func(ctx context.Context, in childagent.Input, _ agentturn.Transcript, cfg agentturn.Config) (agentturn.Config, openresponses.Items, error) {
		model, think := o.now(env)
		return call(ctx, env, cfg, model, think), openresponses.Items{openresponses.UserText(in.Input)}, nil
	}
}

// list joins names as prose: "read, glob and ls".
func list(names []string) string {
	switch len(names) {
	case 0:
		return "available"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
