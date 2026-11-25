package toolrender

import (
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/agentconsole/view"
)

func bashCalls(cmds ...string) []view.Call {
	var out []view.Call
	for _, c := range cmds {
		a, _ := json.Marshal(map[string]string{"command": c})
		out = append(out, view.Call{Name: "bash", Args: string(a), State: view.CallEnded, Committed: true})
	}
	return out
}

// withOut sets the output of the calls, one each.
func withOut(calls []view.Call, outs ...string) []view.Call {
	for i := range calls {
		calls[i].Output = outs[i]
	}
	return calls
}

func readCalls(paths ...string) []view.Call {
	var out []view.Call
	for _, p := range paths {
		a, _ := json.Marshal(map[string]string{"path": p})
		out = append(out, view.Call{Name: "read", Args: string(a), State: view.CallEnded, Committed: true})
	}
	return out
}

func TestSummarizersSumUpARun(t *testing.T) {
	other := json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}}}`)
	foreign := bashCalls("ls")
	foreign[0].Schema = other
	garbled := []view.Call{{Name: "bash", Args: `{"comm`}}

	for _, tc := range []struct {
		name  string
		tool  string
		calls []view.Call
		want  string // "" is a decline
	}{
		{"one command", "bash", bashCalls("ls -la"), "ran 1 command: ls"},
		{"three commands", "bash", bashCalls("mkdir -p x", "cd x", "touch a"), "ran 3 commands: mkdir, cd, touch"},
		{"four are all listed", "bash", bashCalls("a", "b", "c", "d"), "ran 4 commands: a, b, c, d"},
		{"more than four", "bash", bashCalls("a", "b", "c", "d", "e", "f"), "ran 6 commands: a, b, c, d +2 more"},
		{"only the first line", "bash", bashCalls("git add .\nrm -rf /"), "ran 1 command: git"},
		{"assignments are not the program", "bash", bashCalls("TOKEN=hunter2 make test"), "ran 1 command: make"},
		{"an unparseable command is skipped, still counted", "bash", bashCalls("mkdir x", "$(evil) y", `"quoted" z`, "cd x"), "ran 4 commands: mkdir, cd"},
		{"control characters never show", "bash", bashCalls("ls\x1b[31m\x07 -l", "echo\x1b"), ""},
		{"a long word is cut", "bash", bashCalls("/very/long/path/to/some/program/binary --x"), "ran 1 command: /very/long/path/to/some…"},
		{"nothing to say declines", "bash", bashCalls("", "  ", "(cd x && ls)"), ""},
		{"no calls declines", "bash", nil, ""},
		{"arguments of another shape decline", "bash", garbled, ""},
		{"a schema it does not fit declines", "bash", foreign, ""},
		{"success adds nothing", "bash", withOut(bashCalls("ls", "true"), "a\n[exit 0]", "[exit 0]"), "ran 2 commands: ls, true"},
		{"no status line adds nothing", "bash", withOut(bashCalls("ls"), "a b c"), "ran 1 command: ls"},
		{"one non-zero", "bash", withOut(bashCalls("git status", "false"), "ok\n[exit 0]", "[exit 1]"), "ran 2 commands: git, false · 1 failed"},
		{"several", "bash", withOut(bashCalls("a", "b", "c"), "[exit 2]", "x\n[exit 0]", "[exit 127]\n"), "ran 3 commands: a, b, c · 2 failed"},
		{"a timeout", "bash", withOut(bashCalls("sleep 9"), "partial\n[killed after 30s]"), "ran 1 command: sleep · 1 failed"},
		{"background note, non-zero", "bash", withOut(bashCalls("x &"), "[exit 3; a process it left running in the background still holds its output and keeps running after this call; send a background job's output to a file (cmd > job.log 2>&1 &) so the call does not wait on it]"), "ran 1 command: x · 1 failed"},
		{"background note, zero", "bash", withOut(bashCalls("x &"), "[exit 0; a process it left running in the background still holds its output and keeps running after this call]"), "ran 1 command: x"},
		{"a status in the middle does not count", "bash", withOut(bashCalls("cat log"), "[exit 1]\n[killed after 5s]\nmore\n[exit 0]"), "ran 1 command: cat"},
		{"the output's text is never shown", "bash", withOut(bashCalls("cat log"), "secret-token\n[exit 4]"), "ran 1 command: cat · 1 failed"},
		{"read lists the files", "read", readCalls("/work/a.go", "b/c.go", "/etc/hosts"), "read a.go, b/c.go, /etc/hosts"},
		{"read names a file once", "read", readCalls("a.go", "a.go", "b.go"), "read a.go, b.go"},
		{"read past four", "read", readCalls("a", "b", "c", "d", "e"), "read a, b, c, d +1 more"},
		{"read with no path declines", "read", garbled, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Summarizers("/work")[tc.tool]
			line, ok := s.Summarize(tc.calls)
			if tc.want == "" {
				if ok {
					t.Fatalf("summarized as %q, want a decline", line.String())
				}
				return
			}
			if !ok || line.String() != tc.want {
				t.Errorf("got %q (%v), want %q", line.String(), ok, tc.want)
			}
		})
	}
}
