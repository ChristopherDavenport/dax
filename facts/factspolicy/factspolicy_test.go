package factspolicy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/facts"
)

func tool(name string, fx func(context.Context, json.RawMessage) (facts.Facts, error)) agenttool.Tool {
	t := agenttool.NewFunc(name, "A tool.", nil, func(context.Context, agenttool.Call) (agenttool.Result, error) {
		return agenttool.Text("ran"), nil
	})
	if fx == nil {
		return t
	}
	return facts.With(t, fx)
}

func decide(t *testing.T, ms map[string]agentpolicy.ToolMatcher, rules []agentpolicy.Rule, name, args string) agentpolicy.Verdict {
	t.Helper()
	p, err := agentpolicy.Merge(agentpolicy.RuleSet{Source: agentpolicy.Source{Name: "test", Trusted: true, Rank: 1}, Allow: rules})
	if err != nil {
		t.Fatal(err)
	}
	p.Default = agentpolicy.Ask()
	eng, err := agentpolicy.Build(p, ms)
	if err != nil {
		t.Fatal(err)
	}
	call := &openresponses.FunctionCall{Name: name, Arguments: args, CallID: "c1"}
	v, err := eng.Would(context.Background(), agentturn.ToolCallInfo{Call: call, Args: json.RawMessage(args), Batch: []*openresponses.FunctionCall{call}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The subjects a policy decides on: a tool with no claim is its own
// call, a claim with no calls is the call itself, a claim's calls are
// each a subject, and a claim of nothing at all is refused.
func TestSubjectsComeFromTheClaim(t *testing.T) {
	glob := agentpolicy.ToolMatcher{Match: agentpolicy.GlobMatcher("path")}
	allowA := []agentpolicy.Rule{{Tool: "t", Spec: "a"}}
	for _, tc := range []struct {
		name string
		fx   func(context.Context, json.RawMessage) (facts.Facts, error)
		args string
		want agentturn.ToolAction
	}{
		{"no claim: its own arguments", nil, `{"path":"a"}`, agentturn.Allow},
		{"a claim of nil calls: the call itself", func(context.Context, json.RawMessage) (facts.Facts, error) { return facts.Facts{}, nil }, `{"path":"a"}`, agentturn.Allow},
		{"the claim's calls, not the arguments", func(context.Context, json.RawMessage) (facts.Facts, error) {
			return facts.Facts{Calls: []facts.Call{{Args: json.RawMessage(`{"path":"b"}`)}}}, nil
		}, `{"path":"a"}`, agentturn.Defer},
		{"every call must be allowed", func(context.Context, json.RawMessage) (facts.Facts, error) {
			return facts.Facts{Calls: []facts.Call{{Args: json.RawMessage(`{"path":"a"}`)}, {Args: json.RawMessage(`{"path":"c"}`)}}}, nil
		}, `{"path":"a"}`, agentturn.Defer},
		{"a claim of no calls is refused", func(context.Context, json.RawMessage) (facts.Facts, error) {
			return facts.Facts{Calls: []facts.Call{}}, nil
		}, `{"path":"a"}`, agentturn.Block},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := tool("t", tc.fx)
			ms, err := Matchers([]agenttool.Tool{tl}, []string{"t"}, map[string]agentpolicy.ToolMatcher{"t": glob})
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

func TestAMatcherMayNotBringSubjectsForAClaimingTool(t *testing.T) {
	tl := tool("t", func(context.Context, json.RawMessage) (facts.Facts, error) { return facts.Facts{}, nil })
	_, err := Matchers([]agenttool.Tool{tl}, []string{"t"}, map[string]agentpolicy.ToolMatcher{"t": {
		Match:    agentpolicy.GlobMatcher("path"),
		Subjects: func(a json.RawMessage) ([]agentpolicy.Subject, error) { return []agentpolicy.Subject{{Args: a}}, nil },
	}})
	if err == nil || !strings.Contains(err.Error(), "facts claim") {
		t.Errorf("err = %v", err)
	}
}

// The hook applies a claim's rewrite as an allow and blocks a call
// whose claim fails; no claim or no rewrite decides nothing.
func TestTheHookAppliesARewriteAndDecidesNothingElse(t *testing.T) {
	rw := func(_ context.Context, args json.RawMessage) (facts.Facts, error) {
		if string(args) == `"bad"` {
			return facts.Facts{}, json.Unmarshal([]byte("x"), &struct{}{})
		}
		return facts.Facts{Rewrite: json.RawMessage(`{"stamped":true}`)}, nil
	}
	none := func(context.Context, json.RawMessage) (facts.Facts, error) { return facts.Facts{}, nil }
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
			up := tool("upload", func(context.Context, json.RawMessage) (facts.Facts, error) {
				return facts.Facts{Calls: []facts.Call{{Tool: tc.claim, Args: json.RawMessage(`{"path":"README.md"}`), Text: "upload README.md"}}}, nil
			})
			ms, err := Matchers([]agenttool.Tool{up}, tc.own, map[string]agentpolicy.ToolMatcher{"upload": glob})
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
