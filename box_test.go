package dax

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/internal/box"
	"github.com/ChristopherDavenport/dax/internal/cmdline"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/executor"
)

// boxDocker answers docker by its first argument, each answer once and
// the last for ever, and records the calls. A network has a subnet
// unless the test says otherwise.
type boxDocker struct {
	answers map[string][]string
	calls   [][]string
}

func (d *boxDocker) Docker(_ context.Context, args ...string) ([]byte, error) {
	d.calls = append(d.calls, args)
	if args[0] == "ps" && slices.Contains(args, "label=dax.box.of") {
		// A unit's container: none until the box host made one, then
		// running with the label it was made with.
		for _, c := range slices.Backward(d.calls) {
			if c[0] != "run" || !slices.ContainsFunc(c, func(a string) bool { return strings.HasPrefix(a, "dax.box.of=") }) {
				continue
			}
			for _, a := range c {
				if label, ok := strings.CutPrefix(a, "dax.box.spec="); ok {
					return []byte("running\t" + label + "\n"), nil
				}
			}
		}
		return nil, nil
	}
	q, ok := d.answers[args[0]]
	if !ok && args[0] == "network" {
		q = []string{"172.30.0.0/16 \n"}
	}
	if len(q) == 0 {
		return nil, nil
	}
	out := q[0]
	if len(q) > 1 {
		d.answers[args[0]] = q[1:]
	}
	return []byte(out), nil
}

func TestBoxCommand(t *testing.T) {
	home(t)
	dir := t.TempDir()
	t.Chdir(dir)
	here, err := projectBox(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := program{name: "dax", version: "test"}
	hostCfg := filepath.Join(t.TempDir(), "box-host.json")
	if err := os.WriteFile(hostCfg, []byte(`{"units":{"workspace":{},"open":{"internet":"on","runtime":"io.containerd.kata.v2","exfiltration":"accepted"}},"profiles":{"default":{"workspace":"workspace"},"open":{"workspace":"open"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		args    []string
		answers map[string][]string
		out     string // in what it prints, its spaces made one
		err     string
		calls   [][]string // docker calls it makes, among others
		rules   string     // the box's internet after it
	}{
		{
			name:    "ls",
			args:    []string{"ls"},
			answers: map[string][]string{"ps": {"dax-a-1\trunning\t/src/a\trunsc\n"}},
			out:     "NAME UNIT STATE RUNTIME FILES INTERNET DIR dax-a-1 workspace running runsc project rw ? /src/a",
		},
		{
			name:    "down takes this directory's box, its sidecar and its network",
			args:    []string{"down"},
			answers: map[string][]string{"ps": {"running\tx\ty\n"}, "network": {here.Name + "-net\n"}},
			out:     "removed " + here.Name,
			calls:   [][]string{{"rm", "-f", here.Name}, {"rm", "-f", here.Name + "-proxy"}, {"network", "rm", here.Name + "-net"}},
		},
		{
			name:    "down by name",
			args:    []string{"down", "dax-other-1"},
			answers: map[string][]string{"ps": {"exited\tx\ty\n", ""}, "network": {""}},
			calls:   [][]string{{"rm", "-f", "dax-other-1"}},
		},
		{
			name:    "down of no box",
			args:    []string{"down", "nginx"},
			answers: map[string][]string{"network": {""}},
			err:     "no box nginx",
		},
		{
			name:    "up prints the executor line",
			args:    []string{"up"},
			answers: map[string][]string{"ps": {"", "", "running\t\t\n"}},
			out:     "dax -executor ",
			rules:   "off",
		},
		{
			name:    "up -profile from the box host config",
			args:    []string{"up", "-config", hostCfg, "-profile", "open"},
			answers: map[string][]string{"ps": {"", "", "running\t\t\n"}},
			rules:   "on",
			calls:   [][]string{{"--runtime", "io.containerd.kata.v2"}},
		},
		{
			name:    "up -internet narrows the profile",
			args:    []string{"up", "-config", hostCfg, "-profile", "open", "-internet", "off"},
			answers: map[string][]string{"ps": {"", "", "running\t\t\n"}},
			rules:   "off",
		},
		{name: "up -internet widening the profile", args: []string{"up", "-internet", "on"}, err: "tighten.internet on is more than profile default's off"},
		{name: "up -internet sideways", args: []string{"up", "-internet", "sideways"}, err: "tighten.internet"},
		{name: "up -profile the box host does not have", args: []string{"up", "-profile", "big"}, err: `profile "big"`},
		{name: "up -config that is not there", args: []string{"up", "-config", "/nonexistent/box-host.json"}, err: "box host config"},
		{name: "proxy takes no name", args: []string{"proxy", "x"}, err: "unexpected argument"},
		{name: "no verb", args: nil, err: "usage: dax box status"},
		{name: "another verb", args: []string{"rm"}, err: `box "rm": want status, down, pull, up, ls, serve or proxy`},
		{name: "down NAME takes no config", args: []string{"down", "-config", "/x.json", "a"}, err: "-config goes without NAME"},
		{name: "pull from a local box host", args: []string{"pull"}, err: "the project is already here"},
		{name: "status takes no name", args: []string{"status", "x"}, err: "unexpected argument"},
		{name: "ls takes no name", args: []string{"ls", "x"}, err: "unexpected argument"},
		{name: "down takes one name", args: []string{"down", "a", "b"}, err: `unexpected argument "b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &boxDocker{answers: tc.answers}
			var out bytes.Buffer
			state := t.TempDir()
			c := &box.Client{Runner: d, StateDir: state}
			err := runBoxWith(context.Background(), tc.args, p, &out, c, askInProcess(p, c))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(strings.Fields(out.String()), " "); !strings.Contains(got, tc.out) {
				t.Errorf("printed %q, want %q in it", got, tc.out)
			}
			for _, want := range tc.calls {
				if !slices.ContainsFunc(d.calls, func(c []string) bool { return slices.Equal(c, want) || containsRun(c, want) }) {
					t.Errorf("no docker call %q in %q", want, d.calls)
				}
			}
			if tc.rules != "" {
				r, err := box.ReadRules(filepath.Join(state, here.Name, box.RulesFile))
				if err != nil || r.String() != tc.rules {
					t.Errorf("the box's internet = %v (%v), want %s", r, err, tc.rules)
				}
			}
		})
	}
}

