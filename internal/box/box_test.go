package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fake answers docker by the first argument, in order for repeats,
// and records every call.
type fake struct {
	answers map[string][]answer
	calls   [][]string
}

type answer struct {
	out string
	err error
}

func (f *fake) Docker(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	q := f.answers[args[0]]
	if len(q) == 0 {
		return nil, nil
	}
	a := q[0]
	if len(q) > 1 {
		f.answers[args[0]] = q[1:]
	}
	return []byte(a.out), a.err
}

// verbs is the first argument of each call.
func (f *fake) verbs() []string {
	var v []string
	for _, c := range f.calls {
		v = append(v, c[0])
	}
	return v
}

func testSpec(t *testing.T) Spec {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "dax")
	if err := os.WriteFile(bin, []byte("dax"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Spec{Name: "dax-app-12345678", Dir: "/home/me/src/app", Image: "golang:1.26-bookworm", Runtime: "runsc", Memory: "4g", CPUs: 2, Pids: 512, Network: "bridge", Binary: bin, UID: 1000, GID: 1000}
}

func TestRunArgs(t *testing.T) {
	spec := testSpec(t)
	args := RunArgs(spec, "bin-label")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run -d --init",
		"--name dax-app-12345678",
		"--label dax.box=1",
		"--label dax.box.dir=/home/me/src/app",
		"--label dax.box.runtime=runsc",
		"--label dax.box.bin=bin-label",
		"--runtime runsc",
		"--user 1000:1000",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--read-only",
		"--tmpfs /tmp:exec",
		"--tmpfs /home/dax:exec,mode=1777",
		"-e HOME=/home/dax",
		"--memory 4g --memory-swap 4g",
		"--cpus 2",
		"--pids-limit 512",
		"--network bridge",
		"-v /home/me/src/app:/home/me/src/app",
		"-w /home/me/src/app",
		"-v " + spec.Binary + ":/usr/local/bin/dax:ro",
		"golang:1.26-bookworm sleep infinity",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("run args lack %q:\n%s", want, joined)
		}
	}
	if args[len(args)-3] != spec.Image {
		t.Errorf("image is not before the command: %q", args)
	}
	spec.CPUs = 1.5
	if j := strings.Join(RunArgs(spec, "b"), " "); !strings.Contains(j, "--cpus 1.5") {
		t.Errorf("fractional cpus: %s", j)
	}
}

func TestCommand(t *testing.T) {
	got := Box{Name: "dax-app-12345678", Dir: "/home/me/src/app"}.Command()
	want := []string{"docker", "exec", "-i", "dax-app-12345678", "/usr/local/bin/dax", "execute", "-root", "/home/me/src/app", "-kind", "container", "-ref", "dax-app-12345678"}
	if !slices.Equal(got, want) {
		t.Errorf("Command() = %q, want %q", got, want)
	}
}

func TestNameFor(t *testing.T) {
	for _, tc := range []struct{ dir, prefix string }{
		{"/home/me/src/app", "dax-app-"},
		{"/home/me/src/My App!", "dax-my-app-"},
		{"/home/me/src/.hidden", "dax-hidden-"},
		{"/", "dax-root-"},
		{"/x/" + strings.Repeat("a", 40), "dax-" + strings.Repeat("a", 32) + "-"},
	} {
		got := NameFor(tc.dir)
		if !strings.HasPrefix(got, tc.prefix) || len(got) != len(tc.prefix)+8 {
			t.Errorf("NameFor(%q) = %q, want %q and eight hex digits", tc.dir, got, tc.prefix)
		}
		if NameFor(tc.dir) != got {
			t.Errorf("NameFor(%q) is not stable", tc.dir)
		}
	}
	if NameFor("/a/app") == NameFor("/b/app") {
		t.Error("two projects with one base share a name")
	}
}

