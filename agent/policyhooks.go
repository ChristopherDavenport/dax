package agent

import (
	"context"
	"fmt"

	"github.com/ChristopherDavenport/agentturn"

	"github.com/ChristopherDavenport/dax/extension"
)

// toolHook is a hook in the policy's decision of a call.
type toolHook = func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)

// policyHooks are the extensions' PolicyHooks, built over env, in the
// extensions' order: the Decide hooks, folded with the policy's
// verdict strictest-wins, and the Answer hooks, which may lift the
// policy's own ask.
func policyHooks(exts []extension.Extension, env extension.Env) (decide, answer []toolHook, err error) {
	for _, e := range exts {
		if e.PolicyHooks == nil {
			continue
		}
		h, err := e.PolicyHooks(env)
		if err != nil {
			return nil, nil, fmt.Errorf("extension %s: policy hooks: %w", e.Name, err)
		}
		if h.Decide != nil {
			decide = append(decide, h.Decide)
		}
		if h.Answer != nil {
			answer = append(answer, h.Answer)
		}
	}
	return decide, answer, nil
}

// notForCallers are the answering hooks hs, each answering nothing for
// an A2A caller's call or its sub-agent's: a caller is held to the
// person's rules and asked as before (peerFloor), and what a person's
// own hook would allow is not the caller's to have.
func notForCallers(hs []toolHook) []toolHook {
	out := make([]toolHook, 0, len(hs))
	for _, h := range hs {
		out = append(out, func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
			if _, peer := ctx.Value(peerKey{}).(string); peer {
				return nil, nil
			}
			return h(ctx, info)
		})
	}
	return out
}
