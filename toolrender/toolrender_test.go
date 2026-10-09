package toolrender

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentconsole/view"
	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"

	"github.com/ChristopherDavenport/dax/tool"
)

// files is the project every case's tools run in.
var files = map[string]string{
	"a.go":      "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7\nline 8\nline 9\nline 10\n",
	"sub/b.go":  "package b\n\nfunc Steer() {}\n",
	"sub/c.txt": "steer here\n",
}

func project(t *testing.T) (string, *tool.Files) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	return dir, tool.NewFiles(ws)
}

// run runs a call of tl and returns it as the record holds it once it
// ended: the output the model saw, an error as the loop writes one.
func run(t *testing.T, tl agenttool.Tool, args string) view.Call {
	t.Helper()
	res, err := tl.Execute(context.Background(), agenttool.Call{ID: "call_1", Args: json.RawMessage(args)})
	if err != nil {
		res = agenttool.ErrorResult(err)
	}
	return view.Call{CallID: "call_1", Name: tl.Name(), Args: args, Schema: tl.Parameters(), State: view.CallEnded, Output: tool.Text(res), Committed: true}
}

// drawn is a body as text, or nil when the renderer declined.
func drawn(ls []toolview.Line, ok bool) []string {
	if !ok {
		return nil
	}
	out := []string{}
	for _, l := range ls {
		out = append(out, l.String())
	}
	return out
}

func head(r toolview.Renderer, c view.Call) string {
	l, ok := r.Head(c)
	if !ok {
		return "(declined)"
	}
	return l.String()
}

func TestRenderersDrawWhatTheToolsWrote(t *testing.T) {
	for _, c := range []struct {
		name, tool, args string
		head             string
		body, expanded   []string // nil: the renderer declines
	}{
		{name: "read a range", tool: "read", args: `{"path":"a.go","offset":3,"limit":4}`,
			head: "a.go:3-6", body: []string{"· 4 lines (4 more lines; use offset=7)"}},
		{name: "read a whole file", tool: "read", args: `{"path":"sub/b.go"}`,
			head: "sub/b.go", body: []string{"· 3 lines"}},
		{name: "read past the end", tool: "read", args: `{"path":"a.go","offset":40}`,
			head: "a.go:40-", body: []string{"(file has 10 lines; offset 40 is past the end)"}},
		{name: "write", tool: "write", args: `{"path":"new.go","content":"a\nb\n"}`,
			head: "new.go (2 lines)", body: []string{}, expanded: []string{"+ a", "+ b"}},
		{name: "edit", tool: "edit", args: `{"path":"a.go","old_string":"line 3\nline 4","new_string":"line 3\nLINE 4\nline 4b"}`,
			head: "a.go +2 −1", body: []string{"  line 3", "- line 4", "+ LINE 4", "+ line 4b"},
			expanded: []string{"  line 3", "- line 4", "+ LINE 4", "+ line 4b"}},
		{name: "edit that misses", tool: "edit", args: `{"path":"a.go","old_string":"nope","new_string":"yes"}`,
			head: "a.go +1 −1", body: []string{"old_string not found in file"},
			expanded: []string{"old_string not found in file", "- nope", "+ yes"}},
		{name: "glob", tool: "glob", args: `{"pattern":"**/*.go"}`,
			head: "**/*.go", body: []string{"· 2 files"}},
		{name: "glob in a directory", tool: "glob", args: `{"pattern":"*.txt","path":"sub"}`,
			head: "*.txt in sub", body: []string{"· 1 file"}},
		{name: "glob that finds nothing", tool: "glob", args: `{"pattern":"*.rs"}`,
			head: "*.rs", body: []string{"· no files match"}},
		{name: "glob stopped at its limit", tool: "glob", args: `{"pattern":"**/*","max_results":2}`,
			head: "**/*", body: []string{"· 2 files (stopped at 2)"}},
		{name: "grep", tool: "grep", args: `{"pattern":"[Ss]teer"}`,
			head: "/[Ss]teer/", body: []string{"· 2 matches in 2 files"}},
		{name: "grep narrowed", tool: "grep", args: `{"pattern":"steer","include":"*.go","ignore_case":true}`,
			head: "/steer/ (*.go, -i)", body: []string{"· 1 match in 1 file"}},
		{name: "grep that finds nothing", tool: "grep", args: `{"pattern":"zebra"}`,
			head: "/zebra/", body: []string{"· no matches"}},
		{name: "grep with a bad pattern", tool: "grep", args: `{"pattern":"("}`,
			head: "/(/", body: []string{"bad pattern: error parsing regexp: missing closing ): `(`"},
			expanded: []string{"bad pattern: error parsing regexp: missing closing ): `(`"}},
		{name: "ls", tool: "ls", args: `{}`,
			head: ".", body: []string{"· 2 entries"}},
		{name: "ls a directory", tool: "ls", args: `{"path":"sub"}`,
			head: "sub", body: []string{"· 2 entries"}},
		{name: "bash", tool: "bash", args: `{"command":"echo hi"}`,
			head: "$ echo hi", body: []string{"· 1 line (ctrl+o)"}, expanded: []string{"hi", "exit 0"}},
		{name: "bash that prints nothing", tool: "bash", args: `{"command":"true"}`,
			head: "$ true", body: []string{}, expanded: []string{"exit 0"}},
		{name: "bash that fails", tool: "bash", args: `{"command":"printf 'a\\nb\\nc\\nd\\nError: boom\\n'; exit 3"}`,
			head:     `$ printf 'a\nb\nc\nd\nError: boom\n'; exit 3`,
			body:     []string{"exit 3", "… 2 lines before", "c", "d", "Error: boom"},
			expanded: []string{"a", "b", "c", "d", "Error: boom", "exit 3"}},
		{name: "bash killed", tool: "bash", args: `{"command":"sleep 5","timeout_seconds":1}`,
			head: "$ sleep 5 (timeout 1s)", body: []string{"killed after 1s"}, expanded: []string{"killed after 1s"}},
		{name: "bash with no command", tool: "bash", args: `{"command":" "}`,
			head: "(declined)", body: []string{"command is required"}, expanded: []string{"command is required"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, ws := project(t)
			tools := map[string]agenttool.Tool{}
			for _, tl := range tool.Builtins(ws, 1<<20) {
				tools[tl.Name()] = tl
			}
			r := Renderers(dir)[c.tool]
			call := run(t, tools[c.tool], c.args)
			if got := head(r, call); got != c.head {
				t.Errorf("head = %q, want %q (output %q)", got, c.head, call.Output)
			}
			if got := drawn(r.Body(call, false)); !slices.Equal(got, c.body) {
				t.Errorf("body = %q, want %q (output %q)", got, c.body, call.Output)
			}
			if got := drawn(r.Body(call, true)); !slices.Equal(got, c.expanded) {
				t.Errorf("expanded = %q, want %q (output %q)", got, c.expanded, call.Output)
			}
		})
	}
}

