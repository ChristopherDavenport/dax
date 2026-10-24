package box

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func testUnitSpec(t *testing.T) Spec {
	t.Helper()
	s := testSpec(t)
	s.Of, s.Unit = s.Name, "gh"
	s.Name = UnitName(s.Of, s.Unit)
	s.Image, s.SidecarImage = "node:24-bookworm-slim", "golang:1.26-bookworm"
	s.Memory, s.CPUs, s.Pids = "1g", 1, 256
	s.Secrets = map[string]string{"GITHUB_TOKEN": "env:GH_TEST_TOKEN"}
	s.Rules = Rules{Internet: InternetAllow, Allow: []string{"api.github.com"}}
	s.Decl = "decl1"
	return s
}

func TestUnitRunArgs(t *testing.T) {
	spec := testUnitSpec(t)
	args := UnitRunArgs(spec, "172.31.0.2", "/data/dax-app-12345678-gh")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run -d --init",
		"--name dax-app-12345678-gh",
		"--label dax.box.of=dax-app-12345678",
		"--label dax.box.unit=gh",
		"--label dax.box.files=none",
		"--runtime runsc",
		"--user 1000:1000",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--read-only",
		"--tmpfs /tmp:exec",
		"-e HTTPS_PROXY=http://172.31.0.2:3128",
		"-e NODE_USE_ENV_PROXY=1",
		"--memory 1g --memory-swap 1g",
		"--pids-limit 256",
		"--network dax-app-12345678-gh-net",
		"-e GITHUB_TOKEN",
		"--ulimit nproc=256:256",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("unit args lack %q:\n%s", want, joined)
		}
	}
	if args[len(args)-3] != spec.Image || args[len(args)-2] != "sleep" {
		t.Errorf("the image and its process are not last: %q", args[len(args)-3:])
	}
	for _, not := range []string{spec.Binary, Binary, "-v " + spec.Dir, "-w ", StatePath, Label + "=1", "GH_TEST_TOKEN", "GITHUB_TOKEN="} {
		if strings.Contains(joined, not) {
			t.Errorf("unit args hold %q: %s", not, joined)
		}
	}
	// The project read-only, and state.
	spec.Files = Files{Project: FilesRO, State: FilesRW}
	joined = strings.Join(UnitRunArgs(spec, "172.31.0.2", "/data/x"), " ")
	for _, want := range []string{"-v /home/me/src/app:/home/me/src/app:ro", "-w /home/me/src/app", "-v /data/x:/state", "-e DAX_UNIT_STATE=/state", "--label dax.box.files=project ro, state rw"} {
		if !strings.Contains(joined, want) {
			t.Errorf("unit args lack %q:\n%s", want, joined)
		}
	}
	spec.Files = Files{Project: FilesRW}
	if j := strings.Join(UnitRunArgs(spec, "172.31.0.2", ""), " "); !strings.Contains(j, "-v /home/me/src/app:/home/me/src/app -w") {
		t.Errorf("rw project: %s", j)
	}
	// The sidecar runs dax in the workspace's image, not the unit's.
	side := strings.Join(SidecarArgs(spec, "b", "172.31.0.2", "/rules"), " ")
	if !strings.Contains(side, "golang:1.26-bookworm /usr/local/bin/dax box proxy") || strings.Contains(side, "node:24") {
		t.Errorf("the unit's sidecar image: %s", side)
	}
}

func TestRunArgsState(t *testing.T) {
	spec := testSpec(t)
	if j := strings.Join(RunArgs(spec, "b", "172.30.0.2", "/data/box"), " "); strings.Contains(j, StatePath) {
		t.Errorf("a box without state mounts it: %s", j)
	}
	label := specLabel(spec)
	spec.Files = Files{Project: FilesRW, State: FilesRW}
	j := strings.Join(RunArgs(spec, "b", "172.30.0.2", "/data/box"), " ")
	for _, want := range []string{"-v /data/box:/state", "-e DAX_UNIT_STATE=/state", "--label dax.box.files=project rw, state rw", "--label dax.box.unit=workspace"} {
		if !strings.Contains(j, want) {
			t.Errorf("box args lack %q:\n%s", want, j)
		}
	}
	if specLabel(spec) == label {
		t.Error("state did not change the box's spec label")
	}
}

