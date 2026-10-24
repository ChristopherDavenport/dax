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

// fake answers docker by what each call is (verb), in order for
// repeats, and records every call.
type fake struct {
	answers map[string][]answer
	calls   [][]string
}

type answer struct {
	out string
	err error
}

// verb names a docker call for the fake: its first argument, told
// apart where a box makes several kinds of one (a network's
// subcommand, the sidecar's run, start, rm and ps, the proxy check).
func verb(args []string) string {
	has := func(s string) bool { return slices.Contains(args, s) }
	sidecar := slices.ContainsFunc(args, func(a string) bool {
		return strings.HasSuffix(a, "-proxy") || strings.HasPrefix(a, labelProxy+"=") || strings.HasPrefix(a, "label="+labelProxy+"=")
	})
	switch {
	case args[0] == "network" && has(EgressNetwork):
		return "network " + args[1] + " egress"
	case args[0] == "network":
		return "network " + args[1]
	case args[0] == "exec" && has("-check"):
		return "exec check"
	case sidecar && args[0] != "logs":
		return args[0] + " proxy"
	}
	return args[0]
}

func (f *fake) Docker(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	v := verb(args)
	q := f.answers[v]
	if len(q) == 0 {
		return nil, nil
	}
	a := q[0]
	if len(q) > 1 {
		f.answers[v] = q[1:]
	}
	return []byte(a.out), a.err
}

// verbs is each call's verb.
func (f *fake) verbs() []string {
	var v []string
	for _, c := range f.calls {
		v = append(v, verb(c))
	}
	return v
}

// call is the first call whose verb is v.
func (f *fake) call(v string) []string {
	for _, c := range f.calls {
		if verb(c) == v {
			return c
		}
	}
	return nil
}

func testSpec(t *testing.T) Spec {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "dax")
	if err := os.WriteFile(bin, []byte("dax"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Spec{Name: "dax-app-12345678", Dir: "/home/me/src/app", Image: "golang:1.26-bookworm", Runtime: "runsc", Memory: "4g", CPUs: 2, Pids: 512, Binary: bin, UID: 1000, GID: 1000, Rules: Rules{Internet: InternetOff}}
}

func TestRunArgs(t *testing.T) {
	spec := testSpec(t)
	args := RunArgs(spec, "bin-label", "172.30.0.2")
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
		"-e GOPATH=/home/dax/go",
		"-e HTTP_PROXY=http://172.30.0.2:3128 -e HTTPS_PROXY=http://172.30.0.2:3128",
		"-e http_proxy=http://172.30.0.2:3128 -e https_proxy=http://172.30.0.2:3128",
		"-e NO_PROXY=localhost,127.0.0.1 -e no_proxy=localhost,127.0.0.1",
		"--memory 4g --memory-swap 4g",
		"--cpus 2",
		"--pids-limit 512",
		"--network dax-app-12345678-net",
		"-v /home/me/src/app:/home/me/src/app",
		"-w /home/me/src/app",
		"-v " + spec.Binary + ":/usr/local/bin/dax:ro",
		"golang:1.26-bookworm sleep infinity",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("run args lack %q:\n%s", want, joined)
		}
	}
	if n := strings.Count(joined, "--network"); n != 1 {
		t.Errorf("the box is on %d networks, want its own alone:\n%s", n, joined)
	}
	if args[len(args)-3] != spec.Image {
		t.Errorf("image is not before the command: %q", args)
	}
	spec.CPUs = 1.5
	if j := strings.Join(RunArgs(spec, "b", "172.30.0.2"), " "); !strings.Contains(j, "--cpus 1.5") {
		t.Errorf("fractional cpus: %s", j)
	}
}

