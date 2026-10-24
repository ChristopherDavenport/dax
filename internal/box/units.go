package box

// This file is a box's MCP units: each a container of its own beside
// the box, made from the unit's envelope, on its own internal network
// behind its own sidecar.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnsureUnit gives the container of an MCP unit, spec, running with its
// sidecar and under its rules: the one there, started again if it
// stopped, or a new one, its image built first when it is built. A
// container made from another declaration is refused, since making it
// again would end every other session's server in it. It returns the
// container's name.
func (c *Client) EnsureUnit(ctx context.Context, spec Spec) (string, error) {
	if spec.Of == "" || spec.Unit == "" || spec.Name != UnitName(spec.Of, spec.Unit) {
		return "", fmt.Errorf("unit %q of box %q: a unit's container is named for its box and unit", spec.Unit, spec.Of)
	}
	for _, p := range []string{spec.Dir, spec.Binary} {
		if strings.Contains(p, ":") {
			return "", fmt.Errorf("box: %s: a path with a colon cannot be mounted", p)
		}
	}
	if spec.Files.Project != "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("box: %w", err)
		}
		if h, err := filepath.EvalSymlinks(home); err == nil {
			home = h
		}
		if err := CheckMounts(spec, home); err != nil {
			return "", err
		}
	}
	bin, err := binaryLabel(spec.Binary)
	if err != nil {
		return "", err
	}
	if err := c.prepare(ctx, &spec); err != nil {
		return "", err
	}
	return spec.Name, c.ensureUnit(ctx, spec, bin, true)
}

func (c *Client) ensureUnit(ctx context.Context, spec Spec, bin string, again bool) error {
	have, found, err := c.findUnit(ctx, spec.Name)
	if err != nil {
		return err
	}
	if found && have.spec != unitSpecLabel(spec) {
		return fmt.Errorf("unit %s of box %s was made with other settings (its declaration, image or user) than these; remove the box with `dax box down %s` and start again", spec.Unit, spec.Of, spec.Of)
	}
	if err := c.setRules(spec); err != nil {
		return err
	}
	switch {
	case !found:
		err := c.createUnit(ctx, spec, bin)
		if again && errors.Is(err, errInUse) {
			return c.ensureUnit(ctx, spec, bin, false)
		}
		return err
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
		return c.checkUnit(ctx, spec, ip)
	}
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
	return c.checkSidecar(ctx, spec, ip)
}

func (c *Client) createUnit(ctx context.Context, spec Spec, bin string) error {
	if c.Starting != nil {
		c.Starting(fmt.Sprintf("starting unit %s of box %s (%s under %s, files %s)", spec.Unit, spec.Of, spec.Image, spec.Runtime, spec.Files))
	}
	// Resolved first, so a secret that cannot be leaves nothing made.
	env, err := c.resolveSecrets(ctx, spec)
	if err != nil {
		return err
	}
	ip, err := c.boxNetwork(ctx, spec.Name)
	if err != nil {
		return err
	}
	if err := c.upSidecar(ctx, spec, bin, ip); err != nil {
		return err
	}
	data, err := c.dataDir(spec.Name)
	if err != nil {
		return err
	}
	args := UnitRunArgs(spec, ip, data)
	if len(env) > 0 {
		_, err = c.call(ctx, Call{Args: args, Env: env})
	} else {
		_, err = c.Runner.Docker(ctx, args...)
	}
	if err != nil {
		if isUnknownRuntime(err) {
			return fmt.Errorf("unit %s: %w: %s; register it with docker (docs/sandbox.md, \"Runtimes\") or name another in the box host's config", spec.Unit, ErrRuntime, spec.Runtime)
		}
		if strings.Contains(err.Error(), "is already in use") {
			return fmt.Errorf("unit %s: %w", spec.Unit, errInUse)
		}
		return fmt.Errorf("unit %s: %w", spec.Unit, err)
	}
	return c.checkUnit(ctx, spec, ip)
}

// checkUnit makes sure a unit's container stayed up and its sidecar
// answers.
func (c *Client) checkUnit(ctx context.Context, spec Spec, ip string) error {
	have, found, err := c.findUnit(ctx, spec.Name)
	if err != nil {
		return err
	}
	if !found || have.state != "running" {
		logs, _ := c.Runner.Docker(ctx, "logs", "--tail", "20", spec.Name)
		return fmt.Errorf("unit %s did not stay up (its image needs sh and sleep): %s", spec.Unit, strings.TrimSpace(string(logs)))
	}
	return c.checkSidecar(ctx, spec, ip)
}

// checkSidecar asks, from the sidecar itself (a unit's image need not
// run dax), whether its proxy answers on the unit's network.
func (c *Client) checkSidecar(ctx context.Context, spec Spec, ip string) error {
	if _, err := c.Runner.Docker(ctx, "exec", ProxyName(spec.Name), Binary, "box", "proxy", "-check", ip+":"+ProxyPort); err != nil {
		logs, _ := c.Runner.Docker(ctx, "logs", "--tail", "20", ProxyName(spec.Name))
		return fmt.Errorf("unit %s: its sidecar %s does not answer (%w): %s", spec.Unit, ProxyName(spec.Name), err, strings.TrimSpace(string(logs)))
	}
	return nil
}

