package agent

import (
	"context"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/dax/internal/executor"
)

// DialExecutorURL reaches `dax execute -listen` over HTTP and checks it
// as DialExecutor does; nothing listening there is an error that says
// so.
func TestDialExecutorURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b := newRemoteBox(t)
	os.WriteFile(filepath.Join(b.dir, "notes.txt"), []byte("notes\n"), 0o644)
	done := make(chan struct{})
	ts := httptest.NewServer(executor.Handler(b.srv, done))
	t.Cleanup(func() {
		close(done)
		ts.CloseClientConnections()
		ts.Close()
	})
	ex, err := DialExecutorURL(ctx, ts.URL, ExecutorOptions{Name: "dax", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	if name, version := ex.Server(); name != "dax" || version != "box" {
		t.Errorf("server %s %s", name, version)
	}
	if root := ex.Workspace().Root(); root != "/work" {
		t.Errorf("root %q", root)
	}
	if got, err := fs.ReadFile(ex.Workspace().FS(), "notes.txt"); err != nil || string(got) != "notes\n" {
		t.Errorf("a file through it: %q, %v", got, err)
	}

	gone := httptest.NewServer(nil)
	url := gone.URL
	gone.Close()
	if _, err := DialExecutorURL(ctx, url, ExecutorOptions{Name: "dax", Version: "test"}); err == nil || !strings.Contains(err.Error(), "executor") {
		t.Errorf("nothing listening: %v", err)
	}
}