func TestSidecarArgs(t *testing.T) {
	spec := testSpec(t)
	args := SidecarArgs(spec, "bin-label", "172.30.0.2", "/state/dax-app-12345678")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run -d --init",
		"--name dax-app-12345678-proxy",
		"--label dax.box.proxy=dax-app-12345678",
		"--label dax.box.bin=bin-label",
		"--runtime runc",
		"--user 1000:1000",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--read-only",
		"--memory 128m --memory-swap 128m",
		"--pids-limit 64",
		"--network name=dax-box-egress",
		"--network name=dax-app-12345678-net,ip=172.30.0.2",
		"-v " + spec.Binary + ":/usr/local/bin/dax:ro",
		"-v /state/dax-app-12345678:/etc/dax-box:ro",
		"golang:1.26-bookworm /usr/local/bin/dax box proxy -listen :3128 -rules /etc/dax-box/egress.json",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("sidecar args lack %q:\n%s", want, joined)
		}
	}
	if slices.Contains(args, Label+"=1") {
		t.Errorf("the sidecar is labelled a box, so List would show it: %s", joined)
	}
	if strings.Contains(joined, spec.Dir) {
		t.Errorf("the sidecar mounts the project: %s", joined)
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
	side := func(state, bin string) answer { return answer{out: state + "\t" + bin + "\n"} }
	same := specLabel(spec)
	subnet := answer{out: "172.30.0.0/16 \n"}
	gone := answer{err: errors.New("docker network: network dax-app-12345678-net not found")}
	// What making a box on its network, with its sidecar, then checking
	// it, takes.
	made := []string{"ps proxy", "network inspect egress", "run proxy", "run", "ps", "exec", "exec check"}
	for _, tc := range []struct {
		name    string
		answers map[string][]answer
		verbs   []string
		err     string
		warned  bool
		ip      string // the sidecar's address the box was made with
	}{
		{
			name:    "absent, no network: network made with a subnet, sidecar, box, checked",
			answers: map[string][]answer{"ps": {{}, row("running", same, bin)}, "network inspect": {gone, subnet}},
			verbs:   append([]string{"ps", "network inspect", "network create", "network inspect", "network rm", "network create"}, made...),
			ip:      "172.30.0.2",
		},
		{
			name:    "absent, network left behind: network kept",
			answers: map[string][]answer{"ps": {{}, row("running", same, bin)}, "network inspect": {{out: "10.9.8.0/24 fd00::/64 \n"}}},
			verbs:   append([]string{"ps", "network inspect"}, made...),
			ip:      "10.9.8.2",
		},
		{
			name: "absent, no egress network: made",
			answers: map[string][]answer{"ps": {{}, row("running", same, bin)}, "network inspect": {subnet},
				"network inspect egress": {{err: errors.New("not found")}}},
			verbs: []string{"ps", "network inspect", "ps proxy", "network inspect egress", "network create egress", "run proxy", "run", "ps", "exec", "exec check"},
			ip:    "172.30.0.2",
		},
		{
			name:    "absent, a sidecar of another dax left behind: made again",
			answers: map[string][]answer{"ps": {{}, row("running", same, bin)}, "network inspect": {subnet}, "ps proxy": {side("exited", "old")}},
			verbs:   []string{"ps", "network inspect", "ps proxy", "rm proxy", "network inspect egress", "run proxy", "run", "ps", "exec", "exec check"},
			ip:      "172.30.0.2",
		},
		{
			name:    "running, sidecar running: used as they are",
			answers: map[string][]answer{"ps": {row("running", same, bin)}, "ps proxy": {side("running", bin)}},
			verbs:   []string{"ps", "ps proxy"},
		},
		{
			name:    "running, sidecar stopped: sidecar started and checked",
			answers: map[string][]answer{"ps": {row("running", same, bin)}, "ps proxy": {side("exited", bin)}, "network inspect": {subnet}},
			verbs:   []string{"ps", "ps proxy", "network inspect", "ps proxy", "start proxy", "exec check"},
		},
		{
			name:    "running, sidecar gone: sidecar made and checked",
			answers: map[string][]answer{"ps": {row("running", same, bin)}, "network inspect": {subnet}},
			verbs:   []string{"ps", "ps proxy", "network inspect", "ps proxy", "network inspect egress", "run proxy", "exec check"},
		},
		{
			name:    "stopped: sidecar started first, then the box, checked",
			answers: map[string][]answer{"ps": {row("exited", same, bin), row("running", same, bin)}, "ps proxy": {side("exited", bin)}, "network inspect": {subnet}},
			verbs:   []string{"ps", "network inspect", "ps proxy", "start proxy", "start", "ps", "exec", "exec check"},
		},
		{
			name:    "stopped, its network gone: refused",
			answers: map[string][]answer{"ps": {row("exited", same, bin)}, "network inspect": {gone}},
			verbs:   []string{"ps", "network inspect"},
			err:     "network is gone",
		},
		{
			name:    "other settings: refused",
			answers: map[string][]answer{"ps": {row("running", "other", bin)}},
			verbs:   []string{"ps"},
			err:     "other settings (image, runtime, limits or user) than these; remove it with `dax box down dax-app-12345678`",
		},
		{
			name:    "dax changed: box and sidecar made again with a warning",
			answers: map[string][]answer{"ps": {row("running", same, "old"), row("running", same, bin)}, "network inspect": {subnet}},
			verbs:   append([]string{"ps", "rm", "rm proxy", "network inspect"}, made...),
			warned:  true,
			ip:      "172.30.0.2",
		},
		{
			name:    "unknown runtime: refused, never another",
			answers: map[string][]answer{"ps": {{}}, "network inspect": {subnet}, "run": {{err: errors.New("docker run: docker: Error response from daemon: unknown or invalid runtime name: runsc")}}},
			verbs:   []string{"ps", "network inspect", "ps proxy", "network inspect egress", "run proxy", "run"},
			err:     "runtime not registered",
		},
		{
			name:    "dax does not run in the image",
			answers: map[string][]answer{"ps": {{}, row("running", same, bin)}, "network inspect": {subnet}, "exec": {{err: errors.New("docker exec: exec format error")}}},
			verbs:   []string{"ps", "network inspect", "ps proxy", "network inspect egress", "run proxy", "run", "ps", "exec"},
			err:     "glibc",
		},
		{
			name:    "sidecar does not answer",
			answers: map[string][]answer{"ps": {{}, row("running", same, bin)}, "network inspect": {subnet}, "exec check": {{err: errors.New("docker exec: connection refused")}}, "logs": {{out: "listen: address in use"}}},
			verbs:   append(append([]string{"ps", "network inspect"}, made...), "logs"),
			err:     "does not answer",
		},
		{
			name:    "made by another session meanwhile: that box used",
			answers: map[string][]answer{"ps": {{}, row("running", same, bin)}, "network inspect": {subnet}, "ps proxy": {{}, side("running", bin)}, "run": {{err: errors.New(`docker run: docker: Error response from daemon: Conflict. The container name "/dax-app-12345678" is already in use`)}}},
			verbs:   []string{"ps", "network inspect", "ps proxy", "network inspect egress", "run proxy", "run", "ps", "ps proxy"},
		},
		{
			name:    "sidecar made by another session meanwhile: taken",
			answers: map[string][]answer{"ps": {{}, row("running", same, bin)}, "network inspect": {subnet}, "run proxy": {{err: errors.New("docker run: Conflict. The container name is already in use")}}},
			verbs:   append([]string{"ps", "network inspect"}, made...),
			ip:      "172.30.0.2",
		},
		{
			name:    "taken twice: refused",
			answers: map[string][]answer{"ps": {{}, {}}, "network inspect": {subnet, subnet}, "run": {{err: errors.New("docker run: is already in use")}, {err: errors.New("docker run: is already in use")}}},
			verbs:   []string{"ps", "network inspect", "ps proxy", "network inspect egress", "run proxy", "run", "ps", "network inspect", "ps proxy", "network inspect egress", "run proxy", "run"},
			err:     "already in use",
		},
		{
			name:    "did not stay up",
			answers: map[string][]answer{"ps": {{}, row("exited", same, bin)}, "network inspect": {subnet}, "logs": {{out: "sleep: not found"}}},
			verbs:   []string{"ps", "network inspect", "ps proxy", "network inspect egress", "run proxy", "run", "ps", "logs"},
			err:     "sleep: not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{answers: tc.answers}
			warned := false
			c := &Client{Runner: f, Warn: func(string) { warned = true }, StateDir: t.TempDir()}
			b, err := c.Ensure(context.Background(), spec)
			if tc.err == "" && err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("Ensure error = %v, want one naming %q", err, tc.err)
			}
			if !slices.Equal(f.verbs(), tc.verbs) {
				t.Errorf("docker calls = %q,\n                 want %q", f.verbs(), tc.verbs)
			}
			if warned != tc.warned {
				t.Errorf("warned = %v, want %v", warned, tc.warned)
			}
			if b.Name != spec.Name || b.Dir != spec.Dir {
				t.Errorf("box = %+v", b)
			}
			for _, call := range f.calls {
				if verb(call) == "run" && !slices.Contains(call, "runsc") {
					t.Errorf("run without the spec's runtime: %q", call)
				}
				if verb(call) == "network create" && !slices.Contains(call, "--internal") {
					t.Errorf("the box's network is not internal: %q", call)
				}
			}
			if tc.ip != "" {
				if run := strings.Join(f.call("run"), " "); !strings.Contains(run, "HTTPS_PROXY=http://"+tc.ip+":3128") {
					t.Errorf("box not told its sidecar at %s: %s", tc.ip, run)
				}
				if run := strings.Join(f.call("run proxy"), " "); !strings.Contains(run, ",ip="+tc.ip) {
					t.Errorf("sidecar not at %s: %s", tc.ip, run)
				}
			}
			if creates := filterVerb(f.calls, "network create"); len(creates) == 2 && !slices.Contains(creates[1], "--subnet") {
				t.Errorf("the network is not made again with its subnet: %q", creates[1])
			}
			_, statErr := os.Stat(filepath.Join(c.StateDir, spec.Name, RulesFile))
			// A box refused for its settings keeps its rules; any other
			// run writes them first.
			if wrote := statErr == nil; wrote == strings.Contains(tc.err, "other settings") {
				t.Errorf("rules written = %v for %q", wrote, tc.name)
			}
		})
	}
}

