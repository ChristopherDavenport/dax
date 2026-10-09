// Package workspace is where a session's tools act: a file tree,
// processes, a working directory and an environment. A session runs
// over one Workspace, whichever machine it is on, and every tool that
// touches files or runs a command goes through it, so a tool written
// for one runs unchanged on another. Local is the one in this module,
// the working directory on this machine; a container or a remote
// runtime is another implementation of the same interface.
//
// The interface follows the design the agentworkspace study settled on
// (examples/openhands-workspace), so that when that sibling module
// exists dax moves to it by a rename: a small value with a file system,
// one-shot execution and a descriptor the session records. It adds a
// stream to Command, which dax's bash needs to report output as it
// arrives, and leaves the persistent shell out until a tool needs one.
//
// The package is pre-1.0: its API may change in a minor version.
package workspace

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"
)

// ErrOutside is what a name that leaves the workspace gets, whether it
// names another directory outright, climbs out with "..", or goes out
// through a symbolic link.
var ErrOutside = errors.New("path is outside the workspace")

// Kinds a Descriptor names, as agentsession's env entry records them.
const (
	KindLocal     = "local"
	KindContainer = "container"
	KindRemote    = "remote"
)

// Workspace is where a session's tools act. Names given to it are
// fs.ValidPath names relative to Root; turning a path the model wrote
// into one is the tool's (see tool.Files).
type Workspace interface {
	// Root is the absolute path of the working directory as the
	// workspace's own processes see it: the checkout on this machine,
	// /workspace in a container. It is what the model is told and what
	// the session records as its cwd.
	Root() string
	// FS reads the workspace's files. Opening a name never blocks: a
	// FIFO or a device opens, or fails, at once, and a tool checks what
	// it opened is a regular file. A name that leaves the workspace is
	// an error that errors.Is ErrOutside. An FS that can tell a link
	// from what it leads to implements fs.ReadLinkFS; one that cannot
	// leaves the policy's checks that follow links unable to, and so
	// asking.
	FS() fs.FS
	// WriteFile creates or replaces a regular file, creating the
	// directories above it. It refuses a FIFO or a device where the file
	// is, and does not block on one.
	WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error
	// Remove deletes a file or an empty directory.
	Remove(ctx context.Context, name string) error
	// Env is the environment every process in the workspace starts
	// with, the provider's key and other credentials already removed.
	Env() []string
	// Exec runs one command to completion in a fresh process.
	Exec(ctx context.Context, cmd Command) (*Output, error)
	// Descriptor is what the session records about where the tools ran.
	Descriptor() Descriptor
	// Close releases the workspace. It is used until Close and not after.
	Close() error
}

// Command is one process to run.
type Command struct {
	// Args is argv; Args[0] is resolved in the workspace.
	Args []string
	// Dir is relative to Root; "" is Root.
	Dir string
	// Env is added to Workspace.Env.
	Env []string
	// Stdin is the process's standard input; nil is none.
	Stdin []byte
	// Timeout ends the process, and everything it started, after this
	// long; zero is no limit but the context's.
	Timeout time.Duration
	// Stream, when set, receives standard output and standard error,
	// interleaved as they arrive, and Output holds neither.
	Stream io.Writer
}

// Output is how a process ended.
type Output struct {
	Stdout, Stderr []byte
	// ExitCode is the process's status; -1 when it was killed.
	ExitCode int
	// TimedOut reports that Command.Timeout ended it.
	TimedOut bool
}

// Descriptor is what the session records about a workspace, in its env
// entry: agentsession's workspace kind and ref, and the root, which is
// the entry's cwd.
type Descriptor struct {
	// Kind is KindLocal, KindContainer or KindRemote.
	Kind string
	// Ref is what the harness resolves to it: an image digest, a host,
	// an instance ID; empty for the local machine.
	Ref string
	// Root is Workspace.Root.
	Root string
}
