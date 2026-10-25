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

// repoWithCommit makes a repository in dir with one commit.
func repoWithCommit(t *testing.T, dir string) {
	t.Helper()
	mustMkdir(t, dir)
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "c")
}

// The guard holds the project's .git read-only, whole, with every
// checked-out submodule's .git, an in-project core.hooksPath and
// .husky; it writes nothing in the repository, and refuses a guard path
// that is a link or leads out of the project.
func TestGuardMounts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	// No global or system config reaches git: core.hooksPath is the
	// repository's own.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	submodule := func(t *testing.T, dir, path string) {
		t.Helper()
		lib := filepath.Join(t.TempDir(), "lib")
		repoWithCommit(t, lib)
		gitIn(t, dir, "submodule", "add", "-q", lib, path)
		gitIn(t, dir, "commit", "-q", "-m", "sub")
	}
	for _, tc := range []struct {
		name string
		// setup makes the project in dir (resolved) and returns the
		// paths, relative to dir, the guard should hold.
		setup func(t *testing.T, dir string) []string
		state string
		err   string
		// absent are paths, relative to dir, the guard must not make.
		absent []string
	}{
		{
			name: "a repository: its .git, whole",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				return []string{".git"}
			},
			state: GuardOn,
		},
		{
			name: "no hooks: none made",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				if err := os.RemoveAll(filepath.Join(dir, ".git", "hooks")); err != nil {
					t.Fatal(err)
				}
				return []string{".git"}
			},
			state:  GuardOn,
			absent: []string{".git/hooks"},
		},
		{
			name: "a submodule: its .git file, its git dir under .git",
			setup: func(t *testing.T, dir string) []string {
				repoWithCommit(t, dir)
				submodule(t, dir, "sub")
				return []string{".git", "sub/.git"}
			},
			state: GuardOn,
		},
		{
			name: "a submodule nested in a submodule",
			setup: func(t *testing.T, dir string) []string {
				repoWithCommit(t, dir)
				lib := filepath.Join(t.TempDir(), "lib")
				repoWithCommit(t, lib)
				submodule(t, lib, "deep")
				gitIn(t, dir, "submodule", "add", "-q", lib, "sub")
				gitIn(t, dir, "submodule", "update", "-q", "--init", "--recursive")
				gitIn(t, dir, "commit", "-q", "-m", "sub")
				return []string{".git", "sub/.git", "sub/deep/.git"}
			},
			state: GuardOn,
		},
		{
			name: "a submodule not checked out: nothing of its own",
			setup: func(t *testing.T, dir string) []string {
				repoWithCommit(t, dir)
				submodule(t, dir, "sub")
				gitIn(t, dir, "submodule", "deinit", "-q", "-f", "sub")
				return []string{".git"}
			},
			state: GuardOn,
		},
		{
			name: "a worktree whose git dir is outside the project: its .git file",
			setup: func(t *testing.T, dir string) []string {
				main := t.TempDir()
				repoWithCommit(t, main)
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				gitIn(t, main, "worktree", "add", "-q", dir)
				return []string{".git"}
			},
			state: GuardOn,
		},
		{
			name: "a .git file whose git dir is in the project: both",
			setup: func(t *testing.T, dir string) []string {
				mustMkdir(t, filepath.Join(dir, "meta"))
				gitIn(t, dir, "init", "-q", "--separate-git-dir", filepath.Join(dir, "meta", "repo.git"))
				return []string{".git", "meta/repo.git"}
			},
			state: GuardOn,
		},
		{
			name: "core.hooksPath in the project",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				mustMkdir(t, filepath.Join(dir, "tools", "hooks"))
				gitIn(t, dir, "config", "core.hooksPath", "tools/hooks")
				return []string{".git", "tools/hooks"}
			},
			state: GuardOn,
		},
		{
			name: "core.hooksPath in the project, not there yet: refused, not made",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				gitIn(t, dir, "config", "core.hooksPath", "tools/hooks")
				return nil
			},
			err:    "not there yet",
			absent: []string{"tools"},
		},
		{
			name: "core.hooksPath outside the project: the box cannot reach it",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				gitIn(t, dir, "config", "core.hooksPath", t.TempDir())
				return []string{".git"}
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
			name: "core.hooksPath through a link in the project: refused",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				mustMkdir(t, filepath.Join(dir, "real", "hooks"))
				if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "tools")); err != nil {
					t.Fatal(err)
				}
				gitIn(t, dir, "config", "core.hooksPath", "tools/hooks")
				return nil
			},
			err: "symbolic link",
		},
		{
			name: ".husky",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				mustMkdir(t, filepath.Join(dir, ".husky"))
				return []string{".git", ".husky"}
			},
			state: GuardOn,
		},
		{
			name: ".husky a link: refused, the link would stay writable",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				mustMkdir(t, filepath.Join(dir, "husky"))
				if err := os.Symlink(filepath.Join(dir, "husky"), filepath.Join(dir, ".husky")); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			err: "symbolic link",
		},
		{
			name: ".git a link: refused",
			setup: func(t *testing.T, dir string) []string {
				real := filepath.Join(dir, "repo")
				mustMkdir(t, real)
				gitIn(t, real, "init", "-q")
				if err := os.Symlink(filepath.Join(real, ".git"), filepath.Join(dir, ".git")); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			err: "symbolic link",
		},
		{
			name: "hooks a link out of the project: held with the read-only .git",
			setup: func(t *testing.T, dir string) []string {
				gitIn(t, dir, "init", "-q")
				hooks := filepath.Join(dir, ".git", "hooks")
				if err := os.RemoveAll(hooks); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), hooks); err != nil {
					t.Fatal(err)
				}
				return []string{".git"}
			},
			state: GuardOn,
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
			for _, a := range tc.absent {
				if _, serr := os.Lstat(filepath.Join(dir, a)); serr == nil {
					t.Errorf("%s was made in the repository", a)
				}
			}
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
	guarded.GuardMounts = []string{spec.Dir + "/.git", spec.Dir + "/.husky"}
	args := RunArgs(guarded, "bin", "172.30.0.2", "")
	joined := strings.Join(args, " ")
	project := strings.Index(joined, "-v "+spec.Dir+":"+spec.Dir+" ")
	dotgit := strings.Index(joined, "-v "+spec.Dir+"/.git:"+spec.Dir+"/.git:ro")
	husky := strings.Index(joined, "-v "+spec.Dir+"/.husky:"+spec.Dir+"/.husky:ro")
	if project < 0 || dotgit < 0 || husky < 0 || !(project < dotgit && dotgit < husky) {
		t.Errorf("guard mounts missing or before the project's:\n%s", joined)
	}
	if strings.Contains(strings.Join(plain, " "), ":ro -v") && strings.Contains(strings.Join(plain, " "), ".git") {
		t.Errorf("an unguarded box mounts git paths: %q", plain)
	}
	if specLabel(spec) == specLabel(guarded) {
		t.Error("the guard is not in the spec label")
	}
	other := guarded
	other.GuardMounts = append(append([]string(nil), guarded.GuardMounts...), spec.Dir+"/sub/.git")
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

