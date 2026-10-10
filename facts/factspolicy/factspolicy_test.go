package factspolicy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

func tool(name string, fx func(context.Context, json.RawMessage) (agenttool.Facts, error)) agenttool.Tool {
	// A nil fx makes no claim.
	return agenttool.NewFunc(name, "A tool.", nil, func(context.Context, agenttool.Call) (agenttool.Result, error) {
		return agenttool.Text("ran"), nil
	}, agenttool.WithFacts(fx))
}

func decide(t *testing.T, ms map[string]agentpolicy.ToolMatcher, rules []agentpolicy.Rule, name, args string) agentpolicy.Verdict {
	t.Helper()
	v, err := decideIn(t, context.Background(), ms, rules, name, args)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// decideIn is what the engine would decide of a call of name with args
// under ctx, the decision's context.
func decideIn(t *testing.T, ctx context.Context, ms map[string]agentpolicy.ToolMatcher, rules []agentpolicy.Rule, name, args string) (agentpolicy.Verdict, error) {
	t.Helper()
	return decideUnder(t, ctx, ms, agentpolicy.RuleSet{Allow: rules}, name, args)
}

// decideUnder is what the engine would decide of a call of name with
// args under ctx and the rules of rs, asking by default.
func decideUnder(t *testing.T, ctx context.Context, ms map[string]agentpolicy.ToolMatcher, rs agentpolicy.RuleSet, name, args string) (agentpolicy.Verdict, error) {
	t.Helper()
	rs.Source = agentpolicy.Source{Name: "test", Trusted: true, Rank: 1}
	p, err := agentpolicy.Merge(rs)
	if err != nil {
		t.Fatal(err)
	}
	p.Default = agentpolicy.Ask()
	eng, err := agentpolicy.Build(p, ms)
	if err != nil {
		t.Fatal(err)
	}
	call := &openresponses.FunctionCall{Name: name, Arguments: args, CallID: "c1"}
	return eng.Would(ctx, agentturn.ToolCallInfo{Call: call, Args: json.RawMessage(args), Batch: []*openresponses.FunctionCall{call}})
}

// The subjects a policy decides on: a tool with no claim is its own
// call, a claim with no calls is the call itself, a claim's calls are
// each a subject, and a claim of nothing at all is refused.
func TestSubjectsComeFromTheClaim(t *testing.T) {
	glob := agentpolicy.ToolMatcher{Match: agentpolicy.GlobMatcher("path")}
	allowA := []agentpolicy.Rule{{Tool: "t", Spec: "a"}}
	for _, tc := range []struct {
		name string
		fx   func(context.Context, json.RawMessage) (agenttool.Facts, error)
		args string
		want agentturn.ToolAction
	}{
		{"no claim: its own arguments", nil, `{"path":"a"}`, agentturn.Allow},
		{"a claim of nil calls: the call itself", func(context.Context, json.RawMessage) (agenttool.Facts, error) { return agenttool.Facts{}, nil }, `{"path":"a"}`, agentturn.Allow},
		{"the claim's calls, not the arguments", func(context.Context, json.RawMessage) (agenttool.Facts, error) {
			return agenttool.Facts{Calls: []agenttool.FactCall{{Args: json.RawMessage(`{"path":"b"}`)}}}, nil
		}, `{"path":"a"}`, agentturn.Defer},
		{"every call must be allowed", func(context.Context, json.RawMessage) (agenttool.Facts, error) {
			return agenttool.Facts{Calls: []agenttool.FactCall{{Args: json.RawMessage(`{"path":"a"}`)}, {Args: json.RawMessage(`{"path":"c"}`)}}}, nil
		}, `{"path":"a"}`, agentturn.Defer},
		{"a claim of no calls is refused", func(context.Context, json.RawMessage) (agenttool.Facts, error) {
			return agenttool.Facts{Calls: []agenttool.FactCall{}}, nil
		}, `{"path":"a"}`, agentturn.Block},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := tool("t", tc.fx)
			ms, err := Matchers([]agenttool.Tool{tl}, []string{"t"}, nil, map[string]agentpolicy.ToolMatcher{"t": glob})
			if err != nil {
				t.Fatal(err)
			}
			if (ms["t"].Subjects == nil) != (tc.fx == nil) {
				t.Errorf("subjects set = %v for a tool that claims = %v", ms["t"].Subjects != nil, tc.fx != nil)
			}
			if v := decide(t, ms, allowA, "t", tc.args); v.Action != tc.want {
				t.Errorf("= %v (%s), want %v", v.Action, v.Reason, tc.want)
			}
		})
	}
}

