package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"slices"
	"strings"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/internal/render"
	"github.com/ChristopherDavenport/dax/tool"
)

// Executor is `dax execute` running elsewhere, connected: the
// extensions' tools served from a container or another host, where
// their calls run, their facts are read and their stamps are made and
// checked. A session given one (Options.Executor) builds no tool of an
// extension in this process; it decides each call under its own policy
// and sends it there. It is exported so that a program built on dax
// can start its session against a sandbox as dax's command line does
// with -executor. The caller closes it, after the session.
type Executor struct {
	r    *executor.Remote
	view *executorView
}

// ExecutorOptions say how to start an executor and what to call the
// session to it.
type ExecutorOptions struct {
	// Name and Version are the program's, which the executor is told.
	Name, Version string
	// PassEnv and KeyEnv are Options' own: the command that starts the
	// executor (a docker exec, an ssh) gets this machine's environment
	// with credentials removed, KeyEnv's variable among them unless
	// PassEnv names it, so the model's key never reaches the sandbox.
	PassEnv []string
	KeyEnv  string
}

// DialExecutor starts command, a command line split on spaces that
// runs `dax execute` where the tools are to act (`docker exec -i box
// dax execute -root /work`), and connects to it over its standard input
// and output. The pipe is the credential: there is no listener and no
// token. Its standard error is dax's, cleaned. A program that is not a
// dax executor (one whose MCP server lacks the facts method or dax's
// capability, or lists tools the capability does not name) is refused
// and stopped.
func DialExecutor(ctx context.Context, command string, o ExecutorOptions) (*Executor, error) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, errors.New("executor: empty command")
	}
	cmd := exec.Command(fields[0], fields[1:]...)
	cmd.Env = tool.DefaultEnv(o.PassEnv, o.KeyEnv)
	// Its diagnostics are text from a program in the sandbox; they get
	// the same cleaning as a tool's output.
	cmd.Stderr = render.CleanWriter(stderr)
	return connectExecutor(ctx, &mcp.CommandTransport{Command: cmd}, o.Name, o.Version)
}

// connectExecutor connects to an executor over t.
func connectExecutor(ctx context.Context, t mcp.Transport, name, version string) (*Executor, error) {
	r, err := executor.Connect(ctx, t, executor.ConnectOptions{Name: name, Version: version})
	if err != nil {
		return nil, err
	}
	return &Executor{r: r, view: &executorView{d: r.Descriptor()}}, nil
}

// Server is the name and version the executor gave, its program's.
func (e *Executor) Server() (name, version string) { return e.r.Server() }

// Workspace is the executor's workspace as this machine sees it: its
// root and descriptor, the executor's word for where its tools act.
// It cannot write, remove or run anything, and is not a
// workspace.Starter; the tools do that in the executor. Its file system
// is empty for now, so the project's files (AGENTS.md, .dax/skills,
// .dax/config.json) are not read through it.
func (e *Executor) Workspace() workspace.Workspace { return e.view }

// Close ends the calls in flight and the connection, which stops the
// executor's process.
func (e *Executor) Close() error { return e.r.Close() }

// String names the executor and where it acts, for a banner.
func (e *Executor) String() string {
	name, version := e.Server()
	d := e.r.Descriptor()
	where := d.Kind
	if d.Ref != "" {
		where += " " + d.Ref
	}
	return fmt.Sprintf("%s %s · %s · %s", name, version, where, d.Root)
}

// errViewOnly is what the executor's view says to anything that would
// act through it.
var errViewOnly = fmt.Errorf("the tools act in the executor, not through this view: %w", errors.ErrUnsupported)

// executorView is an executor's workspace as the session holds it:
// the root and descriptor it records and tells the model, and nothing
// to act with.
type executorView struct{ d workspace.Descriptor }

var _ workspace.Workspace = (*executorView)(nil)

func (v *executorView) Root() string                     { return v.d.Root }
func (v *executorView) FS() fs.FS                        { return emptyFS{} }
func (v *executorView) Env() []string                    { return nil }
func (v *executorView) Descriptor() workspace.Descriptor { return v.d }
func (v *executorView) Close() error                     { return nil }

func (v *executorView) WriteFile(context.Context, string, []byte, fs.FileMode) error {
	return errViewOnly
}

func (v *executorView) Remove(context.Context, string) error { return errViewOnly }

func (v *executorView) Exec(context.Context, workspace.Command) (*workspace.Output, error) {
	return nil, errViewOnly
}

// emptyFS is a file system with an empty root directory.
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	switch {
	case !fs.ValidPath(name):
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	case name == ".":
		return emptyDir{}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// emptyDir is emptyFS's root.
type emptyDir struct{}

func (emptyDir) Stat() (fs.FileInfo, error) { return emptyDir{}, nil }
func (emptyDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: ".", Err: errors.New("is a directory")}
}
func (emptyDir) Close() error { return nil }
func (emptyDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n > 0 {
		return nil, io.EOF
	}
	return nil, nil
}

// emptyDir is its own fs.FileInfo.
func (emptyDir) Name() string       { return "." }
func (emptyDir) Size() int64        { return 0 }
func (emptyDir) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (emptyDir) ModTime() time.Time { return time.Time{} }
func (emptyDir) IsDir() bool        { return true }
func (emptyDir) Sys() any           { return nil }

// keepOpen is an executor the session runs its tools through and does
// not close: the caller's Executor.
type keepOpen struct{ executor.Executor }

func (keepOpen) Close() error { return nil }

// runs refuses an extension with Tools that the executor does not run:
// its tools would be offered nowhere, and a session of a program built
// with it would run without them unsaid.
func (e *Executor) runs(exts []extension.Extension) error {
	served := e.r.Extensions()
	for _, x := range exts {
		if x.Tools != nil && !slices.Contains(served, x.Name) {
			name, version := e.Server()
			return fmt.Errorf("the executor (%s %s) does not run %s", name, version, x.Name)
		}
	}
	return nil
}
