package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// outcome is how a session's one call of name went: whether the policy
// asked about it and whether it ran (each question answered no).
func outcome(t *testing.T, o Options, name string, ran *atomic.Bool) (asked bool) {
	t.Helper()
	ctx := context.Background()
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := promptOn(ctx, s, "go", func(c *openresponses.FunctionCall, _ string) bool {
		if c.Name == name {
			asked = true
		}
		return false
	}); err != nil {
		t.Fatal(err)
	}
	return asked
}

// A matcher's own subjects may name only its extension's tools, as a
// claim's calls may. acme's fetch, which makes no claim, with a matcher
// whose subject is a read of README.md would otherwise run unasked on
// dax-coding's allow of read, a rule acme does not own; it is decided as
// a call no rule names, so it asks.
func TestAMatcherCannotBorrowAnotherExtensionsRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tool  string // the tool the matcher's subject names
		rules policy.Rules
		asked bool
	}{
		{"dax-coding's read", "read", policy.Rules{}, true},
		{"itself, allowed by its own rule", "", policy.Rules{Allow: []string{"fetch"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran atomic.Bool
			fetch := agenttool.NewFunc("fetch", "Fetch.", json.RawMessage(`{"type":"object"}`),
				func(context.Context, agenttool.Call) (agenttool.Result, error) {
					ran.Store(true)
					return agenttool.Text("fetched"), nil
				})
			o := options(t, &scripted{calls: [][2]string{{"fetch", `{"url":"https://example.com/x"}`}}})
			o.Policy = confirmPolicy(t)
			o.Extensions = append(o.Extensions, extension.Extension{
				Name:  "acme",
				Tools: func(extension.ToolEnv) []agenttool.Tool { return []agenttool.Tool{fetch} },
				Matchers: extension.FixedMatchers(map[string]agentpolicy.ToolMatcher{"fetch": {
					Match: agentpolicy.GlobMatcher("url"),
					Subjects: func(_ context.Context, args json.RawMessage) ([]agentpolicy.Subject, error) {
						a := args
						if tc.tool != "" {
							a = json.RawMessage(`{"path":"README.md"}`)
						}
						return []agentpolicy.Subject{{Tool: tc.tool, Args: a, Text: "fetch"}}, nil
					},
				}}),
				Policy: tc.rules,
			})
			asked := outcome(t, o, "fetch", &ran)
			if asked != tc.asked || ran.Load() == tc.asked {
				t.Errorf("asked = %v, ran = %v; want asked %v", asked, ran.Load(), tc.asked)
			}
		})
	}
}

// probe is a tool an extension's kit adds (one it Owns), with a facts
// claim of a read of path, as dax-skills' skill claims the file it
// serves.
func probe(path string, ran *atomic.Bool) agenttool.Tool {
	return agenttool.NewFunc("probe", "Probe.", json.RawMessage(`{"type":"object"}`),
		func(context.Context, agenttool.Call) (agenttool.Result, error) {
			ran.Store(true)
			return agenttool.Text("probed"), nil
		}, agenttool.WithFacts(func(context.Context, json.RawMessage) (agenttool.Facts, error) {
			return agenttool.Facts{Calls: []agenttool.FactCall{{Tool: "read", Args: json.RawMessage(`{"path":"` + path + `"}`), Text: "probe " + path}}}, nil
		}))
}