func TestEnsureUnit(t *testing.T) {
	bin := func(spec Spec) string {
		b, err := binaryLabel(spec.Binary)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	row := func(state, label string) answer { return answer{out: state + "\t" + label + "\n"} }
	subnet := answer{out: "172.31.0.0/16 \n"}
	made := []string{"ps proxy", "network inspect egress", "run proxy", "run unit", "ps unit", "exec check"}
	for _, tc := range []struct {
		name    string
		edit    func(*Spec)
		env     map[string]string
		answers func(Spec) map[string][]answer
		verbs   []string
		err     string
		secrets []string // in run's environment
	}{
		{
			name: "absent: network, sidecar, the unit with its secret, checked",
			env:  map[string]string{"GH_TEST_TOKEN": "dummy-not-a-secret"},
			answers: func(s Spec) map[string][]answer {
				return map[string][]answer{"ps unit": {{}, row("running", unitSpecLabel(s))}, "network inspect": {subnet}}
			},
			verbs:   append([]string{"ps unit", "network inspect"}, made...),
			secrets: []string{"GITHUB_TOKEN=dummy-not-a-secret"},
		},
		{
			name: "a secret not set: nothing made",
			answers: func(s Spec) map[string][]answer {
				return map[string][]answer{"ps unit": {{}}}
			},
			verbs: []string{"ps unit"},
			err:   "unit gh: secret GITHUB_TOKEN: env:GH_TEST_TOKEN: not set where the box host runs; for a local box host name it in executor.pass_env",
		},
		{
			name: "built: the image made first",
			edit: func(s *Spec) {
				s.Secrets = nil
				b := Build{Base: "node:24-bookworm-slim", Run: []string{"npm i -g x@1"}}
				s.Build, s.Image = &b, b.Tag("gh")
			},
			answers: func(s Spec) map[string][]answer {
				return map[string][]answer{"image inspect": {{err: errors.New("no such image")}}, "ps unit": {{}, row("running", unitSpecLabel(s))}, "network inspect": {subnet}}
			},
			verbs: append([]string{"image inspect", "build", "ps unit", "network inspect"}, made...),
		},
		{
			name: "built before: not built again",
			edit: func(s *Spec) {
				s.Secrets = nil
				b := Build{Base: "node:24-bookworm-slim", Run: []string{"npm i -g x@1"}}
				s.Build, s.Image = &b, b.Tag("gh")
			},
			answers: func(s Spec) map[string][]answer {
				return map[string][]answer{"image inspect": {{out: "sha256:1\n"}}, "ps unit": {{}, row("running", unitSpecLabel(s))}, "network inspect": {subnet}}
			},
			verbs: append([]string{"image inspect", "ps unit", "network inspect"}, made...),
		},
		{
			name: "running, sidecar running: used as they are",
			answers: func(s Spec) map[string][]answer {
				return map[string][]answer{"ps unit": {row("running", unitSpecLabel(s))}, "ps proxy": {{out: "running\t" + bin(s) + "\n"}}}
			},
			verbs: []string{"ps unit", "ps proxy"},
		},
		{
			name: "stopped: sidecar, then the unit, started and checked; no secret resolved",
			answers: func(s Spec) map[string][]answer {
				return map[string][]answer{"ps unit": {row("exited", unitSpecLabel(s)), row("running", unitSpecLabel(s))}, "ps proxy": {{out: "exited\t" + bin(s) + "\n"}}, "network inspect": {subnet}}
			},
			verbs: []string{"ps unit", "network inspect", "ps proxy", "start proxy", "start", "ps unit", "exec check"},
		},
		{
			name: "another declaration: refused",
			answers: func(s Spec) map[string][]answer {
				return map[string][]answer{"ps unit": {row("running", "other")}}
			},
			verbs: []string{"ps unit"},
			err:   "unit gh of box dax-app-12345678 was made with other settings (its declaration, image or user) than these; remove the box with `dax box down dax-app-12345678`",
		},
		{
			name: "did not stay up",
			env:  map[string]string{"GH_TEST_TOKEN": "x"},
			answers: func(s Spec) map[string][]answer {
				return map[string][]answer{"ps unit": {{}, row("exited", unitSpecLabel(s))}, "network inspect": {subnet}, "logs": {{out: "sleep: not found"}}}
			},
			verbs:   []string{"ps unit", "network inspect", "ps proxy", "network inspect egress", "run proxy", "run unit", "ps unit", "logs"},
			err:     "its image needs sh and sleep",
			secrets: []string{"GITHUB_TOKEN=x"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := testUnitSpec(t)
			if tc.edit != nil {
				tc.edit(&spec)
			}
			f := &fake{answers: tc.answers(spec)}
			dir := t.TempDir()
			c := &Client{Runner: f, StateDir: filepath.Join(dir, "rules"), DataDir: filepath.Join(dir, "data"), LookupEnv: func(k string) (string, bool) {
				v, ok := tc.env[k]
				return v, ok
			}}
			name, err := c.EnsureUnit(context.Background(), spec)
			if tc.err == "" && err != nil {
				t.Fatalf("EnsureUnit: %v", err)
			}
			if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("EnsureUnit error = %v, want one naming %q", err, tc.err)
			}
			if tc.err == "" && name != "dax-app-12345678-gh" {
				t.Errorf("container = %q", name)
			}
			if !slices.Equal(f.verbs(), tc.verbs) {
				t.Errorf("docker calls = %q, want %q", f.verbs(), tc.verbs)
			}
			if got := f.envs["run unit"]; !slices.Equal(got, tc.secrets) {
				t.Errorf("the unit's run was given %q, want %q", got, tc.secrets)
			}
			for _, call := range f.calls {
				for _, a := range call {
					if strings.Contains(a, "dummy-not-a-secret") {
						t.Errorf("a secret's value in docker's arguments: %q", call)
					}
				}
			}
			if err != nil && strings.Contains(err.Error(), "dummy-not-a-secret") {
				t.Errorf("a secret's value in an error: %v", err)
			}
			if df := f.stdins["build"]; strings.Contains(strings.Join(f.verbs(), " "), "build") && !strings.HasPrefix(string(df), "FROM node:24-bookworm-slim\nRUN npm i -g x@1\n") {
				t.Errorf("the build was given %q", df)
			}
		})
	}
}

