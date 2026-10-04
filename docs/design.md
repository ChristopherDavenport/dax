# Design

dex is a coding agent built as one session that owns the conversation's
state, a loop that emits fine-grained events, and fronts that are thin
subscribers. There is no runtime extension system. What the model can
do is a fixed set of built-in tools plus MCP servers the user
configures.

Most of the machinery is not in this repository. dex is the product that
assembles sibling libraries, each independently versioned, and keeps
only what is specific to a coding agent. This document says what that is
and the rules the assembly keeps.

## Who owns what

| Concern | Owner | dex's part |
|---|---|---|
| Wire types, the provider client | `openresponses` and its `providers/anthropic`, `providers/gemini` | `internal/provider` picks one from the config |
| The agent loop, events, steering, follow-ups, retry | `agentturn` | `internal/agent` configures and drives it |
| Tool contract, MCP client | `agenttool` | `internal/tool`: read, write, edit, glob, grep, ls, bash |
| Assembling a loop from parts | `agentkit` | `internal/agent.open` is one `agentkit.New` call |
| Allow, ask, deny | `agentpolicy` | `internal/policy`: the default rules, the bash splitter's use |
| The session record | `agentsession` | the store, `-list`, `-verify`, `-resume`, `-gc` |
| AGENTS.md, skills, memory | `agentsmd`, `agentskill`, `agentmemory` | directories and budgets |
| Settings | dex | `internal/config` |
| Presentation | dex (REPL) | `cmd/dex/front.go`, `internal/render` |

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
   newer and older dex alike; the format's own versioning says when a
   reader is too old, and dex says so on `-resume`.
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
file. That is dex's whole concurrency policy.

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

dex's part of the system prompt is a role line, a nudge toward glob, grep
and ls over shell commands, the user's `instructions_file`, and the
working directory. `agentkit` renders and joins the rest in a fixed
order: the skill catalogue, the memory block, then the AGENTS.md chain
(`~/.dex/AGENTS.md`, then every `AGENTS.md` from `/` down to the working
directory, nearest last). What a layer left out, a file over the budget
or a skill that would not load, is printed at start as `omitted:`.

## Tools

`read`, `write`, `edit`, `glob`, `grep` and `ls` go through a
`tool.Workspace`, an `os.Root` over the working directory. A path that is
absolute outside it, climbs out with `..`, or reaches out through a
symbolic link is refused with the same error, because the root refuses
the name before the file system follows it. `bash` runs in the directory
but is not confined; a shell reaches whatever the user does, and the
policy is what stands in front of it.

`glob` and `grep` skip `.git`, `node_modules`, `vendor` and a few other
trees by name. There is no `.gitignore` reader. A path named explicitly
as the search root is searched even if it is one of those.

## Policy

Rules are agentpolicy's: a tool name or `tool(specifier)`. Precedence is
deny, then ask, then allow, then the default. The shipped allow list is
the read-only tools and a few commands that only look; the default asks.
Rules in the user's config add to it. A project's config is tighten-only:
it may add ask and deny rules (no allow, no carve-outs), and its source
ranks below the user's so it cannot cancel their rules.

`bash` has a subject splitter: the command is cut at unquoted `;`, `&`,
`|`, `&&`, `||` and newlines, a redirect to a file becomes a subject for
the write tool on the target, and command or process substitution adds a
subject no rule matches. Every subject must be allowed for the call to
be. This is a splitter, not a shell parser, and it errs toward asking.

## Security model

The model is untrusted input to a machine that can run commands, and so
is the repository it works in. dex's defences are layered and none is a
sandbox:

1. Ask by default; auto-allow only what is checked, as a safe subset
   rather than a list of bad syntax. A bash line is allowed only if it
   parses into stages joined by `&&` and `|` in a strict subset of the
   syntax and every stage passes a per-command read-only check (tool.Analyzer),
   with git's effective config read first; the splitter that reads
   anything else is for the question and for deny and ask rules, and never
   allows.
2. File tools go through an `os.Root`; a name that leaves is refused by
   the system, not by a string test.
3. A project config can only tighten; its source ranks below the user's,
   so it cannot cancel a user's rule, and the fields that send data or
   start programs are refused.
4. Files from the repository that go into the prompt are screened for
   links out of the workspace.
5. Children get a scrubbed environment; resource use is bounded; the
   store is private; terminal output is cleaned.

What it does not protect against (no OS sandbox, approved commands run
unconfined, prompt injection can still ask, the user's own allow rules,
`-trust-skills`) is in the README's Security model section.

## Settings

Three layers, each overriding the one below: the user's
`~/.config/dex/config.json`, the project's `.dex/config.json`, the
flags. The files are strict JSON: an unknown field is an error naming
the file. A project file comes from a repository, so it may only tighten
the policy: it may not set the provider, model, endpoint, instructions,
skills, memory or MCP servers. See the README for the schema.

## Fronts

A front implements `Hooks()` (how a question reaches the user) and
`Run(ctx, session)`. `cmd/dex/front.go` is where one is chosen. Today
there is the REPL and print (`-p`). A terminal UI is a third front that
subscribes to the same events and answers the same two hooks.

## Not built

Compaction beyond what `agentturn/compact` does (a local summary or a
server's endpoint, above a token budget); branch summaries and a session
tree to navigate; an RPC mode and ACP for editors; prompt templates
(`/name args`); a model catalogue with context windows. Each would be a
front or an assembly option, not a change to the rules above.
