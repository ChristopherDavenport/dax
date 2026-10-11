package agent

import (
	"context"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// promptOn runs text on s as a front does, through its control, with
// approve answering each call the policy asks about and each question a
// sub-agent's call puts (nil leaves both to the rules' defaults: calls
// refused, questions unasked), and returns how the last run ended.
func promptOn(ctx context.Context, s *Session, text string, approve func(*openresponses.FunctionCall, string) bool) (*agentturn.RunEnd, error) {
	return Drive(ctx, s, rulesOf(approve), openresponses.UserText(text))
}

// rulesOf is approve as an autonomous controller's rules.
func rulesOf(approve func(*openresponses.FunctionCall, string) bool) Rules {
	if approve == nil {
		return Rules{}
	}
	return Rules{
		Permit: func(c *openresponses.FunctionCall, reason string) (bool, string) { return approve(c, reason), "" },
		Reply: func(q agentturn.Question) agenttool.Answer {
			if q.Call != nil && approve(askedCall(q), q.Elicitation.Message) {
				return agenttool.Answer{Action: agenttool.ActionAccept}
			}
			return agenttool.Answer{Action: agenttool.ActionDecline}
		},
	}
}

// askedCall is the call a question is about as the model made it.
func askedCall(q agentturn.Question) *openresponses.FunctionCall {
	return &openresponses.FunctionCall{CallID: q.Call.CallID, Name: q.Call.Name, Arguments: string(q.Call.Args)}
}
