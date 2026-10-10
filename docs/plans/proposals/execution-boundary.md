# The execution boundary: everything that touches the world can run in a sandbox

Repositories: dax (the executor and its two forms), agenttool (facts as
an optional per-call claim in the tool contract, carried by `mcpserver`
and `mcpclient` beside the tool calls MCP already carries), agentpolicy
(subjects taken from a tool's facts, with a context, able to fail). dax's part comes
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

// Facts is agenttool's facts claim (built; see below).
type Facts = agenttool.Facts
```

The session wraps it for the loop: each `Tool` becomes an
`agenttool.Tool` whose `Execute` is `Executor.Call` and whose facts
claim is `Executor.Facts`. Nothing else changes, because the in-process
session already decides on claims (below). The static parts of a
matcher stay with control: `Match` reads only a specifier and a
subject's arguments (`ext/coding/policy.go` `pathMatcher`), and aliases
and lifts are configuration.

### The claim, as built

dax built the in-process half as the draft of agenttool's claim, and
agenttool v0.0.19 took it in its own names: `Call` is `FactCall`,
`Claimer` is `Factual`, `Of` is `FactsOf`, `Claims` is `IsFactual`, and
`With` is the `WithFacts` option of `New` and `NewFunc`, with `Wrap`
forwarding the claim. dax's draft was:

```go
// package facts: agenttool and the standard library only.
type Call struct {
	Tool string          // "" is the called tool; "read" for what cat reads; a name no rule names for what cannot be read
	Args json.RawMessage
	Text string          // what a question shows
}

type Facts struct {
	Calls   []Call          // nil: the call itself; empty: nothing readable, which the policy refuses
	Rewrite json.RawMessage // the arguments it runs with if the policy allows it (bash's stamped plan)
}

type Claimer interface {
	Facts(ctx context.Context, args json.RawMessage) (Facts, error)
}

func Of(ctx context.Context, t agenttool.Tool, args json.RawMessage) (Facts, bool, error)
func With(t agenttool.Tool, fn func(context.Context, json.RawMessage) (Facts, error)) agenttool.Tool
```

- dax-coding's seven tools make the claim from the analysis that was
  the policy's splitters (`tool.BashSubjects`, `tool.PathSubjects`).
  Within one claim the stamp is of the facts it reports (bash's, of the
  plan of the same reading of the command). The policy's verdict and
  the hook's rewrite read the claim once between them: each decision
  pins the call's reading (Gaps, 1 and 2).
- A claimed call names the claiming tool or another tool of its own
  extension (bash's `read` of what `cat` reads); one that names any
  other tool is decided as a tool no rule names, so it asks, and never
  borrows another extension's rules (`factspolicy.SubjectsOf`). A claim
  that fails blocks the call. A claim refuses arguments with a key that
  is one of the fields it reads in another case, and dax-coding's tools
  refuse a key that is any of their fields in another case, since a
  tool's decoder takes any case and a claim reads the exact key.
- `facts/factspolicy` turns claims into agentpolicy's subjects
  (`Matchers`: a matcher for a claiming tool supplies `Match` only, and
  bringing `Subjects` too is an error) and into one generic
  `BeforeToolCall` that applies `Rewrite` as an allow folded under the
  verdict. dax-coding ships no hook and no subjects of its own.
- `agenttool.Wrap` drops a claim agenttool does not know, so `Of` looks
  through wrappers; `With` forwards every claim agenttool reads, as
  `Wrap` does. Both are reasons the claim belongs in agenttool, where
  `Wrap` would forward it.
- Every policy, exploit and confinement test, and the cross-workspace
  table, passes through the claims with its expectations unchanged.

### Checked against dax-coding

Every place the policy path and the tools read the machine today, and
where it lands:

| Today | Reads | Under the cut |
|---|---|---|
| `tool.BashSubjects` (`tool/shell.go:37`) | runs `Analyzer.Check`, which stats paths, resolves links and runs git | `Facts.Subjects` for bash |
| `tool.PathSubjects` (`tool/shell.go:304`) with `view.rel`/`view.resolve` (`tool/view.go:50`, `:57`) | normalises the path, follows links within the workspace, adds `dax:links-unknown` (`tool/shell.go:291`, `:332`) when links cannot be read | `Facts.Subjects` for read, write, edit, glob, grep, ls |
| `tool.Analyzer.Check` (`tool/check.go:826`): `stageFirst`, `rels`, `checkCd` (`:699`, `:736`, `:802`), globs through `strictFS` (`tool/view.go:165`) | the workspace's files and links | inside `Facts` |
| `execConfigKey` (`tool/gitconfig.go:61`) and its scripts | runs `git config` and `sh` in the workspace | inside `Facts` |
| `tool.StampArgs` (`tool/stamp.go`), called before facts by dax-coding's `stampBash` hook, now bash's claim | runs `Check`, signs the plan with the process's key (`tool/stamp.go:14`) | `Facts.Rewrite`; the key stays in the executor |
| bash's `command` re-check (`tool/bash.go:150`) | runs `Check` again and compares the stamp | inside `Call`, unchanged |
| the file tools, search, bash (`tool/fs.go`, `tool/search.go`, `tool/bash.go`) | `Files` over the workspace | inside `Call` |
| `matchers`' `Match` (`ext/coding/policy.go:112`), `aliases`, `lifts`, `allowRules`, `askRules` | nothing | control, unchanged |

Nothing on the policy path is left reading the machine outside the
executor. Gaps the sketch has to close:

1. **Subjects have no context.** **Closed** in agentpolicy v0.0.12:
   `agentpolicy.Subjects` was `func(args json.RawMessage) ([]Subject,
   error)` and dax's splitters read the claim under
   `context.Background()`. It is now
   `func(ctx context.Context, args json.RawMessage) ([]Subject, error)`,
   and the engine passes the context of the decision that asks
   (`Decide`'s, `Would`'s, or, when the batch hold reads a sibling, the
   held decision's); `factspolicy.Subjects` and `tool.BashSubjects` ask
   under it, so a remote facts call is cancelled with the decision and
   keeps its deadline, and a cancelled context blocks the call. The
   engine may still evaluate a call more than once (`Would`, the
   decision, a sub-agent's `childPolicy`), so the pin stays: in
   `internal/executor` each decision pins its call (`Set.Pin`, keyed
   by the tool and the arguments' exact bytes), the first reading is
   made under the pin's context and every other reading of that
   decision gets it, an error included, which blocks the call. A reading
   no decision pinned, a sibling the batch hold reads, is made under
   the reader's context and not kept, and an entry goes with its last
   pin. The engine still reads the rewrite's arguments afresh. Since
   then a model response's facts are read together for its decisions
   (`Set.PinBatch`): its calls in one request, their rewrites in a
   second, every reading under a decision's context answered from them,
   so a response costs two requests and the pin is left for a call no
   batch holds.
2. **Facts and effects happen at different moments.** Between them the
   sandbox can change. For bash this is handled: its stamp signs the
   plan and the facts together, and the tool re-checks and refuses a
   line that no longer analyses to that plan on those facts
   (`errChanged`, `tool/stamp.go`); a plan alone did not catch a path
   that became a link inside the workspace (#45). The file tools' facts
   stamp does the same (`errTouched`): a call runs only if its facts, recomputed
   at the call, are the stamped ones, whether the policy allowed it or
   a person approved it, in the main agent or a sub-agent. Two windows
   are left. One is the moment between that recompute and the tool's
   own open. The other was between readings: the policy decided on one
   reading of the claim and the stamp was of another (the hook's), and
   a sub-agent's check (`childPolicy`) took the rewrite from a reading
   after its verdict and did not re-decide, so one change in that
   moment was stamped and run. The pin of (1) closes it: the stamp is of
   the reading the verdict was decided on
   (`TestACallRunsWithTheFactsItsVerdictWasDecidedOn`). Not closed: a
   third party's claim with no check when it runs, and two identical
   calls decided at once (parallel tasks) share one reading, which is
   still consistent. A link swapped is
   followed only within the workspace (`os.Root`), so confinement holds
   either way. The executor does not widen either window.
3. **Replay hints do not cross MCP.** `agenttool.WithReplay` has no MCP
   field and `mcpclient` has no option for it; a remote call reads as
   replay-unknown. dax-coding sets none today, so nothing is lost now.
   The in-process executor carries the claim (`Executor.Replay`), so a
   third party's replay claim survives the adapter.

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

What MCP lacks is facts. They belong first in agenttool's root contract,
as an optional per-call claim beside `Confined` and `Replayable`
(`agenttool/tool.go:341`, `:407`): a tool that can say what a call would
touch implements it, in tool-call terms, so the contract needs no policy
import. Facts are the calls this call amounts to (`read {"path": ".env"}`,
`write {"path": "out/x"}` for a redirect, `bash {"command": "git status"}`
for a stage), which is the shape of `agentpolicy.Subject` already, plus
an optional rewrite (bash's stamped plan). agentpolicy then takes a
tool's subjects from its facts. In process, dax-coding's tools implement
the claim; over the wire, `mcpserver` answers it as a method of its own
on the same connection (`execution/facts`, say) and `mcpclient`'s tools
implement the claim by asking. A method, not a reserved tool name: a
tool name could reach the model's tool list if a client failed to hide
it, and a method cannot. Not a new protocol: the calls, progress,
questions, cancellation and records are what MCP already carries, and an
executor is then also an ordinary MCP server any other host can use
without the facts.

The read-only flag, `Sequential`, `Resource` and whether a tool claims
facts at all travel in the tool's `_meta` in the listing, as facts
about the tool, not as MCP annotations, which are hints the policy must
not use alone (`agenttool/tool.go:278`).

Round trips. Each call that reaches the policy costs a facts request
before its call, so over a slow link the executor's place in the loop
shows. Two things keep it to what it must be:

- A tool that claims no facts is its own one fact, so control decides
  it on its arguments with no facts request. The listing says which
  tools claim; that is a fact about the tool, not an annotation, so
  skipping the request for one cannot widen what the policy allows.
- The calls of one model response are decided together, so their facts
  are fetched in one request (`execution/facts` takes a list), and a
  response of five calls costs one round trip for facts, not five (as
  built, two: the calls, then the rewrites they ask for, which the
  engine decides as calls of their own). The
  stamp each call's facts carry is checked when that call runs, so a
  batch fetched early still runs only on the facts that were stamped,
  which, with facts fetched once per call, are the facts it was decided
  on.

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
  per-process random key). Facts return the plan and its stamp, of the
  plan and the facts; control allows the call carrying the stamp; the
  executor runs only that plan on those facts and refuses one that
  changed. Control cannot forge a stamp, and does
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

- **agenttool**: the claim as built in dax's `facts`, beside `Confined`
  and `Replayable`, with `Wrap` forwarding it (done in v0.0.19:
  `Factual`, `Facts`, `FactCall`, `FactsOf`); `mcpserver` and
  `mcpclient` carry it as a reserved
  request on the same connection, with read-only, sequential and
  resource in the listing so a client does not configure them by hand
  (done in v0.0.20: `execution/facts`, opt-in on the client with
  `WithClaims()`, which dax does not pass yet).
- **agentpolicy**: subjects taken from a tool's claim, what dax's
  `factspolicy.Matchers` does (agentpolicy already depends on agenttool
  through agentturn), and a context on subjects, so a facts call can
  be cancelled and bounded (the context is done in v0.0.12: `Subjects`
  takes the decision's `context.Context`).

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
