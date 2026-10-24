// Package box keeps a project's box: a long-lived container, run under
// a sandboxing runtime (runsc by default; any runtime docker has
// registered, Kata's among them), that a session's
// tools run in through `docker exec -i ... dax execute`. The project's
// directory is mounted at its own path, this machine's dax binary is
// mounted read-only as the box's dax, so the executor is always the
// session's version, and the container holds nothing else that
// survives it: its root file system is read-only and its home and /tmp
// are memory.
//
// A box has no route out. It sits alone on an internal network of its
// own with its sidecar, a second container that runs this dax's egress
// proxy (Proxy) and is the box's only way to the internet: HTTP and
// HTTPS, to the hosts its rules file takes (Rules), which a session
// writes and the proxy reads again, so the box's internet changes
// without making either container again.
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
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The labels a box carries. Label marks a container as a box, so List
// and Remove touch no other; labelProxy marks a box's sidecar, and
// labelOwner a box's network, with the box's name.
const (
	Label        = "dax.box"
	labelDir     = "dax.box.dir"
	labelRuntime = "dax.box.runtime"
	labelSpec    = "dax.box.spec"
	labelBinary  = "dax.box.bin"
	labelProxy   = "dax.box.proxy"
	labelOwner   = "dax.box.owner"
)

// Binary is where the box's dax is mounted.
const Binary = "/usr/local/bin/dax"

// Home is the box's home directory, memory that any user may write.
const Home = "/home/dax"

// EgressNetwork is the ordinary docker network every box's sidecar
// reaches the internet through; the boxes are never on it.
const EgressNetwork = "dax-box-egress"

// ProxyRuntime is the sidecar's runtime. Under some sandboxing
// runtimes (gVisor's, verified) docker's own name server on a
// user-defined network does not answer, so the sidecar, which must
// resolve the hosts it is asked for, runs under runc; it runs only
// dax's proxy, never a command the model wrote.
const ProxyRuntime = "runc"

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
	// Runtime is the OCI runtime docker runs the box with: runsc,
	// io.containerd.kata.v2, any docker has registered. It is never
	// replaced by another.
	Runtime string
	// Memory is the box's memory limit as docker takes it ("4g"), swap
	// included, so it has none beyond it.
	Memory string
	// CPUs and Pids bound the box's processors and processes.
	CPUs float64
	Pids int64
	// Binary is this machine's dax, mounted read-only at box.Binary.
	Binary string
	// UID and GID the box's processes run as, so the files they write
	// in Dir are the user's.
	UID, GID int
	// Rules are the box's internet. They are not what the box is made
	// from: each session writes them, for every session in the box.
	Rules Rules
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

// ProxyName is box name's sidecar.
func ProxyName(name string) string { return name + "-proxy" }

// NetworkName is box name's internal network.
func NetworkName(name string) string { return name + "-net" }

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
// Ensure does that the caller did not ask for: making a box again, or
// changing the internet of a box other sessions share.
type Client struct {
	Runner Runner
	Warn   func(msg string)
	// Starting, when set, is told before a box is made, which may pull
	// its image first.
	Starting func(msg string)
	// Notice, when set, is told what a session should know of its box
	// each time: that it has no internet.
	Notice func(msg string)
	// StateDir is where each box's rules file is kept, in a directory
	// named for the box; empty is dax/box under the user's cache
	// directory.
	StateDir string
}

// Info is a box as List finds it.
type Info struct {
	Name, State, Dir, Runtime, Internet string
}

// ErrRuntime is a runtime docker does not know.
var ErrRuntime = errors.New("runtime not registered with docker")

// isUnknownRuntime reports whether docker refused a run for its
// runtime: docker says "unknown or invalid runtime name: X" whichever
// runtime was asked for, and some versions "unknown runtime".
func isUnknownRuntime(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown or invalid runtime") || strings.Contains(msg, "unknown runtime")
}

// errInUse is a box's name taken between looking for it and making it.
var errInUse = errors.New("name already in use")

// stateDir is the directory that holds box name's rules file.
func (c *Client) stateDir(name string) (string, error) {
	root := c.StateDir
	if root == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("box: %w", err)
		}
		root = filepath.Join(cache, "dax", "box")
	}
	return filepath.Join(root, name), nil
}

