package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
)

// confined builds a workspace beside a directory that holds a secret,
// with symbolic links in the workspace that lead out and one that
// stays in.
func confined(t *testing.T) (ws *Files, dir, outside string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "work")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{dir, outside, filepath.Join(dir, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("hunter2\nTODO secret\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "inside.txt"), []byte("ok\n"), 0o644))
	must(os.Symlink(outside, filepath.Join(dir, "linkdir")))                               // directory out
	must(os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "linkfile"))) // file out
	must(os.Symlink("../../outside", filepath.Join(dir, "sub", "up")))                     // relative, out
	must(os.Symlink("inside.txt", filepath.Join(dir, "alias.txt")))                        // stays in
	ws = newWS(t, dir)
	return ws, dir, outside
}

func TestFileToolsRefusePathsOutsideTheWorkspace(t *testing.T) {
	ws, dir, outside := confined(t)
	secret := filepath.Join(outside, "secret.txt")
	tests := []struct {
		name string
		tool string
		args string
	}{
		{"read absolute", "read", `{"path":"` + secret + `"}`},
		{"read dotdot", "read", `{"path":"../outside/secret.txt"}`},
		{"read dotdot after a name", "read", `{"path":"sub/../../outside/secret.txt"}`},
		{"read through a directory link", "read", `{"path":"linkdir/secret.txt"}`},
		{"read through a file link", "read", `{"path":"linkfile"}`},
		{"read through a relative link", "read", `{"path":"sub/up/secret.txt"}`},
		{"read through a link by absolute path", "read", `{"path":"` + filepath.Join(dir, "linkdir", "secret.txt") + `"}`},
		{"write absolute", "write", `{"path":"` + filepath.Join(outside, "new.txt") + `","content":"x"}`},
		{"write dotdot", "write", `{"path":"../outside/new.txt","content":"x"}`},
		{"write through a directory link", "write", `{"path":"linkdir/new.txt","content":"x"}`},
		{"write over a file link", "write", `{"path":"linkfile","content":"x"}`},
		{"write mkdir through a link", "write", `{"path":"linkdir/a/b.txt","content":"x"}`},
		{"edit through a file link", "edit", `{"path":"linkfile","old_string":"hunter2","new_string":"x"}`},
		{"edit through a directory link", "edit", `{"path":"linkdir/secret.txt","old_string":"hunter2","new_string":"x"}`},
		{"ls absolute", "ls", `{"path":"` + outside + `"}`},
		{"ls through a link", "ls", `{"path":"linkdir"}`},
		{"glob path dotdot", "glob", `{"pattern":"*","path":".."}`},
		{"glob path through a link", "glob", `{"pattern":"*","path":"linkdir"}`},
		{"grep path absolute", "grep", `{"pattern":"hunter2","path":"` + outside + `"}`},
		{"grep path through a link", "grep", `{"pattern":"hunter2","path":"linkdir"}`},
		{"grep file link", "grep", `{"pattern":"hunter2","path":"linkfile"}`},
	}
	byName := map[string]func() interface {
		Name() string
	}{}
	_ = byName
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tl := map[string]func() (string, error){
				"read":  func() (string, error) { return call(context.Background(), Read(ws), tc.args) },
				"write": func() (string, error) { return call(context.Background(), Write(ws), tc.args) },
				"edit":  func() (string, error) { return call(context.Background(), Edit(ws), tc.args) },
				"ls":    func() (string, error) { return call(context.Background(), LS(ws), tc.args) },
				"glob":  func() (string, error) { return call(context.Background(), Glob(ws), tc.args) },
				"grep":  func() (string, error) { return call(context.Background(), Grep(ws), tc.args) },
			}[tc.tool]
			out, err := tl()
			if err == nil || !strings.Contains(err.Error(), "outside the workspace") {
				t.Fatalf("err = %v, out = %q; want an outside-the-workspace refusal", err, out)
			}
			if strings.Contains(out, "hunter2") {
				t.Fatalf("leaked the secret: %q", out)
			}
		})
	}
	// Nothing was written outside.
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 {
		t.Errorf("outside directory changed: %v", entries)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(b) != "hunter2\nTODO secret\n" {
		t.Errorf("secret was modified: %q", b)
	}
}

