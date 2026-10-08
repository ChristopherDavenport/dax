# The execution boundary: everything that touches the world can run in a sandbox

Repositories: dax (the executor and its two forms), agenttool (`mcpserver`
and `mcpclient`: a facts call beside the tool calls MCP already carries),
agentpolicy (subjects that take a context and can fail). dax's part comes
first and needs no sibling release to start: its in-process form is the
code dax has today, reorganised.

## The requirement

A user should be able to run everything that acts on files, processes and
the network inside a sandbox, and the model and the rest somewhere else.
That is the one hard requirement. Everything else in a turn is
orchestration: the loop (agentturn), the decisions on what may run
(agentpolicy, the person or the rules answering a hold), the model calls
(`openresponses.Streamer`), and the record (`agentsession.Store`). Those
are procedural, already behind interfaces, and where each runs is a
deployment choice: together in one process, as dax runs today, or apart.

So the boundary that has to be right is the one around execution, and it
has to be a boundary a sandbox can enforce: nothing outside it reads the
sandbox's files or starts its processes, and nothing inside it decides
what is allowed.

## Where it cuts: tool calls, not primitives

There are two places to cut.

| Cut | The wire carries | Inside the sandbox | Outside |
|---|---|---|---|
| Primitives (`agentworkspace-module.md`: a remote `workspace.Workspace`) | file reads and writes, `Exec`, stats, link reads | the operations | the tools' logic and the policy's inspections, reading the sandbox's files over the wire |
| Tool calls (this proposal) | a call to run, and the facts the policy needs about a call | the tools, their workspace, MCP servers, the inspections | the loop, the model, the decisions, the record |

The primitive cut works, and dax already has it in process
(`workspace.Workspace`, `tool.Files`). Over a wire it is chatty: `grep`
reads every file it searches across the connection (`tool/search.go`
walks `Files`), and the bash analyzer stats, resolves links and runs
`git` one round trip at a time (`tool/view.go:57` `resolve`,
`tool/check.go:826` `Check`, `tool/gitconfig.go:61`). It also leaves the
inspections outside the sandbox, reading what the sandbox chooses to
serve. It fits a workspace that is only storage (a mounted volume, a
remote file service) and a host that must not run anything.

The tool-call cut puts the whole of execution in one place. The executor
answers two kinds of question:

- **Facts**: what a call would touch, without acting. The policy's
  subjects (`agentpolicy.Subject`): the stages of a command line, the
  paths it reads, where a link leads, the git configuration key that
  names a program, a subject no rule names when a link cannot be read.
  And, for bash, the plan the analysis found and its stamp.
- **Effects**: run the call, report its progress, ask its questions,
  return its result.

Control evaluation decides on facts and never touches the sandbox. The
workspace interface stays: it is how the executor touches its own
machine, and how a container or a remote runtime implements that for an
executor that does not run inside it.

Coupled is in process: the executor is dax-coding over `workspace.Local`,
as now. Decoupled is a dax executor serving from inside the sandbox, and
the rest of dax calling it.

## The executor

A sketch in dax's terms; the names are for discussion.

