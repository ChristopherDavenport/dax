package toolsearch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/policy"
)

// tool_search runs unasked under the shipped rule, which names nothing
// else: "builtin": false drops the allow, and a user's deny or ask of
// it wins, as of any extension's tool. Loading a tool allows none of
// its calls: a held server's tool is decided by its own rules, the
// fallback here.
func TestToolSearchIsAllowedAndNothingElse(t *testing.T) {
	e := New()
	shipped := policy.Shipped{Name: e.Name, Rules: e.Policy, Owns: e.Owns}
	for _, tc := range []struct {
		name string
		edit func(*policy.Settings)
		tool string
		want agentturn.ToolAction
	}{
		{"shipped", nil, agentkit.ToolSearchTool, agentturn.Allow},
		{"a loaded tool", nil, "mcp__web__search", agentturn.Defer},
		{"builtin off", func(s *policy.Settings) { s.Builtin = false }, agentkit.ToolSearchTool, agentturn.Defer},
		{"the user's deny", func(s *policy.Settings) { s.User.Deny = []string{agentkit.ToolSearchTool} }, agentkit.ToolSearchTool, agentturn.Block},
		{"the project's ask", func(s *policy.Settings) { s.Project.Ask = []string{agentkit.ToolSearchTool} }, agentkit.ToolSearchTool, agentturn.Defer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := policy.Settings{Builtin: true, Fallback: "ask", Shipped: []policy.Shipped{shipped}}
			if tc.edit != nil {
				tc.edit(&s)
			}
			p, err := policy.Build(s)
			if err != nil {
				t.Fatal(err)
			}
			eng, err := agentpolicy.Build(p, nil)
			if err != nil {
				t.Fatal(err)
			}
			args := `{"query":"select:mcp__web__search"}`
			call := &openresponses.FunctionCall{Name: tc.tool, Arguments: args, CallID: "c1"}
			v, err := eng.Would(context.Background(), agentturn.ToolCallInfo{Call: call, Args: json.RawMessage(args), Batch: []*openresponses.FunctionCall{call}})
			if err != nil {
				t.Fatal(err)
			}
			if v.Action != tc.want {
				t.Errorf("%s: %v (%s), want %v", tc.tool, v.Action, v.Reason, tc.want)
			}
		})
	}
}