func TestAHeadNamesThePathWhileTheArgumentsStream(t *testing.T) {
	rs := Renderers("/work")
	write := view.Call{Name: "write", Args: `{"path":"/work/internal/x.go","content":"package x\nfunc`, State: view.CallOpen}
	if got := head(rs["write"], write); got != "internal/x.go" {
		t.Errorf("write streaming: %q", got)
	}
	if got := drawn(rs["write"].Body(write, true)); got != nil {
		t.Errorf("write streaming has a body: %q", got)
	}
	edit := view.Call{Name: "edit", Args: `{"path":"a.go","old_string":"x","new_str`, State: view.CallOpen}
	if got := head(rs["edit"], edit); got != "a.go" {
		t.Errorf("edit streaming: %q", got)
	}
	if got := head(rs["bash"], view.Call{Name: "bash", Args: `{"command":"go te`, State: view.CallOpen}); got != "(declined)" {
		t.Errorf("bash streaming: %q", got)
	}
	if got := head(rs["read"], view.Call{Name: "read", Args: `{"pa`, State: view.CallOpen}); got != "(declined)" {
		t.Errorf("read streaming: %q", got)
	}
}

func TestAPathOutsideTheWorkspaceStaysAsGiven(t *testing.T) {
	rs := Renderers("/work")
	for p, want := range map[string]string{"/work/a.go": "a.go", "/work": ".", "/workshop/a.go": "/workshop/a.go", "/etc/passwd": "/etc/passwd", "../x": "../x"} {
		if got := head(rs["read"], view.Call{Name: "read", Args: `{"path":"` + p + `"}`}); got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
}

func TestAToolOfAnotherShapeIsDeclined(t *testing.T) {
	rs := Renderers("")
	other := json.RawMessage(`{"type":"object","properties":{"file":{"type":"string"}}}`)
	c := view.Call{Name: "read", Args: `{"path":"a.go"}`, Schema: other, State: view.CallEnded, Output: "     1\tx\n"}
	if _, ok := rs["read"].Head(c); ok {
		t.Error("read drew a call against another schema")
	}
	// A sub-agent's call carries no schema, and is drawn.
	c.Schema = nil
	if got := head(rs["read"], c); got != "a.go" {
		t.Errorf("a child call: %q", got)
	}
}

func TestACallWaitingOnAPermissionShowsWhatItWouldDo(t *testing.T) {
	rs := Renderers("")
	edit := view.Call{Name: "edit", Args: `{"path":"a.go","old_string":"a\nb","new_string":"a\nc"}`, State: view.CallDeferred}
	if got, want := drawn(rs["edit"].Body(edit, false)), []string{"  a", "- b", "+ c"}; !slices.Equal(got, want) {
		t.Errorf("edit: %q, want %q", got, want)
	}
	bash := view.Call{Name: "bash", Args: `{"command":"cd sub &&\n  go test ./...\n  go vet ./..."}`, State: view.CallDeferred}
	if got, want := head(rs["bash"], bash), "$ cd sub && (+2 lines)"; got != want {
		t.Errorf("bash head: %q, want %q", got, want)
	}
	if got, want := drawn(rs["bash"].Body(bash, false)), []string{"    go test ./...", "    go vet ./..."}; !slices.Equal(got, want) {
		t.Errorf("bash body: %q, want %q", got, want)
	}
	// Running, the client shows what the command prints.
	bash.State, bash.Partial = view.CallRunning, "ok\n"
	if got := drawn(rs["bash"].Body(bash, false)); got != nil {
		t.Errorf("a running command has a body: %q", got)
	}
}

func TestASubAgentShowsItsBriefAndTheStartOfItsReport(t *testing.T) {
	rs := SubAgents()
	c := view.Call{Name: "task", State: view.CallRunning,
		Args: `{"input":"Rename Steer to Nudge in internal/agent.\nKeep the tests passing.","context":"fork","model":"main"}`}
	if got, want := head(rs["task"], c), "Rename Steer to Nudge in internal/agent. (fork, main model)"; got != want {
		t.Errorf("head: %q, want %q", got, want)
	}
	if got := drawn(rs["task"].Body(c, false)); got != nil {
		t.Errorf("a running task has a body: %q", got)
	}
	c.State, c.Output = view.CallEnded, "Done.\n\nRenamed it in 4 files.\nThe tests pass.\n"
	if got, want := drawn(rs["task"].Body(c, false)), []string{"Done.", "Renamed it in 4 files.", "… 1 more line (ctrl+o)"}; !slices.Equal(got, want) {
		t.Errorf("report: %q, want %q", got, want)
	}
	if got := drawn(rs["task"].Body(c, true)); got != nil {
		t.Errorf("expanded, the client shows the report: %q", got)
	}
	long := view.Call{Name: "explore", Args: `{"input":"` + strings.Repeat("x", 200) + `"}`}
	if got := head(rs["explore"], long); len([]rune(got)) != headRunes {
		t.Errorf("a long brief is not cut to %d: %d", headRunes, len([]rune(got)))
	}
}

func TestDiff(t *testing.T) {
	for _, c := range []struct {
		name     string
		old, new string
		want     []string
	}{
		{"a line changed", "a\nb\nc", "a\nB\nc", []string{"  a", "- b", "+ B", "  c"}},
		{"a line added", "a\nc", "a\nb\nc", []string{"  a", "+ b", "  c"}},
		{"a line removed", "a\nb\nc", "a\nc", []string{"  a", "- b", "  c"}},
		{"kept lines between changes", "x\na\ny", "X\na\nY", []string{"- x", "+ X", "  a", "- y", "+ Y"}},
		{"from nothing", "", "a", []string{"+ a"}},
		{"to nothing", "a", "", []string{"- a"}},
	} {
		got := drawn(diffLines(diff(lines(c.old), lines(c.new)), true), true)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestACollapsedDiffKeepsToTheChange(t *testing.T) {
	var old, new []string
	for i := range 30 {
		old = append(old, "same")
		new = append(new, "same")
		if i == 14 {
			for j := range 12 {
				new = append(new, "added "+string(rune('a'+j)))
			}
		}
	}
	got := drawn(diffLines(diff(old, new), false), true)
	want := []string{"  same", "+ added a", "+ added b", "+ added c", "+ added d", "+ added e", "+ added f", "+ added g", "… 6 more lines (ctrl+o)"}
	if !slices.Equal(got, want) {
		t.Errorf("collapsed: %q, want %q", got, want)
	}
	if n := len(diffLines(diff(old, new), true)); n != 42 {
		t.Errorf("expanded: %d lines, want all 42", n)
	}
}

func TestABigDiffIsRemovedThenAdded(t *testing.T) {
	var old, new []string
	for i := range 600 {
		old = append(old, "o"+string(rune(i)))
		new = append(new, "n"+string(rune(i)))
	}
	ops := diff(old, new)
	if a, r := counts(ops); a != 600 || r != 600 || ops[0].kind != '-' || ops[600].kind != '+' {
		t.Errorf("a diff past maxDiffCells: +%d -%d, first %c, 601st %c", a, r, ops[0].kind, ops[600].kind)
	}
}

func TestParseArgs(t *testing.T) {
	for raw, want := range map[string]struct {
		keys     []string
		complete bool
	}{
		`{"a":"x","b":1}`:          {[]string{"a", "b"}, true},
		`{"a":"x","b":[1,2`:        {[]string{"a"}, false},
		`{"a":"x","b":{"c":1},"d"`: {[]string{"a", "b"}, false},
		`{"a":"x`:                  {nil, false},
		``:                         {nil, false},
		`[1]`:                      {nil, false},
		`not json`:                 {nil, false},
	} {
		a := parseArgs(raw)
		var keys []string
		for k := range a.fields {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, want.keys) || a.complete != want.complete {
			t.Errorf("%q: %v complete %v, want %v complete %v", raw, keys, a.complete, want.keys, want.complete)
		}
	}
}

func TestTheLinesCarryTheirRoles(t *testing.T) {
	rs := Renderers("")
	roles := func(l toolview.Line) []toolview.Role {
		var out []toolview.Role
		for _, s := range l {
			out = append(out, s.Role)
		}
		return out
	}
	edit := view.Call{Name: "edit", Args: `{"path":"a.go","old_string":"a\nb","new_string":"a\nc"}`, State: view.CallEnded, Output: "edited a.go"}
	h, _ := rs["edit"].Head(edit)
	if got, want := roles(h), []toolview.Role{toolview.Emphasis, toolview.Plain, toolview.Added, toolview.Plain, toolview.Removed}; !slices.Equal(got, want) {
		t.Errorf("edit head roles %v, want %v", got, want)
	}
	body, _ := rs["edit"].Body(edit, false)
	var got []toolview.Role
	for _, l := range body {
		got = append(got, roles(l)...)
	}
	if want := []toolview.Role{toolview.Dim, toolview.Removed, toolview.Added}; !slices.Equal(got, want) {
		t.Errorf("edit body roles %v, want %v", got, want)
	}
	bash := view.Call{Name: "bash", Args: `{"command":"false"}`, State: view.CallEnded, Output: "[exit 1]"}
	if body, _ := rs["bash"].Body(bash, false); len(body) != 1 || body[0][0].Role != toolview.Error {
		t.Errorf("a failing command's status is not an error: %v", body)
	}
	bash.Output = "[exit 0]"
	if body, _ := rs["bash"].Body(bash, true); len(body) != 1 || body[0][0].Role != toolview.Dim {
		t.Errorf("a passing command's status is not dim: %v", body)
	}
	failed := view.Call{Name: "read", Args: `{"path":"a.go"}`, State: view.CallEnded, Output: "Error: open a.go: no such file"}
	if body, _ := rs["read"].Body(failed, false); len(body) != 1 || body[0][0].Role != toolview.Error {
		t.Errorf("a failed read is not an error: %v", body)
	}
}

// dax-coding's renderers and the sub-agents' are separate sets, for
// two extensions, and draw no tool in common.
func TestTheRendererSetsAreSeparate(t *testing.T) {
	coding, agents := Renderers(""), SubAgents()
	for name := range agents {
		if _, ok := coding[name]; ok {
			t.Errorf("both sets draw %s", name)
		}
	}
	for _, name := range []string{"read", "write", "edit", "glob", "grep", "ls", "bash"} {
		if coding[name] == nil {
			t.Errorf("no renderer for %s", name)
		}
	}
	for _, name := range []string{"task", "explore"} {
		if agents[name] == nil {
			t.Errorf("no renderer for %s", name)
		}
	}
}
