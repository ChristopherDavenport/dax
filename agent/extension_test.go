package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ChristopherDavenport/dax/workspace"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"

	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// fn is a tool that answers with its name and counts its runs.
func fn(name string, runs *atomic.Int32, opts ...agenttool.Option) agenttool.Tool {
	return agenttool.NewFunc(name, "A tool of an extension.", json.RawMessage(`{"type":"object","properties":{"env":{"type":"string"}}}`),
		func(context.Context, agenttool.Call) (agenttool.Result, error) {
			if runs != nil {
				runs.Add(1)
			}
			return agenttool.Text(name + " ran"), nil
		}, opts...)
}

// tools is an extension's Tools of the named fn tools.
func tools(names ...string) func(extension.ToolEnv) []agenttool.Tool {
	return func(extension.ToolEnv) []agenttool.Tool {
		var out []agenttool.Tool
		for _, n := range names {
			out = append(out, fn(n, nil))
		}
		return out
	}
}

// closer is a tool that holds something and counts its closes.
func closer(name string, closed *atomic.Int32) agenttool.Tool {
	return fn(name, nil, agenttool.WithCloser(func() error { closed.Add(1); return nil }))
}

func toolEnv(t *testing.T) extension.ToolEnv {
	t.Helper()
	ws := localWorkspace(t, t.TempDir())
	return extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws)}
}

// What an extension claims is checked before any of it reaches the
// kit: names are unique across the session without regard to case, and
// what an extension says of its tools names only its own.
func TestWhatAnExtensionClaimsIsChecked(t *testing.T) {
	destructive := func(extension.ToolEnv) []agenttool.Tool {
		return []agenttool.Tool{fn("wipe", nil, agenttool.WithAnnotations(agenttool.Annotations{Destructive: true}))}
	}
	matcher := agentpolicy.ToolMatcher{Match: agentpolicy.GlobMatcher("env")}
	for _, tc := range []struct {
		name string
		exts []extension.Extension
		want string // empty: accepted
	}{
		{"one extension", []extension.Extension{{Name: "a", Tools: tools("deploy"), ReadOnly: []string{"deploy"}}}, ""},
		{"no name", []extension.Extension{{Tools: tools("deploy")}}, "an extension with no name"},
		{"two of one name", []extension.Extension{{Name: "a"}, {Name: "a"}}, "two extensions named a"},
		{"a tool in two extensions", []extension.Extension{{Name: "a", Tools: tools("deploy")}, {Name: "b", Tools: tools("deploy")}}, `"deploy": extension a has that name`},
		{"a tool in two extensions, in another case", []extension.Extension{{Name: "a", Tools: tools("deploy")}, {Name: "b", Tools: tools("Deploy")}}, `"Deploy": extension a has that name`},
		{"a tool dax-coding has", []extension.Extension{coding.New(0), {Name: "b", Tools: tools("Read")}}, `"Read": extension dax-coding has that name`},
		{"an alias in its own tool's case", []extension.Extension{{Name: "a", Tools: tools("ship"), Aliases: map[string][]string{"Ship": {"ship"}}}}, ""},
		{"dax-coding's aliases", []extension.Extension{coding.New(0)}, ""},
		{"an alias that is another's tool", []extension.Extension{{Name: "a", Tools: tools("ship")}, {Name: "b", Tools: tools("x"), Aliases: map[string][]string{"Ship": {"x"}}}}, `alias "Ship": extension a has that name`},
		{"an alias of another's tool", []extension.Extension{{Name: "a", Tools: tools("ship")}, {Name: "b", Tools: tools("x"), Aliases: map[string][]string{"Go": {"ship"}}}}, `alias Go names "ship", which is not one of its tools`},
		{"an MCP server's name for a tool", []extension.Extension{{Name: "a", Tools: tools("mcp__fs__read")}}, "mcp__ names an MCP server's tools"},
		{"an MCP server's name, owned", []extension.Extension{{Name: "a", Owns: []string{"MCP__x"}}}, "mcp__ names an MCP server's tools"},
		{"a read-only name that is no tool", []extension.Extension{{Name: "a", Tools: tools("deploy"), ReadOnly: []string{"status"}}}, `read-only "status" is not one of its tools`},
		{"a read-only tool annotated destructive", []extension.Extension{{Name: "a", Tools: destructive, ReadOnly: []string{"wipe"}}}, `"wipe" is read-only but annotated destructive`},
		{"a destructive tool not read-only", []extension.Extension{{Name: "a", Tools: destructive}}, ""},
		{"a matcher for another's tool", []extension.Extension{{Name: "a", Tools: tools("deploy")}, {Name: "b", Matchers: extension.FixedMatchers(map[string]agentpolicy.ToolMatcher{"deploy": matcher})}}, `matcher for "deploy", which is not one of its tools`},
		{"a matcher for a tool it owns", []extension.Extension{{Name: "a", Owns: []string{"deploy"}, Matchers: extension.FixedMatchers(map[string]agentpolicy.ToolMatcher{"deploy": matcher})}}, ""},
		{"a lift of another's tool", []extension.Extension{coding.New(0), {Name: "b", Lifts: []string{"read"}}}, `lifts "read", which is not one of its tools`},
		{"a nil tool", []extension.Extension{{Name: "a", Tools: func(extension.ToolEnv) []agenttool.Tool { return []agenttool.Tool{nil} }}}, "extension a: a nil tool"},
		{"an owned name another's tool has", []extension.Extension{{Name: "a", Tools: tools("skill")}, {Name: "b", Owns: []string{"skill"}}}, `"skill": extension a has that name`},
		{"an owned name in two extensions", []extension.Extension{{Name: "a", Owns: []string{"memory_save"}}, {Name: "b", Owns: []string{"Memory_Save"}}}, `"Memory_Save": extension a has that name`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := build(tc.exts, toolEnv(t))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if a == nil {
					t.Fatal("no assembly")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one saying %q", err, tc.want)
			}
		})
	}
}

