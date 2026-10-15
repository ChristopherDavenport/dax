package agent

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"

	"github.com/ChristopherDavenport/dax/internal/executor"
)

// DialExecutor's command is started again when the process it started
// dies: a read of the workspace's files after the kill goes on over a
// new process of the same executor, a call runs there, and the
// reconnection is one of dax's warnings.
func TestAKilledExecutorIsStartedAgain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("notes\n"), 0o644))
	t.Setenv(executorRoot, root)
	var held syncBuffer
	defer CaptureWarnings(&held)()
	ex, err := DialExecutor(ctx, []string{os.Args[0]}, ExecutorOptions{Name: "dax", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	// pid is the executor's process, the parent of bash's.
	pid := func() int {
		t.Helper()
		res, err := ex.r.Call(ctx, executor.Call{Tool: "bash", Call: agenttool.Call{ID: "b", Args: json.RawMessage(`{"command":"echo $PPID"}`)}})
		if err != nil {
			t.Fatal(err)
		}
		line, _, _ := strings.Cut(res.Output.String(), "\n")
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("bash said %q", res.Output.String())
		}
		return n
	}
	first := pid()
	must(t, syscall.Kill(first, syscall.SIGKILL))
	if b, err := fs.ReadFile(ex.Workspace().FS(), "notes.txt"); err != nil || string(b) != "notes\n" {
		t.Fatalf("a read after the kill: %q, %v", b, err)
	}
	if second := pid(); second == first {
		t.Errorf("the call ran in the killed process %d", first)
	}
	if got := held.String(); !strings.Contains(got, "dax: executor connection lost; reconnected to dax child") {
		t.Errorf("warned %q", got)
	}
}