// Ensure gives the box spec describes, running, with its sidecar and
// under spec's rules: the one there, started again if it stopped, or a
// new one. A box made from other settings is refused, since making it
// again would end every other session's tools in it; a box whose dax
// changed is made again, with a warning, since its mount holds the old
// binary and an executor of another version may not speak this
// session's protocol.
func (c *Client) Ensure(ctx context.Context, spec Spec) (Box, error) {
	b := Box{Name: spec.Name, Dir: spec.Dir}
	home, err := os.UserHomeDir()
	if err != nil {
		return b, fmt.Errorf("box: %w", err)
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	if err := CheckMounts(spec, home); err != nil {
		return b, err
	}
	bin, err := binaryLabel(spec.Binary)
	if err != nil {
		return b, err
	}
	if err := c.ensure(ctx, spec, bin, true); err != nil {
		return b, err
	}
	if spec.Rules.Internet == InternetOff {
		c.notice(fmt.Sprintf("box %s has no internet; -box-internet=on or box.allow to open it", spec.Name))
	}
	return b, nil
}

// ensure is Ensure once the spec is checked. Another session may make
// the same box between find and run; then docker refuses the name, and
// ensure looks again, once, and takes the box that session made.
func (c *Client) ensure(ctx context.Context, spec Spec, bin string, again bool) error {
	have, found, err := c.find(ctx, spec.Name)
	if err != nil {
		return err
	}
	if found && have.spec != specLabel(spec) {
		return fmt.Errorf("box %s was made with other settings (image, runtime, limits or user) than these; remove it with `dax box down %s` and start again", spec.Name, spec.Name)
	}
	if err := c.setRules(spec); err != nil {
		return err
	}
	switch {
	case !found:
		err := c.create(ctx, spec, bin)
		if again && errors.Is(err, errInUse) {
			return c.ensure(ctx, spec, bin, false)
		}
		return err
	case have.bin != bin:
		c.warn(fmt.Sprintf("dax changed since box %s was made; making it again, which ends any other session's tools in it", spec.Name))
		for _, n := range []string{spec.Name, ProxyName(spec.Name)} {
			if _, err := c.Runner.Docker(ctx, "rm", "-f", n); err != nil {
				return err
			}
		}
		return c.create(ctx, spec, bin)
	case have.state != "running":
		ip, err := c.proxyIP(ctx, spec.Name)
		if err != nil {
			return err
		}
		if err := c.upSidecar(ctx, spec, bin, ip); err != nil {
			return err
		}
		if _, err := c.Runner.Docker(ctx, "start", spec.Name); err != nil {
			return err
		}
		return c.check(ctx, spec, ip)
	}
	// Running: its sidecar may have stopped, or been removed, on its own.
	state, sbin, err := c.sidecar(ctx, spec.Name)
	if err != nil {
		return err
	}
	if state == "running" && sbin == bin {
		return nil
	}
	ip, err := c.proxyIP(ctx, spec.Name)
	if err != nil {
		return err
	}
	if err := c.upSidecar(ctx, spec, bin, ip); err != nil {
		return err
	}
	return c.checkProxy(ctx, spec, ip)
}

// setRules writes spec's rules to the box's rules file, warning when
// they change those of a box other sessions may share.
func (c *Client) setRules(spec Spec) error {
	dir, err := c.stateDir(spec.Name)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, RulesFile)
	if old, err := ReadRules(path); err == nil && !old.Same(spec.Rules) {
		c.warn(fmt.Sprintf("box %s internet: %s -> %s; applies to every session in the box", spec.Name, old, spec.Rules))
	}
	if err := WriteRules(path, spec.Rules); err != nil {
		return fmt.Errorf("box %s: rules: %w", spec.Name, err)
	}
	return nil
}