func TestEnsure(t *testing.T) {
	spec := testSpec(t)
	bin, err := binaryLabel(spec.Binary)
	if err != nil {
		t.Fatal(err)
	}
	row := func(state, spec, bin string) answer { return answer{out: state + "\t" + spec + "\t" + bin + "\n"} }
	same := specLabel(spec)
	for _, tc := range []struct {
		name    string
		answers map[string][]answer
		verbs   []string
		err     string
		warned  bool
	}{
		{
			name:    "absent: made and checked",
			answers: map[string][]answer{"ps": {{}, row("running", same, bin)}},
			verbs:   []string{"ps", "run", "ps", "exec"},
		},
		{
			name:    "running and the same: used as it is",
			answers: map[string][]answer{"ps": {row("running", same, bin)}},
			verbs:   []string{"ps"},
		},
		{
			name:    "stopped: started and checked",
			answers: map[string][]answer{"ps": {row("exited", same, bin), row("running", same, bin)}},
			verbs:   []string{"ps", "start", "ps", "exec"},
		},
		{
			name:    "other settings: refused",
			answers: map[string][]answer{"ps": {row("running", "other", bin)}},
			verbs:   []string{"ps"},
			err:     "dax box down dax-app-12345678",
		},
		{
			name:    "dax changed: made again with a warning",
			answers: map[string][]answer{"ps": {row("running", same, "old"), row("running", same, bin)}},
			verbs:   []string{"ps", "rm", "run", "ps", "exec"},
			warned:  true,
		},
		{
			name:    "unknown runtime: refused, never another",
			answers: map[string][]answer{"ps": {{}}, "run": {{err: errors.New("docker run: docker: Error response from daemon: unknown or invalid runtime name: runsc")}}},
			verbs:   []string{"ps", "run"},
			err:     "runtime not registered",
		},
		{
			name:    "dax does not run in the image",
			answers: map[string][]answer{"ps": {{}, row("running", same, bin)}, "exec": {{err: errors.New("docker exec: exec format error")}}},
			verbs:   []string{"ps", "run", "ps", "exec"},
			err:     "glibc",
		},
		{
			name:    "made by another session meanwhile: that box used",
			answers: map[string][]answer{"ps": {{}, row("running", same, bin)}, "run": {{err: errors.New(`docker run: docker: Error response from daemon: Conflict. The container name "/dax-app-12345678" is already in use`)}}},
			verbs:   []string{"ps", "run", "ps"},
		},
		{
			name:    "taken twice: refused",
			answers: map[string][]answer{"ps": {{}, {}}, "run": {{err: errors.New("docker run: is already in use")}, {err: errors.New("docker run: is already in use")}}},
			verbs:   []string{"ps", "run", "ps", "run"},
			err:     "already in use",
		},
		{
			name:    "did not stay up",
			answers: map[string][]answer{"ps": {{}, row("exited", same, bin)}, "logs": {{out: "sleep: not found"}}},
			verbs:   []string{"ps", "run", "ps", "logs"},
			err:     "sleep: not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{answers: tc.answers}
			warned := false
			c := &Client{Runner: f, Warn: func(string) { warned = true }}
			b, err := c.Ensure(context.Background(), spec)
			if tc.err == "" && err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("Ensure error = %v, want one naming %q", err, tc.err)
			}
			if !slices.Equal(f.verbs(), tc.verbs) {
				t.Errorf("docker calls = %q, want %q", f.verbs(), tc.verbs)
			}
			if warned != tc.warned {
				t.Errorf("warned = %v, want %v", warned, tc.warned)
			}
			if b.Name != spec.Name || b.Dir != spec.Dir {
				t.Errorf("box = %+v", b)
			}
			for _, call := range f.calls {
				if call[0] == "run" && !slices.Contains(call, "runsc") {
					t.Errorf("run without the spec's runtime: %q", call)
				}
			}
		})
	}
}

