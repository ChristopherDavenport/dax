// Package plan is dax-plan, the extension of plan mode's exit: the
// exit_plan_mode tool, with which the model puts its plan to the person
// and, once they approve it, moves the session to acceptEdits to carry
// it out (#147). It is built only from what package extension offers
// any extension.
package plan

import (
	"context"
	"errors"
	"strings"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agenttool"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/toolrender"
)

// Name is the extension's name, and so its rules' source:
// extension:dax-plan.
const Name = "dax-plan"

// Tool is the plan's exit.
const Tool = "exit_plan_mode"

// Approved is the reason the mode switch the person's approval makes is
// recorded with (the dax.mode entry).
const Approved = "plan approved"

// New is dax-plan. exit_plan_mode asks in plan mode, the one call that
// mode asks about, and is refused bare in every other; the person's
// approval runs it, and it moves the session to acceptEdits from the
// next turn, whatever mode the person was in before plan (an approved
// plan is a step toward edits, never toward bypassPermissions). It is
// the person's own: an A2A caller is refused it, so a caller can never
// move the person's session out of plan mode.
func New() extension.Extension {
	deny := policy.Rules{Deny: []string{Tool}}
	return extension.Extension{
		Name:       Name,
		Owns:       []string{Tool},
		PersonOnly: []string{Tool},
		ModeRules: map[string]policy.Rules{
			policy.ModePlan:        {Ask: []string{Tool}},
			policy.ModeDefault:     deny,
			policy.ModeAcceptEdits: deny,
			policy.ModeDontAsk:     deny,
			policy.ModeBypass:      deny,
		},
		Kit: func(env extension.Env) ([]agentkit.Option, error) {
			return []agentkit.Option{agentkit.WithTools(tool(env))}, nil
		},
		Renderers: func(string) toolview.Renderers { return toolrender.Plan() },
	}
}

type exitArgs struct {
	Plan string `json:"plan" desc:"The plan, in Markdown: what you will change, in which files, in what order, and how you will check it"`
}

func tool(env extension.Env) agenttool.Tool {
	return agenttool.New(Tool,
		"In plan mode, put your finished plan to the user for approval. Call it once you have read enough to plan, with the whole plan. "+
			"If the user approves, the permission mode becomes acceptEdits from your next turn and you carry the plan out; if not, you stay in plan mode and are told why. "+
			"Outside plan mode it is refused.",
		func(ctx context.Context, a exitArgs) (string, error) {
			if strings.TrimSpace(a.Plan) == "" {
				return "", errors.New("plan is empty; give the whole plan")
			}
			if err := env.SetMode(ctx, policy.ModeAcceptEdits, Approved); err != nil {
				return "", err
			}
			return "The user approved the plan. The permission mode is acceptEdits from your next turn: file writes and edits run without asking; commands and other tools still ask. Carry the plan out.", nil
		},
		agenttool.WithAnnotations(agenttool.Annotations{Title: "Exit plan mode"}),
	)
}