func TestEnsureUnitRefusesAName(t *testing.T) {
	spec := testUnitSpec(t)
	spec.Name = "elsewhere"
	if _, err := (&Client{Runner: &fake{}}).EnsureUnit(context.Background(), spec); err == nil {
		t.Fatal("a unit's container not named for its box and unit was made")
	}
	spec = testUnitSpec(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	spec.Dir, spec.Files = home, Files{Project: FilesRO}
	f := &fake{}
	if _, err := (&Client{Runner: f}).EnsureUnit(context.Background(), spec); err == nil || len(f.calls) != 0 {
		t.Fatalf("a unit over the home directory: %v, %q", err, f.calls)
	}
}

func TestResolveSecret(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("from-a-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{LookupEnv: func(k string) (string, bool) {
		if k == "SET" {
			return "from-env", true
		}
		return "", false
	}}
	for _, tc := range []struct{ ref, want, err string }{
		{ref: "env:SET", want: "from-env"},
		{ref: "env:UNSET", err: "executor.pass_env"},
		{ref: "command:printf '  from-a-command\\n'", want: "from-a-command"},
		{ref: "command:echo leaked-output >&2; exit 3", err: "exit 3"},
		{ref: "command:true", err: "gave nothing"},
		{ref: "file:" + token, want: "from-a-file"},
		{ref: "file:" + filepath.Join(dir, "none"), err: "no such file"},
		{ref: "file:" + empty, err: "empty"},
	} {
		got, err := c.resolveSecret(context.Background(), tc.ref)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v, want %q", tc.ref, err, tc.err)
			}
			if err != nil && strings.Contains(err.Error(), "leaked-output") {
				t.Errorf("%s: a command's output in the error: %v", tc.ref, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s = %q, %v; want %q", tc.ref, got, err, tc.want)
		}
	}
}