// askInProcess is askBoxHost with the box host run here, in this
// process, over c: the request goes to runBoxServe as the launcher's
// words after "box serve" give it, as a box host started by them would
// read it.
func askInProcess(p program, c *box.Client) askFunc {
	return func(ctx context.Context, argv, _ []string, method string, params any) (json.RawMessage, error) {
		// Over ssh the box host's words are one, the remote command,
		// quoted for the login's shell: read back as that shell would.
		if last := argv[len(argv)-1]; argv[0] == "ssh" {
			words, err := cmdline.Split(last)
			if err != nil {
				return nil, err
			}
			argv = words
		}
		i := slices.Index(argv, "serve")
		if i < 0 {
			return nil, fmt.Errorf("not a box host's command: %q", argv)
		}
		req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		if err := runBoxServe(ctx, argv[i+1:], p, bytes.NewReader(append(req, '\n')), &out, c, nil); err != nil {
			return nil, err
		}
		var resp struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			return nil, fmt.Errorf("answer %q: %w", out.String(), err)
		}
		if resp.Error != nil {
			return nil, errors.New(resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// What up prints is a -executor line that splits back into the box's
// command, which -box would run.
func TestBoxUpPrintsTheBoxsCommand(t *testing.T) {
	home(t)
	dir := filepath.Join(t.TempDir(), "my project")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	d := &boxDocker{answers: map[string][]string{"ps": {"", "", "running\t\t\n"}}}
	var out bytes.Buffer
	if err := runBoxWith(context.Background(), []string{"up"}, program{name: "dax"}, &out, &box.Client{Runner: d, StateDir: t.TempDir()}, nil); err != nil {
		t.Fatal(err)
	}
	words, err := cmdline.Split(strings.TrimSpace(out.String()))
	if err != nil || len(words) != 3 || words[1] != "-executor" {
		t.Fatalf("printed %q: %q, %v", out.String(), words, err)
	}
	argv, err := cmdline.Split(words[2])
	if err != nil {
		t.Fatal(err)
	}
	b, err := projectBox(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, b.Command()) {
		t.Errorf("the -executor line runs %q, the box's command is %q", argv, b.Command())
	}
	i := slices.IndexFunc(d.calls, func(c []string) bool { return c[0] == "run" && slices.Contains(c, box.Label+"=1") })
	if i < 0 {
		t.Fatalf("up made no box: %q", d.calls)
	}
	if run := d.calls[i]; !slices.Contains(run, box.DefaultRuntime) || !slices.Contains(run, box.DefaultImage) {
		t.Errorf("up made the box with %q, want the defaults", run)
	}
}

// The sidecar's dax runs `dax box proxy`; its -check is what the box's
// check runs against it.
func TestBoxProxyCheck(t *testing.T) {
	rules := filepath.Join(t.TempDir(), box.RulesFile)
	srv := httptest.NewServer(box.NewProxy(rules, t.Logf))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := runBoxWith(context.Background(), []string{"proxy", "-check", addr}, program{name: "dax"}, &bytes.Buffer{}, &box.Client{}, nil); err != nil {
		t.Errorf("box proxy -check %s: %v", addr, err)
	}
}

// -list in a box lists the sessions run in it, from its address alone:
// the box host's command, here or over ssh. The test binary stands in
// for the box host (it serves as dax execute whatever its arguments),
// and a stand-in ssh on PATH runs it; -list then finds each session
// without starting either.
func TestListFindsTheBoxsSessions(t *testing.T) {
	h := home(t)
	bin := t.TempDir()
	ssh := filepath.Join(bin, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nexec '"+os.Args[0]+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := listRig{t: t, root: filepath.Join(t.TempDir(), "sessions")}
	here := t.TempDir()
	cfg := filepath.Join(h, "box.json")
	if err := os.WriteFile(cfg, []byte(`{"box":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sshCfg := filepath.Join(h, "box-ssh.json")
	if err := os.WriteFile(sshCfg, []byte(`{"box":{"host":"ssh://me@build:2222","project":"/srv/app"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	launcher := func(path string) []string {
		t.Helper()
		user, err := loadUser(path)
		if err != nil {
			t.Fatal(err)
		}
		bs, err := config.BoxOf(user, config.Flags{})
		if err != nil {
			t.Fatal(err)
		}
		argv, _, err := boxLauncher(*bs, here)
		if err != nil {
			t.Fatal(err)
		}
		return argv
	}
	local := r.record(here, nil, "")
	inBox := r.record(here, launcher(cfg), t.TempDir())
	overSSH := r.record(here, launcher(sshCfg), t.TempDir())
	// ssh is gone: -list must not need it.
	if err := os.Remove(ssh); err != nil {
		t.Fatal(err)
	}

	list := func(cfg string, f config.Flags) []string {
		t.Helper()
		var out bytes.Buffer
		if err := runList(context.Background(), &out, r.root, here, cfg, f); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			if f := strings.Fields(line); len(f) > 2 {
				ids = append(ids, f[2])
			}
		}
		return ids
	}
	on, off := true, false
	if got := list("", config.Flags{}); !slices.Equal(got, []string{local}) {
		t.Errorf("-list here = %q, want the local session %q", got, local)
	}
	// Each address finds its own session: the local box host's and the
	// ssh one's are two addresses, each the latest of its own workspace.
	for _, tc := range []struct {
		name, cfg string
		f         config.Flags
		want      string
	}{
		{"-box", "", config.Flags{Box: &on}, inBox},
		{"the config's box", cfg, config.Flags{}, inBox},
		{"the config's ssh box", sshCfg, config.Flags{}, overSSH},
	} {
		if got := list(tc.cfg, tc.f); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("-list with %s = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := list(cfg, config.Flags{Box: &off}); !slices.Equal(got, []string{local}) {
		t.Errorf("-list -box=false = %q, want %q", got, local)
	}
}

func TestBoxRefusalsOnTheCommandLine(t *testing.T) {
	h := home(t)
	t.Chdir(t.TempDir())
	cfg := filepath.Join(h, "executor.json")
	if err := os.WriteFile(cfg, []byte(`{"executor":{"command":"ssh host dax execute"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := program{name: "dax", version: "test"}
	for _, tc := range []struct {
		args  []string
		wants string
	}{
		{[]string{"-box", "-executor", "ssh host dax execute"}, "give -box=false to use this executor"},
		{[]string{"-box", "-config", cfg}, `give -executor "" to run them in a box`},
		{[]string{"-box-host", "ssh://build"}, "-box-host goes with -box"},
		{[]string{"-box-profile", "go"}, "-box-profile goes with -box"},
		{[]string{"-box", "-box-host", "build"}, "box.host"},
		{[]string{"-box", "-box-profile", "--privileged"}, "-box-profile"},
		{[]string{"-box-internet", "on"}, "-box-internet goes with -box"},
		{[]string{"-box", "-box-internet", "sideways"}, "want off, on or allow"},
		{[]string{"-box", "-list", "-executor", "ssh host dax execute"}, "give -box=false"},
	} {
		err := run(context.Background(), append(tc.args, "-sessions", "", "-p", "hi"), p)
		if err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.wants)
		}
	}
}

// containsRun reports whether call is a docker run with the words of
// want next to each other in it.
func containsRun(call, want []string) bool {
	if len(call) == 0 || call[0] != "run" {
		return false
	}
	for i := 0; i+len(want) <= len(call); i++ {
		if slices.Equal(call[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func TestBoxLauncher(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my 'project'")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := projectBox(dir)
	if err != nil {
		t.Fatal(err)
	}
	self, err := box.Self()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		s    config.BoxSettings
		want []string
		// remote is the command the ssh login's shell runs, as it
		// splits it.
		remote []string
	}{
		{
			name: "local",
			s:    config.BoxSettings{Host: config.BoxLocal},
			want: []string{self, "box", "serve", "-project", b.Dir},
		},
		{
			name: "local with a box host config",
			s:    config.BoxSettings{Host: config.BoxLocal, HostConfig: "/u/box-host.json"},
			want: []string{self, "box", "serve", "-project", b.Dir, "-config", "/u/box-host.json"},
		},
		{
			name:   "ssh, the same path there",
			s:      config.BoxSettings{Host: "ssh://build", SSH: &config.SSHTarget{Host: "build"}},
			want:   []string{"ssh", "-T", "-o", "BatchMode=yes", "--", "build"},
			remote: []string{"dax", "box", "serve", "-project", b.Dir},
		},
		{
			name:   "ssh with a user, a port, arguments, a path and a dax there",
			s:      config.BoxSettings{Host: "ssh://me@build:2222", SSH: &config.SSHTarget{User: "me", Host: "build", Port: "2222"}, SSHArgs: []string{"-F", "/u/ssh.conf"}, Project: "/srv/my app", RemoteDax: "/opt/dax/bin/dax"},
			want:   []string{"ssh", "-T", "-o", "BatchMode=yes", "-p", "2222", "-F", "/u/ssh.conf", "--", "me@build"},
			remote: []string{"/opt/dax/bin/dax", "box", "serve", "-project", "/srv/my app"},
		},
		{
			name:   "ssh, a checkout there: its URL in place of a path",
			s:      config.BoxSettings{Host: "ssh://build", SSH: &config.SSHTarget{Host: "build"}, Checkout: &config.BoxCheckout{URL: "https://github.com/me/app"}},
			want:   []string{"ssh", "-T", "-o", "BatchMode=yes", "--", "build"},
			remote: []string{"dax", "box", "serve", "-checkout", "https://github.com/me/app"},
		},
		{
			name:   "ssh, a checkout of a ref",
			s:      config.BoxSettings{Host: "ssh://build", SSH: &config.SSHTarget{Host: "build"}, Checkout: &config.BoxCheckout{URL: "ssh://git@github.com/me/app", Ref: "release/1.2"}},
			want:   []string{"ssh", "-T", "-o", "BatchMode=yes", "--", "build"},
			remote: []string{"dax", "box", "serve", "-checkout", "ssh://git@github.com/me/app", "-ref", "release/1.2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.s.Request = box.Request{Profile: "go", Tighten: box.Tighten{Internet: "off"}}
			argv, req, err := boxLauncher(tc.s, dir)
			if err != nil {
				t.Fatal(err)
			}
			if string(req) != `{"profile":"go","tighten":{"internet":"off"}}` {
				t.Errorf("request = %s", req)
			}
			if tc.remote == nil {
				if !slices.Equal(argv, tc.want) {
					t.Fatalf("argv = %q, want %q", argv, tc.want)
				}
				return
			}
			if len(argv) != len(tc.want)+1 || !slices.Equal(argv[:len(tc.want)], tc.want) {
				t.Fatalf("argv = %q, want %q and the remote command", argv, tc.want)
			}
			// The login's shell splits the remote command as cmdline does.
			got, err := cmdline.Split(argv[len(argv)-1])
			if err != nil || !slices.Equal(got, tc.remote) {
				t.Errorf("the remote command %q splits to %q (%v), want %q", argv[len(argv)-1], got, err, tc.remote)
			}
		})
	}
}

// fakeChild is the executor in the box for runBoxServe's test: it
// answers the session's initialize with a fixed line and ends when its
// input does.
type fakeChild struct {
	inW  *io.PipeWriter
	out  *io.PipeReader
	done chan struct{}
	got  *lockedBuffer
}

func newFakeChild() *fakeChild {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &fakeChild{inW: inW, out: outR, done: make(chan struct{}), got: &lockedBuffer{}}
	go func() {
		defer close(c.done)
		io.Copy(c.got, inR)
		outW.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n"))
		outW.Close()
	}()
	return c
}

func (c *fakeChild) Stdin() io.WriteCloser { return c.inW }
func (c *fakeChild) Stdout() io.Reader     { return c.out }
func (c *fakeChild) Wait() error           { <-c.done; return nil }

func TestBoxServe(t *testing.T) {
	home(t)
	dir := t.TempDir()
	b, err := projectBox(dir)
	if err != nil {
		t.Fatal(err)
	}
	hostCfg := filepath.Join(t.TempDir(), "box-host.json")
	if err := os.WriteFile(hostCfg, []byte(`{"default_profile":"go","units":{"go":{"image":"golang:1.26-bookworm","allow":["proxy.golang.org"],"exfiltration":"accepted"},"vm":{"runtime":"io.containerd.kata.v2"}},"profiles":{"go":{"workspace":"go"},"vm":{"workspace":"vm"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	init := func(box string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"experimental":{"` + executor.CapabilityKey + `":{"maxReadBytes":1024` + box + `}}}}}` + "\n"
	}
	p := program{name: "dax", version: "test"}
	for _, tc := range []struct {
		name    string
		args    []string
		first   string
		err     string
		rules   string   // the box's internet after it
		runWith []string // words next to each other in the box's docker run
	}{
		{name: "the default profile", args: []string{"-config", hostCfg, "-project", dir}, first: init(""), rules: "allow proxy.golang.org", runWith: []string{"--runtime", "runsc"}},
		{name: "a profile by name", args: []string{"-config", hostCfg, "-project", dir}, first: init(`,"box":{"profile":"vm"}`), rules: "off", runWith: []string{"--runtime", "io.containerd.kata.v2"}},
		{name: "a tightening", args: []string{"-config", hostCfg, "-project", dir}, first: init(`,"box":{"tighten":{"internet":"off","memory":"1g"}}`), rules: "off", runWith: []string{"--memory", "1g"}},
		{name: "a widening", args: []string{"-config", hostCfg, "-project", dir}, first: init(`,"box":{"tighten":{"internet":"on"}}`), err: "more than profile go's allow"},
		{name: "an MCP server", args: []string{"-config", hostCfg, "-project", dir}, first: init(`,"box":{"mcp":["github"]}`), err: "not one of profile go's MCP units (none)"},
		{name: "no box host config: the built-in", args: []string{"-project", dir}, first: init(""), rules: "off", runWith: []string{"golang:1.26-bookworm", "sleep", "infinity"}},
		{name: "a box host config that is not there", args: []string{"-config", "/nonexistent.json", "-project", dir}, first: init(""), err: "box host config"},
		{name: "a project that is not there", args: []string{"-project", "/nonexistent/project"}, first: init(""), err: "nonexistent"},
		{name: "an argument", args: []string{"x"}, err: "unexpected argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &boxDocker{answers: map[string][]string{"ps": {"", "", "running\t\t\n"}}}
			state := t.TempDir()
			var started []string
			child := newFakeChild()
			start := func(_ context.Context, argv []string) (box.Child, error) {
				started = argv
				return child, nil
			}
			var out bytes.Buffer
			err := runBoxServe(context.Background(), tc.args, p, strings.NewReader(tc.first), &out, &box.Client{Runner: d, StateDir: state}, start)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				if tc.first != "" && !strings.Contains(out.String(), tc.err) && !strings.Contains(err.Error(), "box host config") {
					t.Errorf("the session was told %q, want the reason %q", out.String(), tc.err)
				}
				if started != nil {
					t.Errorf("an executor was started after a refusal: %q", started)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(started, b.Command()) {
				t.Errorf("started %q, want the box's executor %q", started, b.Command())
			}
			if child.got.String() != tc.first {
				t.Errorf("the executor was sent %q, want the session's initialize as it came", child.got.String())
			}
			if !strings.Contains(out.String(), `"result"`) {
				t.Errorf("the session was sent %q, want the executor's answer", out.String())
			}
			if !slices.ContainsFunc(d.calls, func(c []string) bool {
				return c[0] == "run" && slices.Contains(c, box.Label+"=1") && containsRun(c, tc.runWith)
			}) {
				t.Errorf("no box run with %q in %q", tc.runWith, d.calls)
			}
			r, err := box.ReadRules(filepath.Join(state, b.Name, box.RulesFile))
			if err != nil || r.String() != tc.rules {
				t.Errorf("the box's internet = %v (%v), want %s", r, err, tc.rules)
			}
		})
	}
}

// A real session's handshake goes through the box host: the executor
// client dials runBoxServe over pipes, the box host makes the box
// (a fake docker) and starts the test binary as the dax execute in it,
// and the client gets that executor's workspace and tools, whichever
// handshake (server/discover or initialize) its SDK opens with.
func TestBoxServeRelaysARealSession(t *testing.T) {
	home(t)
	dir := t.TempDir()
	root := t.TempDir()
	start := func(ctx context.Context, argv []string) (box.Child, error) {
		cmd := exec.CommandContext(ctx, os.Args[0])
		cmd.Env = append(os.Environ(), executeRoot+"="+root)
		cmd.Stderr = os.Stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmdChild{cmd: cmd, stdin: stdin, stdout: stdout}, nil
	}
	for _, tc := range []struct {
		name string
		box  json.RawMessage
		err  string
	}{
		{name: "the default profile", box: json.RawMessage(`{}`)},
		{name: "a refused profile", box: json.RawMessage(`{"profile":"big"}`), err: `profile \"big\"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			d := &boxDocker{answers: map[string][]string{"ps": {"", "", "running\t\t\n"}}}
			toHost, fromSession := io.Pipe()
			toSession, fromHost := io.Pipe()
			served := make(chan error, 1)
			go func() {
				err := runBoxServe(ctx, []string{"-project", dir}, program{name: "dax", version: "test"}, toHost, fromHost, &box.Client{Runner: d, StateDir: t.TempDir()}, start)
				fromHost.Close()
				served <- err
			}()
			dial := func(context.Context) (sdk.Transport, error) {
				return &sdk.IOTransport{Reader: toSession, Writer: fromSession}, nil
			}
			r, err := executor.Dial(ctx, dial, executor.ConnectOptions{Name: "dax", Version: "test", Box: tc.box})
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), strings.ReplaceAll(tc.err, `\"`, `"`)) {
					t.Fatalf("Dial error = %v, want the box host's reason %q", err, tc.err)
				}
				fromSession.Close()
				<-served
				return
			}
			if err != nil {
				fromSession.Close()
				t.Fatalf("Dial: %v (the box host: %v)", err, <-served)
			}
			if desc := r.Descriptor(); desc.Kind != "container" || desc.Ref != "box" || desc.Root != root {
				t.Errorf("descriptor = %+v, want the in-box executor's", desc)
			}
			// What the box host resolved reaches the session, in the
			// handshake's answer: the built-in profile, its workspace unit.
			var info box.BoxInfo
			if err := json.Unmarshal(r.Box(), &info); err != nil || info.Profile != box.DefaultProfile || len(info.Units) != 1 || info.Units[0].Name != box.DefaultUnit || info.Units[0].Files != "project rw" || info.Units[0].Internet != box.InternetOff {
				t.Errorf("the session was told the box %s (%v)", r.Box(), err)
			}
			tools, err := r.Tools(ctx)
			if err != nil || len(tools) == 0 {
				t.Errorf("tools = %v, %v", tools, err)
			}
			r.Close()
			fromSession.Close()
			select {
			case <-served:
			case <-ctx.Done():
				t.Fatal("the box host did not end with the session")
			}
		})
	}
}

// A unit's server, through the box host: the handshake's answer names
// the session's units, and a dax/process.start of ["dax-unit", NAME]
// makes the unit's container (a fake docker) and runs the server in it
// (a stand-in docker that runs it here), answered by the box host, the
// executor never asked.
func TestBoxServeUnit(t *testing.T) {
	home(t)
	dir := t.TempDir()
	hostCfg := filepath.Join(t.TempDir(), "box-host.json")
	if err := os.WriteFile(hostCfg, []byte(`{"units":{"workspace":{},"echo":{"image":"node:24","command":["cat"],"files":{"project":"ro"}}},"profiles":{"default":{"workspace":"workspace","mcp":["echo"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stand := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(stand, []byte("#!/bin/sh\n[ \"$1\" = exec ] || exit 2\nshift\n[ \"$1\" = -i ] && shift\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := projectBox(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := &boxDocker{answers: map[string][]string{"ps": {"", "", "running\t\t\n"}}}
	c := &box.Client{Runner: d, StateDir: t.TempDir(), DataDir: t.TempDir(), DockerPath: stand}
	child := newFakeChild()
	start := func(context.Context, []string) (box.Child, error) { return child, nil }
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- runBoxServe(context.Background(), []string{"-config", hostCfg, "-project", dir}, program{name: "dax", version: "test"}, inR, outW, c, start)
		outW.Close()
	}()
	lines := make(chan string, 16)
	go func() {
		br := bufio.NewReader(outR)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	next := func() string {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("the box host ended")
			}
			return l
		case <-time.After(10 * time.Second):
			t.Fatal("no answer from the box host")
		}
		return ""
	}
	// The unit's container is made on its first start (boxDocker answers
	// its ps from the run the box host gave).
	io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"experimental":{"`+executor.CapabilityKey+`":{"box":{}}}}}}`+"\n")
	// The fake executor answers the initialize when its input ends; the
	// box host's own answers come first.
	io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"dax/process.start","params":{"args":["dax-unit","echo"]}}`+"\n")
	got := next()
	if !strings.Contains(got, `"id":"unit-1"`) {
		t.Fatalf("start answered %s", got)
	}
	io.WriteString(inW, `{"jsonrpc":"2.0","id":3,"method":"dax/process.write","params":{"id":"unit-1","data":"aGkK"}}`+"\n")
	io.WriteString(inW, `{"jsonrpc":"2.0","id":4,"method":"dax/process.read","params":{"id":"unit-1","fd":1}}`+"\n")
	for range 2 {
		if l := next(); strings.Contains(l, `"id":4`) && !strings.Contains(l, `"data":"aGkK"`) {
			t.Fatalf("read answered %s", l)
		}
	}
	inW.Close()
	init := next()
	if !strings.Contains(init, `"result"`) {
		t.Fatalf("the executor's answer: %s", init)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(child.got.String(), "dax-unit") {
		t.Errorf("the executor was sent the unit's start: %s", child.got.String())
	}
	if !slices.ContainsFunc(d.calls, func(c []string) bool {
		return c[0] == "run" && slices.Contains(c, "dax.box.of="+b.Name) && containsRun(c, []string{"-v", dir + ":" + dir + ":ro"})
	}) {
		t.Errorf("no unit run with the project read-only in %q", d.calls)
	}
}

// isolateGit keeps git from the machine's config and gives commits a
// name, for the tests that run it.
func isolateGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
}

func gitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A checkout end to end, in this process: the box host keeps only what
// its config allows, a session's start clones it, and status, pull and
// down reach it through the box host the user's config names, over the
// ssh launcher's words, never another project.
func TestBoxServeCheckout(t *testing.T) {
	h := home(t)
	isolateGit(t)
	ctx := context.Background()
	p := program{name: "dax", version: "test"}
	src := filepath.Join(t.TempDir(), "srv")
	up := filepath.Join(src, "up")
	os.MkdirAll(src, 0o755)
	gitAt(t, src, "init", "--quiet", "-b", "main", up)
	os.WriteFile(filepath.Join(up, "a.txt"), []byte("one"), 0o644)
	gitAt(t, up, "add", "a.txt")
	gitAt(t, up, "commit", "--quiet", "-m", "one")
	url := "file://" + up
	coDir := filepath.Join(t.TempDir(), "checkouts")
	cfgDir := filepath.Join(h, ".config", "dax")
	os.MkdirAll(cfgDir, 0o700)
	hostCfg := filepath.Join(cfgDir, "box-host.json")
	write := func(allow string) {
		t.Helper()
		data := `{"checkouts":{"allow":[` + allow + `],"dir":"` + coDir + `"}}`
		if allow == "" {
			data = `{}`
		}
		if err := os.WriteFile(hostCfg, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ask := func(c *box.Client, method string, params any, args ...string) (json.RawMessage, error) {
		return askInProcess(p, c)(ctx, append([]string{"dax", "box", "serve"}, args...), nil, method, params)
	}
	c := &box.Client{Runner: &boxDocker{}, StateDir: t.TempDir()}

	// Refused before anything: no checkouts.allow, then one that does not
	// match.
	write("")
	if _, err := ask(c, box.MethodStatus, nil, "-checkout", url); err == nil || !strings.Contains(err.Error(), "keeps no checkouts") {
		t.Errorf("no allow: %v", err)
	}
	write(`"https://github.com/me/*"`)
	if _, err := ask(c, box.MethodStatus, nil, "-checkout", url); err == nil || !strings.Contains(err.Error(), "not among") {
		t.Errorf("not allowed: %v", err)
	}
	write(`"file://` + src + `/*"`)
	if _, err := ask(c, box.MethodStatus, nil, "-checkout", "file://"+src+"/up/../../etc"); err == nil {
		t.Error("took a path with ..")
	}

	// Before a session: not cloned, nothing made.
	res, err := ask(c, box.MethodStatus, nil, "-checkout", url)
	if err != nil {
		t.Fatal(err)
	}
	var st boxStatus
	json.Unmarshal(res, &st)
	if !strings.Contains(st.Note, "not cloned yet") || !strings.HasPrefix(st.Project, coDir) {
		t.Errorf("status before: %+v", st)
	}
	if _, err := os.Stat(st.Project); !os.IsNotExist(err) {
		t.Errorf("status made %s", st.Project)
	}

	// A session's start clones it, and the box's executor acts there.
	d := &boxDocker{answers: map[string][]string{"ps": {"", "", "running\t\t\n"}}}
	sc := &box.Client{Runner: d, StateDir: t.TempDir()}
	var started []string
	start := func(_ context.Context, argv []string) (box.Child, error) {
		started = argv
		return newFakeChild(), nil
	}
	first := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}` + "\n"
	var out bytes.Buffer
	if err := runBoxServe(ctx, []string{"-checkout", url}, p, strings.NewReader(first), &out, sc, start); err != nil {
		t.Fatal(err)
	}
	if gitAt(t, st.Project, "rev-parse", "HEAD") != gitAt(t, up, "rev-parse", "HEAD") {
		t.Error("not cloned at the upstream's head")
	}
	if !slices.Contains(started, st.Project) {
		t.Errorf("the executor started %q, not in %s", started, st.Project)
	}

	// The session's work, committed in the checkout, comes back through
	// pull, from a user config that names the box host over ssh.
	os.WriteFile(filepath.Join(st.Project, "b.txt"), []byte("from the box"), 0o644)
	gitAt(t, st.Project, "add", "b.txt")
	gitAt(t, st.Project, "commit", "--quiet", "-m", "from the box")
	head := gitAt(t, st.Project, "rev-parse", "HEAD")
	userCfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(userCfg, []byte(`{"box":{"host":"ssh://me@build","checkout":{"url":"`+url+`.git/"}}}`), 0o600)
	local := filepath.Join(t.TempDir(), "local")
	gitAt(t, filepath.Dir(local), "init", "--quiet", "-b", "main", local)
	t.Chdir(local)
	var printed bytes.Buffer
	if err := runBoxWith(ctx, []string{"pull", "-config", userCfg}, p, &printed, c, askInProcess(p, c)); err != nil {
		t.Fatal(err)
	}
	if got := gitAt(t, local, "rev-parse", "dax-box/up"); got != head {
		t.Errorf("dax-box/up at %s, want %s", got, head)
	}
	if !strings.Contains(printed.String(), "into dax-box/up, a new branch") {
		t.Errorf("pull printed %q", printed.String())
	}
	printed.Reset()
	if err := runBoxWith(ctx, []string{"status", "-config", userCfg}, p, &printed, c, askInProcess(p, c)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"checkout " + url + " (the remote's default branch)", "head     " + head[:12] + " on main"} {
		if !strings.Contains(printed.String(), want) {
			t.Errorf("status printed %q, want %q in it", printed.String(), want)
		}
	}
	// Down acts on this box host's project's box, whatever it is asked.
	res, err = ask(c, box.MethodDown, map[string]any{"state": true, "box": "dax-other-1"}, "-checkout", url)
	var removed struct {
		Removed string `json:"removed"`
	}
	json.Unmarshal(res, &removed)
	if err != nil || removed.Removed != box.NameFor(st.Project) {
		t.Errorf("down removed %q (%v), want %s", removed.Removed, err, box.NameFor(st.Project))
	}
	// Another request of the box host's.
	if _, err := ask(c, "dax/box.rm", nil, "-checkout", url); err == nil || !strings.Contains(err.Error(), "not one of the box host's requests") {
		t.Errorf("rm: %v", err)
	}
}
