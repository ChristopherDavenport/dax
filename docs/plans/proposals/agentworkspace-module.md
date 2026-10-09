# agentworkspace: create the module, with Local, a remote client and server, and a long-lived process

Note: the remote workspace here is the primitive cut. For a sandbox that runs the tools, execution-boundary.md cuts at tool calls instead; this module is then what the executor runs over inside the sandbox.

Repository: new sibling, `github.com/ChristopherDavenport/agentworkspace`.
Draft for filing; it creates a repository, so it is the user's call.

## The problem

The tools a coding agent runs (read, write, edit, bash) act on the local
disk through `os` and `os/exec`. The openhands-workspace study designed
the seam that lets the same tools run unchanged against a local
directory, a container and a remote runtime, and placed it in a new
sibling module (`examples/openhands-workspace/design.md:77-108`): not in
agenttool, whose contract imports only openresponses and the standard
library, and not in a product, because every product and agenteval want
the same tools against the same backends. Eight rounds later the module
still does not exist (`examples/round8/openhands-workspace/design.md`,
"Build order": Container, Remote and the shell's `Send`/`Incomplete` not
built), and only the session's `workspace` member of the env entry has
shipped (`agentsession/lifecycle.go:85-89`, `:389-396`).

dax now runs every tool and every policy check through such an interface,
as its own package (`dax/workspace`), so that it can move to the sibling
by a rename. Without the sibling, dax has only `Local`: a container or a
remote machine cannot be a dax workspace, and dax's MCP stdio servers run
where the agent runs, not in the workspace.

## The proposal

Create the module from the study's design and dax's package as the
working draft (dax `workspace/workspace.go`, `workspace/local.go`):

```go
type Workspace interface {
	Root() string                  // the working directory as the workspace's processes see it
	FS() fs.FS                     // reads; Open never blocks; outside is ErrOutside
	WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error
	Remove(ctx context.Context, name string) error
	Env() []string                 // a copy; credentials already removed
	Exec(ctx context.Context, c Command) (*Output, error)
	Start(ctx context.Context, c Command) (Process, error) // new: long-lived, with pipes
	Descriptor() Descriptor
	Close() error
}
```

What dax's draft adds to the study's interface (`design.md:110-200`), each
for a reason found in use:

- **`Command.Stream`**: stdout and stderr interleaved as they arrive.
  dax's bash reports progress from the output as it comes; a one-shot
  `Output` only at the end cannot.
- **`FS().Open` never blocks.** A FIFO or a device where a file should be
  hung dax's write tool, and the write lock with it. Local opens with
  `O_NONBLOCK` and refuses what is not a regular file before truncating.
- **`fs.ReadLinkFS` where the backend can tell a link from its target.**
  dax's policy decides a read of `notes.txt` that is a link to `.env` as
  a read of `.env`, and a bash glob through a link out of the workspace as
  outside. Over a workspace those checks must resolve links in the
  workspace, not on the host; a backend that cannot implements plain
  `fs.FS`, and the checks ask instead of allowing.

And from the study, unchanged: `Descriptor{Kind, Ref, Root, …}` maps to
agentsession's env entry (`EnvEntry.SetWorkspace(kind, ref)`, other
members through `Workspace.SetMember`, `agentsession/lifecycle.go:398`),
so a replay knows which file system a path was in.

New:

