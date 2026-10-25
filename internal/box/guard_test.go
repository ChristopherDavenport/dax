package box

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.invalid",
		"GIT_ALLOW_PROTOCOL=file")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// The guard holds read-only what git on the box host's machine trusts
// and runs in the project: config and hooks of every git dir in it,
// core.hooksPath and .husky when they are in it, and nothing it cannot
// hold apart from the project.
func TestGuardMounts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	// No global or system config reaches git: core.hooksPath is the
	// repository's own.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	for _, tc := range []struct {
		name string
		// setup makes the project in dir (resolved) and returns the
		// paths, relative to dir, the guard should hold.
		setup func(t *testing.T, dir string) []string
		state string
		err   string
	}{
		{
			name: "a repository",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				return []string{".git/config", ".git/hooks"}
			},
			state: GuardOn,
		},
		{
			name: "no hooks: made, so nothing in the box can make them",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				if err := os.RemoveAll(filepath.Join(dir, ".git", "hooks")); err != nil {
					t.Fatal(err)
				}
				return []string{".git/config", ".git/hooks"}
			},
			state: GuardOn,
		},
		{
			name: "a submodule's git dir, and one nested in it",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				for _, m := range []string{"modules/sub", "modules/sub/modules/deep"} {
					gd := filepath.Join(dir, ".git", m)
					mustMkdir(t, gd)
					for _, f := range []string{"HEAD", "config"} {
						if err := os.WriteFile(filepath.Join(gd, f), []byte("x\n"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}
				return []string{".git/config", ".git/hooks",
					".git/modules/sub/config", ".git/modules/sub/hooks",
					".git/modules/sub/modules/deep/config", ".git/modules/sub/modules/deep/hooks"}
			},
			state: GuardOn,
		},
		{
			name: "a worktree whose git dir is outside the project: nothing to hold",
			setup: func(t *testing.T, dir string) []string {
				main := t.TempDir()
				gitIn(t, main, "init", "-q")
				gitIn(t, main, "commit", "-q", "--allow-empty", "-m", "c")
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				gitIn(t, main, "worktree", "add", "-q", dir)
				return nil
			},
			state: GuardOn,
		},
		{
			name: "a .git file whose git dir is in the project",
			setup: func(t *testing.T, dir string) []string {
				inner := filepath.Join(dir, "meta", "repo.git")
				mustMkdir(t, filepath.Dir(inner))
				gitIn(t, filepath.Dir(inner), "init", "-q", "--bare", "repo.git")
				if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: meta/repo.git\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return []string{"meta/repo.git/config", "meta/repo.git/hooks"}
			},
			state: GuardOn,
		},
		{
			name: "core.hooksPath in the project, made when missing",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				gitIn(t, dir, "config", "core.hooksPath", "tools/hooks")
				return []string{".git/config", ".git/hooks", "tools/hooks"}
			},
			state: GuardOn,
		},
		{
			name: "core.hooksPath outside the project: the box cannot reach it",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				gitIn(t, dir, "config", "core.hooksPath", t.TempDir())
				return []string{".git/config", ".git/hooks"}
			},
			state: GuardOn,
		},
		{
			name: "core.hooksPath the project's root: refused",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				gitIn(t, dir, "config", "core.hooksPath", ".")
				return nil
			},
			err: "project's root",
		},
		{
			name: ".husky",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				mustMkdir(t, filepath.Join(dir, ".husky"))
				return []string{".git/config", ".git/hooks", ".husky"}
			},
			state: GuardOn,
		},
		{
			name: "hooks a link out of the project: refused",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				hooks := filepath.Join(dir, ".git", "hooks")
				if err := os.RemoveAll(hooks); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), hooks); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			err: "leads out of the project",
		},
		{
			name: "hooks a link within the project: held where it leads",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				hooks := filepath.Join(dir, ".git", "hooks")
				if err := os.RemoveAll(hooks); err != nil {
					t.Fatal(err)
				}
				mustMkdir(t, filepath.Join(dir, "githooks"))
				if err := os.Symlink(filepath.Join(dir, "githooks"), hooks); err != nil {
					t.Fatal(err)
				}
				return []string{".git/config", "githooks"}
			},
			state: GuardOn,
		},
		{
			name: "a git dir with no config: refused",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				if err := os.Remove(filepath.Join(dir, ".git", "config")); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			err: "no config",
		},
		{
			name: "not a repository",
			setup: func(t *testing.T, dir string) []string {
				return nil
			},
			state: GuardNotRepo,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir = filepath.Join(dir, "project")
			mustMkdir(t, dir)
			rel := tc.setup(t, dir)
			mounts, state, err := GuardMounts(context.Background(), dir)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("GuardMounts = %v, %q, %v; want an error naming %q", mounts, state, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if state != tc.state {
				t.Errorf("state %q, want %q", state, tc.state)
			}
			var want []string
			for _, r := range rel {
				want = append(want, filepath.Join(dir, r))
			}
			slices.Sort(want)
			if !slices.Equal(mounts, want) {
				t.Errorf("mounts\n got %q\nwant %q", mounts, want)
			}
			for _, m := range mounts {
				if _, err := os.Stat(m); err != nil {
					t.Errorf("%s: %v", m, err)
				}
			}
		})
	}
}

