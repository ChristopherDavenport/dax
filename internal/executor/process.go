package executor

// The executor's processes: a process that lives longer than one call,
// with pipes to it, started in the executor's workspace for the session
// at the other end of its pipe, so that an MCP stdio server the user
// configured runs where the tools act and not on the session's machine.
//
// The methods and their semantics are agentworkspace's mcpworkspace
// (the agentworkspace/process.* methods, package wire): custom JSON-RPC
// methods, not tools, since a tool would be offered to the model of any
// harness connected to `dax execute`, and this is arbitrary execution.
// The executor answers them for its workspace on each session's server
// (mcpworkspace.Processes, serve.go); the session (Remote.Start) is the
// only caller, and what it starts is what the user's configuration,
// flags and /mcp add name; the executor holds no list of its own and
// reads no config. What is dax's here is the connection: the capability
// that says the executor starts processes, and the redial, under which
// a process lives on the connection it was started over alone.

import (
	"context"
	"errors"
	"fmt"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace"
)

// Start starts cmd in the executor's workspace and returns at once:
// workspace.Starter's Start, over the connection (mcpworkspace.Start).
// The process's standard input and output are requests to the
// executor, a write at a time and a read at a time; its standard error,
// when cmd.Stream is set, is read in the background and written to
// cmd.Stream as it arrives. It ends with Close, the end of ctx,
// cmd.Timeout, the connection's close or the executor's. It is started
// over the current connection, a new one if that was lost, and lives on
// that one alone: when it is lost, the executor ends the process with
// its session, and the process's reads, writes and wait fail with
// errProcessLost. It is not started again.
//
// An executor that does not answer the process methods (no start in
// its capability: an older dax execute, or a workspace there that
// cannot start a process) is refused with an error that is
// errors.ErrUnsupported; nothing is started anywhere else.
func (r *Remote) Start(ctx context.Context, cmd workspace.Command) (workspace.Process, error) {
	switch {
	case len(cmd.Args) == 0:
		return nil, errors.New("executor: no command")
	case cmd.Stdin != nil:
		return nil, errors.New("executor: Start takes no Command.Stdin; write to Process.Stdin")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cn, err := r.conn(ctx)
	if err != nil {
		return nil, err
	}
	if cn.start == nil {
		return nil, fmt.Errorf("executor (%s %s) cannot start a process (no start in %s); update dax execute where it runs: %w", cn.server.Name, cn.server.Version, CapabilityKey, errors.ErrUnsupported)
	}
	p, err := mcpworkspace.Start(ctx, mcpworkspace.Conn{
		Session: cn.c.Session(),
		Bounds:  *cn.start,
		Done:    cn.procs,
		Lost:    errProcessLost,
		Err:     cn.lostErr,
	}, cmd)
	if err != nil {
		// The start's own error, as the start request gave it: not the
		// loss mapping (lostErr), since a process that never started
		// did not end with the connection, and a timeout here is no
		// reason to probe it.
		return nil, fmt.Errorf("executor: %w", err)
	}
	return p, nil
}
