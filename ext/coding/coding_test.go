package coding

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
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
}

// The stamp touches a bash call alone, and decides nothing that could
// overrule the policy: an allow folds under the policy's verdict.
func TestTheStampTouchesOnlyBash(t *testing.T) {
	dir := t.TempDir()
	ws, files := local(t, dir)
	hook := New(0).BeforeToolCall(extension.ToolEnv{Workspace: ws, Files: files})
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
		{"deploy", `{"command":"pwd"}`},
	} {
		if d := call(tc.name, tc.args); d != nil {
			t.Errorf("%s: the stamp decided %+v", tc.name, d)
		}
	}
	if d := call("bash", `{"command":"pwd"}`); d == nil || d.Action != agentturn.Allow || !strings.Contains(string(d.Args), "dax_stamp") {
		t.Errorf("a read-only bash line is not stamped: %+v", d)
	}
	if d := call("bash", `{"command":"touch x","dax_stamp":"deadbeef"}`); d == nil || strings.Contains(string(d.Args), "dax_stamp") {
		t.Errorf("a stamp the model made is not taken off: %+v", d)
	}
	if d := call("bash", `not json`); d != nil {
		t.Errorf("arguments that are not JSON: %+v, want left to the tool", d)
	}
}