// The tools build made before it refused are closed; it owns them.
func TestARefusedBuildClosesWhatItBuilt(t *testing.T) {
	var closed atomic.Int32
	exts := []extension.Extension{
		{Name: "a", Tools: func(extension.ToolEnv) []agenttool.Tool { return []agenttool.Tool{closer("held", &closed)} }},
		{Name: "b", Tools: tools("held")},
	}
	if _, err := build(exts, toolEnv(t)); err == nil {
		t.Fatal("a clash was accepted")
	}
	if n := closed.Load(); n != 1 {
		t.Errorf("closed %d times, want once", n)
	}
}

// An extension's tools reach the main agent beside dax-coding's.
func TestAnExtensionsToolsReachTheMainAgent(t *testing.T) {
	o := options(t, &echo.Adapter{})
	o.Extensions = append(o.Extensions, extension.Extension{Name: "acme", Tools: tools("deploy", "status")})
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var names []string
	for _, tl := range s.Tools() {
		names = append(names, tl.Name)
	}
	for _, want := range []string{"read", "bash", "deploy", "status"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools %v lack %s", names, want)
		}
	}
}

// A call an extension's shipped rule allows is recorded as that
// extension's decision: the verdict in the session names its source,
// not dax-coding's.
func TestTheRecordSaysWhichExtensionsRuleAllowedACall(t *testing.T) {
	ctx := context.Background()
	var runs atomic.Int32
	o := options(t, &scripted{calls: [][2]string{{"deploy", `{"env":"staging"}`}}})
	o.Policy = confirmPolicy(t)
	o.Extensions = append(o.Extensions, extension.Extension{
		Name:   "acme",
		Tools:  func(extension.ToolEnv) []agenttool.Tool { return []agenttool.Tool{fn("deploy", &runs)} },
		Policy: policy.Rules{Allow: []string{"deploy"}},
	})
	o.Approve = func(c *openresponses.FunctionCall, _ string) bool {
		t.Errorf("asked about %s, which acme's rule allows", c.Name)
		return false
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(ctx, "ship it"); err != nil {
		t.Fatal(err)
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("deploy ran %d times, want once", n)
	}
	var found bool
	for _, line := range strings.Split(string(projected(t, o, s)), "\n") {
		if strings.Contains(line, `"tool":"deploy"`) && strings.Contains(line, `"action":"allow"`) {
			found = true
			if !strings.Contains(line, `"source":"`+policy.SourceExtension("acme")+`"`) {
				t.Errorf("the deploy verdict does not name acme's source:\n%s", line)
			}
		}
	}
	if !found {
		t.Error("the session records no verdict allowing deploy")
	}
}

// An extension's matcher reads its own tool's calls: a user's deny of
// deploy(prod) blocks prod, and staging falls to the default, which
// asks.
func TestAnExtensionsMatcherIsThePolicys(t *testing.T) {
	ctx := context.Background()
	var runs atomic.Int32
	o := options(t, &scripted{calls: [][2]string{{"deploy", `{"env":"prod"}`}, {"deploy", `{"env":"staging"}`}}})
	o.Policy = &policy.Settings{Builtin: true, Fallback: "ask", User: policy.Rules{Deny: []string{"deploy(prod)"}}}
	o.Extensions = append(o.Extensions, extension.Extension{
		Name:     "acme",
		Tools:    func(extension.ToolEnv) []agenttool.Tool { return []agenttool.Tool{fn("deploy", &runs)} },
		Matchers: extension.FixedMatchers(map[string]agentpolicy.ToolMatcher{"deploy": {Match: agentpolicy.GlobMatcher("env")}}),
	})
	var asked []string
	o.Approve = func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Arguments)
		return false
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Prompt(ctx, "ship it"); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "staging") {
		t.Errorf("asked about %q, want only the staging deploy", asked)
	}
	if n := runs.Load(); n != 0 {
		t.Errorf("deploy ran %d times; prod is denied and staging refused", n)
	}
}

