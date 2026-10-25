package executor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace/wire"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// filesBox is a server whose workspace holds a file, a directory, a
// link inside, a link out to a secret, a link to nothing, a file over
// the read bound and, where it can be made, a FIFO.
func filesBox(t *testing.T) (executorServer, string) {
	t.Helper()
	s := newServer(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret"), "SECRET=1\n")
	writeFile(t, filepath.Join(s.dir, "AGENTS.md"), "Be brief.\n")
	writeFile(t, filepath.Join(s.dir, "dir", "a.txt"), "a\n")
	writeFile(t, filepath.Join(s.dir, "dir", "sub", "b.txt"), "b\n")
	writeFile(t, filepath.Join(s.dir, "big"), strings.Repeat("x", wire.MaxFileBytes+1))
	for link, target := range map[string]string{
		"in":      "AGENTS.md",
		"out":     filepath.Join(outside, "secret"),
		"climb":   "../" + filepath.Base(outside) + "/secret",
		"nowhere": "missing",
	} {
		if err := os.Symlink(target, filepath.Join(s.dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	syscall.Mkfifo(filepath.Join(s.dir, "fifo"), 0o644)
	return s, outside
}

// The client reads the executor's workspace through its file system:
// stat follows a link that stays inside, lstat and readlink show it, a
// listing is sorted and typed, and every way out (a link, "..", an
// absolute name) is workspace.ErrOutside, as it is in the executor; an
// invalid name is fs.ErrInvalid and a missing one fs.ErrNotExist. A
// read over MaxFileBytes, of a FIFO or of a directory is refused.
func TestTheClientReadsTheExecutorsFiles(t *testing.T) {
	s, _ := filesBox(t)
	r, _ := s.remote(t)
	fsys := r.FS()
	for _, tc := range []struct {
		name    string
		do      func() (string, error)
		want    string // the result, when no error
		wantErr error  // errors.Is; nil with errText: any error
		errText string
	}{
		{"read a file", func() (string, error) { b, err := fs.ReadFile(fsys, "AGENTS.md"); return string(b), err }, "Be brief.\n", nil, ""},
		{"read through a link inside", func() (string, error) { b, err := fs.ReadFile(fsys, "in"); return string(b), err }, "Be brief.\n", nil, ""},
		{"open and read", func() (string, error) {
			f, err := fsys.Open("dir/a.txt")
			if err != nil {
				return "", err
			}
			defer f.Close()
			b := make([]byte, 10)
			n, err := f.Read(b)
			return string(b[:n]), err
		}, "a\n", nil, ""},
		{"stat follows a link inside", func() (string, error) {
			fi, err := fs.Stat(fsys, "in")
			if err != nil {
				return "", err
			}
			return fi.Name() + " " + fi.Mode().Type().String(), nil
		}, "in ----------", nil, ""},
		{"lstat shows the link", func() (string, error) {
			fi, err := fs.Lstat(fsys, "out")
			if err != nil {
				return "", err
			}
			return fi.Mode().Type().String(), nil
		}, "L---------", nil, ""},
		{"readlink", func() (string, error) { return fs.ReadLink(fsys, "in") }, "AGENTS.md", nil, ""},
		{"a listing, sorted and typed", func() (string, error) {
			es, err := fs.ReadDir(fsys, ".")
			var out []string
			for _, e := range es {
				out = append(out, e.Name()+":"+e.Type().String())
			}
			return strings.Join(out, " "), err
		}, "", nil, ""}, // checked below
		{"stat a link out", func() (string, error) { _, err := fs.Stat(fsys, "out"); return "", err }, "", workspace.ErrOutside, ""},
		{"read a link out", func() (string, error) { _, err := fs.ReadFile(fsys, "out"); return "", err }, "", workspace.ErrOutside, ""},
		{"read a relative link that climbs out", func() (string, error) { _, err := fs.ReadFile(fsys, "climb"); return "", err }, "", workspace.ErrOutside, ""},
		{"open a link out", func() (string, error) { _, err := fsys.Open("out"); return "", err }, "", workspace.ErrOutside, ""},
		{"..", func() (string, error) { _, err := fs.ReadFile(fsys, "../secret"); return "", err }, "", workspace.ErrOutside, ""},
		{"an absolute name", func() (string, error) { _, err := fs.Stat(fsys, "/etc/passwd"); return "", err }, "", workspace.ErrOutside, ""},
		{"an invalid name", func() (string, error) { _, err := fs.Stat(fsys, "dir/../AGENTS.md"); return "", err }, "", fs.ErrInvalid, ""},
		{"a missing name", func() (string, error) { _, err := fs.Stat(fsys, "nothing"); return "", err }, "", fs.ErrNotExist, ""},
		{"a link to nothing", func() (string, error) { _, err := fs.Stat(fsys, "nowhere"); return "", err }, "", fs.ErrNotExist, ""},
		{"readlink of a file", func() (string, error) { return fs.ReadLink(fsys, "AGENTS.md") }, "", nil, "invalid argument"},
		{"a read over the bound", func() (string, error) { _, err := fs.ReadFile(fsys, "big"); return "", err }, "", nil, "over the 1048576-byte limit"},
		{"a read of a directory", func() (string, error) { _, err := fs.ReadFile(fsys, "dir"); return "", err }, "", nil, "not a regular file"},
		{"a read of an opened directory", func() (string, error) {
			f, err := fsys.Open("dir")
			if err != nil {
				return "", err
			}
			defer f.Close()
			_, err = f.Read(make([]byte, 1))
			return "", err
		}, "", nil, "is a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.do()
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.errText != "":
				if err == nil || !strings.Contains(err.Error(), tc.errText) {
					t.Errorf("err = %v, want %q", err, tc.errText)
				}
			case err != nil:
				t.Fatal(err)
			case tc.want != "" && got != tc.want:
				t.Errorf("got %q, want %q", got, tc.want)
			case tc.want == "":
				for _, w := range []string{"AGENTS.md:----------", "big:", "climb:L---------", "dir:d---------", "in:L---------", "out:L---------"} {
					if !strings.Contains(got, w) {
						t.Errorf("listing %q lacks %q", got, w)
					}
				}
				if !strings.HasPrefix(got, "AGENTS.md:") {
					t.Errorf("listing %q is not sorted", got)
				}
			}
		})
	}
	if _, err := os.Lstat(filepath.Join(s.dir, "fifo")); err == nil {
		start := time.Now()
		if _, err := fs.ReadFile(fsys, "fifo"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("a FIFO: %v", err)
		}
		if time.Since(start) > 10*time.Second {
			t.Error("the read of a FIFO waited on it")
		}
	}
	// A directory's file system over the wire behaves as io/fs says.
	sub, err := fs.Sub(fsys, "dir")
	if err != nil {
		t.Fatal(err)
	}
	if err := fstest.TestFS(sub, "a.txt", "sub/b.txt"); err != nil {
		t.Error(err)
	}
}

// A request the executor does not answer in time fails, as does one to
// an executor that is gone; neither waits on.
func TestAFilesRequestThatCannotBeAnsweredFails(t *testing.T) {
	old := filesTimeout
	filesTimeout = 100 * time.Millisecond
	t.Cleanup(func() { filesTimeout = old })
	s, _ := filesBox(t)
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	var stalled atomic.Bool
	s.srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "resources/read" && stalled.Load() {
				select {
				case <-stall:
				case <-ctx.Done():
				}
			}
			return next(ctx, method, req)
		}
	})
	r, ss := s.remote(t)
	if _, err := fs.ReadFile(r.FS(), "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	stalled.Store(true)
	start := time.Now()
	if _, err := fs.Stat(r.FS(), "AGENTS.md"); err == nil || !strings.Contains(err.Error(), "executor") {
		t.Errorf("a stalled executor: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("waited %v", d)
	}
	stalled.Store(false)
	ss.Close()
	if _, err := fs.ReadFile(r.FS(), "AGENTS.md"); err == nil || !strings.Contains(err.Error(), "executor") {
		t.Errorf("an executor that is gone: %v", err)
	}
}