```go
// Package executor is where a session's calls act. Its in-process form
// is the session's own tools over its workspace; its remote form is a
// client of an executor served from inside a sandbox.
package executor

// Executor runs a session's world-touching tools and says what a call
// would touch. It decides nothing: whether a call runs is the policy's,
// on the facts the executor gave.
type Executor interface {
	// Tools are the tools it runs, as the model and the policy see
	// them: definition, annotations, and which only look (explore's).
	Tools(ctx context.Context) ([]Tool, error)
	// Facts is what call would touch, for the policy, and the arguments
	// it would run in place of the call's if the policy allows the call
	// without asking (bash's stamped plan). It acts on nothing. An error
	// is a call the policy cannot read, which asks.
	Facts(ctx context.Context, call Call) (Facts, error)
	// Call runs call. Progress (agenttool.Progress) and questions
	// (agenttool.Elicitor) reach the caller through ctx, as in process.
	Call(ctx context.Context, call Call) (agenttool.Result, error)
	// Descriptor is the workspace the calls act in, for the record.
	Descriptor() workspace.Descriptor
	Close() error
}

type Tool struct {
	Definition  *openresponses.FunctionTool
	Annotations agenttool.Annotations
	ReadOnly    bool
	Sequential  bool
	Resource    string
}

type Call struct {
	ID   string          // the function call's call_id
	Tool string
	Args json.RawMessage
}

type Facts struct {
	// Subjects are the call as the policy's rules see it; nil is the
	// call's own arguments, one subject.
	Subjects []agentpolicy.Subject
	// Rewrite, when set, is the arguments the call runs with if the
	// policy allows it without asking: bash's arguments carrying the
	// stamp of the plan the analysis approved.
	Rewrite json.RawMessage
}
```

The session wraps it for the loop: each `Tool` becomes an
`agenttool.Tool` whose `Execute` is `Executor.Call`; the policy's
matcher for a tool takes its subjects from `Facts`; the extension's
`BeforeToolCall` that dax-coding uses for the stamp becomes "allow with
`Facts.Rewrite`", folded under the policy's verdict as today
(`ext/coding/coding.go:74` `stampBash`). The static parts of a matcher
stay with control: `Match` reads only a specifier and a subject's
arguments (`ext/coding/policy.go:112` `pathMatcher`), and aliases and
lifts are configuration (`ext/coding/policy.go`).

### Checked against dax-coding

Every place the policy path and the tools read the machine today, and
where it lands:

| Today | Reads | Under the cut |
|---|---|---|
| `tool.BashSubjects` (`tool/shell.go:37`) | runs `Analyzer.Check`, which stats paths, resolves links and runs git | `Facts.Subjects` for bash |
| `tool.PathSubjects` (`tool/shell.go:304`) with `view.rel`/`view.resolve` (`tool/view.go:50`, `:57`) | normalises the path, follows links within the workspace, adds `dax:links-unknown` (`tool/shell.go:291`, `:332`) when links cannot be read | `Facts.Subjects` for read, write, edit, glob, grep, ls |
| `tool.Analyzer.Check` (`tool/check.go:826`): `stageFirst`, `rels`, `checkCd` (`:699`, `:736`, `:802`), globs through `strictFS` (`tool/view.go:165`) | the workspace's files and links | inside `Facts` |
| `execConfigKey` (`tool/gitconfig.go:61`) and its scripts | runs `git config` and `sh` in the workspace | inside `Facts` |
| `tool.StampArgs` (`tool/stamp.go:39`), called by `stampBash` (`ext/coding/coding.go:74`) | runs `Check`, signs the plan with the process's key (`tool/stamp.go:14`) | `Facts.Rewrite`; the key stays in the executor |
| bash's `command` re-check (`tool/bash.go:150`) | runs `Check` again and compares the stamp | inside `Call`, unchanged |
| the file tools, search, bash (`tool/fs.go`, `tool/search.go`, `tool/bash.go`) | `Files` over the workspace | inside `Call` |
| `matchers`' `Match` (`ext/coding/policy.go:112`), `aliases`, `lifts`, `allowRules`, `askRules` | nothing | control, unchanged |

Nothing on the policy path is left reading the machine outside the
executor. Gaps the sketch has to close:

