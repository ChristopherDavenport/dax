package policy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// matchers read the test tools' calls: deploy by its environment.
var matchers = map[string]agentpolicy.ToolMatcher{"deploy": {Match: agentpolicy.GlobMatcher("env")}}

// acme ships rules for its deploy and rollback tools, and an alias for
// both.
func acme(r Rules) Shipped {
	return Shipped{Name: "acme", Rules: r, Owns: []string{"deploy", "rollback", "Ship"}}
}

// verdict builds the engine for s and asks what it does with one call.
func verdict(t *testing.T, s Settings, tool, args string) agentpolicy.Verdict {
	t.Helper()
	p, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := agentpolicy.Build(p, matchers, agentpolicy.WithAliases(map[string][]string{"Ship": {"deploy", "rollback"}}))
	if err != nil {
		t.Fatal(err)
	}
	call := &openresponses.FunctionCall{Name: tool, Arguments: args, CallID: "c1"}
	v, err := eng.Would(context.Background(), agentturn.ToolCallInfo{Call: call, Args: json.RawMessage(args), Batch: []*openresponses.FunctionCall{call}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func env(e string) string { return `{"env":"` + e + `"}` }

// An extension's rules rank below the user's config and above the
// project's: deny beats ask beats allow whatever the source, a user's
// carve-out can lift an extension's ask, and a project can only make
// things stricter.
func TestShippedRulesRankBetweenTheUserAndTheProject(t *testing.T) {
	with := func(r Rules, edit func(*Settings)) Settings {
		s := Settings{Builtin: true, Fallback: "ask", Shipped: []Shipped{acme(r)}}
		if edit != nil {
			edit(&s)
		}
		return s
	}
	allowDeploy := Rules{Allow: []string{"deploy"}}
	askProd := Rules{Allow: []string{"deploy"}, Ask: []string{"deploy(prod)"}}
	tests := []struct {
		name string
		s    Settings
		tool string
		args string
		want agentturn.ToolAction
	}{
		{"without a rule a tool asks", Settings{Fallback: "ask"}, "deploy", env("x"), agentturn.Defer},
		{"a shipped allow runs it unasked", with(allowDeploy, nil), "deploy", env("x"), agentturn.Allow},
		{"a shipped allow with a specifier", with(Rules{Allow: []string{"deploy(staging)"}}, nil), "deploy", env("staging"), agentturn.Allow},
		{"a shipped allow with a specifier reaches no further", with(Rules{Allow: []string{"deploy(staging)"}}, nil), "deploy", env("prod"), agentturn.Defer},
		{"a shipped ask beats its own allow", with(askProd, nil), "deploy", env("prod"), agentturn.Defer},
		{"a shipped deny", with(Rules{Deny: []string{"rollback"}}, nil), "rollback", `{}`, agentturn.Block},
		{"an alias it owns", with(Rules{Allow: []string{"Ship"}}, nil), "rollback", `{}`, agentturn.Allow},
		{"a user deny beats a shipped allow", with(allowDeploy, func(s *Settings) { s.User.Deny = []string{"deploy"} }), "deploy", env("x"), agentturn.Block},
		{"a user ask beats a shipped allow", with(allowDeploy, func(s *Settings) { s.User.Ask = []string{"deploy"} }), "deploy", env("x"), agentturn.Defer},
		{"a shipped ask beats a user's plain allow", with(askProd, func(s *Settings) { s.User.Allow = []string{"deploy"} }), "deploy", env("prod"), agentturn.Defer},
		{"a user's carve-out lifts a shipped ask", with(askProd, func(s *Settings) { s.User.Ask = []string{"deploy(!prod)"} }), "deploy", env("prod"), agentturn.Allow},
		{"a project's carve-out does not", with(askProd, func(s *Settings) { s.Project.Ask = []string{"deploy(!prod)"} }), "deploy", env("prod"), agentturn.Defer},
		{"a project ask applies to a shipped allow", with(allowDeploy, func(s *Settings) { s.Project.Ask = []string{"deploy"} }), "deploy", env("x"), agentturn.Defer},
		{"a project deny applies to a shipped allow", with(allowDeploy, func(s *Settings) { s.Project.Deny = []string{"deploy"} }), "deploy", env("x"), agentturn.Block},
		{"a project's allow is withheld", Settings{Fallback: "ask", Project: Rules{Allow: []string{"deploy"}}}, "deploy", env("x"), agentturn.Defer},
		{"no built-in drops a shipped allow", with(allowDeploy, func(s *Settings) { s.Builtin = false }), "deploy", env("x"), agentturn.Defer},
		{"no built-in keeps a shipped deny", with(Rules{Deny: []string{"rollback"}}, func(s *Settings) { s.Builtin, s.Fallback = false, "allow" }), "rollback", `{}`, agentturn.Block},
		{"no built-in keeps a shipped ask", with(askProd, func(s *Settings) { s.Builtin, s.Fallback = false, "allow" }), "deploy", env("prod"), agentturn.Defer},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if v := verdict(t, tc.s, tc.tool, tc.args); v.Action != tc.want {
				t.Errorf("= %v (%s), want %v", v.Action, v.Reason, tc.want)
			}
		})
	}
}

// A user's allow rule with a specifier for a tool the extension lifts
// for cancels the extension's ask for it: allowing a path is meant.
// Only a trusted layer's does, and only for a tool in Lifts.
func TestAUsersAllowLiftsAnAskOnlyForALiftedTool(t *testing.T) {
	ask := Rules{Ask: []string{"deploy(prod)"}}
	lifted := acme(ask)
	lifted.Lifts = []string{"deploy"}
	for _, tc := range []struct {
		name    string
		shipped Shipped
		edit    func(*Settings)
		want    agentturn.ToolAction
	}{
		{"the user's allow lifts it", lifted, func(s *Settings) { s.User.Allow = []string{"deploy(prod)"} }, agentturn.Allow},
		{"not for a tool the extension does not lift", acme(ask), func(s *Settings) { s.User.Allow = []string{"deploy(prod)"} }, agentturn.Defer},
		{"not from the project", lifted, func(s *Settings) { s.Project.Allow = []string{"deploy(prod)"} }, agentturn.Defer},
		{"a bare allow is not a specific one", lifted, func(s *Settings) { s.User.Allow = []string{"deploy"} }, agentturn.Defer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Settings{Builtin: true, Fallback: "ask", Shipped: []Shipped{tc.shipped}}
			tc.edit(&s)
			if v := verdict(t, s, "deploy", env("prod")); v.Action != tc.want {
				t.Errorf("= %v (%s), want %v", v.Action, v.Reason, tc.want)
			}
		})
	}
}