// findUnit is the state and spec label of the MCP unit's container
// name; false when there is none.
func (c *Client) findUnit(ctx context.Context, name string) (found, bool, error) {
	out, err := c.Runner.Docker(ctx, "ps", "-a", "--filter", "label="+labelOf, "--filter", "name=^"+name+"$", "--format",
		`{{.State}}\t{{.Label "`+labelSpec+`"}}`)
	if err != nil {
		return found{}, false, err
	}
	ls := lines(out)
	if len(ls) == 0 {
		return found{}, false, nil
	}
	state, spec, _ := strings.Cut(ls[0], "\t")
	return found{state: state, spec: spec}, true, nil
}

// resolveSecrets is spec's secrets as NAME=value entries for docker's
// own environment, each reference resolved on this machine: env: from
// the box host's environment, command: run with sh, its output
// trimmed, file: read and trimmed of its last newline. An error names
// the secret and its reference, never a value.
func (c *Client) resolveSecrets(ctx context.Context, spec Spec) ([]string, error) {
	var env []string
	for _, name := range sortedNames(spec.Secrets) {
		ref := spec.Secrets[name]
		v, err := c.resolveSecret(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("unit %s: secret %s: %s: %w", spec.Unit, name, ref, err)
		}
		env = append(env, name+"="+v)
	}
	return env, nil
}

func (c *Client) resolveSecret(ctx context.Context, ref string) (string, error) {
	kind, rest, _ := strings.Cut(ref, ":")
	switch kind {
	case "env":
		lookup := c.LookupEnv
		if lookup == nil {
			lookup = os.LookupEnv
		}
		v, ok := lookup(rest)
		if !ok || v == "" {
			return "", errors.New("not set where the box host runs; for a local box host name it in executor.pass_env")
		}
		return v, nil
	case "command":
		cmd := exec.CommandContext(ctx, "sh", "-c", rest)
		out, err := cmd.Output()
		if err != nil {
			// Its standard error may hold what it was resolving: only the
			// status is said.
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return "", fmt.Errorf("the command failed (exit %d)", ee.ExitCode())
			}
			return "", errors.New("the command could not run")
		}
		v := strings.TrimSpace(string(out))
		if v == "" {
			return "", errors.New("the command gave nothing")
		}
		return v, nil
	case "file":
		b, err := os.ReadFile(rest)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", errors.New("no such file")
			}
			return "", errors.New("the file cannot be read")
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			return "", errors.New("the file is empty")
		}
		return v, nil
	}
	return "", errors.New("not a reference")
}

// listUnits is every MCP unit's container, running or not.
func (c *Client) listUnits(ctx context.Context) ([]Info, error) {
	out, err := c.Runner.Docker(ctx, "ps", "-a", "--filter", "label="+labelOf, "--format",
		`{{.Names}}\t{{.State}}\t{{.Label "`+labelOf+`"}}\t{{.Label "`+labelUnit+`"}}\t{{.Label "`+labelRuntime+`"}}\t{{.Label "`+labelFiles+`"}}\t{{.Label "`+labelDir+`"}}\t{{.Image}}`)
	if err != nil {
		return nil, err
	}
	var units []Info
	for _, line := range lines(out) {
		f := strings.Split(line, "\t")
		for len(f) < 8 {
			f = append(f, "")
		}
		info := Info{Name: f[0], State: f[1], Of: f[2], Unit: f[3], Runtime: f[4], Files: f[5], Dir: f[6], Image: f[7], Internet: "?"}
		if dir, err := c.stateDir(info.Name); err == nil {
			if r, err := ReadRules(filepath.Join(dir, RulesFile)); err == nil {
				info.Internet = r.String()
			}
		}
		units = append(units, info)
	}
	return units, nil
}

// removeContainer removes container name, its sidecar, its network and
// its rules; it reports whether anything of it was there.
func (c *Client) removeContainer(ctx context.Context, name string, there bool) (bool, error) {
	if there {
		if _, err := c.Runner.Docker(ctx, "rm", "-f", name); err != nil {
			return true, err
		}
	}
	left := there
	if state, _, err := c.sidecar(ctx, name); err == nil && state != "" {
		left = true
		if _, err := c.Runner.Docker(ctx, "rm", "-f", ProxyName(name)); err != nil {
			return true, err
		}
	}
	if out, err := c.Runner.Docker(ctx, "network", "ls", "--filter", "label="+labelOwner+"="+name, "--format", "{{.Name}}"); err == nil && len(lines(out)) > 0 {
		left = true
		if _, err := c.Runner.Docker(ctx, "network", "rm", NetworkName(name)); err != nil {
			return true, err
		}
	}
	if dir, err := c.stateDir(name); err == nil {
		os.RemoveAll(dir)
	}
	return left, nil
}
