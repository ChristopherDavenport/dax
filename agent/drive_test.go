package agent

import (
	"context"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// promptOn runs text on s as a front does, through its Turn, with
// approve answering each call the policy asks about and each question a
// sub-agent's call puts (nil leaves both to the rules' defaults: calls
// refused, questions unasked), and returns how the last run ended.
func promptOn(ctx context.Context, s *Session, text string, approve func(*openresponses.FunctionCall, string) bool) (*agentturn.RunEnd, error) {
	return Drive(ctx, s.Turn(), rulesOf(approve), openresponses.UserText(text))
}

// rulesOf is approve as an autonomous controller's rules.
func rulesOf(approve func(*openresponses.FunctionCall, string) bool) Rules {
	if approve == nil {
		return Rules{}
	}
	return Rules{
		Permit: func(c *openresponses.FunctionCall, reason string) (bool, string) { return approve(c, reason), "" },
		Reply: func(q Question) Reply {
			return Reply{Accept: q.Call != nil && approve(q.Call, q.Text)}
		},
	}
}