1. **Subjects have no context.** `agentpolicy.Subjects` is
   `func(args json.RawMessage) ([]Subject, error)`
   (`agentpolicy/matcher.go:104`); dax's bash splitter calls `Check` with
   `context.Background()` (`tool/shell.go:54`). A remote facts call needs
   a context and a deadline, and the engine may evaluate a call more than
   once (`Would`, the decision, a sub-agent's `childPolicy`). Until
   agentpolicy takes a context, the session fetches `Facts` once per call
   (by call ID and arguments) under the call's context and the matcher
   reads the cached value; an error or a timeout is a subject no rule
   names, which asks, as `dax:links-unknown` does today.
2. **Facts and effects happen at different moments.** Between them the
   sandbox can change. For bash this is already handled: the tool
   re-checks and refuses a plan that no longer analyses to its stamp
   (`errChanged`, `tool/stamp.go`). For the file tools it is the same gap
   dax has in process today: a link swapped after the decision is
   followed only within the workspace (`os.Root`), so the confinement
   holds while the secret-path ask can be raced. The executor does not
   widen it, but it is worth stating.
3. **Replay hints do not cross MCP.** `agenttool.WithReplay` has no MCP
   field and `mcpclient` has no option for it; a remote call reads as
   replay-unknown. dax-coding sets none today, so nothing is lost now.

## The wire: MCP, plus a facts call

agenttool already serves and consumes tools over MCP, and carries most of
what `Call` needs:

| `Executor.Call` needs | MCP via agenttool |
|---|---|
| definitions, schema | `mcpserver.Definition` (`agenttool/mcpserver/mcp.go:196`) |
| annotations | served as MCP tool annotations (`mcpserver/mcp.go:202`), read back through `AnnotationsOf` (`mcpclient/mcp.go:15`) |
| progress | `NotifyProgress` from `agenttool.Progress` (`mcpserver/mcp.go:305-320`), routed to the call by `mcpclient` |
| questions (elicitation) | `mcpserver` asks through the session (`mcpserver/elicit.go`); `mcpclient.WithElicitation` answers (`mcpclient/mcp.go:351`) |
| cancellation | ending a call cancels its request (`mcpclient/mcp.go:623`) |
| the tool's record | `_meta` under `RecordMetaKey` (`mcpserver/mcp.go:44`, `mcpclient/mcp.go:114`) |
| sequential, resource, confinement | set on the client side by option (`mcpclient/mcp.go:203`, `:224`, `:245`); MCP has no field |
| policy | none on the server, by design: "A loop's hooks around tool calls belong to whoever hosts the loop" (`mcpserver/mcp.go`, package doc) |

What MCP lacks is the facts call. Recommendation: MCP for calls, and the
facts call as one reserved request on the same connection, served by
`mcpserver` and called by `mcpclient` (a method of its own, or a reserved
tool name `mcpclient` keeps out of the model's list), whose result is the
`Facts` above as JSON. Not a new protocol: the calls, progress,
questions, cancellation and records are what MCP already carries, and an
executor is then also an ordinary MCP server any other host can use
without the facts.

The read-only flag, `Sequential` and `Resource` travel in the facts
call's listing or in the tool's `_meta`, since MCP's annotations are
hints the policy must not use alone (`agenttool/tool.go:278`).

## What else in dax touches the machine

| Today | Where | Under the cut |
|---|---|---|
| MCP servers started by the session (`agent/agent.go:898` `mcpTransport`, `exec.Command` at `:903`) | the host running the turn | inside the executor: the executor starts them and serves their tools with its own |
| project skills (`ext/skills/skills.go:54-55`, `env.Dir()`) | this machine's `Dir` | the project's files are the sandbox's: read through the executor (agentskill reads an `fs.FS`, `agentskill.Source`), or the executor serves the skill tool |
| AGENTS.md (`agent/agent.go:374`, `:382`) | this machine's `Dir` | read through the executor; agentsmd needs `Options.FS` (`instruction-sources-fs.md`) |
| the project config (`cli.go:328` `loadSettings`) | this machine's `.dax/config.json` | read through the executor; it is dax's own file |
| memory (`ext/memory/memory.go:42` `filestore.Open`) | the user's store | control: it is the user's, not the project's, and its tools touch no workspace |
| the record | the controller | control: tool results come back as results and the controller writes them; the executor writes nothing durable |
| `agent.Options.Dir` | instruction sources on this machine | goes away for a remote executor: instructions come from the executor's files |

dax's `Extension` already splits along this line field by field. `Tools`,
`ReadOnly`, the `Subjects` half of `Matchers`, and a rewriting
`BeforeToolCall` are execution; `Policy`, `Aliases`, `Lifts`, the `Match`
half of `Matchers`, `Instructions`, `Renderers` and most `Kit` options are
control. A remote executor runs the execution half of the same
extensions; the controller runs the rest. A `Kit` option that adds a tool
that reads the project (the skill tool) is the one case that straddles,
and is listed above.

## Security

- **What each side trusts.** Control trusts the executor's facts only
  about the sandbox itself. A compromised sandbox can lie about what a
  call touches, but only inside the sandbox, where whatever is allowed
  already runs; nothing it reports can widen what control allows,
  because the rules, the asks and the decisions are control's.
- **The stamp.** The key stays in the executor (`tool/stamp.go:14`, a
  per-process random key). Facts return the plan and its stamp; control
  allows the call carrying the stamp; the executor runs only that plan
  and refuses one that changed. Control cannot forge a stamp, and does
  not need to.
- **Confinement is a claim with a consequence.** agentpolicy skips a bare
  ask rule for a call that `ConfinedBy` says runs in a sandbox
  (`agentpolicy/engine.go`, via `agenttool.ConfinedBy`; `mcpclient`
  claims it by option, `mcpclient/mcp.go:228-256`). dax should claim
  confinement for an executor's tools only when the user configured the
  sandbox as such, never by default.
- **Authentication.** Every connection between controller and executor
  is authenticated over TLS, with separate scopes for listing, facts and
  calls. agentsession's RFC 0003 security section is the model. An
  executor serves one controller at a time unless configured otherwise.
- **An unreachable executor.** A call fails with an error the model
  reads; a facts call that fails or times out is a subject no rule names,
  so the call asks or is refused, never allowed.
- **Resource limits** (CPU, memory, network, the file system's size) are
  the sandbox's, not dax's.

## What dax does with it

1. An `executor` package with the interface above. The session builds
   its tools, the matchers' subjects and the stamp from an `Executor`
   instead of from the extensions' tools directly.
2. dax-coding becomes the in-process executor's content: its tools over
   the session's workspace, its subjects and its stamp as `Facts`. Nothing
   changes for a local session.
3. A mode that serves an executor from inside a sandbox (`dax execute`,
   say): it runs the execution half of the configured extensions over
   `workspace.Local`, starts the MCP servers it is given, and serves
   tools and facts over MCP with the facts extension.
4. A client: `-executor <url>` (or a config field the project layer may
   not set) makes the session's executor the remote one.

None of the other wires is needed for the sandbox case. The turn, the
policy and its human or autonomous plane, the model calls and the record
all stay with the controller, in one process, as they are today. The
control contract over a wire (`agentturn-control.md`), agentconsole as a
view over it (`agentconsole-view.md`), and the record over the wire
(agentsession RFC 0003) remain choices about where those procedural parts
run, for the cases that want them apart too.

## What the siblings need

- **agenttool** (`mcpserver`, `mcpclient`): the facts call on the same
  connection, and carrying read-only, sequential and resource in the
  listing so a client does not configure them by hand.
- **agentpolicy**: a context on subjects (`Subjects` with a
  `context.Context`, or an engine option that supplies one), so a facts
  call can be cancelled and bounded.

## Open questions

- Whether the facts call is a method of its own or a reserved tool name:
  a method is cleaner and needs both sides to know it; a reserved tool
  works through any MCP stack.
- Whether `Facts` for one call can depend on the batch (a write the same
  batch made). Today each call is analysed alone, before the batch runs.
- Whether the executor should also offer the primitive cut (serve its
  workspace) for a controller that wants to read files itself, or
  whether a controller that wants that should use agentworkspace's
  remote workspace instead.
- How a sandbox's skill tool and instruction files reach a controller
  that renders the prompt: through the executor as files, or as a
  rendered part.