// A tool an extension owns, added by its kit, is decided on its facts
// claim. A claim of a read the extension holds the tool to (HeldTo) is
// held to read's asks and denies, dax-coding's secret-path ask and a
// user's deny, and never allowed by read's allows: the call still
// needs the extension's own allow. Without HeldTo the read asks, as
// any claim of another extension's tool does.
func TestAToolAnExtensionOwnsIsDecidedOnItsClaim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		held    bool
		rules   policy.Rules // acme's
		user    policy.Rules
		asked   bool
		ran     bool
		blocked string // in the call's output, when the policy refused it
	}{
		{"read's secret-path ask", ".env", true, policy.Rules{Allow: []string{"probe"}}, policy.Rules{}, true, false, ""},
		{"nothing of read's fires", "README.md", true, policy.Rules{Allow: []string{"probe"}}, policy.Rules{}, false, true, ""},
		{"read's allow does not allow it", "README.md", true, policy.Rules{}, policy.Rules{Allow: []string{"read(README.md)"}}, true, false, ""},
		{"a user's deny of read refuses it", "config/x.md", true, policy.Rules{Allow: []string{"probe"}}, policy.Rules{Deny: []string{"read(config/**)"}}, false, false, "config/x.md"},
		{"not held to read: asks", "README.md", false, policy.Rules{Allow: []string{"probe"}}, policy.Rules{}, true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran atomic.Bool
			o := options(t, &scripted{calls: [][2]string{{"probe", `{}`}}})
			o.Policy = confirmPolicy(t)
			o.Policy.User = tc.user
			e := extension.Extension{
				Name:   "acme",
				Owns:   []string{"probe"},
				Policy: tc.rules,
				Kit: func(extension.Env) ([]agentkit.Option, error) {
					return []agentkit.Option{agentkit.WithTools(probe(tc.path, &ran))}, nil
				},
			}
			if tc.held {
				e.HeldTo = map[string][]string{"probe": {"read"}}
			}
			o.Extensions = append(o.Extensions, e)
			ctx := context.Background()
			s, err := New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			asked := false
			if _, err := promptOn(ctx, s, "go", func(c *openresponses.FunctionCall, _ string) bool {
				asked = asked || c.Name == "probe"
				return false
			}); err != nil {
				t.Fatal(err)
			}
			if asked != tc.asked || ran.Load() != tc.ran {
				t.Errorf("asked = %v, ran = %v; want asked %v, ran %v", asked, ran.Load(), tc.asked, tc.ran)
			}
			if tc.blocked != "" {
				var out string
				for _, it := range s.Agent().State().Transcript {
					if fo, ok := it.(*openresponses.FunctionCallOutput); ok {
						out = fo.Output.String()
					}
				}
				if !strings.Contains(out, "denied") || !strings.Contains(out, tc.blocked) {
					t.Errorf("output %q, want denied on %s", out, tc.blocked)
				}
			}
		})
	}
}

// What an extension says of a name it owns is checked: a matcher that
// gives subjects of its own for a kit tool that claims is refused once
// the kit is built, as for one of its Tools, and HeldTo holds only its
// own tools, to another extension's.
func TestWhatAnExtensionSaysOfWhatItOwnsIsChecked(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*extension.Extension)
		want string
	}{
		{"a matcher's subjects for a tool that claims", func(e *extension.Extension) {
			e.Matchers = extension.FixedMatchers(map[string]agentpolicy.ToolMatcher{"probe": {
				Subjects: func(_ context.Context, args json.RawMessage) ([]agentpolicy.Subject, error) {
					return []agentpolicy.Subject{{Args: args}}, nil
				},
			}})
		}, "facts claim"},
		{"HeldTo of a tool it does not have", func(e *extension.Extension) {
			e.HeldTo = map[string][]string{"bash": {"read"}}
		}, "not one of its tools"},
		{"HeldTo to its own tool", func(e *extension.Extension) {
			e.HeldTo = map[string][]string{"probe": {"probe"}}
		}, "not another extension's tool"},
		{"HeldTo to no tool", func(e *extension.Extension) {
			e.HeldTo = map[string][]string{"probe": {""}}
		}, "not another extension's tool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran atomic.Bool
			o := options(t, &scripted{})
			o.Policy = confirmPolicy(t)
			e := extension.Extension{
				Name: "acme",
				Owns: []string{"probe"},
				Kit: func(extension.Env) ([]agentkit.Option, error) {
					return []agentkit.Option{agentkit.WithTools(probe("README.md", &ran))}, nil
				},
			}
			tc.edit(&e)
			o.Extensions = append(o.Extensions, e)
			s, err := New(context.Background(), o)
			if err == nil {
				s.Close()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("New = %v, want an error saying %q", err, tc.want)
			}
		})
	}
}