// A guarded box, made or reused, tells each session its repository is
// read-only in it; an unguarded one says nothing of it.
func TestEnsureGuardNotice(t *testing.T) {
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
	for _, guarded := range []bool{true, false} {
		spec := testSpec(t)
		spec.Dir = dir
		spec.Rules = Rules{Internet: InternetOff}
		bin, err := binaryLabel(spec.Binary)
		if err != nil {
			t.Fatal(err)
		}
		made := spec
		if guarded {
			made.Guard = true
			made.GuardMounts, made.GuardState, err = GuardMounts(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
		}
		f := &fake{answers: map[string][]answer{
			"ps":       {{out: "running\t" + specLabel(made) + "\t" + bin + "\t" + guardTag(made) + "\n"}},
			"ps proxy": {{out: "running\t" + bin + "\n"}},
		}}
		var notices []string
		c := &Client{Runner: f, StateDir: t.TempDir(), Notice: func(m string) { notices = append(notices, m) }}
		ask := spec
		ask.Guard = guarded
		b, err := c.Ensure(context.Background(), ask)
		if err != nil {
			t.Fatalf("guarded %v: %v", guarded, err)
		}
		said := slices.ContainsFunc(notices, func(n string) bool {
			return strings.Contains(n, "the repository is read-only in this box") && strings.Contains(n, "commit outside it")
		})
		if said != guarded || (b.Guard == GuardOn) != guarded {
			t.Errorf("guarded %v: guard %q, notices %q", guarded, b.Guard, notices)
		}
	}
}
