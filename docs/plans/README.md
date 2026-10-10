# The plan: a distributed loop of clean planes

The reference for the work that #35 and #36 start: where the system is
going, what each repository is asked to change, in what order, and how
each change fits that repository's own mission. The issue drafts it
points to are in [proposals/](proposals/README.md); the design dax keeps
today is [docs/design.md](../design.md).

## The goal

A turn runs as a distributed loop. Everything that touches the world
can run in a sandbox, on its own machine if need be, so that it can be
secure; the model can be anywhere; the rest of the system is
procedural orchestration. Whether the parts are coupled in one process
or decoupled over a wire is then a deployment choice, not a different
system. Running everything on a laptop is the same arrangement with the
wires left out.

## The model

Four parts meet at the turn, each behind an interface whose in-process
implementation is the bypass:

| Part | What it does | Interface | In process | Elsewhere |
|---|---|---|---|---|
| AI | inference | `openresponses.Streamer` | Ollama on this host | any provider, any host |
| Execution | everything that touches the world: the tools, their files and processes, MCP servers | an executor: facts and effects (proposal: execution-boundary) | dax-coding over `workspace.Local` | `dax execute` in a sandbox |
| Control evaluation | decides at the hold: the policy's rules, and whoever answers what the policy asks (a person, rules, a model) | `agent.Turn` and `agent.Rules` in dax today | the REPL, the terminal client, `-p`, `agent.Drive` | a front over a wire (optional, later) |
| The record | what every part agrees happened | `agentsession.Store` | the local content-addressed store | RFC 0003 |

The cut that has to be right is the execution boundary, and it is drawn
at tool calls, not at file and process primitives. The executor answers
facts (what a call would touch: the paths it reads and writes, the
commands a shell line runs, bash's plan and its stamp) and performs
effects (runs the call). Control decides on the facts and never touches
the sandbox. Nothing the sandbox reports can widen what control allows:
bash runs only a plan control approved, under a key that never leaves
the executor.

The view (agentconsole) reads the record. Input is a product's (dax's
text box), and goes to control evaluation.

A2A is not a part. It is the boundary between two such systems: to the
caller the other is a tool call its policy decides; to the callee the
caller is the controller that prompts it.

## Where things stand

After #35 and #36, in dax:

- The session knows no tool. Everything the model can do is an
  `extension.Extension`, dax's own included (dax-coding, dax-agents,
  dax-skills, dax-memory). Each extension's rules are a policy source of
  their own and may name only its own tools.
- The tools and every policy check act through `workspace.Workspace`,
  agentworkspace's since its v0.0.1; the session takes its workspace
  and its store as values and records which workspace it ran in.
- The human or autonomous plane is `agent.Turn`; every front, and a
  controller with no front, drives a session through it.
- `Extension` already splits along the execution boundary: `Tools`,
  `ReadOnly` and the inspecting half of `Matchers` are execution;
  `Policy`, `Aliases`, `Lifts`, the matching half, `Instructions` and
  `Renderers` are control.

What still touches the machine dax runs on, and belongs in the executor:
MCP stdio servers in a workspace that cannot start a process (in one
that can, `workspace.Starter`, they start there; with `-executor` they
start in the executor, or fail when it cannot). Project skills,
AGENTS.md and the project config are read through the workspace, and
with `-executor` through the executor, which serves its workspace's
files read-only.

## Changes by repository

Each row is checked against the repository's own statement of what it
is (its AGENTS.md or README). "Fits" means the change is the kind the
repository exists to hold; a tension is named where there is one.