// An extension's rules name its own tools only: one that would allow
// dax-coding's bash, or carve out its asks, is an error at start.
func TestAnExtensionsRuleForAnotherExtensionsToolFailsNew(t *testing.T) {
	for _, rules := range []policy.Rules{
		{Allow: []string{"bash(make:*)"}},
		{Allow: []string{"Edit"}},
		{Ask: []string{"read(!ci:.env*)"}},
	} {
		o := options(t, &echo.Adapter{})
		o.Policy = confirmPolicy(t)
		o.Extensions = append(o.Extensions, extension.Extension{Name: "acme", Tools: tools("deploy"), Policy: rules})
		if s, err := New(context.Background(), o); err == nil {
			s.Close()
			t.Errorf("%+v: accepted", rules)
		}
	}
}

// Kit options are built after every extension's tools, so an extension
// listed early sees a tool one listed after it adds, and its Env says
// what the session is.
func TestKitOptionsSeeEveryExtensionsTools(t *testing.T) {
	var all, readOnly []string
	var prompt, name, dir string
	o := options(t, &echo.Adapter{})
	o.Instructions = "Use tabs."
	early := extension.Extension{Name: "early", Instructions: "EARLY", Kit: func(e extension.Env) ([]agentkit.Option, error) {
		for _, tl := range e.Tools() {
			all = append(all, tl.Name())
		}
		for _, tl := range e.ReadOnlyTools() {
			readOnly = append(readOnly, tl.Name())
		}
		prompt, name, dir = e.SystemPrompt("early"), e.Name(), e.Dir()
		return nil, nil
	}}
	late := extension.Extension{Name: "late", Tools: tools("peek", "poke"), ReadOnly: []string{"peek"}, Instructions: "LATE"}
	o.Extensions = append([]extension.Extension{early}, append(o.Extensions, late)...)
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !slices.Contains(all, "peek") || !slices.Contains(all, "poke") || !slices.Contains(all, "read") {
		t.Errorf("Env.Tools = %v", all)
	}
	if !slices.Contains(readOnly, "peek") || slices.Contains(readOnly, "poke") || !slices.Contains(readOnly, "bash") {
		t.Errorf("Env.ReadOnlyTools = %v", readOnly)
	}
	if strings.Contains(prompt, "EARLY") || !strings.Contains(prompt, "LATE") || !strings.Contains(prompt, "Use tabs.") {
		t.Errorf("SystemPrompt(early) = %q", prompt)
	}
	if name != "dax" || dir != o.Dir {
		t.Errorf("Env names %q in %q", name, dir)
	}
}

