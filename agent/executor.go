package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"

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
	// with credentials removed, except those PassEnv names, and never
	// KeyEnv's variable, whatever names it, so the model's key never
	// reaches the sandbox.
	PassEnv []string
	KeyEnv  string
	// LauncherEnv names more variables the command that starts the
	// executor is given, credentials included: ssh's SSH_AUTH_SOCK, the
	// token a kubectl credential plugin reads. They are that command's
	// alone; the session's tools and MCP servers run in the executor,
	// with its environment, and never see them. Naming KeyEnv is an
	// error.
	LauncherEnv []string
}

// DialExecutor starts the program argv[0] with the arguments argv[1:],
// used exactly and with no shell, which runs `dax execute` where the
// tools are to act ({"docker", "exec", "-i", "box", "dax", "execute",
// "-root", "/work"}), and connects to it over its standard input and
// output. The pipe is the credential: there is no listener and no
// token. What it writes to standard error is cleaned and goes where
// dax warns at the time (CaptureWarnings). A program that is not a dax
// executor (one whose MCP server lacks the facts method or dax's
// capability, or lists tools the capability does not name) is refused
// and stopped.
func DialExecutor(ctx context.Context, argv []string, o ExecutorOptions) (*Executor, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, errors.New("executor: empty command")
	}
	env, err := launcherEnv(os.Environ(), o)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	// Its diagnostics are text from a program in the sandbox; they get
	// the same cleaning as a tool's output, and are held with dax's
	// warnings while a front has the screen.
	cmd.Stderr = render.CleanWriter(warnings{})
	return connectExecutor(ctx, &mcp.CommandTransport{Command: cmd}, o.Name, o.Version)
}

// launcherEnv is base, this machine's environment, as the command that
// starts the executor gets it: without credentials but those PassEnv
// and LauncherEnv name, and without KeyEnv's variable even if PassEnv
// names it.
func launcherEnv(base []string, o ExecutorOptions) ([]string, error) {
	if o.KeyEnv != "" && slices.Contains(o.LauncherEnv, o.KeyEnv) {
		return nil, fmt.Errorf("executor: %s holds the model's key, which never reaches the executor; take it out of executor.pass_env", o.KeyEnv)
	}
	env := tool.ChildEnv(base, slices.Concat(o.PassEnv, o.LauncherEnv), o.KeyEnv)
	if o.KeyEnv != "" {
		env = slices.DeleteFunc(env, func(kv string) bool {
			name, _, _ := strings.Cut(kv, "=")
			return name == o.KeyEnv
		})
	}
	return env, nil
}

// connectExecutor connects to an executor over t.
func connectExecutor(ctx context.Context, t mcp.Transport, name, version string) (*Executor, error) {
	r, err := executor.Connect(ctx, t, executor.ConnectOptions{Name: name, Version: version})
	if err != nil {
		return nil, err
	}
	return &Executor{r: r, view: &executorView{d: r.Descriptor(), fsys: r.FS(), r: r}}, nil
}

// Server is the name and version the executor gave, its program's.
func (e *Executor) Server() (name, version string) { return e.r.Server() }

// Workspace is the executor's workspace as this machine sees it: its
// root and descriptor, the executor's word for where its tools act,
// and its files, read-only, through the executor, which serves them
// confined to its workspace as its tools' reads are. The session reads
// the project's AGENTS.md and .dax/skills through it, and a program
// reads the project's .dax/config.json through it as dax's command
// line does. It cannot write, remove or run a command; the tools do
// that in the executor. It is a workspace.Starter whose Start starts
// the process in the executor (`dax execute`'s process methods), so
// the session's MCP servers run there, where the tools act; an
// executor that cannot start one (an older dax execute) refuses, and
// nothing starts on this machine in its place.
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
// the root and descriptor it records and tells the model, the files it
// reads through the executor, and processes it starts there; nothing
// else to act with.
type executorView struct {
	d    workspace.Descriptor
	fsys fs.FS
	r    *executor.Remote
}

var _ workspace.Starter = (*executorView)(nil)

func (v *executorView) Root() string                     { return v.d.Root }
func (v *executorView) FS() fs.FS                        { return v.fsys }
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

// Start starts cmd in the executor, at its root, with its environment
// and cmd's: an MCP server the user configured or added. An executor
// without the process methods refuses it, as unsupported, with what to
// do; it is never started here instead.
func (v *executorView) Start(ctx context.Context, cmd workspace.Command) (workspace.Process, error) {
	return v.r.Start(ctx, cmd)
}

// keepOpen is an executor the session runs its tools through and does
// not close: the caller's Executor, which reads a model response's
// facts in one request (executor.Batcher).
type keepOpen struct{ *executor.Remote }

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