// A guarded box mounts each guard path read-only after the project's
// read-write mount; its spec label and guard label differ from the same
// box without one, and from one whose guard holds other paths.
func TestRunArgsGuard(t *testing.T) {
	spec := testSpec(t)
	plain := RunArgs(spec, "bin", "172.30.0.2", "")
	guarded := spec
	guarded.Guard, guarded.GuardState = true, GuardOn
	guarded.GuardMounts = []string{spec.Dir + "/.git/config", spec.Dir + "/.git/hooks"}
	args := RunArgs(guarded, "bin", "172.30.0.2", "")
	joined := strings.Join(args, " ")
	project := strings.Index(joined, "-v "+spec.Dir+":"+spec.Dir+" ")
	cfg := strings.Index(joined, "-v "+spec.Dir+"/.git/config:"+spec.Dir+"/.git/config:ro")
	hooks := strings.Index(joined, "-v "+spec.Dir+"/.git/hooks:"+spec.Dir+"/.git/hooks:ro")
	if project < 0 || cfg < 0 || hooks < 0 || !(project < cfg && cfg < hooks) {
		t.Errorf("guard mounts missing or before the project's:\n%s", joined)
	}
	if strings.Contains(strings.Join(plain, " "), ":ro -v") && strings.Contains(strings.Join(plain, " "), ".git") {
		t.Errorf("an unguarded box mounts git paths: %q", plain)
	}
	if specLabel(spec) == specLabel(guarded) {
		t.Error("the guard is not in the spec label")
	}
	other := guarded
	other.GuardMounts = append([]string{spec.Dir + "/.git/modules/sub/config"}, guarded.GuardMounts...)
	if specLabel(other) == specLabel(guarded) || guardTag(other) == guardTag(guarded) {
		t.Error("other guard paths read as the same guard")
	}
	if guardTag(spec) != "none" {
		t.Errorf("guardTag of an unguarded box = %q", guardTag(spec))
	}
	if !strings.Contains(joined, labelGuard+"="+guardTag(guarded)) {
		t.Errorf("no guard label: %s", joined)
	}
}

// A box made without the guard, or with another, is refused by a
// session that asks for one, and the refusal names the guard.
func TestEnsureRefusesAnotherGuard(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q")
	spec := testSpec(t)
	spec.Dir = dir
	bin, err := binaryLabel(spec.Binary)
	if err != nil {
		t.Fatal(err)
	}
	// What is there: the same box, made without a guard (no guard label,
	// as before the guard existed).
	f := &fake{answers: map[string][]answer{"ps": {{out: "running\t" + specLabel(spec) + "\t" + bin + "\n"}}}}
	guarded := spec
	guarded.Guard = true
	_, err = (&Client{Runner: f}).Ensure(context.Background(), guarded)
	if err == nil || !strings.Contains(err.Error(), "git guard") || !strings.Contains(err.Error(), "dax box down") {
		t.Fatalf("Ensure = %v, want the guard named and dax box down", err)
	}
	for _, c := range f.calls {
		if c[0] == "run" || c[0] == "rm" {
			t.Errorf("docker %q: the box was touched", c)
		}
	}
}