// CheckMounts refuses a spec whose mounts would defeat the box or that
// docker would misread: a project at the root, at home or above it,
// whose mount would hand the tools every key and config there (dax's
// own among them), and a path with a colon, where docker's -v splits.
func CheckMounts(spec Spec, home string) error {
	for _, p := range []string{spec.Dir, spec.Binary} {
		if strings.Contains(p, ":") {
			return fmt.Errorf("box: %s: a path with a colon cannot be mounted", p)
		}
	}
	dir := filepath.Clean(spec.Dir)
	if dir == "/" || (home != "" && (dir == home || strings.HasPrefix(home, dir+"/"))) {
		return fmt.Errorf("box: %s holds your home directory, which the box would mount with every key and config in it; start dax in the project's directory", spec.Dir)
	}
	return nil
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
		info := Info{Name: f[0], State: f[1], Dir: f[2], Runtime: f[3], Internet: "?"}
		if dir, err := c.stateDir(info.Name); err == nil {
			if r, err := ReadRules(filepath.Join(dir, RulesFile)); err == nil {
				info.Internet = r.String()
			}
		}
		boxes = append(boxes, info)
	}
	return boxes, nil
}

// Remove stops and removes the box name, its sidecar, its network and
// its rules; a container that is not a box is refused. What a box left
// behind without itself (a sidecar, a network) is removed too.
func (c *Client) Remove(ctx context.Context, name string) error {
	_, found, err := c.find(ctx, name)
	if err != nil {
		return err
	}
	if found {
		if _, err := c.Runner.Docker(ctx, "rm", "-f", name); err != nil {
			return err
		}
	}
	left := false
	if state, _, err := c.sidecar(ctx, name); err == nil && state != "" {
		left = true
		if _, err := c.Runner.Docker(ctx, "rm", "-f", ProxyName(name)); err != nil {
			return err
		}
	}
	if out, err := c.Runner.Docker(ctx, "network", "ls", "--filter", "label="+labelOwner+"="+name, "--format", "{{.Name}}"); err == nil && len(lines(out)) > 0 {
		left = true
		if _, err := c.Runner.Docker(ctx, "network", "rm", NetworkName(name)); err != nil {
			return err
		}
	}
	if dir, err := c.stateDir(name); err == nil {
		os.RemoveAll(dir)
	}
	if !found && !left {
		return fmt.Errorf("no box %s", name)
	}
	return nil
}

// RunArgs are docker's arguments that make the box: no capabilities
// and no way to gain any, a read-only root file system with memory for
// /tmp and the home, the limits, two mounts, the project at its own
// path and dax read-only, and its internal network alone, with its
// sidecar at proxyIP as the proxy every command is told of. Its process
// does nothing but keep it up; the tools run in the executors docker
// exec starts.
func RunArgs(spec Spec, bin, proxyIP string) []string {
	proxy := "http://" + proxyIP + ":" + ProxyPort
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
		// The Go image's GOPATH, /go, is on the read-only root: its
		// module cache goes in the home with the build cache.
		"-e", "GOPATH=" + Home + "/go",
		"-e", "HTTP_PROXY=" + proxy, "-e", "HTTPS_PROXY=" + proxy,
		"-e", "http_proxy=" + proxy, "-e", "https_proxy=" + proxy,
		"-e", "NO_PROXY=localhost,127.0.0.1", "-e", "no_proxy=localhost,127.0.0.1",
		"--memory", spec.Memory, "--memory-swap", spec.Memory,
		"--cpus", strconv.FormatFloat(spec.CPUs, 'f', -1, 64),
		"--pids-limit", strconv.FormatInt(spec.Pids, 10),
		"--network", NetworkName(spec.Name),
		"-v", spec.Dir + ":" + spec.Dir,
		"-w", spec.Dir,
		"-v", spec.Binary + ":" + Binary + ":ro",
		spec.Image,
		"sleep", "infinity",
	}
}

// SidecarArgs are docker's arguments that make box spec's sidecar: the
// box's image and dax under runc, as hardened as the box and smaller,
// on the egress network and on the box's network at ip, with the box's
// rules directory read-only, running dax's proxy, which listens on its
// address on the box's network alone: the egress network, which every
// box's sidecar shares, never reaches it.
func SidecarArgs(spec Spec, bin, ip, rulesDir string) []string {
	return []string{
		"run", "-d", "--init",
		"--name", ProxyName(spec.Name),
		"--label", labelProxy + "=" + spec.Name,
		"--label", labelBinary + "=" + bin,
		"--runtime", ProxyRuntime,
		"--user", strconv.Itoa(spec.UID) + ":" + strconv.Itoa(spec.GID),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--memory", "128m", "--memory-swap", "128m",
		"--pids-limit", "64",
		"--network", "name=" + EgressNetwork,
		"--network", "name=" + NetworkName(spec.Name) + ",ip=" + ip,
		"-v", spec.Binary + ":" + Binary + ":ro",
		"-v", rulesDir + ":" + RulesMount + ":ro",
		spec.Image,
		Binary, "box", "proxy", "-listen", ip + ":" + ProxyPort, "-rules", RulesMount + "/" + RulesFile,
	}
}