// A kit option from an extension that sets a policy on a session with
// the policy off would decide calls the user turned it off for.
func TestAKitPolicyOnASessionWithoutOneFailsNew(t *testing.T) {
	p, err := policy.Build(policy.Settings{Fallback: "deny"})
	if err != nil {
		t.Fatal(err)
	}
	o := options(t, &echo.Adapter{})
	o.Extensions = append(o.Extensions, extension.Extension{Name: "rogue", Kit: func(extension.Env) ([]agentkit.Option, error) {
		return []agentkit.Option{agentkit.WithPolicy(p, nil)}, nil
	}})
	if s, err := New(context.Background(), o); err == nil || !strings.Contains(err.Error(), "policy") {
		if s != nil {
			s.Close()
		}
		t.Errorf("err = %v, want one about the policy", err)
	}
}

// The session's own options go to the kit after the extensions', so an
// extension's instructions option does not replace the session's
// prompt.
func TestTheSessionsInstructionsWinOverAKitOption(t *testing.T) {
	o := options(t, &echo.Adapter{})
	o.Extensions = append(o.Extensions, extension.Extension{Name: "rogue", Kit: func(extension.Env) ([]agentkit.Option, error) {
		return []agentkit.Option{agentkit.WithInstructions("HIJACKED")}, nil
	}})
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	instr := s.Agent.Config().Instructions
	if strings.Contains(instr, "HIJACKED") || !strings.Contains(instr, "You are dax") {
		t.Errorf("instructions:\n%s", instr)
	}
}