// A verdict names the source of the rule that decided it, and the
// session records that name: a call an extension's rule allowed is that
// extension's decision.
func TestAShippedRulesVerdictNamesItsExtension(t *testing.T) {
	s := Settings{Builtin: true, Fallback: "ask", Shipped: []Shipped{
		acme(Rules{Allow: []string{"deploy"}}),
		{Name: "ops", Rules: Rules{Deny: []string{"restart"}}, Owns: []string{"restart"}},
	}}
	for _, tc := range []struct{ tool, args, source string }{
		{"deploy", env("x"), "extension:acme"},
		{"restart", `{}`, "extension:ops"},
		{"rollback", `{}`, ""},
	} {
		v := verdict(t, s, tc.tool, tc.args)
		got := ""
		if v.Rule != nil {
			got = v.Rule.Source.Name
		}
		if got != tc.source {
			t.Errorf("%s: decided by %q, want %q", tc.tool, got, tc.source)
		}
	}
	if SourceExtension("dax-coding") != "extension:dax-coding" {
		t.Errorf("SourceExtension = %q", SourceExtension("dax-coding"))
	}
}

// An extension's rules may reach only its own tools: a rule for another
// tool, a tool pattern or a carve-out could loosen or cancel what
// another source decided.
func TestShippedRulesReachOnlyTheirOwnTools(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Rules
		want string
	}{
		{"another extension's tool", Rules{Allow: []string{"read"}}, `"read" is not one of the extension's tools`},
		{"another's alias", Rules{Allow: []string{"Read(.env)"}}, `"Read" is not one of the extension's tools`},
		{"a tool pattern", Rules{Deny: []string{"mcp__*"}}, "not a pattern"},
		{"a pattern over its own names", Rules{Allow: []string{"deplo*"}}, "not a pattern"},
		{"a carve-out", Rules{Ask: []string{"deploy(!prod)"}}, "may not ship a carve-out"},
		{"a carve-out in an allow", Rules{Allow: []string{"deploy(!prod)"}}, "may not ship a carve-out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build(Settings{Builtin: true, Fallback: "ask", Shipped: []Shipped{acme(tc.r)}})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "extension acme") {
				t.Errorf("err = %v, want one naming acme and saying %q", err, tc.want)
			}
		})
	}
	// Its own names, in any case, are its own.
	if _, err := Build(Settings{Builtin: true, Shipped: []Shipped{acme(Rules{Allow: []string{"DEPLOY", "ship"}})}}); err != nil {
		t.Errorf("its own names in another case: %v", err)
	}
}

func TestBadShippedRulesAreErrors(t *testing.T) {
	bad := Rules{Allow: []string{"deploy("}}
	for _, tc := range []struct {
		name    string
		shipped []Shipped
		builtin bool
		want    string
	}{
		{"a bad allow", []Shipped{acme(bad)}, true, "extension acme allow"},
		{"a bad ask", []Shipped{acme(Rules{Ask: bad.Allow})}, true, "extension acme ask"},
		{"a bad deny", []Shipped{acme(Rules{Deny: bad.Allow})}, true, "extension acme deny"},
		{"a bad rule the user dropped", []Shipped{acme(bad)}, false, "extension acme allow"},
		{"no name", []Shipped{{Rules: Rules{Allow: []string{"deploy"}}, Owns: []string{"deploy"}}}, true, "without an extension name"},
		{"two of one name", []Shipped{acme(Rules{}), acme(Rules{})}, true, `two extensions named "acme"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build(Settings{Builtin: tc.builtin, Fallback: "ask", Shipped: tc.shipped})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one saying %q", err, tc.want)
			}
		})
	}
}

func TestTheFallback(t *testing.T) {
	for fallback, want := range map[string]agentturn.ToolAction{"allow": agentturn.Allow, "deny": agentturn.Block, "ask": agentturn.Defer, "": agentturn.Defer} {
		if v := verdict(t, Settings{Fallback: fallback}, "deploy", env("x")); v.Action != want {
			t.Errorf("fallback %q = %v, want %v", fallback, v.Action, want)
		}
	}
}