func (c *Client) create(ctx context.Context, spec Spec, bin string) error {
	if c.Starting != nil {
		c.Starting(fmt.Sprintf("starting box %s (%s under %s); a first start pulls the image", spec.Name, spec.Image, spec.Runtime))
	}
	ip, err := c.boxNetwork(ctx, spec.Name)
	if err != nil {
		return err
	}
	if err := c.upSidecar(ctx, spec, bin, ip); err != nil {
		return err
	}
	if _, err := c.Runner.Docker(ctx, RunArgs(spec, bin, ip)...); err != nil {
		if isUnknownRuntime(err) {
			return fmt.Errorf("box %s: %w: %s; register it with docker (docs/sandbox.md, \"Runtimes\") or name another in the box host's config", spec.Name, ErrRuntime, spec.Runtime)
		}
		if strings.Contains(err.Error(), "is already in use") {
			return fmt.Errorf("box %s: %w", spec.Name, errInUse)
		}
		return fmt.Errorf("box %s: %w", spec.Name, err)
	}
	return c.check(ctx, spec, ip)
}

// egressNetwork makes the network sidecars reach the internet through,
// unless it is there.
func (c *Client) egressNetwork(ctx context.Context) error {
	if _, err := c.Runner.Docker(ctx, "network", "inspect", "-f", "{{.Name}}", EgressNetwork); err == nil {
		return nil
	}
	if _, err := c.Runner.Docker(ctx, "network", "create", "--label", Label+"=1", EgressNetwork); err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("box: network %s: %w", EgressNetwork, err)
	}
	return nil
}

// boxNetwork makes box name's internal network, unless it is there, and
// gives the address its sidecar has on it. docker gives a fixed address
// only on a network made with a subnet, so the network is made once to
// learn a subnet docker has free, and again with it.
func (c *Client) boxNetwork(ctx context.Context, name string) (string, error) {
	if ip, err := c.proxyIP(ctx, name); err == nil {
		return ip, nil
	}
	nw := NetworkName(name)
	labels := []string{"--label", Label + "=1", "--label", labelOwner + "=" + name}
	if _, err := c.Runner.Docker(ctx, append(append([]string{"network", "create", "--internal"}, labels...), nw)...); err != nil {
		return "", fmt.Errorf("box %s: network: %w", name, err)
	}
	subnet, err := c.subnet(ctx, nw)
	if _, rmErr := c.Runner.Docker(ctx, "network", "rm", nw); rmErr != nil && err == nil {
		err = rmErr
	}
	if err != nil {
		return "", fmt.Errorf("box %s: network: %w", name, err)
	}
	if _, err := c.Runner.Docker(ctx, append(append([]string{"network", "create", "--internal", "--subnet", subnet.String()}, labels...), nw)...); err != nil {
		return "", fmt.Errorf("box %s: network: %w", name, err)
	}
	return sidecarIP(subnet)
}

// proxyIP is the address box name's sidecar has on the box's network,
// which must be there.
func (c *Client) proxyIP(ctx context.Context, name string) (string, error) {
	subnet, err := c.subnet(ctx, NetworkName(name))
	if err != nil {
		return "", fmt.Errorf("box %s: its network is gone (%w); remove it with `dax box down %s` and start again", name, err, name)
	}
	return sidecarIP(subnet)
}