// decisionKey marks the context of a decision, so a claim can say
// which context it was asked under.
type decisionKey struct{}

// The claim is asked under the context of the decision that reads the
// subjects: a value the decision carries reaches it, and a context
// cancelled while the claim is asked, as a remote executor's reading
// is, blocks the call rather than letting it run.
func TestTheClaimIsAskedUnderTheDecisionsContext(t *testing.T) {
	allowA := []agentpolicy.Rule{{Tool: "t", Spec: "a"}}
	matchers := func(t *testing.T, fx func(context.Context, json.RawMessage) (agenttool.Facts, error)) map[string]agentpolicy.ToolMatcher {
		t.Helper()
		ms, err := Matchers([]agenttool.Tool{tool("t", fx)}, []string{"t"}, nil, map[string]agentpolicy.ToolMatcher{"t": {Match: agentpolicy.GlobMatcher("path")}})
		if err != nil {
			t.Fatal(err)
		}
		return ms
	}
	t.Run("a value on the decision's context reaches the claim", func(t *testing.T) {
		var seen []any
		ms := matchers(t, func(ctx context.Context, args json.RawMessage) (agenttool.Facts, error) {
			seen = append(seen, ctx.Value(decisionKey{}))
			return agenttool.Facts{}, nil
		})
		ctx := context.WithValue(t.Context(), decisionKey{}, "decision-1")
		v, err := decideIn(t, ctx, ms, allowA, "t", `{"path":"a"}`)
		if err != nil || v.Action != agentturn.Allow {
			t.Fatalf("= %v (%s), %v; want allowed", v.Action, v.Reason, err)
		}
		if len(seen) == 0 {
			t.Fatal("the claim was not asked")
		}
		for _, s := range seen {
			if s != "decision-1" {
				t.Errorf("the claim saw %v, want the decision's context", s)
			}
		}
	})
	t.Run("a context cancelled during the decision does not allow the call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ms := matchers(t, func(ctx context.Context, args json.RawMessage) (agenttool.Facts, error) {
			// A remote reading the decision is cancelled under: it
			// answers with the context's error, and would have said
			// the call touches only what the rule allows.
			cancel()
			select {
			case <-ctx.Done():
				return agenttool.Facts{}, ctx.Err()
			case <-time.After(5 * time.Second):
				return agenttool.Facts{}, nil
			}
		})
		v, err := decideIn(t, ctx, ms, allowA, "t", `{"path":"a"}`)
		if err != nil {
			t.Fatal(err)
		}
		if v.Action != agentturn.Block || !strings.Contains(v.Reason, context.Canceled.Error()) {
			t.Errorf("= %v (%s), want blocked by the cancellation", v.Action, v.Reason)
		}
	})
}

