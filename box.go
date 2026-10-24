package dax

// This file is -box and `dax box`: a session's box, made by a box host
// (`dax box serve`, here or over ssh) whose relay is the session's
// executor; and the box host itself.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/ChristopherDavenport/dax/internal/box"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/tool"
)

// projectBox is the box of the project in dir, by name and directory
// alone, without running docker.
func projectBox(dir string) (box.Box, error) {
	pd, err := box.ProjectDir(dir)
	if err != nil {
		return box.Box{}, fmt.Errorf("box: %w", err)
	}
	return box.Box{Name: box.NameFor(pd), Dir: pd}, nil
}

// boxLauncher is the command that starts s's box host for the project
// in dir, and what the session asks of it. It is the session's
// executor command, and its words are the executor's address -list
// derives without starting anything: they name the project, so two
// projects on one box host are two addresses. Here it is this dax as
// `box serve`; over ssh, ssh running the box host's dax there.
func boxLauncher(s config.BoxSettings, dir string) (argv []string, request json.RawMessage, err error) {
	request, err = json.Marshal(s.Request)
	if err != nil {
		return nil, nil, err
	}
	if s.SSH == nil {
		b, err := projectBox(dir)
		if err != nil {
			return nil, nil, err
		}
		self, err := box.Self()
		if err != nil {
			return nil, nil, fmt.Errorf("box: this dax: %w", err)
		}
		argv = []string{self, "box", "serve", "-project", b.Dir}
		if s.HostConfig != "" {
			argv = append(argv, "-config", s.HostConfig)
		}
		return argv, request, nil
	}
	// The project there: a checkout the box host keeps, by its URL, or a
	// path, by default the same path as here, which the box host
	// resolves there.
	where := []string{"-project", s.Project}
	switch {
	case s.Checkout != nil:
		where = []string{"-checkout", s.Checkout.URL}
		if s.Checkout.Ref != "" {
			where = append(where, "-ref", s.Checkout.Ref)
		}
	case s.Project == "":
		b, err := projectBox(dir)
		if err != nil {
			return nil, nil, err
		}
		where[1] = b.Dir
	}
	remote := s.RemoteDax
	if remote == "" {
		remote = "dax"
	}
	argv = []string{"ssh", "-T", "-o", "BatchMode=yes"}
	if s.SSH.Port != "" {
		argv = append(argv, "-p", s.SSH.Port)
	}
	argv = append(argv, s.SSHArgs...)
	target := s.SSH.Host
	if s.SSH.User != "" {
		target = s.SSH.User + "@" + target
	}
	// ssh hands the remote command to the login's shell as one line, so
	// each word is quoted for it.
	argv = append(argv, "--", target, box.Quote(append([]string{remote, "box", "serve"}, where...)))
	return argv, request, nil
}

// ensureBox makes or starts, through c, the box of the project in dir
// as r's workspace unit says, mounting this dax, and gives it running.
// What it does beyond that, a first start or making it again, is said
// through say.
func ensureBox(ctx context.Context, c *box.Client, dir string, r box.Resolved, say func(string)) (box.Box, error) {
	b, err := projectBox(dir)
	if err != nil {
		return b, err
	}
	bin, err := box.Self()
	if err != nil {
		return b, fmt.Errorf("box: this dax: %w", err)
	}
	c.Warn, c.Starting, c.Notice = say, say, say
	w := r.Workspace
	return c.Ensure(ctx, box.Spec{
		Name: b.Name, Dir: b.Dir, Unit: w.Unit,
		Image: w.Image, Build: w.Build, Runtime: w.Runtime, Memory: w.Memory, CPUs: w.CPUs, Pids: w.Pids,
		Files:  w.Files,
		Binary: bin, UID: os.Getuid(), GID: os.Getgid(),
		Rules: w.Rules,
	})
}

// commandOf is r's MCP unit name's command.
func commandOf(r box.Resolved, name string) []string {
	for _, u := range r.Units {
		if u.Unit == name {
			return u.Command
		}
	}
	return nil
}