func TestSearchSkipsLinksThatLeave(t *testing.T) {
	ws, _, _ := confined(t)
	got, err := call(context.Background(), Grep(ws), `{"pattern":"hunter2|TODO|ok"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "secret") {
		t.Errorf("grep followed a link out:\n%s", got)
	}
	if !strings.Contains(got, "inside.txt:1:ok") {
		t.Errorf("grep missed the file inside:\n%s", got)
	}
	files, _ := call(context.Background(), Glob(ws), `{"pattern":"**"}`)
	if strings.Contains(files, "secret") || strings.Contains(files, "linkdir") || strings.Contains(files, "linkfile") {
		t.Errorf("glob listed a link that leaves:\n%s", files)
	}
	ls, _ := call(context.Background(), LS(ws), `{}`)
	if !strings.Contains(ls, "linkdir@ (link outside") || !strings.Contains(ls, "linkfile@ (link outside") {
		t.Errorf("ls does not flag the links that leave:\n%s", ls)
	}
}

func TestPathsInsideTheWorkspaceWork(t *testing.T) {
	ws, dir, _ := confined(t)
	ctx := context.Background()
	for _, p := range []string{"inside.txt", "./inside.txt", "sub/../inside.txt", filepath.Join(dir, "inside.txt"), "alias.txt"} {
		out, err := call(ctx, Read(ws), `{"path":"`+p+`"}`)
		if err != nil || !strings.Contains(out, "ok") {
			t.Errorf("read %q = %q, %v", p, out, err)
		}
	}
	out, err := call(ctx, Write(ws), `{"path":"`+filepath.Join(dir, "new", "f.txt")+`","content":"x"}`)
	if err != nil || !strings.Contains(out, "new/f.txt") {
		t.Errorf("write = %q, %v", out, err)
	}
	if _, err := call(ctx, Read(ws), `{"path":""}`); err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Errorf("empty path: %v", err)
	}
}

func TestAWorkspaceReachedThroughALinkStillMatchesItsRealName(t *testing.T) {
	// The temporary directory may itself be reached through a link, as
	// /var is on macOS; its real name is the one with every link resolved.
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	via := filepath.Join(t.TempDir(), "via")
	if err := os.Symlink(real, via); err != nil {
		t.Fatal(err)
	}
	ws := newWS(t, via)
	for _, p := range []string{filepath.Join(via, "f.txt"), filepath.Join(real, "f.txt")} {
		if out, err := call(context.Background(), Read(ws), `{"path":"`+p+`"}`); err != nil || !strings.Contains(out, "hi") {
			t.Errorf("read %q = %q, %v", p, out, err)
		}
	}
}

func TestReadOnlyArgsFollowsLinksOutOfTheWorkspace(t *testing.T) {
	_, dir, _ := confined(t)
	for arg, want := range map[string]bool{"inside.txt": true, "alias.txt": true, "sub": true, "linkdir": false, "linkfile": false, "linkdir/secret.txt": false, "sub/up": false} {
		if got := inWorkspace(arg, newWS(t, dir)); got != want {
			t.Errorf("inWorkspace(%q) = %v, want %v", arg, got, want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	for in, want := range map[string]string{
		"x": "x", "./x": "x", "a/../x": "x", "/w/x": "x", "/w/./a//b/../x": "a/x", ".": ".", "/w": ".", "a/..": ".",
	} {
		if got, ok := normalizePath("/w", "", in); !ok || got != want {
			t.Errorf("normalizePath(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "..", "../x", "a/../../x", "/etc/passwd", "/wx/y", "/w/../x"} {
		if got, ok := normalizePath("/w", "", in); ok {
			t.Errorf("normalizePath(%q) = %q, want outside", in, got)
		}
	}
	if got, ok := normalizePath("/via", "/real", "/real/x"); !ok || got != "x" {
		t.Errorf("real name: %q %v", got, ok)
	}
}

// The methods a program built on dax gives its own tools refuse the
// same ways out as dax's file tools, and still reach what is inside,
// through a link that stays in.
func TestTheWorkspacesExportedMethodsRefusePathsOutside(t *testing.T) {
	ws, dir, outside := confined(t)
	secret := filepath.Join(outside, "secret.txt")
	ops := map[string]func(path string) error{
		"ReadFile":  func(p string) error { _, err := ws.ReadFile(p, 0); return err },
		"WriteFile": func(p string) error { _, err := ws.WriteFile(p, []byte("x")); return err },
		"Stat":      func(p string) error { _, err := ws.Stat(p); return err },
		"ReadDir":   func(p string) error { _, err := ws.ReadDir(p); return err },
		"Resolve":   func(p string) error { _, err := ws.Resolve(p); return err },
		"Update": func(p string) error {
			_, err := ws.Update(p, 0, func(b []byte) ([]byte, error) { return append(b, 'x'), nil })
			return err
		},
	}
	out := []string{secret, "../outside/secret.txt", "sub/../../outside/secret.txt", "linkdir/secret.txt", "linkfile", "sub/up/secret.txt", filepath.Join(dir, "linkdir", "secret.txt"), "linkdir", "linkdir/a/b.txt"}
	for name, op := range ops {
		for _, p := range out {
			t.Run(name+" "+p, func(t *testing.T) {
				err := op(p)
				if err == nil || !errors.Is(err, ErrOutside) {
					t.Errorf("%s(%q) = %v, want ErrOutside", name, p, err)
				}
			})
		}
	}
	if b, err := os.ReadFile(secret); err != nil || string(b) != "hunter2\nTODO secret\n" {
		t.Fatalf("the secret changed: %q %v", b, err)
	}
	if b, err := ws.ReadFile("alias.txt", 0); err != nil || string(b) != "ok\n" {
		t.Errorf("ReadFile through a link that stays in = %q %v", b, err)
	}
	if _, err := ws.ReadFile("inside.txt", 2); err == nil {
		t.Error("ReadFile over its limit succeeded")
	}
	if rel, err := ws.WriteFile(filepath.Join(dir, "new", "f.txt"), []byte("x")); err != nil || rel != filepath.Join("new", "f.txt") {
		t.Errorf("WriteFile inside = %q %v", rel, err)
	}
	if fi, err := ws.Stat("alias.txt"); err != nil || fi.Size() != 3 {
		t.Errorf("Stat through a link that stays in = %v %v", fi, err)
	}
	if rel, err := ws.Resolve("alias.txt"); err != nil || rel != "inside.txt" {
		t.Errorf("Resolve through a link that stays in = %q %v", rel, err)
	}
	if _, err := NewFiles(noLinks{ws.ws.(*workspace.Local)}).Resolve("alias.txt"); !errors.Is(err, ErrLinksUnknown) {
		t.Errorf("Resolve where links cannot be read = %v, want ErrLinksUnknown", err)
	}
	if es, err := ws.ReadDir("."); err != nil || len(es) == 0 {
		t.Errorf("ReadDir of the workspace = %v %v", es, err)
	}
}

// A FIFO or a device where a file or a directory should be is an error
// at once, for dax's own tools and an extension's alike: nothing blocks
// on it, and the write lock is not held hanging.
func TestTheWorkspaceDoesNotBlockOnAFIFO(t *testing.T) {
	ws, dir, _ := confined(t)
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	for name, op := range map[string]func() error{
		"WriteFile": func() error { _, err := ws.WriteFile("pipe", []byte("x")); return err },
		"ReadFile":  func() error { _, err := ws.ReadFile("pipe", 0); return err },
		"ReadDir":   func() error { _, err := ws.ReadDir("pipe"); return err },
		"Update": func() error {
			_, err := ws.Update("pipe", 0, func(b []byte) ([]byte, error) { return b, nil })
			return err
		},
		"write tool": func() error {
			_, err := call(context.Background(), Write(ws), `{"path":"pipe","content":"x"}`)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- op() }()
			select {
			case err := <-done:
				if err == nil {
					t.Error("a FIFO was taken for a regular file or a directory")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("blocked on a FIFO")
			}
		})
	}
	// The lock was not left held: a write after them all goes through.
	if _, err := ws.WriteFile("after.txt", []byte("ok")); err != nil {
		t.Fatal(err)
	}
}

// Update holds the write lock across its read and its write, so an
// update and dax's edit of one file cannot lose either change; an
// error from its function leaves the file as it was.
func TestUpdateIsOneStepAgainstOtherWrites(t *testing.T) {
	ws, dir, _ := confined(t)
	path := filepath.Join(dir, "count.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ws.Update("count.txt", 0, func(b []byte) ([]byte, error) { return append(b, 'x'), nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if b, _ := os.ReadFile(path); len(b) != 20 {
		t.Errorf("after 20 updates the file holds %d bytes, want 20: an update was lost", len(b))
	}
	if _, err := ws.Update("count.txt", 0, func([]byte) ([]byte, error) { return nil, errors.New("no") }); err == nil {
		t.Error("Update ignored its function's error")
	}
	if b, _ := os.ReadFile(path); len(b) != 20 {
		t.Errorf("a failed update changed the file to %q", b)
	}
}