// subnet is network nw's first IPv4 subnet.
func (c *Client) subnet(ctx context.Context, nw string) (netip.Prefix, error) {
	out, err := c.Runner.Docker(ctx, "network", "inspect", "-f", "{{range .IPAM.Config}}{{.Subnet}} {{end}}", nw)
	if err != nil {
		return netip.Prefix{}, err
	}
	for _, f := range strings.Fields(string(out)) {
		if p, err := netip.ParsePrefix(f); err == nil && p.Addr().Is4() {
			return p.Masked(), nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("network %s has no IPv4 subnet", nw)
}

// sidecarIP is the sidecar's address in subnet: the one after the
// gateway, which docker gives the first.
func sidecarIP(subnet netip.Prefix) (string, error) {
	ip := subnet.Addr().Next().Next()
	if !subnet.Contains(ip) {
		return "", fmt.Errorf("subnet %s is too small for a sidecar", subnet)
	}
	return ip.String(), nil
}

// sidecar is the state of box name's sidecar and the dax it was made
// with; "" when it is gone.
func (c *Client) sidecar(ctx context.Context, name string) (state, bin string, err error) {
	out, err := c.Runner.Docker(ctx, "ps", "-a", "--filter", "label="+labelProxy+"="+name, "--format", `{{.State}}\t{{.Label "`+labelBinary+`"}}`)
	if err != nil {
		return "", "", err
	}
	ls := lines(out)
	if len(ls) == 0 {
		return "", "", nil
	}
	state, bin, _ = strings.Cut(ls[0], "\t")
	return state, bin, nil
}

// upSidecar gives box spec's sidecar running with this dax: the one
// there, started if it stopped, or a new one when it is gone or holds
// another dax. One another session makes meanwhile is taken as it is.
func (c *Client) upSidecar(ctx context.Context, spec Spec, bin, ip string) error {
	state, sbin, err := c.sidecar(ctx, spec.Name)
	if err != nil {
		return err
	}
	switch {
	case state == "running" && sbin == bin:
		return nil
	case state != "" && sbin == bin:
		if _, err := c.Runner.Docker(ctx, "start", ProxyName(spec.Name)); err != nil {
			return fmt.Errorf("box %s: sidecar: %w", spec.Name, err)
		}
		return nil
	case state != "":
		if _, err := c.Runner.Docker(ctx, "rm", "-f", ProxyName(spec.Name)); err != nil {
			return fmt.Errorf("box %s: sidecar: %w", spec.Name, err)
		}
	}
	if err := c.egressNetwork(ctx); err != nil {
		return err
	}
	dir, err := c.stateDir(spec.Name)
	if err != nil {
		return err
	}
	if strings.Contains(dir, ":") {
		return fmt.Errorf("box: %s: a path with a colon cannot be mounted", dir)
	}
	if _, err := c.Runner.Docker(ctx, SidecarArgs(spec, bin, ip, dir)...); err != nil && !strings.Contains(err.Error(), "is already in use") {
		return fmt.Errorf("box %s: sidecar: %w", spec.Name, err)
	}
	return nil
}

// check makes sure the box stayed up, its dax runs there and its proxy
// answers it.
func (c *Client) check(ctx context.Context, spec Spec, ip string) error {
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
	return c.checkProxy(ctx, spec, ip)
}

// checkProxy asks, from inside the box, whether its sidecar answers.
func (c *Client) checkProxy(ctx context.Context, spec Spec, ip string) error {
	if _, err := c.Runner.Docker(ctx, "exec", spec.Name, Binary, "box", "proxy", "-check", ip+":"+ProxyPort); err != nil {
		logs, _ := c.Runner.Docker(ctx, "logs", "--tail", "20", ProxyName(spec.Name))
		return fmt.Errorf("box %s: its sidecar %s does not answer (%w): %s", spec.Name, ProxyName(spec.Name), err, strings.TrimSpace(string(logs)))
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

func (c *Client) notice(msg string) {
	if c.Notice != nil {
		c.Notice(msg)
	}
}

// specLabel is a hash of what the box was made from, but its dax and
// its rules: a box made from other settings is another box. "egress"
// marks a box made behind a sidecar, so one made before is refused.
func specLabel(s Spec) string {
	h := sha256.New()
	for _, v := range []string{s.Dir, s.Image, s.Runtime, s.Memory, strconv.FormatFloat(s.CPUs, 'f', -1, 64), strconv.FormatInt(s.Pids, 10), strconv.Itoa(s.UID), strconv.Itoa(s.GID), "egress"} {
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