// unitSpec is the container of r's MCP unit u, of box b: its sidecar
// runs this dax in the workspace's image, which has the glibc it needs.
func unitSpec(b box.Box, r box.Resolved, u box.Settings) (box.Spec, error) {
	bin, err := box.Self()
	if err != nil {
		return box.Spec{}, fmt.Errorf("box: this dax: %w", err)
	}
	return box.Spec{
		Name: box.UnitName(b.Name, u.Unit), Of: b.Name, Unit: u.Unit, Dir: b.Dir,
		Image: u.Image, Build: u.Build, Runtime: u.Runtime, Memory: u.Memory, CPUs: u.CPUs, Pids: u.Pids,
		Files: u.Files, Secrets: u.Secrets, Decl: u.Decl,
		Binary: bin, UID: os.Getuid(), GID: os.Getgid(),
		Rules: u.Rules, SidecarImage: r.Workspace.Image,
	}, nil
}

// loadHostConfig is the box host config at path, or at its default
// place when path is empty.
func loadHostConfig(path string) (box.HostConfig, error) {
	if path != "" {
		return box.LoadHostConfig(path, true)
	}
	def, err := box.HostConfigPath()
	if err != nil {
		return box.HostConfig{}, err
	}
	return box.LoadHostConfig(def, false)
}

// runBox is `dax box`: serve, the box host; up, ls and down, for this
// machine's boxes; and proxy, the egress proxy a box's sidecar runs.
func runBox(ctx context.Context, args []string, p program, stdout io.Writer) error {
	if len(args) > 0 && args[0] == "serve" {
		return runBoxServe(ctx, args[1:], p, os.Stdin, stdout, &box.Client{Runner: box.CLI{}}, startExecutor)
	}
	return runBoxWith(ctx, args, p, stdout, &box.Client{Runner: box.CLI{}}, askBoxHost)
}

// askFunc sends one of the box host's own requests, method with params,
// to the box host argv starts with env, and gives its answer's result.
type askFunc func(ctx context.Context, argv, env []string, method string, params any) (json.RawMessage, error)

