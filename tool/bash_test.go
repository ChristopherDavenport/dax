package tool

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
)

// runBash runs one bash call, collecting the progress it reports: the
// windows of output as they arrive, before the final result.
func runBash(t *testing.T, dir, command string) (partial []string, result string, err error) {
	t.Helper()
	var mu sync.Mutex
	args, _ := json.Marshal(map[string]string{"command": command})
	b := Bash(newWS(t, dir))
	c := agenttool.Call{
		ID:   "call_1",
		Args: args,
		OnUpdate: func(r agenttool.Result) {
			mu.Lock()
			partial = append(partial, r.Output.String())
			mu.Unlock()
		},
	}
	res, err := b.Execute(context.Background(), c)
	mu.Lock()
	defer mu.Unlock()
	return partial, Text(res), err
}

func TestBashReportsItsOutputWhileItRuns(t *testing.T) {
	// The first line arrives long before the command ends, so it is
	// reported while the call runs and again in the final result; a
	// front sees the command working instead of silent for the second
	// it takes.
	partial, result, err := runBash(t, t.TempDir(), "echo one; sleep 1; echo two")
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) == 0 {
		t.Fatal("no progress reported while the command ran")
	}
	if !strings.Contains(partial[0], "one") || strings.Contains(partial[0], "two") {
		t.Errorf("the first report is not the first output:\n%q", partial)
	}
	if !strings.Contains(result, "one") || !strings.Contains(result, "two") || !strings.Contains(result, "[exit 0]") {
		t.Errorf("the result is not the whole output:\n%q", result)
	}
}

func TestBashProgressIsWindowedAndBounded(t *testing.T) {
	// A command that prints for a while reports every progressEvery,
	// and no report holds more than progressBytes.
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString("echo line; sleep 0.05; ")
	}
	partial, _, err := runBash(t, t.TempDir(), strings.TrimSuffix(b.String(), "; "))
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) < 2 {
		t.Errorf("got %d reports over a second of output, want the first and at least one more", len(partial))
	}
	for i, p := range partial {
		if len(p) > progressBytes {
			t.Errorf("report %d holds %d bytes, want at most %d", i, len(p), progressBytes)
		}
	}
	if !strings.Contains(partial[len(partial)-1], "line") {
		t.Errorf("the last report is not the command's output:\n%q", partial[len(partial)-1])
	}
}

// A workspace with no bash says so as the call's output, which the
// model reads and can act on (the file tools still work), rather than
// failing every call with exec's error (#143). No sh fallback: the
// analyzer and the stamp read bash's grammar.
func TestBashSaysWhenTheWorkspaceHasNoBash(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, result, err := runBash(t, t.TempDir(), "ls")
	if err != nil {
		t.Fatalf("the call failed: %v", err)
	}
	if !strings.Contains(result, "this workspace has no bash") || !strings.Contains(result, "read, write and edit still work") {
		t.Errorf("the model is told %q", result)
	}
}