func TestCheckMounts(t *testing.T) {
	const home = "/home/me"
	for _, tc := range []struct {
		dir, bin string
		err      string
	}{
		{dir: "/home/me/src/app", bin: "/home/me/go/bin/dax"},
		{dir: "/srv/app", bin: "/usr/local/bin/dax"},
		{dir: "/home/meow", bin: "/usr/bin/dax"},
		{dir: "/", bin: "/usr/bin/dax", err: "home directory"},
		{dir: "/home", bin: "/usr/bin/dax", err: "home directory"},
		{dir: "/home/me", bin: "/usr/bin/dax", err: "home directory"},
		{dir: "/home/me/", bin: "/usr/bin/dax", err: "home directory"},
		{dir: "/home/me/a:b", bin: "/usr/bin/dax", err: "colon"},
		{dir: "/home/me/src/app", bin: "/opt/x:y/dax", err: "colon"},
	} {
		err := CheckMounts(Spec{Dir: tc.dir, Binary: tc.bin}, home)
		if tc.err == "" && err != nil {
			t.Errorf("CheckMounts(%q, %q) = %v", tc.dir, tc.bin, err)
		}
		if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("CheckMounts(%q, %q) = %v, want one naming %q", tc.dir, tc.bin, err, tc.err)
		}
	}
}

func TestEnsureRefusesHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	spec := testSpec(t)
	spec.Dir = home
	f := &fake{}
	if _, err := (&Client{Runner: f}).Ensure(context.Background(), spec); err == nil {
		t.Fatal("Ensure made a box over the home directory")
	}
	if len(f.calls) != 0 {
		t.Errorf("docker was run: %q", f.calls)
	}
}

func TestEnsureMissingBinary(t *testing.T) {
	spec := testSpec(t)
	spec.Binary = filepath.Join(t.TempDir(), "gone")
	f := &fake{}
	if _, err := (&Client{Runner: f}).Ensure(context.Background(), spec); err == nil {
		t.Fatal("Ensure made a box without a dax to mount")
	}
	if len(f.calls) != 0 {
		t.Errorf("docker was run: %q", f.calls)
	}
}

func TestSpecLabelCoversTheSettings(t *testing.T) {
	base := testSpec(t)
	for name, change := range map[string]func(*Spec){
		"image":   func(s *Spec) { s.Image = "debian" },
		"runtime": func(s *Spec) { s.Runtime = "runc" },
		"memory":  func(s *Spec) { s.Memory = "8g" },
		"cpus":    func(s *Spec) { s.CPUs = 4 },
		"pids":    func(s *Spec) { s.Pids = 64 },
		"network": func(s *Spec) { s.Network = "none" },
		"uid":     func(s *Spec) { s.UID = 0 },
		"dir":     func(s *Spec) { s.Dir = "/elsewhere" },
	} {
		s := base
		change(&s)
		if specLabel(s) == specLabel(base) {
			t.Errorf("a change of %s keeps the label", name)
		}
	}
	s := base
	s.Binary = "/other/dax"
	if specLabel(s) != specLabel(base) {
		t.Error("the binary is in the spec label; it is the bin label's")
	}
}

func TestListAndRemove(t *testing.T) {
	f := &fake{answers: map[string][]answer{"ps": {
		{out: "dax-a-1\trunning\t/src/a\trunsc\ndax-b-2\texited\t/src/b\trunsc-ptrace\n"},
		{},
	}}}
	c := &Client{Runner: f}
	boxes, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Info{{"dax-a-1", "running", "/src/a", "runsc"}, {"dax-b-2", "exited", "/src/b", "runsc-ptrace"}}
	if !slices.Equal(boxes, want) {
		t.Errorf("List = %+v, want %+v", boxes, want)
	}
	if !slices.Contains(f.calls[0], "label=dax.box=1") {
		t.Errorf("List does not filter by the box label: %q", f.calls[0])
	}
	// The second ps finds nothing: not a box, so nothing is removed.
	if err := c.Remove(context.Background(), "someone-elses"); err == nil || !strings.Contains(err.Error(), "no box") {
		t.Errorf("Remove of a container that is not a box: %v", err)
	}
	if slices.Contains(f.verbs(), "rm") {
		t.Errorf("rm was run: %q", f.calls)
	}
}

func TestQuote(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"docker", "exec", "-i", "dax-a-1"}, "docker exec -i dax-a-1"},
		{[]string{"-root", "/home/me/my app"}, "-root '/home/me/my app'"},
		{[]string{"it's"}, `'it'\''s'`},
		{[]string{""}, "''"},
	} {
		if got := Quote(tc.argv); got != tc.want {
			t.Errorf("Quote(%q) = %s, want %s", tc.argv, got, tc.want)
		}
	}
}