// askBoxHost starts the box host argv names, as -box would, sends it
// one request and reads its answer; the box host's standard error is
// this one's.
func askBoxHost(ctx context.Context, argv, env []string, method string, params any) (json.RawMessage, error) {
	req, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", 1, method, params})
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(append(req, '\n'))
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("box host: %w", err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	decErr := json.NewDecoder(stdout).Decode(&resp)
	io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	switch {
	case decErr == nil && resp.Error != nil:
		return nil, errors.New(resp.Error.Message)
	case decErr == nil:
		return resp.Result, nil
	case waitErr != nil:
		return nil, fmt.Errorf("box host: %w", waitErr)
	}
	return nil, fmt.Errorf("box host: no answer: %w", decErr)
}

// boxHostOf is the box host the user's config at cfgPath names, as a
// session's -box would start it for the project in dir, and the
// environment its launcher gets: this machine's without credentials,
// but those pass_env and executor.pass_env name. With no box in the
// config it is a local box host's default.
func boxHostOf(cfgPath, dir string) (*config.BoxSettings, []string, []string, error) {
	user, err := loadUser(cfgPath)
	if err != nil {
		return nil, nil, nil, err
	}
	pass := slices.Clone(user.PassEnv)
	if user.Executor != nil {
		pass = append(pass, user.Executor.PassEnv...)
	}
	// The config's executor is for sessions; these ask the box host.
	user.Executor = nil
	on := true
	bs, err := config.BoxOf(user, config.Flags{Box: &on})
	if err != nil {
		return nil, nil, nil, err
	}
	argv, _, err := boxLauncher(*bs, dir)
	if err != nil {
		return nil, nil, nil, err
	}
	key := user.APIKeyEnv
	env := tool.ChildEnv(os.Environ(), pass, key)
	return bs, argv, env, nil
}

// startExecutor starts argv, a box's `docker exec ... dax execute`,
// with its standard error the box host's.
func startExecutor(ctx context.Context, argv []string) (box.Child, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
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

type cmdChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
}

func (c cmdChild) Stdin() io.WriteCloser { return c.stdin }
func (c cmdChild) Stdout() io.Reader     { return c.stdout }
func (c cmdChild) Wait() error           { return c.cmd.Wait() }

// runBoxServe is `dax box serve`, the box host: over in and out, the
// session's executor protocol, relayed to the executor in the box the
// session's first message asks for, which it makes through c from its
// config; or, as the first message, one of the box host's own requests
// about its project (status, down, pull), answered before it exits. It
// is a session's executor command, here or over ssh, and writes nothing
// to out but the protocol. Its one project is -project, or -checkout's
// checkout, and no request reaches another.
func runBoxServe(ctx context.Context, args []string, p program, in io.Reader, out io.Writer, c *box.Client, start func(context.Context, []string) (box.Child, error)) error {
	fs := flag.NewFlagSet(p.name+" box serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "", "the box host config; default ~/.config/dax/box-host.json, built-in defaults when it is not there")
	project := fs.String("project", "", "the project's directory on this machine, mounted in the box at its own path; default the current directory")
	checkout := fs.String("checkout", "", "in place of -project, a repository's URL: the project is this box host's checkout of it, cloned and kept current within its config's checkouts.allow")
	ref := fs.String("ref", "", "with -checkout, the branch or tag; default the remote's default branch")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s box serve [flags]   (the box host: a session's executor, started by -box here or over ssh)\n", p.name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("box serve: unexpected argument %q", fs.Arg(0))
	case *checkout != "" && *project != "":
		return errors.New("box serve: -checkout and -project both name the project; give one")
	case *ref != "" && *checkout == "":
		return errors.New("box serve: -ref goes with -checkout")
	}
	if *ref != "" {
		if err := box.CheckRef("-ref", *ref); err != nil {
			return err
		}
	}
	h, err := loadHostConfig(*cfgPath)
	if err != nil {
		return err
	}
	say := func(msg string) { fmt.Fprintf(os.Stderr, "%s: %s\n", p.name, msg) }
	proj := boxProject{dir: *project}
	// A checkout the config does not take is the answer to whatever the
	// session asks, as a refused box is, so it reads why.
	var refused error
	if *checkout != "" {
		proj, refused = checkoutProject(h, *checkout, *ref)
	} else if proj.dir == "" {
		if proj.dir, err = os.Getwd(); err != nil {
			return err
		}
	}
	admin := func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if refused != nil {
			return nil, refused
		}
		return boxAdmin(ctx, c, proj, method, params)
	}
	return box.Relay(ctx, in, out, executor.CapabilityKey, func(ctx context.Context, req box.Request) (box.Hosted, error) {
		if refused != nil {
			return box.Hosted{}, refused
		}
		r, err := h.Resolve(req)
		if err != nil {
			return box.Hosted{}, err
		}
		var co *box.CheckoutInfo
		if proj.url != "" {
			// Cloned, or fetched and moved on only where nothing of a
			// session's is lost; what was done, or why not, is said.
			st, err := box.SyncCheckout(ctx, proj.dir, proj.url, proj.ref)
			if err != nil {
				return box.Hosted{}, err
			}
			say(fmt.Sprintf("checkout %s at %s: %s; head %s", box.Redact(proj.url), proj.dir, st.Note, short(st.Head)))
			co = &box.CheckoutInfo{URL: box.Redact(proj.url), Ref: proj.ref, Head: st.Head}
		}
		b, err := ensureBox(ctx, c, proj.dir, r, say)
		if err != nil {
			return box.Hosted{}, err
		}
		// The units are made with the box, before the session is answered:
		// a build is not held to a process start's timeout, and a unit that
		// cannot be made (a secret that is not set) refuses the session
		// with the reason.
		units := map[string]box.Spec{}
		var names []string
		for _, u := range r.Units {
			spec, err := unitSpec(b, r, u)
			if err != nil {
				return box.Hosted{}, err
			}
			if _, err := c.EnsureUnit(ctx, spec); err != nil {
				return box.Hosted{}, err
			}
			units[u.Unit] = spec
			names = append(names, u.Unit)
		}
		child, err := start(ctx, b.Command())
		if err != nil {
			return box.Hosted{}, err
		}
		info := r.BoxInfo()
		info.Checkout = co
		hosted := box.Hosted{Child: child, Info: info, Units: names}
		if len(units) > 0 {
			// A unit's server runs in its container, over this connection;
			// the container is started again first if it stopped since.
			// What it writes is redacted of the secrets its container
			// holds, and the session is told once per unit and secret.
			var mu sync.Mutex
			told := map[[2]string]bool{}
			hosted.StartUnit = func(ctx context.Context, name string, stderr bool) (box.Proc, error) {
				spec := units[name]
				ctr, err := c.EnsureUnit(ctx, spec)
				if err != nil {
					return nil, err
				}
				return c.StartUnitProc(ctx, spec, ctr, commandOf(r, name), stderr, func(secret string, _ int) {
					mu.Lock()
					first := !told[[2]string{name, secret}]
					told[[2]string{name, secret}] = true
					mu.Unlock()
					if first {
						say(fmt.Sprintf("unit %s: redacted %s in its output; a server that returns its secret is redacted by the box host, see docs/sandbox.md", name, secret))
					}
				})
			}
		}
		return hosted, nil
	}, box.WithAdmin(admin))
}

// boxProject is a box host's one project: a directory, and for a
// checkout its repository's URL and ref.
type boxProject struct {
	dir, url, ref string
}

// checkoutProject is the checkout of raw, a repository's URL, as the
// box host config h keeps it: refused unless h's checkouts.allow
// matches it.
func checkoutProject(h box.HostConfig, raw, ref string) (boxProject, error) {
	norm, err := box.NormalizeURL(raw)
	if err != nil {
		return boxProject{}, fmt.Errorf("-checkout %w", err)
	}
	switch {
	case len(h.Checkouts.Allow) == 0:
		return boxProject{}, fmt.Errorf("this box host keeps no checkouts: its config has no checkouts.allow, so %s is refused", box.Redact(norm))
	case !h.Checkouts.Allows(norm):
		return boxProject{}, fmt.Errorf("%s is not among this box host's checkouts.allow", box.Redact(norm))
	}
	dir, err := h.Checkouts.Path(norm)
	if err != nil {
		return boxProject{}, err
	}
	return boxProject{dir: dir, url: norm, ref: ref}, nil
}

// short is the first twelve hex digits of a commit.
func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// boxStatus is dax/box.status's answer: the project's box and units, and
// its git state, a checkout's with its URL.
type boxStatus struct {
	Project string             `json:"project"`
	Box     string             `json:"box"`
	Units   []boxStatusUnit    `json:"units"`
	Git     *box.CheckoutState `json:"git,omitempty"`
	Note    string             `json:"note,omitempty"`
}

type boxStatusUnit struct {
	Name     string `json:"name"`
	Unit     string `json:"unit"`
	State    string `json:"state"`
	Runtime  string `json:"runtime"`
	Files    string `json:"files"`
	Internet string `json:"internet"`
	Image    string `json:"image"`
}

// boxAdmin answers the box host's own requests about proj, its one
// project, through c.
func boxAdmin(ctx context.Context, c *box.Client, proj boxProject, method string, params json.RawMessage) (any, error) {
	_, statErr := os.Stat(proj.dir)
	missing := errors.Is(statErr, os.ErrNotExist)
	switch method {
	case box.MethodStatus:
		st := boxStatus{Project: proj.dir, Units: []boxStatusUnit{}}
		if missing {
			st.Note = "not there yet: a session's start makes it"
			if proj.url != "" {
				st.Note = "not cloned yet: a session's start clones it"
			}
			st.Box = box.NameFor(filepath.Clean(proj.dir))
			return st, nil
		}
		b, err := projectBox(proj.dir)
		if err != nil {
			return nil, err
		}
		st.Box = b.Name
		all, err := c.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, i := range all {
			if i.Name == b.Name || i.Of == b.Name {
				st.Units = append(st.Units, boxStatusUnit{Name: i.Name, Unit: i.Unit, State: i.State, Runtime: i.Runtime, Files: i.Files, Internet: i.Internet, Image: i.Image})
			}
		}
		if g, err := box.ReadCheckout(ctx, proj.dir); err == nil {
			g.URL, g.Ref = box.Redact(proj.url), proj.ref
			st.Git = &g
		}
		return st, nil
	case box.MethodDown:
		var p struct {
			State bool `json:"state"`
		}
		if len(params) > 0 && string(params) != "null" {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, fmt.Errorf("%s: %w", method, err)
			}
		}
		name := box.NameFor(filepath.Clean(proj.dir))
		if !missing {
			b, err := projectBox(proj.dir)
			if err != nil {
				return nil, err
			}
			name = b.Name
		}
		if err := c.Remove(ctx, name, p.State); err != nil {
			return nil, err
		}
		return map[string]any{"removed": name, "state": p.State}, nil
	case box.MethodPull:
		if missing {
			return nil, fmt.Errorf("pull: %s is not there yet: no session has started in it", proj.dir)
		}
		bundle, err := box.BundleHead(ctx, proj.dir, box.MaxBundle)
		if err != nil {
			return nil, err
		}
		bundle.Name = box.RepoName(filepath.Base(proj.dir))
		if proj.url != "" {
			bundle.Name = box.RepoName(proj.url)
		}
		return bundle, nil
	}
	return nil, fmt.Errorf("%s: not one of the box host's requests (status, down, pull)", method)
}