| Repository | Mission, in its own words | Proposed change | Fit |
|---|---|---|---|
| **dax** | the product that assembles the siblings and keeps only what is specific to a coding agent | an `executor` package (in-process form: dax-coding over the session's workspace; remote form: a client); `dax execute`, which serves an executor from inside a sandbox; skills and the project config read from the workspace | Fits. The executor's in-process form is dax-specific; its wire belongs to the siblings below |
| **agenttool** | the tool contract: what a tool is, how a batch of calls executes, MCP adapters in both directions; the root depends on openresponses and the standard library | **facts as an optional per-call claim** in the root contract, beside `Confined` and `Replayable`: the calls this call amounts to, in tool-call terms (`read {path}`, `bash {command}`), plus an optional rewrite (bash's stamped plan). The MCP adapters carry it, with read-only, sequential and resource, in the listing | Fits: per-call claims are already the contract's pattern, and tool-call terms need no policy import. **Adjust the draft**: it proposes an MCP-only facts call, which would put a contract concept in an adapter |
| **agentpolicy** | decisions: a rule grammar and an engine that become the loop's `BeforeToolCall`, with every verdict recorded | subjects taken from a tool's facts claim; `Subjects` with a context (done in v0.0.12) | Fits: the engine decides; where facts come from is a matcher's concern |
| **agentworkspace** *(new)* | proposed by the openhands-workspace study: the seam that lets the built-in tools act on a local directory, a container or a remote runtime | create it from the study and dax's `workspace` package; `Start` for long-lived processes with pipes (MCP servers in the workspace); `Local` and a remote handler and client | Fits the study's design; it is what the executor runs over inside the sandbox, and the primitive cut for hosts that offer only files and exec |
| **agentsmd** | the AGENTS.md convention: `Chain` finds the files that apply at a path; root is standard library only | `Options.FS`: walk the chain through an `fs.FS` | Fits: `io/fs` is the standard library, and the convention does not care where the files are |
| **agentskill** | the Agent Skills format: `SKILL.md` trees read through `fs.FS`, local or not | nothing | Already fits; dax reads project skills from the workspace with what exists |
| **agentkit** | assembly: one call that turns a product's choices into an `agentturn.Config`; no hidden seams | a release pinning agentsmd's `FS`. Optional, later: `Kit.Control(agent)`, the kit's agent as the control contract (what kitbackend does today) | Fits, provided `Kit.Control` is a helper over exported calls and hides no seam |
| **agentsession** | the reference implementation of the Agent Session Format: one lossless record of what was sent, returned and done | RFC 0003 as drafted (the store over HTTP); a `peer` link relation in RFC 0001 | Fits: both are about the record, and a peer is one more thing the record links |
| **agentturn** | one loop, one transcript type, one tool contract, hooks and queues; everything else is a front or a subscriber; the loop never learns a sub-agent concept | Optional, later: `Control` (the `*Agent` methods it already has, plus `Reply`), questions as events, `front/control`. Peers: cancel on abort, resume `input-required`, authentication for `front/a2a`, opt-in escalation | Fits: `front/control` is a front, questions are events to subscribers, and peers are a front and a tool. `Control` is a description of the loop's existing surface, not a new concept |
| **agentconsole** | a terminal client; the record is the truth for what is committed, and live events carry only what is not | Optional, later: narrow to the view: consume agentturn's control contract and events plus a record follower; `client/native` and `client/kitbackend` move out | Fits its being a client of the record. **Tension**: its rule keeps live events for what is not committed, where this plan's view reads the record alone. RFC 0003's follow marks changes durable or not, which could carry the uncommitted part; that is agentconsole's call |
| **openresponses**, **agentmemory**, **toolbundle** | the model wire; memory between sessions; a tool set as a command-line program | nothing | Untouched. toolbundle remains the fallback for hosts with a shell and no MCP |

## Order

Each step depends only on those above it. Sibling steps are released
before the dax step that uses them, as the workspace's rules require.

1. **Merge #35, then #36** (merge, not squash).
2. **In parallel, no network needed:**
   - dax: the `executor` package with dax-coding as its in-process
     form; project skills and the project config read from the
     workspace. Facts are read once per decision: each decision pins
     its call's reading, so the policy's verdict and the stamp are of
     one reading. The pin stays useful now that agentpolicy takes a
     context (v0.0.12), since the engine still reads a call more than once.
     **Done**: `internal/executor` (`Executor`, `InProcess`, `Set.Pin`);
     every extension's `Tools` run through it, `Kit` tools stay with
     control, and the replay claim crosses the adapter.
   - Instruction sources: agentsmd's `FS`, an agentkit release, and dax
     reading AGENTS.md from the workspace. Independent of everything
     else, so it need not wait for the sandbox (agentskill already
     reads through `fs.FS`). **Done** in agentsmd v0.0.3 and agentkit
     v0.0.8; dax's part, with project skills and the project config
     read through the workspace too and the chain bounded by the
     repository's root as https://agents.md has it, is in review
     (branch `workspace-instructions`).
3. **The sandbox**, the goal's first case:
   - agenttool: the facts claim and its MCP carriage. The claim is
     **done** in agenttool v0.0.19 (`Factual`, `FactsOf`, `WithFacts`,
     forwarded by `Wrap`), and dax's tools make it; the MCP carriage is
     **done** in agenttool v0.0.20 (`execution/facts`, opt-in on the
     client with `WithClaims()`, which dax does not pass yet).
   - agentpolicy: subjects from facts, with a context. The context is
     **done** in agentpolicy v0.0.12: `Subjects` takes the decision's
     context, and dax's splitters and `factspolicy` ask the claim under
     it (execution-boundary.md, gap 1, closed).
   - agentworkspace: the module, `Local`, `Start`. **Done** in
     agentworkspace v0.0.1, from dax's `workspace` package; dax has
     moved to it (its `workspace` package is gone, the import renamed).
   - dax: `dax execute` and the remote executor; MCP servers started in
     the workspace. The MCP servers are **done**: a stdio server starts
     through `Start` when the session's workspace is a
     `workspace.Starter`, and on this machine when it is not.
     `dax execute` and `-executor` are **done** in dax v0.0.6, and the
     project's files (AGENTS.md, `.dax/skills`, `.dax/config.json`)
     read through the executor, which serves its workspace's files as
     read-only MCP resources (`dax-workspace:///{op}{?path}`), are
     **done** (branch `executor-files`). Facts batched per model
     response are **done**: a response's calls are read in one request
     and their rewrites in a second (`Set.PinBatch`), for the main
     agent and a sub-agent. MCP servers with an executor start in it
     (decided over refusing them): `dax execute` starts a process for
     its session through dax-only JSON-RPC methods (`dax/process.*`,
     not tools), with `executor.Remote.Start` their client, **done**
     (branch `process-tunnel`, agenttool v0.0.21's `WithClientSetup`);
     the session's executor view is a `workspace.Starter` over it, so
     every configured server and `/mcp add` runs in the sandbox and an
     executor that cannot start one fails it, **done** (branch
     `executor-mcp`). Follow-up: an executor over an address rather
     than a command.
4. **Where the orchestration runs** (optional): agentturn's `Control`
   and `front/control`, agentkit's `Kit.Control`, agentconsole as the
   view, RFC 0003's store client in dax: `dax serve` and `dax attach`.
5. **Peers** (optional): agentsession's peer link, agentturn's peer
   changes, an `ext/a2a` extension in dax.

## Decided

- **Memory stays with control.** It is the user's knowledge across
  projects, not the sandbox's: dax-memory's store and tools run where
  control runs, never in the executor, so a sandboxed session cannot
  read or write the user's memory directly.

- **The shape of facts**, settled by building it (dax's draft, now
  agenttool's `Factual` since v0.0.19). A tool claims `Facts(ctx, args) (Facts, error)`, an
  optional per-call claim beside `Confined` and `Replayable`. `Facts`
  is `Calls`, the calls this call amounts to in tool-call terms
  (`{Tool, Args, Text}`: a `read` of what `cat` reads, a `write` of a
  redirect's target, a call no rule names for what cannot be read), and
  `Rewrite`, the arguments the call runs with if the policy allows it
  (agenttool's names: `FactCall` for a call, `FactsOf` to read a claim).
  Nil `Calls` is the call itself; empty is nothing readable, which the
  policy refuses. The policy's subjects are the claim's calls
  (`facts/factspolicy`), so a matcher supplies only `Match`; the
  session applies `Rewrite` as one generic hook folded under the
  policy's verdict, so an ask is still asked and an approved call runs
  rewritten, in the main agent and in a sub-agent. A claim that fails
  blocks the call.
  - **The stamp is in the same claim.** It is a fact about the call
    only the tool can state: within a claim, the facts it reports and
    the stamp are of one analysis (bash's, of one reading of the
    command), and the key never leaves the executor. Beside the claim
    it would be a second call over the wire and a second analysis that
    could disagree with the first. The policy's verdict and the hook's
    rewrite read the claim once between them: each decision pins the
    call's reading (step 2, done).
  - **A claim names only its own extension's tools.** A claimed call
    names the claiming tool or another of its extension's tools (bash's
    `read` of what `cat` reads); one that names any other tool is
    decided as a tool no rule names, so it asks, and an extension's
    claim never borrows another's allow rule.
  - **A claim reads its fields by their exact keys, and refuses them
    in another case.** The tool's decoder takes any case, the last such
    key winning; a claim that read `path` while the tool read `Path`
    would put one path to the policy and act on another. The tools
    refuse such arguments too, on their own.
  - **Why it belongs in agenttool.** `agenttool.Wrap` forwards only the
    claims agenttool knows, so a claim defined elsewhere is lost on a
    wrapped tool; dax's `facts.Of` looked through wrappers until the
    claim moved there in v0.0.19, and `Wrap` now forwards it.
  - Every policy, exploit and confinement test passes through the
    claims with its expectations unchanged.
- **Facts cross the wire as an MCP method of their own**
  (`execution/facts`, taking a model response's calls together), not a
  reserved tool name, which could reach the model's tool list.
- **The stamp covers every claiming tool.** A file tool's claim
  (read, write, edit, glob, grep, ls) rewrites its arguments with a
  stamp: an HMAC, under bash's per-process key, of the facts it reported
  (its name and each call's tool and arguments, in order). A call that
  carries one recomputes its facts when it runs and is refused ("ask
  again") if they differ, so a path the policy saw as `notes.txt` that
  became a link to `.env` before the call (which `os.Root` follows,
  `.env` being inside) does not run, whether the policy allowed it or a
  person approved it, in the main agent or a sub-agent. A stamp the
  model supplied is replaced by the rewrite, or refused at the call; a
  call with no stamp, with the policy off, is not checked. Bash's stamp
  signs its plan and its facts together (#45): re-analysing the line at
  the call bound it only to its plan, which is text, so `cat notes.txt`
  decided as a read of `notes.txt` still ran once `notes.txt` was a link
  to `.env`; the analysis saw the new read and nothing compared it. A
  line the analysis allows carries the stamp whether the policy allowed
  it or a person approved it when a rule asked; a line outside the
  analysis that a person approved runs as written, unconfined, as
  before. One window is left: between
  the executor's recompute and its own open of the path. The one
  between the policy's reading of a call's facts and the stamped one
  is closed by the per-decision pin (step 2).

## Decisions still open

- **The view and live events.** The plan recommends that agentconsole
  read the record alone, with uncommitted changes carried by RFC 0003's
  follow (which marks each change durable or not), so there is one
  stream to reconcile, not two. Its own rule keeps live events for what
  is not committed, so the decision is agentconsole's, made when its
  proposal is taken up.
- **Where the policy engine runs** when control and execution are apart.
  Rules and decisions stay with control and facts with execution, as
  built; what is open is the cost: the engine reads a call's subjects
  more than once (its verdict, a sub-agent's check, the batch hold's
  reading of each sibling) and decides the rewrite hook's arguments
  again. The session holds each model response's facts for the
  decisions about it (`internal/executor`'s `Set.PinBatch`), so a
  remote executor is asked twice per response, its calls then their
  rewrites, under the first decision's context (agentpolicy v0.0.12):
  three calls cost the main agent two requests, not twelve, and a
  sub-agent two, not six. Tools that claim no facts cost no request
  (execution-boundary.md, Round trips). The re-reading was also a
  window, closed first by a per-decision pin and now by the batch: a
  sub-agent's check (`childPolicy`) took the rewrite from a reading
  after its verdict and did not re-decide, so a path swapped in that
  moment was stamped and run. A rewrite is read as a call of its own,
  never seeded from the reading that asked for it, since what the
  rewrite claims is what the engine decides. Decided; nothing open.
- **Who answers a peer's own asks.** The callee's controller by default;
  escalation to the caller only where the callee enables it.
