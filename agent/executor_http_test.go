package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"

	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/tool"
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

// ExecutorOptions.MaxReadBytes is the session's read limit as the
// executor's tools take it, capped at the executor's own; a cap below
// it is a warning naming the flag that would lift it.
func TestTheExecutorsToolsReadTheSessionsLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		asked int64
		cut   int64  // where read stops in big.txt
		warns string // "" for none
	}{
		{"below the default", 4096, 4096, ""},
		{"the default", 0, tool.DefaultMaxRead, ""},
		{"above the box's ceiling", 3 << 20, tool.DefaultMaxRead, "pass `dax execute -max-read-bytes 3145728` where it runs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got syncBuffer
			defer CaptureWarnings(&got)()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			b := newRemoteBox(t)
			const size = 3 << 20 // a multiple of the line's 64 bytes
			os.WriteFile(filepath.Join(b.dir, "big.txt"), []byte(strings.Repeat(strings.Repeat("x", 63)+"\n", size/64)), 0o644)
			done := make(chan struct{})
			ts := httptest.NewServer(executor.Handler(b.srv, done))
			t.Cleanup(func() {
				close(done)
				ts.CloseClientConnections()
				ts.Close()
			})
			ex, err := DialExecutorURL(ctx, ts.URL, ExecutorOptions{Name: "dax", Version: "test", MaxReadBytes: tc.asked})
			if err != nil {
				t.Fatal(err)
			}
			defer ex.Close()
			res, err := ex.r.Call(ctx, executor.Call{Tool: "read", Call: agenttool.Call{Args: json.RawMessage(`{"path":"big.txt"}`)}})
			if err != nil {
				t.Fatal(err)
			}
			if out, want := tool.Text(res), fmt.Sprintf("stopped after the first %d of %d bytes", tc.cut, size); !strings.Contains(out, want) {
				t.Errorf("read: %q, want %q", out[max(0, len(out)-200):], want)
			}
			switch w := got.String(); {
			case tc.warns == "" && w != "":
				t.Errorf("warned %q", w)
			case tc.warns != "" && (!strings.HasPrefix(w, "dax: max_read_bytes is 3145728, but the executor (dax box) caps it at") || !strings.Contains(w, tc.warns)):
				t.Errorf("warned %q, want it to say %q", w, tc.warns)
			}
		})
	}
}