// runBoxWith is `dax box up|ls|down|proxy` over c, which the tests give
// a fake docker.
func runBoxWith(ctx context.Context, args []string, p program, stdout io.Writer, c *box.Client, ask askFunc) error {
	usage := fmt.Sprintf("usage: %[1]s box status [-config FILE]   (this project's box, its units and its checkout, from the box host your config's box names)\n       %[1]s box down [-state] [-config FILE]   (remove this project's box and its units, through that box host)\n       %[1]s box down [-state] NAME   (remove the box NAME on this machine's docker)\n       %[1]s box pull [-branch NAME] [-force] [-config FILE]   (fetch the box host's checkout's head into this repository)\n       %[1]s box up [flags]   (make or start this directory's box from this machine's box host config; print its executor's command)\n       %[1]s box ls   (every box on this machine's docker)\n       %[1]s box serve [flags]   (the box host, which -box starts)\n       %[1]s box proxy [flags]   (internal: the egress proxy a box's sidecar runs)", p.name)
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet(p.name+" box "+args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), usage)
		fs.PrintDefaults()
	}
	var cfgPath, profile, internet, listen, rules, check, userCfg, branch *string
	var dropState, force *bool
	switch args[0] {
	case "up":
		cfgPath = fs.String("config", "", "the box host config; default ~/.config/dax/box-host.json")
		profile = fs.String("profile", "", "the box host config's profile; default its default")
		internet = fs.String("internet", "", "narrow the profile's internet: off, allow (its hosts) or on")
	case "down":
		dropState = fs.Bool("state", false, "remove the units' state directories too (a RAG's index); kept by default")
		userCfg = fs.String("config", "", "with no NAME, your config, whose box names the box host; default ~/.config/dax/config.json")
	case "status":
		userCfg = fs.String("config", "", "your config, whose box names the box host; default ~/.config/dax/config.json")
	case "pull":
		userCfg = fs.String("config", "", "your config, whose box names the box host; default ~/.config/dax/config.json")
		branch = fs.String("branch", "", "the branch to fetch into; default dax-box/<the repository's name>")
		force = fs.Bool("force", false, "replace the branch even when it has commits the box's head does not")
	case "proxy":
		listen = fs.String("listen", ":"+box.ProxyPort, "address to serve on")
		rules = fs.String("rules", box.RulesMount+"/"+box.RulesFile, "the rules file, read again when it changes")
		check = fs.String("check", "", "ask the proxy at this address whether it is up, and exit")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	switch args[0] {
	case "up":
		if fs.NArg() > 0 {
			return fmt.Errorf("box up: unexpected argument %q", fs.Arg(0))
		}
		h, err := loadHostConfig(*cfgPath)
		if err != nil {
			return err
		}
		req := box.Request{Profile: *profile, Tighten: box.Tighten{Internet: *internet}}
		r, err := h.Resolve(req)
		if err != nil {
			return err
		}
		b, err := ensureBox(ctx, c, dir, r, func(msg string) { fmt.Fprintf(os.Stderr, "%s: %s\n", p.name, msg) })
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s -executor %s\n", p.name, box.Quote([]string{box.Quote(b.Command())}))
		return nil
	case "ls":
		if fs.NArg() > 0 {
			return fmt.Errorf("box ls: unexpected argument %q", fs.Arg(0))
		}
		boxes, err := c.List(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tUNIT\tSTATE\tRUNTIME\tFILES\tINTERNET\tDIR")
		for _, b := range boxes {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", b.Name, b.Unit, b.State, b.Runtime, b.Files, b.Internet, b.Dir)
		}
		return tw.Flush()
	case "proxy":
		if fs.NArg() > 0 {
			return fmt.Errorf("box proxy: unexpected argument %q", fs.Arg(0))
		}
		if *check != "" {
			return box.CheckProxy(ctx, *check)
		}
		logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
		fmt.Fprintf(os.Stderr, "%s box proxy: serving %s under %s\n", p.name, *listen, *rules)
		return box.ServeProxy(ctx, *listen, box.NewProxy(*rules, logf))
	case "down":
		switch fs.NArg() {
		case 0:
			// This project's box, through the box host that made it, here
			// or there: its state there goes with it.
			_, argv, env, err := boxHostOf(*userCfg, dir)
			if err != nil {
				return err
			}
			res, err := ask(ctx, argv, env, box.MethodDown, map[string]bool{"state": *dropState})
			if err != nil {
				return err
			}
			var r struct {
				Removed string `json:"removed"`
			}
			json.Unmarshal(res, &r)
			fmt.Fprintf(stdout, "removed %s\n", r.Removed)
			return nil
		case 1:
			if *userCfg != "" {
				return errors.New("box down NAME: acts on this machine's docker, with no config; -config goes without NAME")
			}
			name := fs.Arg(0)
			if err := c.Remove(ctx, name, *dropState); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "removed %s\n", name)
			return nil
		}
		return fmt.Errorf("box down: unexpected argument %q", fs.Arg(1))
	case "status":
		if fs.NArg() > 0 {
			return fmt.Errorf("box status: unexpected argument %q", fs.Arg(0))
		}
		_, argv, env, err := boxHostOf(*userCfg, dir)
		if err != nil {
			return err
		}
		res, err := ask(ctx, argv, env, box.MethodStatus, nil)
		if err != nil {
			return err
		}
		var st boxStatus
		if err := json.Unmarshal(res, &st); err != nil {
			return fmt.Errorf("box status: %w", err)
		}
		printStatus(stdout, st)
		return nil
	case "pull":
		if fs.NArg() > 0 {
			return fmt.Errorf("box pull: unexpected argument %q", fs.Arg(0))
		}
		bs, argv, env, err := boxHostOf(*userCfg, dir)
		if err != nil {
			return err
		}
		if bs.SSH == nil {
			return errors.New("box pull: the box host is local, so the project is already here: its commits are this directory's")
		}
		repo, err := gitTop(ctx, dir)
		if err != nil {
			return fmt.Errorf("box pull: run it in the git repository the work should come to: %w", err)
		}
		res, err := ask(ctx, argv, env, box.MethodPull, nil)
		if err != nil {
			return err
		}
		var b box.Bundle
		if err := json.Unmarshal(res, &b); err != nil {
			return fmt.Errorf("box pull: %w", err)
		}
		name := *branch
		if name == "" {
			name = "dax-box/" + b.Name
		}
		before, err := box.FetchBundle(ctx, repo, b, name, *force)
		if err != nil {
			return err
		}
		from := "a detached head"
		if b.Branch != "" {
			from = "branch " + b.Branch
		}
		switch {
		case before == "":
			fmt.Fprintf(stdout, "fetched %s (%s there) into %s, a new branch\n", short(b.Head), from, name)
		case before == b.Head:
			fmt.Fprintf(stdout, "%s is already at %s (%s there)\n", name, short(b.Head), from)
		default:
			fmt.Fprintf(stdout, "fetched %s (%s there) into %s, which was at %s\n", short(b.Head), from, name, short(before))
		}
		if b.Dirty {
			fmt.Fprintln(stdout, "the checkout there has uncommitted changes, which a pull does not bring: commit them in a session first")
		}
		return nil
	}
	return fmt.Errorf("box %q: want status, down, pull, up, ls, serve or proxy\n%s", args[0], usage)
}