- **`Start`** returns a `Process` (stdin writer, stdout and stderr
  readers, `Wait`, `Signal`, `Close`) for a process that outlives one
  call. Its first user is an MCP stdio server: mcp's `IOTransport`
  (`modelcontextprotocol/go-sdk mcp/transport.go:149`) over a `Process`'s
  pipes runs the server inside the workspace instead of `CommandTransport`
  on the host. The study's persistent `Shell` (`design.md:150-180`) is a
  `Process` running a shell, with the sentinel protocol on top, and can be
  built on `Start` rather than being its own method. A `Process` lives no
  longer than its owner: it is started in a process group of its own
  (as dax's `Local.Exec` starts a command), `Close` and the context's end
  send `SIGTERM` to the group and `SIGKILL` after a short grace period,
  and the remote handler closes every process a client started when that
  client's connection ends. Containing what the processes may use is
  the sandbox's (cgroups, the container), not the interface's.
- **Backends.** `Local` (`os.Root`, process groups, `WaitDelay`: dax's
  `local.go`), and `remote`: `remote.Handler(ws Workspace) http.Handler`
  serving any Workspace, and `remote.Dial(url) (Workspace, error)`. A
  container is a `remote` workspace whose server runs in the container
  (`Container` embeds `Remote`, as OpenHands' `DockerWorkspace` does,
  `design.md:56-62`); starting the container is a constructor, not the
  interface's concern.
- **Wire.** FS reads and `Stat`/`Lstat`/`ReadLink`/`ReadDir` as GETs, writes
  as PUTs, `Exec` streaming its output as SSE, `Start` as a WebSocket (or a
  pair of streams) for its pipes. Authenticated over TLS with scopes
  (read, write, exec), modelled on agentsession RFC 0003's security section
  (`agentsession/docs/rfcs/0003-agent-session-store-protocol.md:788-816`):
  `exec` is the scope that runs anything, and is never implied by read.

### The tools

The study moves the built-ins into a `tools/` subpackage so products share
one implementation (`design.md:96-102`). dax's `tool` package is now
written against the interface (`tool.Files` over a `Workspace`, bash over
`Exec`, the policy's path and bash checks over `FS` and `Exec`) and is the
candidate. Proposed: not in the first release. The module ships the
interface, Local and remote first; the tools move once a second product
wants them, with dax's tests (the exploit and confinement tables, and the
table that runs on Local and on a container stand-in) moving with them.

## What it must not do

- Follow a link out of the workspace on any backend: a name that leaves is
  `ErrOutside`, however it leaves.
- Block on a FIFO, a device or a slow remote file without honouring the
  context where the operation takes one.
- Leave a started process running after its owner: a `Process` whose
  `Close` was not called is killed when its context ends or its remote
  client disconnects, with everything it started.
- Pass the host's environment by default: `Env` is what the constructor
  was given, and a nil one is empty, not inherited.
- Depend on anything but the standard library in the root package; the
  remote client and server, and a container starter, are subpackages or
  nested modules.

## Tests

- A conformance suite every backend runs: confinement (absolute, `..`,
  links out, a link swapped mid-operation), FIFO and device refusal,
  `Exec` (status, streams, `Dir`, `Env`, `Stdin`, timeout killing the
  process group, cancel), `Start` (pipes, `Wait`, `Signal`), `Env` a copy.
- `remote.Dial(remote.Handler(Local))` over `httptest` passes the same
  suite.
- An MCP server started through `Start` answers `tools/list` through
  `IOTransport`.
- The study's probe (`examples/round8/openhands-workspace/probe`) runs
  against the module instead of its own copy.

## What dax does with it

dax replaces its `workspace` package with an import of this one (a
rename), offers `-workspace <url>` to run its tools in a remote or
container workspace, runs MCP stdio servers through `Start` in the
workspace, and lets `dax serve` serve its workspace with
`remote.Handler`. dax's `tool` package moves here when the `tools/`
question is settled.

## Open questions

- `Remove` and a recursive remove: the study has `Remove`; dax has needed
  no other. Is `RemoveAll` wanted before a tool needs it?
- Should `Descriptor` carry `Image`, `Digest`, `Platform`, `Host`,
  `Instance` as the study's does (`design.md:186-196`), or `Kind`, `Ref`,
  `Root` and a members map, as agentsession's `Workspace` keeps them?
- One `Process` per call, or does a tool own one across calls and close
  it with `WithCloser` (the study's round 4 rule, `round8/.../design.md:56-62`)?
