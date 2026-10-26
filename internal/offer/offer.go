// Package offer is how dax starts an MCP server that holds secrets, and
// the servers dax workspace -servers offers: a command in a workspace
// offered through agentworkspace's Offer, given the variables its
// secret_env names from the environment of the process that starts it,
// with what it writes, standard output and standard error, read through
// agentredact's secrets filter (Filter) before any of it is passed on.
// A session's own secret-holding server and a workspace's offered one
// are started by this one path, so a server's secrets mean the same
// wherever it runs. A secret stays on the machine whose environment
// holds it: the process that reads it here starts the server here.
package offer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/ChristopherDavenport/agentredact/secrets"
	workspace "github.com/ChristopherDavenport/agentworkspace"

	"github.com/ChristopherDavenport/dax/internal/cmdline"
	"github.com/ChristopherDavenport/dax/internal/render"
)

// MinSecretBytes is the shortest secret value taken: a shorter one
// would be found, and replaced, inside the server's own messages,
// breaking the conversation rather than keeping anything out of it.
const MinSecretBytes = 8

// Server is one server to offer: its name, as
// workspace.ValidServerName holds it, what it is for, its command line,
// split as a shell splits words with nothing expanded, and the
// variables it is given as secrets.
type Server struct {
	Name, Description, Command string
	SecretEnv                  []string
}

// Filter is the OutputFilter every secret-holding server's output is
// read through: each value replaced, in every spelling a JSON encoder
// writes it in, by "[REDACTED NAME]", a line at a time, as the box
// host redacts a unit's. It is a backstop, not a guarantee: a value
// the server encodes otherwise passes through.
func Filter(_ string, values map[string]string, r io.Reader) io.Reader {
	return secrets.New(values).Reader(r, nil)
}

// Offering is workspace.Offering whose secret-holding servers' standard
// error is, past the Filter, cleaned of what would act on a terminal
// (render.Clean) and then redacted again, a line at a time: control
// characters a server puts inside a value ("SEC\aRET") cannot carry it
// past the filter to be joined up by the cleaning a reader of standard
// error applies.
type Offering struct {
	*workspace.Offering
	values map[string]map[string]string // each secret-holding server's, by name
}

var _ workspace.ServerProvider = (*Offering)(nil)

// Offer is ws offering servers, each run in ws at its root with ws's
// environment and the variables its SecretEnv names, their values
// taken from lookup (the starting process's environment), and its
// output read through Filter. A variable lookup does not set, sets to
// nothing, or sets to fewer than MinSecretBytes, is refused, naming the
// variable and never a value.
func Offer(ws workspace.Starter, servers []Server, lookup func(string) (string, bool)) (*Offering, error) {
	offered := make([]workspace.OfferedServer, 0, len(servers))
	values := map[string]map[string]string{}
	for _, s := range servers {
		args, err := cmdline.Split(s.Command)
		if err != nil {
			return nil, fmt.Errorf("server %s: command: %w", s.Name, err)
		}
		if len(args) == 0 {
			return nil, fmt.Errorf("server %s: an empty command", s.Name)
		}
		var env []string
		for _, v := range s.SecretEnv {
			value, ok := lookup(v)
			switch {
			case !ok || value == "":
				return nil, fmt.Errorf("server %s: secret_env %s is not set", s.Name, v)
			case len(value) < MinSecretBytes:
				return nil, fmt.Errorf("server %s: secret_env %s is shorter than %d bytes, too short to redact without breaking the server's own messages", s.Name, v, MinSecretBytes)
			}
			env = append(env, v+"="+value)
			if values[s.Name] == nil {
				values[s.Name] = map[string]string{}
			}
			values[s.Name][v] = value
		}
		offered = append(offered, workspace.OfferedServer{
			Server:  workspace.Server{Name: s.Name, Description: s.Description},
			Command: workspace.Command{Args: args, Env: env},
			Secrets: slices.Clone(s.SecretEnv),
		})
	}
	o, err := workspace.Offer(ws, offered, workspace.WithOutputFilter(Filter))
	if err != nil {
		return nil, err
	}
	return &Offering{Offering: o, values: values}, nil
}

// StartServer starts the server called name, as workspace.Offering
// does; a secret-holding one's standard error reaches stderr cleaned,
// then redacted, a line at a time, the rest of a line it ends without a
// newline once it has exited.
func (o *Offering) StartServer(ctx context.Context, name string, stderr io.Writer) (workspace.Process, error) {
	values := o.values[name]
	if len(values) == 0 || stderr == nil {
		return o.Offering.StartServer(ctx, name, stderr)
	}
	w := &cleanRedact{r: secrets.New(values), out: stderr}
	p, err := o.Offering.StartServer(ctx, name, w)
	if err != nil {
		return nil, err
	}
	return &flushed{Process: p, w: w}, nil
}

// cleanRedact is a writer that cleans each line it is given, then
// redacts it, and writes it on.
type cleanRedact struct {
	r   *secrets.Redactor
	out io.Writer

	mu  sync.Mutex
	buf []byte
}

// maxLine is the most of a line cleanRedact holds before it passes it
// on unended.
const maxLine = 64 << 10

func (c *cleanRedact) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	for {
		i := bytes.IndexByte(c.buf, '\n')
		if i < 0 {
			break
		}
		line := string(c.buf[:i])
		c.buf = c.buf[i+1:]
		if _, err := io.WriteString(c.out, c.r.String(render.Clean(line))+"\n"); err != nil {
			return len(p), err
		}
	}
	if len(c.buf) > maxLine {
		return len(p), c.flushLocked()
	}
	return len(p), nil
}

// flush passes on what is held of a line.
func (c *cleanRedact) flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flushLocked()
}

func (c *cleanRedact) flushLocked() error {
	if len(c.buf) == 0 {
		return nil
	}
	line := string(c.buf)
	c.buf = nil
	_, err := io.WriteString(c.out, c.r.String(render.Clean(line)))
	return err
}

// flushed is a started server whose cleanRedact is flushed once it has
// ended: Offering's process has written all of its standard error by
// the time Wait or Close returns.
type flushed struct {
	workspace.Process
	w *cleanRedact
}

func (p *flushed) Wait() (int, error) {
	status, err := p.Process.Wait()
	p.w.flush()
	return status, err
}

func (p *flushed) Close() error {
	err := p.Process.Close()
	p.w.flush()
	return err
}
