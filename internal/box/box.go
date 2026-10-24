// Package box keeps a project's box: a long-lived container, run under
// a sandboxing runtime (gVisor's runsc by default), that a session's
// tools run in through `docker exec -i ... dax execute`. The project's
// directory is mounted at its own path, this machine's dax binary is
// mounted read-only as the box's dax, so the executor is always the
// session's version, and the container holds nothing else that
// survives it: its root file system is read-only and its home and /tmp
// are memory.
//
// Docker is driven through its command line (Runner), so the package
// needs nothing beyond the standard library, and it is meant to move to
// agentworkspace once a second product wants it.
package box

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The labels a box carries. Label marks a container as a box, so List
// and Remove touch no other.
const (
	Label        = "dax.box"
	labelDir     = "dax.box.dir"
	labelRuntime = "dax.box.runtime"
	labelSpec    = "dax.box.spec"
	labelBinary  = "dax.box.bin"
)

// Binary is where the box's dax is mounted.
const Binary = "/usr/local/bin/dax"

// Home is the box's home directory, memory that any user may write.
const Home = "/home/dax"

// Spec is what a box is made from.
type Spec struct {
	// Name is the container's name; NameFor gives a project's.
	Name string
	// Dir is the project's directory on this machine, mounted at the
	// same path and the box's working directory.
	Dir string
	// Image must have bash, sh and git, and a C library the host's dax
	// runs on (glibc) unless dax was built with CGO_ENABLED=0.
	Image string
	// Runtime is the OCI runtime docker runs the box with: runsc for
	// gVisor. It is never replaced by another.
	Runtime string
	// Memory is the box's memory limit as docker takes it ("4g"), swap
	// included, so it has none beyond it.
	Memory string
	// CPUs and Pids bound the box's processors and processes.
	CPUs float64
	Pids int64
	// Network is the docker network: bridge, none or one of yours.
	Network string
	// Binary is this machine's dax, mounted read-only at box.Binary.
	Binary string
	// UID and GID the box's processes run as, so the files they write
	// in Dir are the user's.
	UID, GID int
}

// Box is a running box.
type Box struct {
	Name, Dir string
}

// Command is the program and its arguments that start `dax execute`
// in b, speaking MCP on its standard input and output. The workspace
// it serves is kind container, ref the box's name, root the project's
// directory: the project's identity, the same however often the box is
// made again.
func (b Box) Command() []string {
	return []string{"docker", "exec", "-i", b.Name, Binary, "execute", "-root", b.Dir, "-kind", "container", "-ref", b.Name}
}

// NameFor is the box name of the project in dir: dax-, the directory's
// base as a container name may spell it, and the first eight hex digits
// of the SHA-256 of dir, so two projects with one base differ.
func NameFor(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, filepath.Base(dir))
	if len(base) > 32 {
		base = base[:32]
	}
	base = strings.Trim(base, "-._")
	if base == "" {
		base = "root"
	}
	return "dax-" + base + "-" + hex.EncodeToString(sum[:4])
}

// ProjectDir is dir as boxes name it: absolute with its links
// resolved, so a project reached through a link is one box.
func ProjectDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Self is this program's executable with its links resolved, the dax
// a box mounts.
func Self() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// Runner runs docker with args and returns what it wrote on standard
// output; its error carries what it wrote on standard error.
type Runner interface {
	Docker(ctx context.Context, args ...string) ([]byte, error)
}

// CLI is the docker command on PATH.
type CLI struct{}

// Docker runs docker.
func (CLI) Docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("docker %s: %s", args[0], msg)
		}
		return out, fmt.Errorf("docker %s: %w", args[0], err)
	}
	return out, nil
}

// Client keeps boxes through a Runner. Warn, when set, is told what
// Ensure does that the caller did not ask for: making a box again.
type Client struct {
	Runner Runner
	Warn   func(msg string)
	// Starting, when set, is told before a box is made, which may pull
	// its image first.
	Starting func(msg string)
}

