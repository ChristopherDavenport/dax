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
- The tools and every policy check act through `workspace.Workspace`;
  the session takes its workspace and its store as values and records
  which workspace it ran in.
- The human or autonomous plane is `agent.Turn`; every front, and a
  controller with no front, drives a session through it.
- `Extension` already splits along the execution boundary: `Tools`,
  `ReadOnly` and the inspecting half of `Matchers` are execution;
  `Policy`, `Aliases`, `Lifts`, the matching half, `Instructions` and
  `Renderers` are control.

What still touches the machine dax runs on, and belongs in the executor:
MCP stdio servers, project skills, AGENTS.md and the project config.

## Changes by repository

Each row is checked against the repository's own statement of what it
is (its AGENTS.md or README). "Fits" means the change is the kind the
repository exists to hold; a tension is named where there is one.

| Repository | Mission, in its own words | Proposed change | Fit |
|---|---|---|---|
| **dax** | the product that assembles the siblings and keeps only what is specific to a coding agent | an `executor` package (in-process form: dax-coding over the session's workspace; remote form: a client); `dax execute`, which serves an executor from inside a sandbox; skills and the project config read from the workspace | Fits. The executor's in-process form is dax-specific; its wire belongs to the siblings below |
| **agenttool** | the tool contract: what a tool is, how a batch of calls executes, MCP adapters in both directions; the root depends on openresponses and the standard library | **facts as an optional per-call claim** in the root contract, beside `Confined` and `Replayable`: the calls this call amounts to, in tool-call terms (`read {path}`, `bash {command}`), plus an optional rewrite (bash's stamped plan). The MCP adapters carry it, with read-only, sequential and resource, in the listing | Fits: per-call claims are already the contract's pattern, and tool-call terms need no policy import. **Adjust the draft**: it proposes an MCP-only facts call, which would put a contract concept in an adapter |
| **agentpolicy** | decisions: a rule grammar and an engine that become the loop's `BeforeToolCall`, with every verdict recorded | subjects taken from a tool's facts claim; `Subjects` with a context (or an engine option that supplies one) | Fits: the engine decides; where facts come from is a matcher's concern |
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
2. **dax, no sibling needed**: the `executor` package with dax-coding as
   its in-process form; project skills and the project config read
   from the workspace. Facts are fetched once per call and cached until
   agentpolicy takes a context.
3. **The sandbox**, the goal's first case:
   - agenttool: the facts claim and its MCP carriage.
   - agentpolicy: subjects from facts, with a context.
   - agentworkspace: the module, `Local`, `Start`.
   - dax: `dax execute` and the remote executor; MCP servers started in
     the workspace.
4. **Instruction sources**: agentsmd's `FS`, an agentkit release, dax
   reads AGENTS.md from the workspace.
5. **Where the orchestration runs** (optional): agentturn's `Control`
   and `front/control`, agentkit's `Kit.Control`, agentconsole as the
   view, RFC 0003's store client in dax: `dax serve` and `dax attach`.
6. **Peers** (optional): agentsession's peer link, agentturn's peer
   changes, an `ext/a2a` extension in dax.

## Decided

- **Memory stays with control.** It is the user's knowledge across
  projects, not the sandbox's: dax-memory's store and tools run where
  control runs, never in the executor, so a sandboxed session cannot
  read or write the user's memory directly.

## Decisions still open

- **Facts' shape in agenttool.** Tool-call terms (the calls a call
  amounts to) keep the contract free of policy; whether a rewrite (the
  stamp) belongs in the same claim or beside it.
- **The view and live events.** Whether agentconsole reads the record
  alone, with uncommitted changes carried by RFC 0003's follow, or keeps
  its live events.
- **Where the policy engine runs** when control and execution are apart.
  The plan keeps rules and decisions with control and facts with
  execution; the engine's fast allows and denies then cost a round trip
  for facts on every call that has a matcher.
- **Who answers a peer's own asks.** The callee's controller by default;
  escalation to the caller only where the callee enables it.