// printStatus writes st as dax box status shows it.
func printStatus(w io.Writer, st boxStatus) {
	fmt.Fprintf(w, "project  %s\n", st.Project)
	if g := st.Git; g != nil {
		if g.URL != "" {
			ref := g.Ref
			if ref == "" {
				ref = "the remote's default branch"
			}
			fmt.Fprintf(w, "checkout %s (%s)\n", g.URL, ref)
		}
		on := "detached"
		if g.Branch != "" {
			on = g.Branch
		}
		dirty := ""
		if g.Dirty {
			dirty = ", uncommitted changes"
		}
		fmt.Fprintf(w, "head     %s on %s%s\n", short(g.Head), on, dirty)
	}
	if st.Note != "" {
		fmt.Fprintf(w, "note     %s\n", st.Note)
	}
	if len(st.Units) == 0 {
		fmt.Fprintf(w, "box      %s: not there\n", st.Box)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tUNIT\tSTATE\tRUNTIME\tFILES\tINTERNET\tIMAGE")
	for _, u := range st.Units {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", u.Name, u.Unit, u.State, u.Runtime, u.Files, u.Internet, u.Image)
	}
	tw.Flush()
}

// gitTop is the top of the git repository dir is in.
func gitTop(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s is not in a git repository", dir)
	}
	return strings.TrimSpace(string(out)), nil
}