func TestListWithUnits(t *testing.T) {
	dir := t.TempDir()
	if err := WriteRules(filepath.Join(dir, "dax-a-1-gh", RulesFile), Rules{Internet: InternetAllow, Allow: []string{"api.github.com"}}); err != nil {
		t.Fatal(err)
	}
	f := &fake{answers: map[string][]answer{
		"ps":      {{out: "dax-a-1\trunning\t/src/a\trunsc\tworkspace\tproject rw\n"}},
		"ps unit": {{out: "dax-a-1-gh\trunning\tdax-a-1\tgh\tkata\tnone\t/src/a\ndax-gone-9-rag\texited\tdax-gone-9\trag\trunsc\tproject ro, state rw\t/src/b\n"}},
	}}
	c := &Client{Runner: f, StateDir: dir}
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Info{
		{Name: "dax-a-1", State: "running", Dir: "/src/a", Runtime: "runsc", Internet: "?", Unit: "workspace", Files: "project rw"},
		{Name: "dax-a-1-gh", State: "running", Dir: "/src/a", Runtime: "kata", Internet: "allow api.github.com", Unit: "gh", Of: "dax-a-1", Files: "none"},
		{Name: "dax-gone-9-rag", State: "exited", Dir: "/src/b", Runtime: "runsc", Internet: "?", Unit: "rag", Of: "dax-gone-9", Files: "project ro, state rw"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("List = %+v\nwant %+v", got, want)
	}
}

func TestRemoveWithUnits(t *testing.T) {
	for _, state := range []bool{false, true} {
		dir := t.TempDir()
		data := filepath.Join(dir, "data")
		for _, n := range []string{"dax-a-1", "dax-a-1-rag"} {
			if err := os.MkdirAll(filepath.Join(data, n), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		f := &fake{answers: map[string][]answer{
			"ps":         {{out: "running\tspec\tbin\n"}},
			"ps unit":    {{out: "dax-a-1-rag\trunning\tdax-a-1\trag\trunsc\tproject ro, state rw\t/src/a\ndax-b-2-gh\trunning\tdax-b-2\tgh\trunsc\tnone\t/src/b\n"}},
			"ps proxy":   {{out: "running\tbin\n"}, {out: "running\tbin\n"}},
			"network ls": {{out: "x\n"}, {out: "x\n"}},
		}}
		c := &Client{Runner: f, StateDir: filepath.Join(dir, "rules"), DataDir: data}
		if err := c.Remove(context.Background(), "dax-a-1", state); err != nil {
			t.Fatal(err)
		}
		want := []string{"ps", "ps unit", "rm", "ps proxy", "rm proxy", "network ls", "network rm", "rm", "ps proxy", "rm proxy", "network ls", "network rm"}
		if !slices.Equal(f.verbs(), want) {
			t.Errorf("state %v: docker calls = %q, want %q", state, f.verbs(), want)
		}
		removed := []string{}
		for _, call := range f.calls {
			if call[0] == "rm" || (call[0] == "network" && call[1] == "rm") {
				removed = append(removed, call[len(call)-1])
			}
		}
		if wantRm := []string{"dax-a-1-rag", "dax-a-1-rag-proxy", "dax-a-1-rag-net", "dax-a-1", "dax-a-1-proxy", "dax-a-1-net"}; !slices.Equal(removed, wantRm) {
			t.Errorf("removed %q, want %q (and never another box's unit)", removed, wantRm)
		}
		for _, n := range []string{"dax-a-1", "dax-a-1-rag"} {
			_, err := os.Stat(filepath.Join(data, n))
			if kept := err == nil; kept == state {
				t.Errorf("state %v: %s's state directory kept = %v", state, n, kept)
			}
		}
	}
}

// standInDocker writes a docker that runs `exec [-i] CONTAINER cmd...`
// here, which is what StartProc and Signal ask of the real one.
func standInDocker(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
[ "$1" = exec ] || exit 2
shift
[ "$1" = -i ] && shift
if [ "$1" = missing ]; then echo "Error response from daemon: No such container: missing" >&2; exit 1; fi
shift
exec "$@"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartProc(t *testing.T) {
	old := closeGrace
	closeGrace = 100 * time.Millisecond
	defer func() { closeGrace = old }()
	c := &Client{DockerPath: standInDocker(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Its output is its own: the wrapper's line is taken first.
	p, err := c.StartProc(ctx, "unit", []string{"sh", "-c", "echo to-stderr >&2; cat"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(p.Stdin(), "hello\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Fatalf("stdout = %q, %v", line, err)
	}
	errLine, err := bufio.NewReader(p.Stderr()).ReadString('\n')
	if err != nil || errLine != "to-stderr\n" {
		t.Fatalf("stderr = %q, %v", errLine, err)
	}
	p.Stdin().Close()
	if status, err := p.Wait(); status != 0 || err != nil {
		t.Errorf("Wait = %d, %v", status, err)
	}
	if done, err := p.Signal("TERM"); !done || err != nil {
		t.Errorf("Signal after exit = %v, %v", done, err)
	}

	// TERM by its id in the container.
	p, err = c.StartProc(ctx, "unit", []string{"sh", "-c", "trap 'exit 7' TERM; while :; do sleep 0.05; done"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stderr() != nil {
		t.Error("a standard error that was not asked for")
	}
	time.Sleep(200 * time.Millisecond)
	if done, err := p.Signal("TERM"); done || err != nil {
		t.Fatalf("Signal = %v, %v", done, err)
	}
	if status, _ := p.Wait(); status != 7 {
		t.Errorf("status after TERM = %d, want 7", status)
	}
	if _, err := p.Signal("HUP"); err == nil {
		t.Error("HUP was sent")
	}

	// Close: input, then TERM, then KILL.
	p, err = c.StartProc(ctx, "unit", []string{"sh", "-c", "trap '' TERM; while :; do sleep 0.05; done"}, false)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end a process that ignores TERM")
	}

	// A container that is not there.
	if _, err := c.StartProc(ctx, "missing", []string{"x"}, false); err == nil || !strings.Contains(err.Error(), "No such container") {
		t.Errorf("a missing container: %v", err)
	}
}
