package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// confined builds a workspace beside a directory that holds a secret,
// with symbolic links in the workspace that lead out and one that
// stays in.
func confined(t *testing.T) (ws *Workspace, dir, outside string) {
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
		if got := inWorkspace(arg, dir); got != want {
			t.Errorf("inWorkspace(%q) = %v, want %v", arg, got, want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	for in, want := range map[string]string{
		"x": "x", "./x": "x", "a/../x": "x", "/w/x": "x", "/w/./a//b/../x": "a/x", ".": ".", "/w": ".", "a/..": ".",
	} {
		if got, ok := NormalizePath("/w", "", in); !ok || got != want {
			t.Errorf("NormalizePath(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "..", "../x", "a/../../x", "/etc/passwd", "/wx/y", "/w/../x"} {
		if got, ok := NormalizePath("/w", "", in); ok {
			t.Errorf("NormalizePath(%q) = %q, want outside", in, got)
		}
	}
	if got, ok := NormalizePath("/via", "/real", "/real/x"); !ok || got != "x" {
		t.Errorf("real name: %q %v", got, ok)
	}
}