func filterVerb(calls [][]string, v string) [][]string {
	var out [][]string
	for _, c := range calls {
		if verb(c) == v {
			out = append(out, c)
		}
	}
	return out
}

func TestEnsureRules(t *testing.T) {
	spec := testSpec(t)
	bin, err := binaryLabel(spec.Binary)
	if err != nil {
		t.Fatal(err)
	}
	running := func() *fake {
		return &fake{answers: map[string][]answer{
			"ps":       {{out: "running\t" + specLabel(spec) + "\t" + bin + "\n"}},
			"ps proxy": {{out: "running\t" + bin + "\n"}},
		}}
	}
	dir := t.TempDir()
	var warns, notices []string
	c := &Client{StateDir: dir, Warn: func(m string) { warns = append(warns, m) }, Notice: func(m string) { notices = append(notices, m) }}
	path := filepath.Join(dir, spec.Name, RulesFile)
	for _, step := range []struct {
		rules  Rules
		warn   string // what a warning names, "" for none
		notice bool
	}{
		{Rules{Internet: InternetOff}, "", true},
		{Rules{Internet: InternetOff}, "", true},
		{Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org"}}, "off -> allow proxy.golang.org", false},
		{Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org"}}, "", false},
		{Rules{Internet: InternetOn}, "allow proxy.golang.org -> on", false},
	} {
		warns, notices = nil, nil
		c.Runner = running()
		spec.Rules = step.rules
		if _, err := c.Ensure(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		got, err := ReadRules(path)
		if err != nil || !got.Same(step.rules) {
			t.Errorf("rules file = %+v (%v), want %+v", got, err, step.rules)
		}
		if step.warn == "" && len(warns) > 0 || step.warn != "" && (len(warns) != 1 || !strings.Contains(warns[0], step.warn) || !strings.Contains(warns[0], "every session")) {
			t.Errorf("rules %v: warnings %q, want one naming %q", step.rules, warns, step.warn)
		}
		if (len(notices) == 1) != step.notice {
			t.Errorf("rules %v: notices %q", step.rules, notices)
		}
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("state directory mode: %v %v", fi.Mode(), err)
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
	if _, err := (&Client{Runner: f, StateDir: t.TempDir()}).Ensure(context.Background(), spec); err == nil {
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
	if _, err := (&Client{Runner: f, StateDir: t.TempDir()}).Ensure(context.Background(), spec); err == nil {
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
		"uid":     func(s *Spec) { s.UID = 0 },
		"dir":     func(s *Spec) { s.Dir = "/elsewhere" },
	} {
		s := base
		change(&s)
		if specLabel(s) == specLabel(base) {
			t.Errorf("a change of %s keeps the label", name)
		}
	}
	for name, change := range map[string]func(*Spec){
		"binary": func(s *Spec) { s.Binary = "/other/dax" },
		"rules":  func(s *Spec) { s.Rules = Rules{Internet: InternetOn} },
	} {
		s := base
		change(&s)
		if specLabel(s) != specLabel(base) {
			t.Errorf("the %s is in the spec label; a change of it makes no other box", name)
		}
	}
}

func TestSidecarIP(t *testing.T) {
	for _, tc := range []struct{ subnet, ip, err string }{
		{"172.30.0.0/16", "172.30.0.2", ""},
		{"10.9.8.0/24", "10.9.8.2", ""},
		{"10.9.8.4/31", "", "too small"},
	} {
		c := &Client{Runner: &fake{answers: map[string][]answer{"network inspect": {{out: tc.subnet + " "}}}}}
		ip, err := c.proxyIP(context.Background(), "b")
		if ip != tc.ip || (tc.err == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("subnet %s: ip %q, err %v; want %q, %q", tc.subnet, ip, err, tc.ip, tc.err)
		}
	}
}

func TestListAndRemove(t *testing.T) {
	dir := t.TempDir()
	if err := WriteRules(filepath.Join(dir, "dax-a-1", RulesFile), Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org", "*.github.com"}}); err != nil {
		t.Fatal(err)
	}
	f := &fake{answers: map[string][]answer{"ps": {
		{out: "dax-a-1\trunning\t/src/a\trunsc\ndax-b-2\texited\t/src/b\trunsc-ptrace\n"},
		{},
	}}}
	c := &Client{Runner: f, StateDir: dir}
	boxes, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Info{{"dax-a-1", "running", "/src/a", "runsc", "allow proxy.golang.org,*.github.com"}, {"dax-b-2", "exited", "/src/b", "runsc-ptrace", "?"}}
	if !slices.Equal(boxes, want) {
		t.Errorf("List = %+v, want %+v", boxes, want)
	}
	if !slices.Contains(f.calls[0], "label=dax.box=1") {
		t.Errorf("List does not filter by the box label: %q", f.calls[0])
	}
	// The second ps finds nothing, nor is a sidecar or network left:
	// not a box, so nothing is removed.
	if err := c.Remove(context.Background(), "someone-elses"); err == nil || !strings.Contains(err.Error(), "no box") {
		t.Errorf("Remove of a container that is not a box: %v", err)
	}
	if slices.ContainsFunc(f.verbs(), func(v string) bool { return strings.HasPrefix(v, "rm") || v == "network rm" }) {
		t.Errorf("something was removed: %q", f.calls)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	if err := WriteRules(filepath.Join(dir, "dax-a-1", RulesFile), Rules{Internet: InternetOn}); err != nil {
		t.Fatal(err)
	}
	f := &fake{answers: map[string][]answer{
		"ps":         {{out: "running\tspec\tbin\n"}},
		"ps proxy":   {{out: "running\tbin\n"}},
		"network ls": {{out: "dax-a-1-net\n"}},
	}}
	c := &Client{Runner: f, StateDir: dir}
	if err := c.Remove(context.Background(), "dax-a-1"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ps", "rm", "ps proxy", "rm proxy", "network ls", "network rm"}; !slices.Equal(f.verbs(), want) {
		t.Errorf("docker calls = %q, want %q", f.verbs(), want)
	}
	if got := f.call("network rm"); got[len(got)-1] != "dax-a-1-net" {
		t.Errorf("network rm %q", got)
	}
	for _, c := range f.calls {
		if slices.Contains(c, EgressNetwork) {
			t.Errorf("the shared egress network was touched: %q", c)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "dax-a-1")); !os.IsNotExist(err) {
		t.Errorf("state directory left: %v", err)
	}
	// A box removed by hand leaves its sidecar and network: down still
	// removes them.
	f = &fake{answers: map[string][]answer{"ps proxy": {{out: "exited\tbin\n"}}, "network ls": {{out: "dax-a-1-net\n"}}}}
	c.Runner = f
	if err := c.Remove(context.Background(), "dax-a-1"); err != nil {
		t.Fatalf("Remove of what a box left: %v", err)
	}
	if want := []string{"ps", "ps proxy", "rm proxy", "network ls", "network rm"}; !slices.Equal(f.verbs(), want) {
		t.Errorf("docker calls = %q, want %q", f.verbs(), want)
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
