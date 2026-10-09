package memory_test

import (
	"context"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
)

// prompt runs text on s as a front does, through its Turn, with approve
// answering each call the policy asks about and each question a
// sub-agent's call puts (nil leaves both to the rules' defaults: calls
// refused, questions unasked), and returns how the last run ended.
func prompt(ctx context.Context, s *agent.Session, text string, approve func(*openresponses.FunctionCall, string) bool) (*agentturn.RunEnd, error) {
	var r agent.Rules
	if approve != nil {
		r.Permit = func(c *openresponses.FunctionCall, reason string) (bool, string) { return approve(c, reason), "" }
		r.Reply = func(q agent.Question) agent.Reply {
			return agent.Reply{Accept: q.Call != nil && approve(q.Call, q.Text)}
		}
	}
	return agent.Drive(ctx, s.Turn(), r, openresponses.UserText(text))
}
