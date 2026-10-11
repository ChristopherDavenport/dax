package agents_test

import (
	"context"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
)

// prompt runs text on s as a front does, through its control, with approve
// answering each call the policy asks about and each question a
// sub-agent's call puts (nil leaves both to the rules' defaults: calls
// refused, questions unasked), and returns how the last run ended.
func prompt(ctx context.Context, s *agent.Session, text string, approve func(*openresponses.FunctionCall, string) bool) (*agentturn.RunEnd, error) {
	var r agent.Rules
	if approve != nil {
		r.Permit = func(c *openresponses.FunctionCall, reason string) (bool, string) { return approve(c, reason), "" }
		r.Reply = func(q agentturn.Question) agenttool.Answer {
			if q.Call != nil && approve(&openresponses.FunctionCall{CallID: q.Call.CallID, Name: q.Call.Name, Arguments: string(q.Call.Args)}, q.Elicitation.Message) {
				return agenttool.Answer{Action: agenttool.ActionAccept}
			}
			return agenttool.Answer{Action: agenttool.ActionDecline}
		}
	}
	return agent.Drive(ctx, s, r, openresponses.UserText(text))
}
