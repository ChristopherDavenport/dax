package dax

// This file is -box and `dax box`: a session's box, made by a box host
// (`dax box serve`, here or over ssh) whose relay is the session's
// executor; and the box host itself.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"text/tabwriter"

	"github.com/ChristopherDavenport/dax/internal/box"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/executor"
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
	project := s.Project
	if project == "" {
		// The same path as here, which the box host resolves there.
		b, err := projectBox(dir)
		if err != nil {
			return nil, nil, err
		}
		project = b.Dir
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
	argv = append(argv, "--", target, box.Quote([]string{remote, "box", "serve", "-project", project}))
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
	return runBoxWith(ctx, args, p, stdout, &box.Client{Runner: box.CLI{}})
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
// config. It is a session's executor command, here or over ssh, and
// writes nothing to out but the protocol.
func runBoxServe(ctx context.Context, args []string, p program, in io.Reader, out io.Writer, c *box.Client, start func(context.Context, []string) (box.Child, error)) error {
	fs := flag.NewFlagSet(p.name+" box serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "", "the box host config; default ~/.config/dax/box-host.json, built-in defaults when it is not there")
	project := fs.String("project", "", "the project's directory on this machine, mounted in the box at its own path; default the current directory")
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
	if fs.NArg() > 0 {
		return fmt.Errorf("box serve: unexpected argument %q", fs.Arg(0))
	}
	h, err := loadHostConfig(*cfgPath)
	if err != nil {
		return err
	}
	dir := *project
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return err
		}
	}
	say := func(msg string) { fmt.Fprintf(os.Stderr, "%s: %s\n", p.name, msg) }
	return box.Relay(ctx, in, out, executor.CapabilityKey, func(ctx context.Context, req box.Request) (box.Hosted, error) {
		r, err := h.Resolve(req)
		if err != nil {
			return box.Hosted{}, err
		}
		b, err := ensureBox(ctx, c, dir, r, say)
		if err != nil {
			return box.Hosted{}, err
		}
		child, err := start(ctx, b.Command())
		if err != nil {
			return box.Hosted{}, err
		}
		hosted := box.Hosted{Child: child, Info: r.BoxInfo()}
		units := map[string]box.Settings{}
		for _, u := range r.Units {
			units[u.Unit] = u
			hosted.Units = append(hosted.Units, u.Unit)
		}
		if len(units) > 0 {
			// A unit's container is made when its server is first started,
			// and its server runs there, over this connection.
			hosted.StartUnit = func(ctx context.Context, name string, stderr bool) (box.Proc, error) {
				spec, err := unitSpec(b, r, units[name])
				if err != nil {
					return nil, err
				}
				ctr, err := c.EnsureUnit(ctx, spec)
				if err != nil {
					return nil, err
				}
				return c.StartProc(ctx, ctr, units[name].Command, stderr)
			}
		}
		return hosted, nil
	})
}

// runBoxWith is `dax box up|ls|down|proxy` over c, which the tests give
// a fake docker.
func runBoxWith(ctx context.Context, args []string, p program, stdout io.Writer, c *box.Client) error {
	usage := fmt.Sprintf("usage: %[1]s box up [flags]   (make or start this directory's box from this machine's box host config; print its executor's command)\n       %[1]s box ls\n       %[1]s box down [-state] [name]   (remove a box and its units; default this directory's)\n       %[1]s box serve [flags]   (the box host, which -box starts)\n       %[1]s box proxy [flags]   (internal: the egress proxy a box's sidecar runs)", p.name)
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet(p.name+" box "+args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), usage)
		fs.PrintDefaults()
	}
	var cfgPath, profile, internet, listen, rules, check *string
	var dropState *bool
	switch args[0] {
	case "up":
		cfgPath = fs.String("config", "", "the box host config; default ~/.config/dax/box-host.json")
		profile = fs.String("profile", "", "the box host config's profile; default its default")
		internet = fs.String("internet", "", "narrow the profile's internet: off, allow (its hosts) or on")
	case "down":
		dropState = fs.Bool("state", false, "remove the units' state directories too (a RAG's index); kept by default")
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
		name := ""
		switch fs.NArg() {
		case 0:
			b, err := projectBox(dir)
			if err != nil {
				return err
			}
			name = b.Name
		case 1:
			name = fs.Arg(0)
		default:
			return fmt.Errorf("box down: unexpected argument %q", fs.Arg(1))
		}
		if err := c.Remove(ctx, name, *dropState); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s\n", name)
		return nil
	}
	return fmt.Errorf("box %q: want up, ls, down, serve or proxy\n%s", args[0], usage)
}
