// Package offer is how dax starts an MCP server that holds secrets, and
// the servers dax workspace -servers offers: a command in a workspace
// offered through agentworkspace's Offer, given the variables its
// secret_env names from the environment of the process that starts it,
// with what it writes, standard output and standard error, read through
// agentredact's secrets filter (Filter) before any of it is passed on.
// A session's own secret-holding server and a workspace's offered one
// are started by this one path, so a server's secrets mean the same
// wherever it runs.
package offer

import (
	"fmt"
	"io"
	"slices"

	"github.com/ChristopherDavenport/agentredact/secrets"
	workspace "github.com/ChristopherDavenport/agentworkspace"

	"github.com/ChristopherDavenport/dax/internal/cmdline"
)

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

// Offer is ws offering servers, each run in ws at its root with ws's
// environment and the variables its SecretEnv names, their values
// taken from lookup (the starting process's environment), and its
// output read through Filter. A variable lookup does not set, or sets
// to nothing, is refused, naming the variable and never a value.
func Offer(ws workspace.Starter, servers []Server, lookup func(string) (string, bool)) (*workspace.Offering, error) {
	offered := make([]workspace.OfferedServer, 0, len(servers))
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
			if !ok || value == "" {
				return nil, fmt.Errorf("server %s: secret_env %s is not set", s.Name, v)
			}
			env = append(env, v+"="+value)
		}
		offered = append(offered, workspace.OfferedServer{
			Server:  workspace.Server{Name: s.Name, Description: s.Description},
			Command: workspace.Command{Args: args, Env: env},
			Secrets: slices.Clone(s.SecretEnv),
		})
	}
	return workspace.Offer(ws, offered, workspace.WithOutputFilter(Filter))
}
