# Design

dax is a coding agent built as one session that owns the conversation's
state, a loop that emits fine-grained events, and fronts that are thin
subscribers. It is a minimal core, meant to be extended. The session
knows no tool: everything the model can do comes from an extension, and
dax's own capabilities are extensions like any other (see Extending). A
Go program built on dax adds its own at compile time. There is no runtime
plugin system; at run time the user adds tools only as MCP servers.

Where the tools act is no more special than which tools there are. dax
should feel the same whether its tools act on this machine or elsewhere:
the session runs over one `workspace.Workspace` (the agentworkspace
module's), every tool and every
check the policy makes goes through it, and this machine's directory is
one implementation of it (see Workspaces).

Most of the machinery is not in this repository. dax is the product that
assembles sibling libraries, each independently versioned, and keeps
only what is specific to a coding agent. This document says what that is
and the rules the assembly keeps.

## Planes

A turn is where three planes meet, and dax is built so that each is
reached through an interface whose in-process implementation is only
the case with the wire left out. Which machine provides any of them
makes no difference to the turn.

| Plane | What it does | What the turn sees | In process | Elsewhere |
|---|---|---|---|---|
| AI | inference | `openresponses.Streamer` | Ollama on this host | any provider, any host |
| Execution | acts on files, processes and the world, through the tools | the executor (`internal/executor`): the extensions' tools, their facts and their calls, over a `workspace.Workspace` | `executor.InProcess` over `workspace.Local` | `dax execute` in a container or on a host: the same tools served over stdio MCP (`executor.NewServer`), their facts, replay claims and stamps made there, reached by `-executor` (`executor.Remote`) |
| Human or autonomous | prompts, steers, answers permissions and questions | the turn's contract, agentturn's to own (`agent.Turn` until it does); a view follows the record (agentconsole) | the REPL, `-p` (`agent.Drive`), the terminal client over glue | a front over a wire (an agentturn front beside `front/a2a`), or a controller that answers by rule |
| The record | what the planes agree happened | `agentsession.Store` | the local content-addressed store | a store over the wire (agentsession RFC 0003) |

So running dax on a laptop, in a container with the user attached from
elsewhere, or headless under a controller are one system with the
planes in different places, not three modes. A person at a terminal and
a controller that answers by rule are the same plane: whoever holds the
Turn. The plane's control contract is the turn's, in agentturn's terms
(prompt, steer, answer, abort, the run's events, the questions asked
while a call runs), so it belongs with agentturn, beside its A2A and
Responses fronts; `agent.Turn` is the shape dax proposes for it, and the
session is its in-process implementation. What a view shows is the
record, which leaves through agentsession (a Follower; RFC 0003 later);
agentconsole is such a view, and the terminal client reaches the Turn
through glue in `tuiadapter.go` that says what it fakes.

A2A is not a plane. It is the boundary between two such systems, each
with its own turn, planes and record: to the caller the other system is
part of its execution plane, a tool call its policy decides
(`agentturn/tools/a2a`); to the callee the caller is its human or
autonomous plane, the controller that prompts it (`agentturn/front/a2a`).
A peer sees tasks and artifacts, never the other's transcript, which is
why a front uses the Turn and not A2A. A call the callee's policy asks
about is answered by the callee's own human plane; the caller's tools it
was lent come back to the caller to run.

Where dax does not keep to this yet, and what each needs:

- The human plane: the contract is dax's `agent.Turn` until agentturn
  has one, and there is no wire for it yet (an agentturn front). dax's
  own controls (`agent.Controls`: model, reasoning, MCP, the session's
  assembly) need a channel beside it on that wire. agentconsole's
  `client.Backend` cannot carry a Turn whole; `tuiadapter.go` lists what
  its glue fakes or drops.
- The execution plane: `dax execute` serves the tools over stdio MCP
  and `-executor` (`agent.Options.Executor`, `executor.Remote`) runs a
  session's tools there, reading the project's files (AGENTS.md,
  `.dax/skills`, `.dax/config.json`) through it, but a session with an
  executor refuses MCP servers, which would run on this machine; facts
  are read one request per reading, not batched per response, and the
  project's files one request per stat, listing or read. An MCP stdio server starts in the workspace only when
  the workspace can start a long-lived process with pipes
  (`workspace.Starter`); in one that cannot, it starts where the turn
  runs.
- Peers: a call to one is not linked in the caller's record as a
  sub-agent's session is, an abort does not cancel the remote task,
  and `front/a2a` has no authentication.

## Who owns what

| Concern | Owner | dax's part |
|---|---|---|
| Wire types, the provider client | `openresponses` and its `providers/anthropic`, `providers/gemini` | `internal/provider` picks one from the config; `vertex` routes each request by model family to the anthropic or the gemini adapter over Vertex AI clients |
| The agent loop, events, steering, follow-ups, retry | `agentturn` | `agent` configures and drives it |
| Assembling a loop from parts | `agentkit` | `agent.open` is one `agentkit.New` call over the session's options and every extension's |
| What the model can do | dax's extensions; a program's | `extension` is the type; `ext/coding` (dax-coding), `ext/agents` (dax-agents), `ext/skills` (dax-skills), `ext/memory` (dax-memory) |
| Tool contract, MCP client, the facts claim | `agenttool` | `tool`: dax-coding's read, write, edit, glob, grep, ls, bash, each making the facts claim (`agenttool.Factual`), and `Files`, the tools' view of a workspace; `facts/factspolicy`: the policy's subjects and the rewrite hook from those claims |
| Where the tools act | `agentworkspace`: the `Workspace` interface, `Starter`, and `Local`, this machine's directory | the session's workspace (`Options.Workspace`, or a `Local` over `Dir`); MCP stdio servers started in it through `Start` |
| Where the tools run | dax | `internal/executor`: the `Executor` the session runs the extensions' tools through, `InProcess`, `Set`, the tools bound as the kit's, with each decision's facts pinned, `NewServer`, the tools served over MCP by `dax execute` (`execute.go`), and `Remote`, the client a session started with `-executor` runs them through (`agent.Executor`) |
| Allow, ask, deny | `agentpolicy` | `policy` merges one source per extension with the user's and the project's; dax-coding's rules, matchers and the bash splitter's use are in `ext/coding` |
| The session record | `agentsession` | `Options.Store` or the store at `Root`, `-list`, `-verify`, `-resume`, `-gc` |
| AGENTS.md | `agentsmd` | the session: the chain's extent, reading it through the workspace, screening and budget |
| Skills, memory | `agentskill`, `agentmemory` | dax-skills and dax-memory: directories, trust, scopes |
| Settings | dax | `internal/config` |
| Presentation | dax (REPL); agentconsole (the terminal client) | `front.go`, `internal/render`; `toolrender` draws dax-coding's and dax-agents' calls in the terminal client |
| The command line | dax | `dax.Main`; `cmd/dax` calls it with no options |

## Rules that hold

1. **One owner of conversation state.** The `agentturn.Agent` holds the
   transcript and the pending calls. A front sends it commands
   (`Prompt`, `Steer`, `FollowUp`, `Abort`); it does not hold a pointer
   into state. `State()` is a snapshot.
2. **Events are dispatched in order, synchronously, from the loop.** A
   subscriber that wants to be decoupled owns its own buffered channel.
   This is what keeps a renderer consistent with the transcript.
3. **The event stream is the interface to a front.** The REPL, print mode
   and any future front consume the same events. If a front needs
   something the stream does not carry, the event is added in the loop;
   there is no side channel.
4. **Nothing above the session imports a provider or a tool.** A front
   gets a `*agent.Session` and the callbacks it supplied; the model, the
   tools and the policy are chosen before it runs.
5. **Everything durable is an entry in the session store.** If it is not
   in the record it does not survive a restart: the transcript, each
   policy verdict, the memory manifest, a model switch. `-verify`
   rebuilds every recorded request and checks its hash.
6. **Unknown kinds decode, they do not fail.** The record is read by
   newer and older dax alike; the format's own versioning says when a
   reader is too old, and dax says so on `-resume`.
7. **`context.Context` is the only cancellation mechanism.** `Abort`
   cancels the run's context; a call cut off in flight is answered on the
   next prompt as having possibly run.
8. **Fail toward asking.** The policy's default is ask. A bash command it
   cannot read is asked about or blocked, never allowed because a rule
   happened to match its first word.

## The run

A turn is one assistant message plus its tool calls and their results.
Calls in a batch run concurrently except a sequential tool, and `bash`
is sequential, so a command never races a concurrent edit of the same
file. That is dax's whole concurrency policy.

After each tool batch the loop polls for steering messages and, when the
model returns with no tool calls, for follow-ups. A line typed in the REPL
during a run is steering; `/follow text` is a follow-up.

A call the policy asks about ends the run with the call pending. The front
asks the user, the answer is recorded as a person's, and the run is
resumed. Calls the policy allowed but held because a sibling asks are
released with the answer. A call a resumed session found unanswered is
answered with text that says what is known: it was cut off and may have
run; it never reached its tool; it was denied.

## The prompt

The session's part of the system prompt is a role line, each extension's
instructions in order (dax-coding's nudge toward glob, grep and ls over
shell commands; dax-agents' guide to the sub-agents), the user's
`instructions_file`, and the working directory. `agentkit` renders and
joins the rest in a fixed order: the skill catalogue, the memory block,
then the AGENTS.md chain (`~/.dax/AGENTS.md`, then every `AGENTS.md`
from the repository's root down to the working directory, nearest last).
The chain follows the convention (https://agents.md), which places files
at the repository's root and below it: the repository's root is the
nearest directory at or above the start holding a `.git`, nothing above
it is read, and with no repository the chain is the start directory's
file alone. The project's files are named in the prompt by their names
in the workspace (`AGENTS.md`), as agentkit renders the chain it reads
through the workspace's file system; the user's file keeps its absolute
path. Between the user's file and the chain come the files the user
names in `agents_md_global`, for every session whatever the repository
holds; like `~/.dax/AGENTS.md` they are the user's, not the project's,
so they are read on this machine whatever the workspace, a container's
included, and are not screened. A project's config may not set it.
What a layer left out, a file over the budget or a skill that
would not load, is printed at start as `omitted:`.

When dax-agents is on, the main agent's prompt carries its guide to the
sub-agents: its context lasts the session, so broad reading goes to
`explore` and a change it can brief completely to `task`, while a known
file, a small change or code it must see to decide stays with it. It
checks a report before building on it and passes the findings on in its
reply, since a front shows only the start of a report. The tools'
descriptions say how each works; the guide says how to divide the work.
A `task` sub-agent is told the main prompt without the guide
(`Env.SystemPrompt` leaving dax-agents out), having no sub-agents of its
own.

## Tools

The tools below are dax-coding's. `read`, `write`, `edit`, `glob`,
`grep` and `ls` go through `tool.Files` over the session's workspace,
and `bash` runs through the workspace's `Exec`; none uses `os` or
`os/exec` itself. On `workspace.Local`, an `os.Root` over the working
directory, a path that is absolute outside it, climbs out with `..`, or
reaches out through a symbolic link is refused with the same error,
because the root refuses the name before the file system follows it.
`bash` runs at the workspace's root but is not confined; a shell reaches
whatever the workspace's processes can, and the policy is what stands in
front of it.

A running `bash` command reports its output as progress as it arrives,
the window since the last report, so a front shows the command working
over the run's events and not only when it ends. The model still sees
the command's whole output as the result; progress reaches fronts
alone and is not recorded.

`glob` and `grep` skip `.git`, `node_modules`, `vendor` and a few other
trees by name. There is no `.gitignore` reader. A path named explicitly
as the search root is searched even if it is one of those.

## Policy

Rules are agentpolicy's: a tool name or `tool(specifier)`. Precedence is
deny, then ask, then allow, then the default, whatever the source. Each
extension's rules are a source of their own, `extension:<name>`, which
the record of every verdict one decides names; dax-coding's allow list
is the read-only tools and a few commands that only look, and the
default asks. A shipped rule may name only its extension's tools and
aliases, not a pattern and not a carve-out, so one extension cannot
loosen another's tools or cancel its rules.

Sources rank for carve-outs: the user's config above the extensions,
the extensions above the project's config. Rules in the user's config
add to the extensions'; an extension's ask or deny holds against a plain
allow of the user's, and a carve-out in the user's config lifts it, as
does an allow with a specifier for a tool the extension lists in
`Lifts` (dax-coding's file tools: `read(.env)` opens `.env`). A
project's config is tighten-only: it may add ask and deny rules (no
allow, no carve-outs), and its source ranks below the others so it
cannot cancel their rules. `"builtin": false` drops the extensions'
allow rules and keeps their asks and denies, so a repository that sets
it loosens nothing.

`bash` has a subject splitter: the command is cut at unquoted `;`, `&`,
`|`, `&&`, `||` and newlines, a redirect to a file becomes a subject for
the write tool on the file it opens (normalised from the directory a
plain `cd` before it leads to, and from the root, and through the links
on its way, as a file tool's path is), and command or process
substitution adds a subject no rule matches. A line with such a redirect
that the policy lets run is stamped with those subjects and runs as typed
only while they are the same. Every subject must be allowed for the call to
be. This is a splitter, not a shell parser, and it errs toward asking.

## Security model

The model is untrusted input to a machine that can run commands, and so
is the repository it works in. dax's defences are layered and none is a
sandbox:

1. Ask by default; auto-allow only what is checked, as a safe subset
   rather than a list of bad syntax. A bash line is allowed only if it
   parses into stages joined by `&&` and `|` in a strict subset of the
   syntax and every stage passes a per-command read-only check (tool.Analyzer),
   with git's effective config read first; the splitter that reads
   anything else is for the question and for deny and ask rules, and never
   allows.
2. File tools go through the workspace; on this machine that is an
   `os.Root`, and a name that leaves is refused by the system, not by a
   string test. The policy's checks read the workspace the command runs
   in, not this machine, and a check that cannot follow links there
   asks.
3. A project config can only tighten; its source ranks below the user's,
   so it cannot cancel a user's rule, and the fields that send data or
   start programs are refused.
4. Files from the repository that go into the prompt or the settings
   (AGENTS.md, `.dax/skills`, `.dax/config.json`) are read through the
   workspace, whose file system refuses a link out, and screened there
   first, so a file that would be refused is left out and reported
   rather than failing the session; a config that would be refused is
   an error, since it only tightens.
5. Children get a scrubbed environment; resource use is bounded; the
   store is private; terminal output is cleaned.

What it does not protect against (no OS sandbox, approved commands run
unconfined, prompt injection can still ask, the user's own allow rules,
`-trust-skills`) is in the README's Security model section.

## Settings

Three layers, each overriding the one below: the user's
`~/.config/dax/config.json`, the project's `.dax/config.json`, read
through a workspace over the project, the flags. The files are strict
JSON: an unknown field is an error naming the file. A project file comes
from a repository, so it may only tighten the policy: it may not set the
provider, model, endpoint, instructions, skills, memory or MCP servers.
See the README for the schema.

## Fronts

A front is the human plane: it drives the session's `agent.Turn` and
uses `agent.Controls` for its commands and start lines. `front.go` is
where one is chosen. The REPL renders the agent's events and answers
permissions and questions on standard input; print (`-p`) is
`agent.Drive`, a controller whose rules ask on standard input; the
terminal client, agentconsole, is a view of the record that drives the
Turn through the glue in `tuiadapter.go`. The session asks nothing of a
front it was built with: every permission is a run that ended for input,
answered with `Turn.Answer`, and every question asked while a call runs
goes to whoever holds `Turn.Questions`, refused when nobody does.

## Workspaces

A `workspace.Workspace` is where a session's tools act: a root (the
working directory as the workspace's own processes see it), a file
system whose opens never block, `WriteFile` and `Remove`, the
environment its processes start with, `Exec` of one command to
completion with an optional stream of its output, a `Descriptor`
(kind, ref, root) and `Close`. A name that leaves it is an error that
is `ErrOutside`, and a name that is not `fs.ValidPath` is refused. The
interface is the agentworkspace module's, which began as dax's own
`workspace` package and which dax imports under that name; the
persistent shell stays out until a tool needs one. A workspace that
can run a process for longer than one call, with pipes to it, is also
a `workspace.Starter`. dax uses `workspace.Local`: this machine's
directory as an `os.Root`, each command in its own process group,
killed with it, and a `Starter`. A container or a remote runtime
implements the same interface; agentworkspace has none yet.

Equal means nothing in the session or in dax-coding asks which one it
is:

- `tool.Files` turns a path the model wrote, absolute in the root or
  relative to it, into the workspace's name, and holds the lock that
  keeps one write from landing between another's read and write. The
  file tools and an extension's tools go through it; `bash` goes
  through `Exec`.
- The policy's checks read the workspace the call acts in. A path
  matcher resolves links through the workspace's file system
  (`fs.ReadLinkFS`); the bash analyzer stats the paths a command names,
  follows their links and expands its globs there, and the git-config
  check finds `.git` and runs `git config` through `Exec`, passing paths
  as arguments. A workspace whose file system cannot read links leaves
  these checks unable to follow them, and they ask: a path subject no
  rule names, a bash stage that is not read-only.
- The project's instructions are read through the workspace. The
  session reads the AGENTS.md chain through its file system
  (`agentsmd.Options.FS`), screening each file there first; dax-skills
  offers `.dax/skills` as an `agentskill.Source` over it, screened by
  walking it there, every link required to lead inside `.dax/skills`
  (`tool.Files.Resolve`), since the skill tool runs unasked; the command line reads `.dax/config.json` through a
  `workspace.Local` over the project, or with `-executor` through the
  executor's workspace once it is connected. One exception reads this machine:
  when the workspace's descriptor says it is `Dir` on this machine and
  the session started below its repository's root, the AGENTS.md files
  between the two are outside the workspace and are read from `Dir`'s
  ancestors, a link that leaves `Dir` left out. A workspace elsewhere
  gives only its own files, and so does an executor's, whatever its
  descriptor says: an executor of kind `local` may be on another host.
- The session records the workspace: the header's `cwd`, the env
  entry's `cwd` and its `workspace` kind and ref come from the
  descriptor, so a resume into another workspace is recorded as a
  substitution. The model is told the root as the working directory.

`agent.Options.Workspace` is the caller's workspace, which the caller
closes; without one the session opens a `workspace.Local` over `Dir`,
with the scrubbed environment, and closes it. `Options.Store` is the
same for the record: an `agentsession.Store` the caller opened and
closes, or, without it, the content-addressed store at `Root`.

What is not equal yet, and why:

- A workspace whose root is below its repository's root sees no
  AGENTS.md above its root unless it is this machine's `Dir`: its file
  system ends at the root.
- An MCP stdio server runs in the workspace only when it is a
  `workspace.Starter`, over the process's pipes (the MCP SDK's
  `IOTransport`), at its root and with its environment; in a workspace
  that cannot start a process, it runs on this machine, with this
  machine's environment scrubbed.
- agentsession's RFC 0003 puts a store behind HTTP, and its client is
  meant for `Options.Store`, but it is not a drop-in for today's
  `Store`: opening takes a lease, an append returns a new result, and a
  lost lease is the harness's to handle. The seam takes today's
  interface; the rest waits for `agentsession/remote`. `Session.Path`
  and the store's admin functions (`-list`, `-verify`, `-gc`) are the
  local store's.
- A front driving a session elsewhere (agentconsole's backend over a
  wire, ACP) is not built.

## Extending

The session owns the model, the store, the policy engine, the workspace
and the scrubbed environment, the prompt frame, AGENTS.md, MCP servers
and compaction. Everything else is an `extension.Extension`, and dax's
own are built from the same fields a program's are:

| Extension | Package | Offers |
|---|---|---|
| dax-coding | `ext/coding` | read, write, edit, glob, grep, ls, bash; their rules, matchers, aliases, the bash stamp, the prompt's nudge, renderers |
| dax-agents | `ext/agents` | explore and task, from every extension's tools; the delegation guide |
| dax-skills | `ext/skills` | the skill tool and catalogue; grants under `-trust-skills` |
| dax-memory | `ext/memory` | the memory tools and block |

`dax.Main` builds dax-coding always and the others as the settings say,
less those `WithoutExtension` names, then the program's from
`WithExtension`.

A session builds its extensions in two phases. First every extension's
`Tools`, over an `extension.ToolEnv`: the session's `Workspace`, the
`tool.Files` over it that a path from the model goes through
(`ReadFile`, `WriteFile`, `Update`, `Stat`, `ReadDir`, `Rel`; `Update`
and `WriteFile` hold the lock dax-coding's `write` and `edit` hold),
and the read limit. A process runs through `Workspace.Exec` with
`Workspace.Env()`, the environment with credentials removed, a copy
each call. A tool is built over that rather than over the kit
(`agentkit.WithDeferredTools`) because what a dax tool needs is dax's,
which the kit does not hold. Then each extension's `Kit`, which
returns agentkit options (skills, memory, child agents, guards) over an
`extension.Env` that sees every extension's tools, so dax-agents gives
`explore` a read-only tool an extension listed after it adds. The
session's own options go after the extensions', so where an option
replaces, the session's is in force; a `Kit` that sets a policy on a
session whose policy is off is an error. One tool value is shared by
every agent that has it, possibly at once, so a tool that keeps state
guards it, as agenttool's contract asks.

An extension's `Tools` are execution; the tools its `Kit` adds
(memory, explore and task, skill) are control and run where the
session does. The session runs the execution through an executor
(`internal/executor`): today `InProcess`, every extension's `Tools`
built once over the `ToolEnv`, and later one served from a sandbox.
`dax execute` serves the same tools, built the same way over a
`workspace.Local` where it runs, through `executor.NewServer`: the
tools themselves go to agenttool's mcpserver, so their facts and
replay claims are answered and bash's and the file tools' stamps are
made and checked in that process, under its key, and a stamp made
elsewhere is refused. What MCP cannot carry (the descriptor, the
extensions, the in-process order, each tool's extension, `ReadOnly`
and strictness) is in the experimental capability
`io.github.christopherdavenport.dax/executor`. It reads no config and
runs no policy; the pipe that started it is its one client. A
session reaches it through `executor.Remote` (`agent.DialExecutor`,
`-executor`), which takes the claims (mcpclient's `WithClaims`) and
refuses a server that would not give them: one without the facts
method or the capability, of another version, listing a tool the
capability does not name or the reverse, or saying a tool claims facts
that is listed without the claim. A client that took such tools would
decide their calls on the model's arguments, a wider policy than the
user's. The session's workspace is then a view of the executor's:
its root and descriptor, nothing to write or run with, and its files,
read-only, which the executor serves as MCP resources under
`dax-workspace:///{op}{?path}` (stat, lstat, readdir, readlink, read)
through its workspace's confined file system, a read bounded at 1 MiB
and a listing at 10,000 entries, each request within 30 seconds. The
reply carries a refusal's kind, so the view's errors are the ones a
local workspace gives (`fs.ErrNotExist`, `workspace.ErrOutside`,
`fs.ErrInvalid`, `fs.ErrPermission`), and the readers of the project's
files (the AGENTS.md screening and chain, dax-skills' screening and
source, `config.LoadProject`) run over it unchanged. The capability
names the template; a client refuses an executor without it rather
than run with the project's config unread. An executor whose files
cannot be read when the session starts fails it, and a project config
that cannot be read fails the command line's start, since it only
tightens; a single AGENTS.md or skills directory that cannot be read
is left out and reported. Each
extension with Tools must be one the executor runs, a matcher may not
give a served tool's subjects (they would read this machine), and a
reading of the facts that fails, takes longer than 30 seconds or finds
the executor gone blocks the call. What the kit, the policy and `Env.Tools` hold are adapters with the
tool's definition, scheduling and annotations, whose calls, facts
claims and replay claims go to the executor; a request carries the
same bytes either way. The executor owns the tools and the session
closes it. Each decision about a call pins the call's facts (`Set.Pin`):
the main agent's for the length of its `BeforeToolCall`, a sub-agent's
from its verdict to the hooks' rewrite, so every reading in between is
one reading, made under the call's context, and the stamp a call runs
with is of the facts its verdict was decided on. The rewrite's own
arguments are another call, read afresh when the engine decides them.
agentpolicy passes the policy's subjects the context of the decision
that reads them, so a reading no decision has pinned, a sibling the
batch hold reads, is made under the held decision's context, and a
cancelled decision fails its reading, which blocks the call.

Each claim is checked at start, across the session and without regard
to case: an extension's name, its tools, the names it `Owns` (tools its
kit options add, such as `skill`) and its aliases are unique; `mcp__` is
an MCP server's; an alias may differ from its own tool only in case
(`Bash` and `bash`); a `ReadOnly` tool, a matcher, an alias target and a
`Lifts` entry name only the extension's own tools; a read-only tool
annotated destructive is refused. A tool's facts claim
(`agenttool.Factual`), what a call would touch, may name only its
extension's tools: a call it names of another's is decided as one no rule names, so it asks. `BeforeToolCall` hooks fold with the
policy, deny over ask over allow, for the main agent and, through
`Env.ChildPolicy`, every sub-agent; with the policy off they are not
run. `extension.Renderers` merges the extensions' renderers for the
terminal client; two that draw one tool are an error, and a renderer
declines a call it was not written for, such as one an older version
recorded.

The rules above hold for every extension: its tools are chosen before a
front runs (rule 4), its calls, results and verdicts are recorded and
name it (rule 5), and it fails toward asking (rule 8). A program that
wants a front of its own builds the session with `agent.New` and the
same `Options.Extensions`. A program that wants to supply its own
agentkit kit is not supported: the session's guarantees (one policy over
every source, the confined workspace, the name checks, the record's
header) are its to keep, and the layer below is agentkit itself. A
function that returns the options the session would pass the kit, for a
program to build on and own, could come later. `config`, `provider`,
`modelinfo`, `prompt`, `render`, `private` and `executor` stay internal
(the workspace is agentworkspace's, public there, as an extension's
tools need it): they are
the command line's and the session's, and `modelinfo` is a trial meant to
move to openresponses.

## Not built

Compaction beyond what `agentturn/compact` does (a local summary or a
server's endpoint, above a token budget); branch summaries and a session
tree to navigate; an RPC mode and ACP for editors; a container or
remote workspace (agentworkspace has the interface and `Local`, not
those);
prompt templates
(`/name args`); a model catalogue with context windows; extension points
for slash commands, the REPL's renderer, or the config's schema. Each
would be a front or an assembly option, not a change to the rules above.
