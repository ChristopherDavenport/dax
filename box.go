package dax

// This file is -box and `dax box`: the project's box, a container dax
// keeps (internal/box), whose `dax execute` is the session's executor.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/ChristopherDavenport/dax/internal/box"
	"github.com/ChristopherDavenport/dax/internal/config"
)

// projectBox is the box of the project in dir, by name and directory
// alone: what -list derives its address from, without running docker.
func projectBox(dir string) (box.Box, error) {
	pd, err := box.ProjectDir(dir)
	if err != nil {
		return box.Box{}, fmt.Errorf("box: %w", err)
	}
	return box.Box{Name: box.NameFor(pd), Dir: pd}, nil
}

// ensureBox makes or starts, through c, the box of the project in dir
// with the settings s, mounting this dax, and gives it running. What it
// does beyond that, a first start or making it again, is said through
// say.
func ensureBox(ctx context.Context, c *box.Client, dir string, s config.BoxSettings, say func(string)) (box.Box, error) {
	b, err := projectBox(dir)
	if err != nil {
		return b, err
	}
	bin, err := box.Self()
	if err != nil {
		return b, fmt.Errorf("box: this dax: %w", err)
	}
	c.Warn, c.Starting = say, say
	return c.Ensure(ctx, box.Spec{
		Name: b.Name, Dir: b.Dir,
		Image: s.Image, Runtime: s.Runtime, Memory: s.Memory, CPUs: s.CPUs, Pids: s.Pids, Network: s.Network,
		Binary: bin, UID: os.Getuid(), GID: os.Getgid(),
	})
}

// runBox is `dax box`: up, ls and down, for the boxes -box keeps.
func runBox(ctx context.Context, args []string, p program, stdout io.Writer) error {
	return runBoxWith(ctx, args, p, stdout, &box.Client{Runner: box.CLI{}})
}

// runBoxWith is runBox over c, which the tests give a fake docker.
func runBoxWith(ctx context.Context, args []string, p program, stdout io.Writer, c *box.Client) error {
	usage := fmt.Sprintf("usage: %[1]s box up [flags]   (make or start this directory's box; print the -executor line)\n       %[1]s box ls\n       %[1]s box down [name]   (remove a box; default this directory's)", p.name)
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet(p.name+" box "+args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), usage)
		fs.PrintDefaults()
	}
	var cfgPath, image, runtime *string
	if args[0] == "up" {
		cfgPath = fs.String("config", "", "user config file, whose box settings up takes; default ~/.config/dax/config.json")
		image = fs.String("image", "", "the box's image, in place of box.image")
		runtime = fs.String("runtime", "", "the box's OCI runtime, in place of box.runtime")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	switch args[0] {
	case "up":
		if fs.NArg() > 0 {
			return fmt.Errorf("box up: unexpected argument %q", fs.Arg(0))
		}
		user, err := loadUser(*cfgPath)
		if err != nil {
			return err
		}
		on := true
		f := config.Flags{Box: &on}
		if given["image"] {
			f.BoxImage = image
		}
		if given["runtime"] {
			f.BoxRuntime = runtime
		}
		// A config's executor is for sessions; up makes a box whatever
		// it says.
		user.Executor = nil
		s, err := config.BoxOf(user, f)
		if err != nil {
			return err
		}
		b, err := ensureBox(ctx, c, dir, *s, func(msg string) { fmt.Fprintf(os.Stderr, "%s: %s\n", p.name, msg) })
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
		fmt.Fprintln(tw, "NAME\tSTATE\tRUNTIME\tDIR")
		for _, b := range boxes {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.Name, b.State, b.Runtime, b.Dir)
		}
		return tw.Flush()
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
		if err := c.Remove(ctx, name); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s\n", name)
		return nil
	}
	return fmt.Errorf("box %q: want up, ls or down\n%s", args[0], usage)
}