func TestAMatcherMayNotBringSubjectsForAClaimingTool(t *testing.T) {
	tl := tool("t", func(context.Context, json.RawMessage) (agenttool.Facts, error) { return agenttool.Facts{}, nil })
	_, err := Matchers([]agenttool.Tool{tl}, []string{"t"}, nil, map[string]agentpolicy.ToolMatcher{"t": {
		Match: agentpolicy.GlobMatcher("path"),
		Subjects: func(_ context.Context, a json.RawMessage) ([]agentpolicy.Subject, error) {
			return []agentpolicy.Subject{{Args: a}}, nil
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "facts claim") {
		t.Errorf("err = %v", err)
	}
}

// The hook applies a claim's rewrite as an allow and blocks a call
// whose claim fails; no claim or no rewrite decides nothing.
func TestTheHookAppliesARewriteAndDecidesNothingElse(t *testing.T) {
	rw := func(_ context.Context, args json.RawMessage) (agenttool.Facts, error) {
		if string(args) == `"bad"` {
			return agenttool.Facts{}, json.Unmarshal([]byte("x"), &struct{}{})
		}
		return agenttool.Facts{Rewrite: json.RawMessage(`{"stamped":true}`)}, nil
	}
	none := func(context.Context, json.RawMessage) (agenttool.Facts, error) { return agenttool.Facts{}, nil }
	if Hook([]agenttool.Tool{tool("plain", nil)}) != nil {
		t.Error("a hook with no tool that claims")
	}
	h := Hook([]agenttool.Tool{tool("rw", rw), tool("none", none), tool("plain", nil)})
	call := func(name, args string) *agentturn.ToolDecision {
		d, err := h(context.Background(), agentturn.ToolCallInfo{Call: &openresponses.FunctionCall{Name: name, Arguments: args}, Args: json.RawMessage(args)})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := call("rw", `{}`); d == nil || d.Action != agentturn.Allow || string(d.Args) != `{"stamped":true}` {
		t.Errorf("rewrite = %+v", d)
	}
	for _, c := range [][2]string{{"none", `{}`}, {"plain", `{}`}, {"other", `{}`}} {
		if d := call(c[0], c[1]); d != nil {
			t.Errorf("%s %s decided %+v", c[0], c[1], d)
		}
	}
	if d := call("rw", `"bad"`); d == nil || d.Action != agentturn.Block || d.Args != nil {
		t.Errorf("a claim that fails = %+v, want blocked", d)
	}
}

// A claim may name its own tool and its extension's other tools, as
// bash's claims a read of what cat reads; a call that names another
// extension's tool is decided as a tool no rule names, so the allow
// rule for the tool it named does not reach it and it asks.
func TestAClaimNamesOnlyItsOwnExtensionsTools(t *testing.T) {
	rules := []agentpolicy.Rule{{Tool: "read", Spec: "README.md"}, {Tool: "upload", Spec: "README.md"}}
	glob := agentpolicy.ToolMatcher{Match: agentpolicy.GlobMatcher("path")}
	for _, tc := range []struct {
		name  string
		claim string
		own   []string
		want  agentturn.ToolAction
	}{
		{"its own tool, by name", "upload", []string{"upload"}, agentturn.Allow},
		{"its own tool, unnamed", "", []string{"upload"}, agentturn.Allow},
		{"another tool of its extension", "read", []string{"upload", "read"}, agentturn.Allow},
		{"another extension's tool", "read", []string{"upload"}, agentturn.Defer},
		{"a tool nobody has", "fetch", []string{"upload"}, agentturn.Defer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := tool("upload", func(context.Context, json.RawMessage) (agenttool.Facts, error) {
				return agenttool.Facts{Calls: []agenttool.FactCall{{Tool: tc.claim, Args: json.RawMessage(`{"path":"README.md"}`), Text: "upload README.md"}}}, nil
			})
			ms, err := Matchers([]agenttool.Tool{up}, tc.own, nil, map[string]agentpolicy.ToolMatcher{"upload": glob})
			if err != nil {
				t.Fatal(err)
			}
			ms["read"] = glob
			if v := decide(t, ms, rules, "upload", `{"what":"~/.ssh/id_rsa"}`); v.Action != tc.want {
				t.Errorf("= %v (%s), want %v", v.Action, v.Reason, tc.want)
			}
		})
	}
}

// A claim's call of a tool its extension holds the claiming tool to
// (HeldTo) is a constraint: that tool's ask and deny rules reach the
// call, and its allow rules never allow it, so the call still needs an
// allow of its own. A tool it is not held to is still one no rule
// names.
func TestAClaimOfAToolItIsHeldToIsAConstraint(t *testing.T) {
	glob := agentpolicy.ToolMatcher{Match: agentpolicy.GlobMatcher("path")}
	rules := func(allowSkill bool) agentpolicy.RuleSet {
		rs := agentpolicy.RuleSet{
			Allow: []agentpolicy.Rule{{Tool: "read", Spec: "README.md"}, {Tool: "read", Spec: "notes.md"}},
			Ask:   []agentpolicy.Rule{{Tool: "read", Spec: ".env"}},
			Deny:  []agentpolicy.Rule{{Tool: "read", Spec: "secret/**"}},
		}
		if allowSkill {
			rs.Allow = append(rs.Allow, agentpolicy.Rule{Tool: "skill"})
		}
		return rs
	}
	for _, tc := range []struct {
		name       string
		claim      string // the tool the claim's call names
		path       string
		allowSkill bool
		want       agentturn.ToolAction
		subject    string // in the verdict's subject; "" any
	}{
		{"read's ask asks", "read", ".env", true, agentturn.Defer, "skill .env"},
		{"read's deny refuses", "read", "secret/x", true, agentturn.Block, "skill secret/x"},
		{"nothing of read's fires: the call's own allow", "read", "plain.md", true, agentturn.Allow, ""},
		{"read's allow does not allow the call", "read", "notes.md", false, agentturn.Defer, ""},
		{"a tool it is not held to asks", "write", "notes.md", true, agentturn.Defer, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sk := tool("skill", func(context.Context, json.RawMessage) (agenttool.Facts, error) {
				return agenttool.Facts{Calls: []agenttool.FactCall{{Tool: tc.claim, Args: json.RawMessage(`{"path":"` + tc.path + `"}`), Text: "skill " + tc.path}}}, nil
			})
			ms, err := Matchers([]agenttool.Tool{sk}, []string{"skill"}, map[string][]string{"skill": {"read"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ms["read"], ms["write"] = glob, glob
			v, err := decideUnder(t, context.Background(), ms, rules(tc.allowSkill), "skill", `{"name":"x"}`)
			if err != nil {
				t.Fatal(err)
			}
			if v.Action != tc.want || tc.subject != "" && v.Subject != tc.subject {
				t.Errorf("= %v (%s, about %q), want %v about %q", v.Action, v.Reason, v.Subject, tc.want, tc.subject)
			}
		})
	}
}

// A matcher's own subjects, for a tool that makes no claim, may name
// only its extension's tools, as a claim's calls may: one that names
// another extension's tool is decided as a tool no rule names, so the
// allow rule for the tool it named does not reach it. HeldTo is the
// claim's, never a matcher's.
func TestAMatchersSubjectsNameOnlyItsOwnExtensionsTools(t *testing.T) {
	rules := []agentpolicy.Rule{{Tool: "read", Spec: "README.md"}, {Tool: "upload", Spec: "README.md"}}
	glob := agentpolicy.ToolMatcher{Match: agentpolicy.GlobMatcher("path")}
	for _, tc := range []struct {
		name  string
		names string
		own   []string
		held  map[string][]string
		want  agentturn.ToolAction
	}{
		{"its own tool", "upload", []string{"upload"}, nil, agentturn.Allow},
		{"another tool of its extension", "read", []string{"upload", "read"}, nil, agentturn.Allow},
		{"another extension's tool", "read", []string{"upload"}, nil, agentturn.Defer},
		{"another extension's tool it is held to", "read", []string{"upload"}, map[string][]string{"upload": {"read"}}, agentturn.Defer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := glob
			m.Subjects = func(context.Context, json.RawMessage) ([]agentpolicy.Subject, error) {
				return []agentpolicy.Subject{{Tool: tc.names, Args: json.RawMessage(`{"path":"README.md"}`), Text: "upload README.md"}}, nil
			}
			ms, err := Matchers([]agenttool.Tool{tool("upload", nil)}, tc.own, tc.held, map[string]agentpolicy.ToolMatcher{"upload": m})
			if err != nil {
				t.Fatal(err)
			}
			ms["read"] = glob
			if v := decide(t, ms, rules, "upload", `{"what":"~/.ssh/id_rsa"}`); v.Action != tc.want {
				t.Errorf("= %v (%s), want %v", v.Action, v.Reason, tc.want)
			}
		})
	}
}