// The session closes the tools its extensions built, once, whether it
// ran or failed to start after building them.
func TestTheSessionClosesTheExtensionsTools(t *testing.T) {
	held := func(closed *atomic.Int32) extension.Extension {
		return extension.Extension{Name: "acme", Tools: func(extension.ToolEnv) []agenttool.Tool { return []agenttool.Tool{closer("held", closed)} }}
	}
	t.Run("at Close", func(t *testing.T) {
		var closed atomic.Int32
		o := options(t, &echo.Adapter{})
		o.Extensions = append(o.Extensions, held(&closed))
		s, err := New(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if n := closed.Load(); n != 0 {
			t.Fatalf("closed %d times while open", n)
		}
		s.Close()
		if n := closed.Load(); n != 1 {
			t.Errorf("closed %d times, want once", n)
		}
	})
	t.Run("when a kit option fails", func(t *testing.T) {
		var closed atomic.Int32
		o := options(t, &echo.Adapter{})
		o.Extensions = append(o.Extensions, held(&closed), extension.Extension{Name: "broken", Kit: func(extension.Env) ([]agentkit.Option, error) {
			return nil, errors.New("no")
		}})
		if s, err := New(context.Background(), o); err == nil || !strings.Contains(err.Error(), "extension broken: no") {
			if s != nil {
				s.Close()
			}
			t.Fatalf("err = %v", err)
		}
		if n := closed.Load(); n != 1 {
			t.Errorf("closed %d times, want once", n)
		}
	})
	t.Run("when the policy is refused", func(t *testing.T) {
		var closed atomic.Int32
		o := options(t, &echo.Adapter{})
		o.Policy = &policy.Settings{Builtin: true, User: policy.Rules{Allow: []string{"bash("}}}
		o.Extensions = append(o.Extensions, held(&closed))
		if s, err := New(context.Background(), o); err == nil {
			s.Close()
			t.Fatal("a bad rule was accepted")
		}
		if n := closed.Load(); n != 1 {
			t.Errorf("closed %d times, want once", n)
		}
	})
}

// An extension's BeforeToolCall is part of the policy: it runs for the
// main agent's calls when a policy is on, and not under -no-policy.
func TestAnExtensionsHookRunsOnlyWithAPolicy(t *testing.T) {
	for _, withPolicy := range []bool{true, false} {
		var mu sync.Mutex
		var seen []string
		o := options(t, &scripted{calls: [][2]string{{"probe", `{}`}}})
		o.Extensions = append(o.Extensions, extension.Extension{
			Name:   "acme",
			Tools:  tools("probe"),
			Policy: policy.Rules{Allow: []string{"probe"}},
			BeforeToolCall: extension.FixedHook(func(_ context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
				mu.Lock()
				seen = append(seen, info.Call.Name)
				mu.Unlock()
				return nil, nil
			}),
		})
		if withPolicy {
			o.Policy = confirmPolicy(t)
		}
		s, err := New(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Prompt(context.Background(), "probe"); err != nil {
			t.Fatal(err)
		}
		s.Close()
		mu.Lock()
		got := slices.Contains(seen, "probe")
		mu.Unlock()
		if got != withPolicy {
			t.Errorf("policy %v: the hook saw the call: %v", withPolicy, got)
		}
	}
}

// rooted is a workspace that says only where its root is.
type rooted struct {
	workspace.Workspace
	root string
}

func (r rooted) Root() string { return r.root }

// The main agent's prompt is the role line, the extensions'
// instructions in order, the user's, and the working directory, which
// is the workspace's root wherever that is, not the directory on this
// machine the instructions are read from.
func TestTheSystemPromptIsInOrder(t *testing.T) {
	o := Options{Dir: "/home/me/p", Instructions: "USER", Extensions: []extension.Extension{{Name: "a", Instructions: "AAA"}, {Name: "b"}, {Name: "c", Instructions: "CCC"}}}
	s := &Session{opts: o, ws: rooted{root: "/workspace"}}
	got := s.systemPrompt()
	last := -1
	for _, want := range []string{"You are dax", "AAA", "CCC", "USER", "Current working directory: /workspace"} {
		i := strings.Index(got, want)
		if i <= last {
			t.Fatalf("%q is out of order in:\n%s", want, got)
		}
		last = i
	}
	if without := s.systemPrompt("a"); strings.Contains(without, "AAA") || !strings.Contains(without, "CCC") {
		t.Errorf("systemPrompt(a):\n%s", without)
	}
}

// A program built on dax names itself in the prompt and the session
// header; with no name the session is dax's.
func TestTheProgramsName(t *testing.T) {
	for _, tc := range []struct{ name, version, wantName, wantVersion string }{
		{"", "", "dax", Version},
		{"acme", "1.2.3", "acme", "1.2.3"},
	} {
		o := options(t, &echo.Adapter{})
		o.Name, o.Version = tc.name, tc.version
		s, err := New(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if instr := s.Agent.Config().Instructions; !strings.Contains(instr, "You are "+tc.wantName+", a coding agent") {
			t.Errorf("%q: instructions:\n%s", tc.name, instr)
		}
		h := s.Kit.Session().Header()
		if h.Harness == nil || h.Harness.Name != tc.wantName || h.Harness.Version != tc.wantVersion {
			t.Errorf("harness %+v, want %s %s", h.Harness, tc.wantName, tc.wantVersion)
		}
		s.Close()
	}
}
