package agent

// The extensions' after-tool hooks (extension.Extension.AfterToolCall):
// the session chains every extension's into one, in the extensions'
// order (agentturn's ChainAfterToolCall), and gives it to the main
// agent and, through Env.ChildAfterToolCall, to every sub-agent (#187).
// agentkit chains its WithAfterToolCall options too (agentkit#139), so
// an extension's Kit option that sets one runs after the extensions'
// hooks, on the main agent alone.

import (
	"context"
	"fmt"

	"github.com/ChristopherDavenport/agentturn"

	"github.com/ChristopherDavenport/dax/extension"
)

// afterHook is an after-tool hook.
type afterHook = func(context.Context, agentturn.ToolResultInfo) (*agentturn.ToolOverride, error)

// afterToolHooks builds the extensions' AfterToolCall hooks over env,
// in the extensions' order.
func afterToolHooks(exts []extension.Extension, env extension.Env) ([]afterHook, error) {
	var hooks []afterHook
	for _, e := range exts {
		if e.AfterToolCall == nil {
			continue
		}
		h, err := e.AfterToolCall(env)
		if err != nil {
			return nil, fmt.Errorf("extension %s: after tool call: %w", e.Name, err)
		}
		if h != nil {
			hooks = append(hooks, h)
		}
	}
	return hooks, nil
}

func (e *sessionEnv) ChildAfterToolCall() func(context.Context, agentturn.ToolResultInfo) (*agentturn.ToolOverride, error) {
	return e.s.afterTool
}