// Info is a box as List finds it.
type Info struct {
	Name, State, Dir, Runtime string
}

// ErrRuntime is a runtime docker does not know.
var ErrRuntime = errors.New("runtime not registered with docker")

// Ensure gives the box spec describes, running: the one there, started
// again if it stopped, or a new one. A box made from other settings is
// refused, since making it again would end every other session's tools
// in it; a box whose dax changed is made again, with a warning, since
// its mount holds the old binary and an executor of another version may
// not speak this session's protocol.
func (c *Client) Ensure(ctx context.Context, spec Spec) (Box, error) {
	b := Box{Name: spec.Name, Dir: spec.Dir}
	bin, err := binaryLabel(spec.Binary)
	if err != nil {
		return b, err
	}
	have, found, err := c.find(ctx, spec.Name)
	if err != nil {
		return b, err
	}
	switch {
	case !found:
		return b, c.create(ctx, spec, bin)
	case have.spec != specLabel(spec):
		return b, fmt.Errorf("box %s was made with other settings (image, runtime, limits or user) than these; remove it with `dax box down %s` and start again", spec.Name, spec.Name)
	case have.bin != bin:
		c.warn(fmt.Sprintf("dax changed since box %s was made; making it again, which ends any other session's tools in it", spec.Name))
		if _, err := c.Runner.Docker(ctx, "rm", "-f", spec.Name); err != nil {
			return b, err
		}
		return b, c.create(ctx, spec, bin)
	case have.state != "running":
		if _, err := c.Runner.Docker(ctx, "start", spec.Name); err != nil {
			return b, err
		}
		return b, c.check(ctx, spec)
	}
	return b, nil
}

// List is every box, running or not.
func (c *Client) List(ctx context.Context) ([]Info, error) {
	out, err := c.Runner.Docker(ctx, "ps", "-a", "--filter", "label="+Label+"=1", "--format",
		`{{.Names}}\t{{.State}}\t{{.Label "`+labelDir+`"}}\t{{.Label "`+labelRuntime+`"}}`)
	if err != nil {
		return nil, err
	}
	var boxes []Info
	for _, line := range lines(out) {
		f := strings.Split(line, "\t")
		for len(f) < 4 {
			f = append(f, "")
		}
		boxes = append(boxes, Info{Name: f[0], State: f[1], Dir: f[2], Runtime: f[3]})
	}
	return boxes, nil
}

// Remove stops and removes the box name; a container that is not a
// box is refused.
func (c *Client) Remove(ctx context.Context, name string) error {
	_, found, err := c.find(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no box %s", name)
	}
	_, err = c.Runner.Docker(ctx, "rm", "-f", name)
	return err
}

// RunArgs are docker's arguments that make the box: no capabilities
// and no way to gain any, a read-only root file system with memory for
// /tmp and the home, the limits, and two mounts, the project at its own
// path and dax read-only. Its process does nothing but keep it up; the
// tools run in the executors docker exec starts.
func RunArgs(spec Spec, bin string) []string {
	return []string{
		"run", "-d", "--init",
		"--name", spec.Name,
		"--label", Label + "=1",
		"--label", labelDir + "=" + spec.Dir,
		"--label", labelRuntime + "=" + spec.Runtime,
		"--label", labelSpec + "=" + specLabel(spec),
		"--label", labelBinary + "=" + bin,
		"--runtime", spec.Runtime,
		"--user", strconv.Itoa(spec.UID) + ":" + strconv.Itoa(spec.GID),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--tmpfs", "/tmp:exec",
		"--tmpfs", Home + ":exec,mode=1777",
		"-e", "HOME=" + Home,
		"--memory", spec.Memory, "--memory-swap", spec.Memory,
		"--cpus", strconv.FormatFloat(spec.CPUs, 'f', -1, 64),
		"--pids-limit", strconv.FormatInt(spec.Pids, 10),
		"--network", spec.Network,
		"-v", spec.Dir + ":" + spec.Dir,
		"-w", spec.Dir,
		"-v", spec.Binary + ":" + Binary + ":ro",
		spec.Image,
		"sleep", "infinity",
	}
}

