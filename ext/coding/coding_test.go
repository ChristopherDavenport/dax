package coding

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/facts/factspolicy"
)

// dax-coding is an extension like any other: its tools in the order the
// prompt names them, the ones that only look marked so, aliases and
// matchers for its own tools alone.
func TestDaxCodingIsAnExtensionOfItsOwnTools(t *testing.T) {
	ws, files := local(t, t.TempDir())
	e := New(0)
	env := extension.ToolEnv{Workspace: ws, Files: files}
	if e.Name != "dax-coding" {
		t.Errorf("name %q", e.Name)
	}
	var names []string
	for _, tl := range e.Tools(env) {
		names = append(names, tl.Name())
	}
	if want := []string{"read", "write", "edit", "glob", "grep", "ls", "bash"}; !slices.Equal(names, want) {
		t.Errorf("tools %v, want %v", names, want)
	}
	if want := []string{"read", "glob", "grep", "ls", "bash"}; !slices.Equal(e.ReadOnly, want) {
		t.Errorf("read-only %v, want %v", e.ReadOnly, want)
	}
	for alias, targets := range e.Aliases {
		for _, tg := range targets {
			if !slices.Contains(names, tg) {
				t.Errorf("alias %s names %q, not one of its tools", alias, tg)
			}
		}
	}
	for name := range e.Matchers(env) {
		if !slices.Contains(names, name) {
			t.Errorf("a matcher for %q, not one of its tools", name)
		}
	}
	for _, l := range e.Lifts {
		if !slices.Contains(names, l) {
			t.Errorf("lifts %q, not one of its tools", l)
		}
	}
	for _, w := range []string{"glob", "grep", "ls", "Read before you edit"} {
		if !strings.Contains(e.Instructions, w) {
			t.Errorf("instructions lack %q: %s", w, e.Instructions)
		}
	}
	if e.Renderers == nil || len(e.Renderers(ws.Root())) != len(names) {
		t.Errorf("renderers do not draw its %d tools", len(names))
	}
	if e.Kit != nil || len(e.Owns) != 0 {
		t.Error("dax-coding adds nothing through the kit")
	}
	// The policy reads the machine only through the tools: each claims
	// its facts, and dax-coding brings no subjects and no hook of its own.
	for _, tl := range e.Tools(env) {
		if !agenttool.IsFactual(tl) {
			t.Errorf("%s makes no facts claim", tl.Name())
		}
	}
	for name, m := range e.Matchers(env) {
		if m.Subjects != nil {
			t.Errorf("the matcher for %s brings subjects; they are the tool's claim", name)
		}
	}
	if e.BeforeToolCall != nil {
		t.Error("dax-coding ships a hook; the stamp is the bash tool's facts")
	}
}

// The stamp: every call of a tool that claims its facts runs with the
// stamp of what it was decided on, bash's the stamp of its plan. It
// decides nothing that could overrule the policy: an allow folds under
// the policy's verdict. It is the session's generic hook over the tools'
// facts claims; dax-coding ships no hook of its own.
func TestTheStampBindsEveryClaimingCall(t *testing.T) {
	dir := t.TempDir()
	ws, files := local(t, dir)
	hook := factspolicy.Hook(New(0).Tools(extension.ToolEnv{Workspace: ws, Files: files}))
	call := func(name, args string) *agentturn.ToolDecision {
		t.Helper()
		c := &openresponses.FunctionCall{Name: name, Arguments: args, CallID: "c1"}
		d, err := hook(context.Background(), agentturn.ToolCallInfo{Call: c, Args: json.RawMessage(args)})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, tc := range []struct{ name, args string }{
		{"read", `{"path":"x","dax_stamp":"deadbeef"}`},
		{"write", `{"path":"x","content":"y"}`},
	} {
		d := call(tc.name, tc.args)
		var got map[string]any
		if d != nil {
			json.Unmarshal(d.Args, &got)
		}
		if d == nil || d.Action != agentturn.Allow || got["dax_stamp"] == nil || got["dax_stamp"] == "deadbeef" || got["path"] != "x" {
			t.Errorf("%s: not stamped with its facts' stamp: %+v", tc.name, d)
		}
	}
	if d := call("deploy", `{"command":"pwd"}`); d != nil {
		t.Errorf("a tool that makes no claim: the stamp decided %+v", d)
	}
	if d := call("bash", `{"command":"pwd"}`); d == nil || d.Action != agentturn.Allow || !strings.Contains(string(d.Args), "dax_stamp") {
		t.Errorf("a read-only bash line is not stamped: %+v", d)
	}
	if d := call("bash", `{"command":"touch x","dax_stamp":"deadbeef"}`); d == nil || strings.Contains(string(d.Args), "dax_stamp") {
		t.Errorf("a stamp the model made is not taken off: %+v", d)
	}
	// A call nothing can be said about never runs on the model's own
	// arguments, the policy's verdict aside.
	if d := call("bash", `not json`); d == nil || d.Action != agentturn.Block {
		t.Errorf("arguments that are not JSON: %+v, want blocked", d)
	}
}