func (c *Client) create(ctx context.Context, spec Spec, bin string) error {
	if c.Starting != nil {
		c.Starting(fmt.Sprintf("starting box %s (%s under %s); a first start pulls the image", spec.Name, spec.Image, spec.Runtime))
	}
	if _, err := c.Runner.Docker(ctx, RunArgs(spec, bin)...); err != nil {
		if strings.Contains(err.Error(), "unknown or invalid runtime name") {
			return fmt.Errorf("box %s: %w: %s; register it (for gVisor, see docs/sandbox.md, \"gVisor\") or name another with box.runtime", spec.Name, ErrRuntime, spec.Runtime)
		}
		return fmt.Errorf("box %s: %w", spec.Name, err)
	}
	return c.check(ctx, spec)
}

// check makes sure the box stayed up and its dax runs there.
func (c *Client) check(ctx context.Context, spec Spec) error {
	have, found, err := c.find(ctx, spec.Name)
	if err != nil {
		return err
	}
	if !found || have.state != "running" {
		logs, _ := c.Runner.Docker(ctx, "logs", "--tail", "20", spec.Name)
		return fmt.Errorf("box %s did not stay up (its image needs sleep): %s", spec.Name, strings.TrimSpace(string(logs)))
	}
	if _, err := c.Runner.Docker(ctx, "exec", spec.Name, Binary, "execute", "-h"); err != nil {
		return fmt.Errorf("box %s cannot run dax (%w): its image needs bash, sh, git and a glibc at least as new as the one dax was built against; an image without glibc (alpine) needs a dax built with CGO_ENABLED=0", spec.Name, err)
	}
	return nil
}

type found struct{ state, spec, bin string }

// find is the box name's state and labels; false when there is none.
func (c *Client) find(ctx context.Context, name string) (found, bool, error) {
	out, err := c.Runner.Docker(ctx, "ps", "-a", "--filter", "label="+Label+"=1", "--filter", "name=^"+name+"$", "--format",
		`{{.State}}\t{{.Label "`+labelSpec+`"}}\t{{.Label "`+labelBinary+`"}}`)
	if err != nil {
		return found{}, false, err
	}
	ls := lines(out)
	if len(ls) == 0 {
		return found{}, false, nil
	}
	f := strings.Split(ls[0], "\t")
	for len(f) < 3 {
		f = append(f, "")
	}
	return found{state: f[0], spec: f[1], bin: f[2]}, true, nil
}

func (c *Client) warn(msg string) {
	if c.Warn != nil {
		c.Warn(msg)
	}
}

// specLabel is a hash of what the box was made from, but its dax: a box
// made from other settings is another box.
func specLabel(s Spec) string {
	h := sha256.New()
	for _, v := range []string{s.Dir, s.Image, s.Runtime, s.Memory, strconv.FormatFloat(s.CPUs, 'f', -1, 64), strconv.FormatInt(s.Pids, 10), s.Network, strconv.Itoa(s.UID), strconv.Itoa(s.GID)} {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// binaryLabel names the dax at path as it is now: its path, size and
// modification time. `go install` writes a new file, which a box's
// mount does not see, so a label that differs is a box to make again.
func binaryLabel(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("box: dax binary: %w", err)
	}
	sum := sha256.Sum256([]byte(path))
	return fmt.Sprintf("%s-%d-%d", hex.EncodeToString(sum[:4]), fi.Size(), fi.ModTime().UnixNano()), nil
}

func lines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Quote is argv as a command line cmdline.Split reads back, for
// printing: a word with anything but letters, digits and -_./:=@,+ is
// single-quoted.
func Quote(argv []string) string {
	words := make([]string, len(argv))
	for i, w := range argv {
		safe := w != ""
		for _, r := range w {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@,+", r)) {
				safe = false
				break
			}
		}
		if safe {
			words[i] = w
		} else {
			words[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
		}
	}
	return strings.Join(words, " ")
}
