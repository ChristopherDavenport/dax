# Changelog

All user-visible changes to dax. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break flags and the config file.

## Unreleased

- Added: `dax execute`, the tools served over MCP on standard input
  and output from inside the place they should act, for a session
  elsewhere to drive: `docker exec -i box dax execute -root /work`,
  `ssh host dax execute`, `kubectl exec -i ...`. The pipe is the
  credential, so there is no listener and no token. It serves the
  tools of every extension with `Tools` (dax-coding, and a program's
  own as `<name> execute`), the real tools, so their facts claims,
  replay claims and stamps are made and checked there, under that
  process's key: a stamp minted anywhere else is refused. Besides
  agenttool's facts method and `_meta`, it says under the experimental
  capability `io.github.christopherdavenport.dax/executor` what MCP
  cannot carry: the workspace's descriptor (`-kind`, `-ref`, the
  root), the extensions, and each tool in the order it runs in process
  with its extension, read-only, facts and strict. It reads no config
  file (the sandbox holds those files, and the policy is the
  session's): `-root`, `-max-read-bytes` and `-pass-env` are flags.
  Standard output carries MCP alone; while it serves, `os.Stdout` is
  standard error.
- Added: `-executor 'command ...'` and the user config's
  `"executor": {"command": "..."}` (refused in a project's file): the
  session starts that command, which runs `dax execute` where the tools
  are to act (`docker exec -i box dax execute -root /work`), and runs
  every extension's tools there over its standard input and output.
  Each call is still decided here, under this session's policy, on the
  facts the executor reads; a reading that fails, takes longer than 30
  seconds or finds the executor gone blocks the call. The command gets
  this machine's environment less its credentials, the model's key's
  variable among them. A program that does not give the claims (no
  facts method, no executor capability, another version, tools the
  capability does not name, or a tool whose facts claim is missing) is
  refused, as is an extension with tools the executor does not run.
  Not yet: MCP servers (a session with `-executor` and any MCP server,
  from the config, `-mcp` or `/mcp add`, refuses to start), the
  project's files (AGENTS.md, `.dax/skills`, `.dax/config.json` are not
  read, and an `omitted:` line says so), and an `http:`, `https:` or
  `unix:` address (refused). The banner names the executor and its
  workspace, and the model, the record and explore are told its root.
- Added: `agent.Executor`, `agent.DialExecutor` and
  `agent.Options.Executor`, so a program built on dax can run its
  session's tools in `dax execute` as `-executor` does. Setting both
  `Options.Executor` and `Options.Workspace` is an error.
- Changed: the explore sub-agent's instructions name the workspace's
  root, where its tools act, rather than the directory dax started in
  (`extension.Env.Dir`); the two differ in a container or with
  `-executor`.
- Dependencies: agenttool/mcpserver v0.0.20, for `dax execute`.
- Security: an auto-allowed bash line runs only on the facts it was
  decided on, as a file tool's call does. Its stamp signed the plan
  alone, which is text, so `cat notes.txt`, decided as a read of
  `notes.txt`, still ran and printed `.env` to the model once
  `notes.txt` became a link to `.env` inside the workspace: made by a
  `ln -sf .env notes.txt` the person approved in the same batch while
  `cat` was held beside it, by another process, or while a rule's
  question about the line was open; a `cd` into a directory that
  became a link did the same. The stamp now signs the plan and the
  claim's calls (each stage, what it reads with the links on its way
  followed, the subjects no rule names) from the one analysis the
  claim makes, and the tool analyses the line again and refuses it
  with "ask again" if either differs. A line outside the analysis that
  a person approves runs as written, as before. Affects v0.0.5 and
  earlier (#45).
- Changed: package `workspace` is removed; dax runs over the
  agentworkspace module (`github.com/ChristopherDavenport/agentworkspace`),
  which began as that package. The names are the same (`Workspace`,
  `Local`, `NewLocal`, `Command`, `Output`, `Descriptor`, the `Kind*`
  constants, `ErrOutside`, `Read`), so a program built on dax changes
  its import to `workspace "github.com/ChristopherDavenport/agentworkspace"`
  and nothing else. dax keeps no alias package: a program that writes
  an extension already imports the siblings dax's API names
  (agenttool, agentpolicy), and a container or remote workspace it
  passes in comes from agentworkspace too, so a second name for the
  same types would only be one more thing to keep in step. What
  differs from dax's package: every file operation and `WriteFile` and
  `Remove` refuse a name that is not `fs.ValidPath` (absolute or
  climbing out is `ErrOutside` everywhere, any other is
  `fs.ErrInvalid`); a `Command.Dir` is checked through the root, so one
  that leads out through a link is `ErrOutside` and one that is not a
  directory is an error; `Local.Close` ends the processes it started.
  `tool.Files` hands the workspace cleaned names, so dax's tools and
  every decision are as before.
- Changed: an MCP stdio server starts in the session's workspace when
  the workspace can start a long-lived process (`workspace.Starter`,
  which `Local` is), at its root and with its environment, over the
  process's pipes; it ends when the session closes it, or with the
  workspace. For the `Local` the session opens itself that is the same
  scrubbed environment as before, so only its working directory moves,
  from dax's to the workspace's root. In a workspace that cannot start
  a process, a server runs on this machine as before.
  `agent.Options.MCP`'s and `PassEnv`'s comments say so.
- Dependencies: agentworkspace v0.0.1.
- Dependencies: agentpolicy v0.0.12, up from v0.0.11, whose `Subjects`
  takes the decision's context, and whose batch hold now holds a call
  beside a sibling it could not read and reads that sibling again
  later in the batch, where it read as blocked (a failing splitter) or
  kept a failed hook's reading; agentkit v0.0.9, up from v0.0.8, the
  release that requires it. agenttool and agenttool/mcpclient v0.0.20,
  up from v0.0.19: dax does not pass mcpclient's `WithClaims()`, so no
  MCP server is asked for facts and its tools claim none, as before;
  a server built on agenttool's `mcpserver` v0.0.20 that marks a tool
  sequential or names its resource in the listing's `_meta` now has
  that tool run one call at a time, which only orders calls. A server
  that does not carry agenttool's `_meta` entry is unchanged.
- Changed: `tool.BashSubjects` and `tool.PathSubjects` return
  agentpolicy v0.0.12's `Subjects`, which takes a `context.Context`
  first. Bash's analysis, the git-config check it runs through the
  workspace's `Exec` included, runs under that context, the
  decision's, where it ran under `context.Background()`; a path's
  reading takes no context. A program that calls either splitter
  directly passes a context.
- Changed: the policy asks a tool's facts claim under the context of
  the decision that reads it (`factspolicy.Subjects`), where it asked
  under `context.Background()`. A claim that a cancelled decision cuts
  off fails, which blocks the call, as any failed claim does. The
  session's own tools are read once per decision under the pin's
  context as before (`internal/executor`'s `Set.Pin`); a call no
  decision has pinned, a sibling the batch hold reads, is read under
  the held decision's context.

## v0.0.5 - 2026-10-09

- Changed: an extension's `Tools` are the session's execution and run
  through its executor (`internal/executor`), which today builds them
  in this process as before; the tools an extension adds through `Kit`
  stay with control. What the kit, the policy and `Env.Tools` and
  `Env.ReadOnlyTools` hold are now adapters that stand in for an
  extension's tools: the same definition, scheduling, annotations,
  facts claim and replay claim, with calls, claims and closing going
  to the executor, but not the extension's own values, and not an
  `io.Closer`. Requests carry the same bytes, and every decision is the
  same. `Extension.Tools`'s comment says so.
- Fixed: a sub-agent's call could run with a stamp of facts its verdict
  was not decided on. The sub-agents' policy check read a call's facts
  claim three times (the policy's subjects, its folded hook, then the
  hook again for the arguments to run) and took the rewrite from the
  last, without deciding it again, so a path or a claim that changed
  between the second and third readings was stamped and run. Each
  decision now pins its call's facts, so every reading in it is one
  reading, made under the call's context; the main agent's decision
  reads them once too.

- Changed: package `facts` is removed in favour of agenttool's facts
  claim (v0.0.19), in its names: `facts.Call`, `facts.Facts`,
  `facts.Claimer`, `facts.Of` and `facts.Claims` are
  `agenttool.FactCall`, `agenttool.Facts`, `agenttool.Factual`,
  `agenttool.FactsOf` and `agenttool.IsFactual`, and `facts.With(t, fn)`
  is the `agenttool.WithFacts(fn)` option of `agenttool.New` and
  `NewFunc`; a tool built otherwise claims by having the `Facts`
  method. `agenttool.Wrap` forwards the claim, so nothing looks through
  wrappers any more. `facts/factspolicy`, the policy's side, keeps its
  path and API, and every decision is the same.
- Dependencies: agenttool and agenttool/mcpclient v0.0.19, for the
  facts claim.
- Changed: AGENTS.md, the project's skills (`.dax/skills`) and its
  config (`.dax/config.json`) are read through the session's workspace,
  as the tools read the project, and not from this machine's directory.
  A container or a remote workspace now gives its own instructions,
  skills and config, and nothing from the directory dax started in.
  For a session on this machine the files read and the files refused
  are the same, with the exceptions below.
- Changed: the AGENTS.md chain follows the convention
  (https://agents.md): it runs from the repository's root, the nearest
  directory at or above the start that holds a `.git` (a worktree's
  `.git` file counts), down to where dax starts, and never above the
  repository. A file in a directory above the repository, such as a
  parent directory that holds several checkouts, is no longer read.
  With no repository only the start directory's `AGENTS.md` is read.
  `~/.dax/AGENTS.md` is read first, as before.
- Changed: in the system prompt, a project `AGENTS.md` is named by its
  path in the workspace (`<project_instructions path="AGENTS.md">`)
  rather than its absolute path on this machine, since agentkit renders
  the chain as the workspace names it; the prompt's working directory
  line gives the root. The user's file, and the files between the
  repository's root and a start below it, keep their absolute paths.
  The `omitted:` lines still name a file by its absolute path.
- Changed (security): screening is now the workspace's confinement, not
  a walk of this machine's directory. A project `AGENTS.md` or
  `.dax/skills` that is a link out of the workspace, or to nothing, is
  still left out and reported; an `AGENTS.md` that is a FIFO or a
  device is now left out too, where reading it would have waited. A
  `.dax/config.json` that is a link out of the workspace, a FIFO, a
  device or over 1 MiB is now an error naming the file, where it was
  read: the file only tightens, so leaving it out would loosen the
  policy.
- Security: a `.dax/skills` that holds a symbolic link leading outside
  it is left out whole and reported ("holds a symbolic link outside the
  skills directory"), even when the link stays in the workspace:
  `.dax/skills/x/notes.md -> ../../../.env` would otherwise let the
  skill tool, which runs unasked, read `.env` without the question
  `read(.env)` gets. Links are followed through the workspace, chains,
  relative and absolute; a link between two skills is still read. On
  a workspace whose file system cannot read links, a skills directory
  with any link is refused. On main a skill with such a link was
  offered and the skill tool refused the one file.
- Added: `tool.Files.Resolve`, where a path leads with its links
  followed through the workspace (`ErrOutside` out of it,
  `tool.ErrLinksUnknown` where the workspace cannot say), for an
  extension that offers a project's files through a tool of its own.
- Changed: a skill in `.dax/skills` is listed at the workspace's root
  joined with `.dax/skills`, and is never trusted under
  `-trust-skills`, whatever this machine's paths say. The project's
  skills still shadow the user's, which shadow `skills_dirs`.
- Added: `agents_md_global` in your config, and `-agents-md-global`,
  your own instruction files for every session whatever the repository
  holds, such as an `AGENTS.md` above your checkouts that the chain no
  longer reaches. They are read on this machine after
  `~/.dax/AGENTS.md` and before the repository's chain, in order, a
  missing one skipped and none screened, as your own files; a container
  session reads them too. A relative path in the config is relative to
  the file, `~` is your home directory; the flag is split on `:` as
  `PATH` is, replaces the config's list, and `""` clears it.
  `-agents-md=false` turns them off with the rest. A project's
  `.dax/config.json` may not set it. `agent.Options.AgentsMDGlobal`
  carries them for a program built on dax.
- Dependencies: agentkit v0.0.8 and agentsmd v0.0.3, for
  `agentsmd.Options.FS`, through which the AGENTS.md chain is read.
- Security: golang.org/x/net is v0.60.0, which fixes GO-2026-6617 and
  GO-2026-6612 in the HTTP/2 code the provider clients use.
- Added: the facts claim (`agenttool.Factual` since agenttool
  v0.0.19), a tool's claim of what a call would touch, said before it
  runs: the calls it amounts to (`read` of what `cat` reads, `write` of
  a redirect's target) and the arguments it runs with if allowed (bash's
  stamped plan). dax-coding's tools make it; the session takes the
  policy's subjects from claims and applies a claim's rewrite as one
  hook folded under the policy's verdict, so the policy reads no machine
  but through the tools. An extension's matcher for a claiming tool
  supplies `Match` only; one that brings `Subjects` too is refused at
  start. A claim that fails blocks the call.
  dax-coding ships no `BeforeToolCall` and no subjects of its own.
- Security: a claim may name only its own extension's tools (bash's
  claims of `read` and `write` are dax-coding's). A call that names
  another extension's tool, an extension's `upload` claiming to be a
  `read` of README.md, is decided as a call no rule names, so it asks,
  and never runs on another extension's allow rule.
- Security: a file tool's call (read, write, edit, glob, grep, ls) runs
  only on the facts it was allowed on, whether the policy allowed it or
  a person approved it, in the main agent or a sub-agent. Its arguments
  carry a stamp of them in `dax_stamp`, a field each tool's schema marks
  "Set by dax; leave it out", as bash's does, and a path that became a
  link elsewhere in the workspace between the decision and the call, a
  `notes.txt` turned into a link to `.env`, is refused with "ask again".
  A stamp the model writes is replaced by dax's or taken off before the
  call runs, and refused by the tool if one ever reaches it.
- Security: a call whose arguments name a field in another case is
  refused, by the policy and again by the tool. The policy read the
  exact key and the tool decodes any case, the last such key winning,
  so the two could disagree on what the call touched: on main (and in
  v0.0.4), `read {"path":"notes.txt","Path":".env"}` was allowed unasked
  as a read of notes.txt and returned `.env`, and an approved
  `write {"path":"hello.txt","content":"x","Path":"victim.txt"}` wrote
  victim.txt. dax-coding's tools refuse any of their fields in another
  case; the claims, the fields they read (`path`, `command`,
  `dax_stamp`).
- Changed: the session has one agent and exposes it as `agent.Turn`, its
  human or autonomous plane: `Prompt`, `Answer` (the policy engine's
  release, then the loop's resume), `Permissions`, `Steer`, `FollowUp`,
  `Abort`, `State`, `Subscribe`, and `Questions` and `Reply` for what a
  sub-agent's call or a tool asks while it runs. `Options.Approve`,
  `Ask`, `Elicit` and `NoAgent`, and `Session.Agent` (a field),
  `Prompt(ctx, text)`, `Steer(text)`, `FollowUp(text)`, `Pending` and
  `TUIConfig` are removed. Every front drives the Turn; the terminal
  client reaches it through glue over agentconsole's backend.
- Added: `agent.Controls` (model, reasoning, MCP servers, `Info`),
  `agent.Drive`, a controller that answers by rule (`-p` is one), and
  `Session.Record`. A session with no store is written to memory, so
  every front can follow its record; `Info.Recorded` says whether it is
  kept.
- Changed: the policy's subject ("about: ...") is on every front's
  question, not only the terminal client's.
- Added: dax is a minimal core that can be extended. The session (model,
  store, policy, workspace, AGENTS.md, MCP servers, compaction) knows no
  tool; everything the model can do comes from an `extension.Extension`,
  and dax's own are extensions like any other: `dax-coding` (the file
  tools and bash), `dax-agents` (explore and task), `dax-skills` and
  `dax-memory`, in `ext/`. A Go program runs `dax.Main` with
  `WithExtension` to add its own (tools, read-only tools for explore,
  matchers, aliases, policy rules, a BeforeToolCall hook, prompt text,
  agentkit options, renderers), `WithoutExtension` to leave one of dax's
  out, and `WithName` to name itself. Tool, alias and owned names are
  checked across the session without regard to case; `mcp__` is MCP's.
  See the README's Building on dax.
- Added: the public packages `dax`, `agent`, `extension`, `policy`,
  `tool`, `toolrender`, `workspace` and `ext/coding`, `ext/agents`,
  `ext/skills`, `ext/memory`, for a program built on dax or a front of
  its own. `tool.Files` is the tools' view of the session's workspace
  for paths the model writes: `ReadFile`, `WriteFile`, `Update`, `Stat`,
  `ReadDir` and `Rel`, confined as dax's file tools are; `Update` and
  `WriteFile` hold the lock `write` and `edit` hold. The API is pre-1.0
  and may change in a minor version.
- Added: where the tools act is a value. `workspace.Workspace` is the
  interface every tool and every check of the policy goes through (a
  root, a file system that never blocks, writes, an environment, `Exec`
  with streamed output, a descriptor), shaped after the agentworkspace
  study so dax can move to that module by a rename; `workspace.Local`,
  this machine's directory over an `os.Root`, is one implementation.
  dax-coding's tools act through it, `bash` through `Exec`, and the
  path matchers, the bash analyzer and the git-config check read the
  workspace the call acts in; one whose file system cannot read links
  makes them ask. `agent.Options.Workspace` takes a workspace and
  `agent.Options.Store` an `agentsession.Store`, each the caller's to
  close, for a container, a remote runtime or a remote store (RFC 0003)
  once one exists; without them the session opens a local workspace
  over `Dir` and the store at `Root`, as before. dax ships no container
  or remote workspace yet. AGENTS.md, project skills and the project
  config are still read from `Dir` on this machine, and MCP servers run
  here.
- Changed: the session header's and the env entry's `cwd`, the env
  entry's workspace kind and ref, and the working directory the model is
  told come from the session's workspace. `Options.Dir` is the directory
  on this machine instructions are read from. `extension.ToolEnv` is
  `{Workspace, Files, MaxReadBytes}`: a tool's processes run through
  `Workspace.Exec` with `Workspace.Env()`, a copy each call. An
  extension's `Matchers` and `BeforeToolCall` are built over that
  `ToolEnv`, as its tools are, so the policy inspects the workspace the
  tools act in and no other (`extension.FixedMatchers` and
  `extension.FixedHook` wrap ones that need nothing of the session);
  `coding.New` takes only `maxRead`. `tool.Workspace` is replaced by
  `tool.Files`, and the tool constructors and `tool.Analyzer` take a
  `*Files`.
- Changed: each extension's policy rules are a source of their own,
  `extension:<name>`, which the record of every verdict one decides
  names; dax's own rules are recorded as `extension:dax-coding` (and
  `extension:dax-agents`, `-skills`, `-memory`), no longer `dax:builtin`.
  A shipped rule may name only its extension's tools and aliases, and
  may not be a pattern or a carve-out. The start line's policy summary
  counts each extension's rules.
- Changed (security): `"builtin": false` drops the shipped allow rules
  only; the shipped asks and denies, the secret-path asks among them,
  stay. Before, a repository's `.dax/config.json` could set it and drop
  the secret-path asks along with the allow list. Lift a shipped ask
  with a carve-out in your config, or for the file tools an allow that
  names the path.
- Changed: the write tool no longer blocks on a FIFO or device; a target
  that is not a regular file is refused.
- Fixed (security): a bash glob through a link out of the workspace
  (`ls out/*`, `out` a link to a directory outside) matched nothing, so
  the line passed the read-only check and ran unasked while bash
  followed the link. The analyzer now refuses a glob whose expansion
  cannot read a directory, and the line asks.
- Changed: with sub-agents on, the main agent's system prompt guides it
  to keep its own context for decisions: broad searches go to `explore`,
  changes it can brief completely go to `task`, and it works directly
  when it knows the file or the change is small. It checks a sub-agent's
  report before relying on it and passes the findings on in its reply.
  Sub-agents, and sessions with `agents` off, are not given the guide.
- Added: `api_key_command` and `-api-key-command`, a program whose output
  is the provider's key, in place of its environment variable. dax runs
  it at start, again once the key is five minutes old, and again when
  the server answers 401 or 403, retrying the refused request once with
  the new key, so a key that expires is replaced without a restart. The
  key stays in memory and out of every environment dax starts. For every
  provider that takes a key; a project file may not set it.
- Added: when the key command fails or the server refuses even a fresh
  key, the turn ends with an error that starts `authentication failed`
  and carries the command's last line of standard error. It is not
  retried, and the command is not run again for a few seconds. The
  terminal client keeps the command's standard error for the error
  instead of drawing it over the screen. `api_key_login` and
  `-api-key-login` add how to sign in again to that error.
- Added: `session_header` and `client_header`, with `-session-header`
  and `-client-header`, for a server that groups calls by session or
  records which client called. Each model call carries the ID of the
  session it is recorded in under the first, a sub-agent's call its own
  session's, and `dax/<version>` under the second. Not for `vertex`; a
  project file may not set them.

## v0.0.4 - 2026-10-06

- Changed: agentconsole v0.0.9. The terminal client labels the
  conversation by role: the model's messages "agent", not "assistant",
  and yours "user", not "you". The tree's branches and the detail pane
  say "agent" too.

## v0.0.3 - 2026-10-06

- Changed: the terminal client draws each of dax's tool calls as what it
  does instead of its arguments as key=value pairs: an edit as its path
  and `+3 −1` with the change as a diff under it, a command as `$` and
  the command with a failure's exit status and last lines under it, a
  read as its path and line range, a search as its pattern with the
  matches counted, a sub-agent as its brief with the start of its report.
  A call waiting on a permission shows what it would do. Ctrl-O shows an
  edit's whole diff, a write's content and a command's whole output.
- Changed: agentconsole v0.0.8. The run line's spinner turns in its dot
  rather than at the right edge, as does each tool call's in motion, and
  while a run goes the line says what it is doing after its figures:
  waiting, thinking, writing, calling or running a tool, retrying. A
  steer typed during a run is listed over the input as queued until the
  run takes it, and one queued before an abort and a quit is taken by
  the next run on resume rather than dropped. Shift-Enter, Alt-Enter or
  Ctrl-J puts a new line in the input. dax hands the client no tool
  renderers yet, so calls are drawn as before.
- Changed: the terminal client leaves only the command that resumes the
  session when it exits, `To resume this session: dax -resume <id>`. The
  start lines (model, session, policy, tools, omissions) and what dax
  noted during the run (skill grants, compactions, denied calls) are
  printed only with the new `-v` flag. Warnings are still printed, and
  waited on, before the client takes the screen.

## v0.0.2 - 2026-10-06

- Added: the `vertex` provider, Claude and Gemini on Google Vertex AI.
  Each request goes to the Anthropic adapter for a `claude-` model and
  the Gemini adapter for a `gemini-` model, so the main agent and the
  sub-agents can run different families. It authenticates with
  Application Default Credentials (`gcloud auth application-default
  login`) and takes the project from `GOOGLE_CLOUD_PROJECT`, else the
  credentials' project, and the location from `GOOGLE_CLOUD_LOCATION`
  or `GOOGLE_CLOUD_REGION`. Vertex publishes no model capabilities, so
  the reasoning effort is fitted from the model ID's generation.
- Changed: agentconsole v0.0.7. The terminal client renders the
  assistant's messages as markdown, shows the run's state, elapsed time
  and tokens on a run line above the input, leads the status line with
  the session's time working, and keeps a mouse selection on the text it
  covers as the conversation scrolls; a click places the input's cursor
  or selects a tree item. It moves to Bubble Tea v2, so Go 1.26 is now
  the floor to build dax.
- Changed: agenttool and agenttool/mcpclient v0.0.16. The new `cli`
  package is not used, and nothing dax uses changes.

## v0.0.1 - 2026-10-04

- Fixed: `make release` can cut the first release. With no plain vX.Y.Z
  tag published, the release guard takes any vX.Y.Z as the first one
  instead of refusing for want of a version floor; it also refuses
  prereleases and a major version the module path does not carry, and
  `make check` runs its tests.
- Changed: the project is named dax. The module is
  `github.com/ChristopherDavenport/dax`, the binary `dax`
  (`go install github.com/ChristopherDavenport/dax/cmd/dax@latest`), and
  its state moves with it: `~/.dax` (AGENTS.md, skills, memory,
  sessions), `~/.config/dax` (config.json, prices.json, instructions)
  and a project's `.dax/`. The old paths are not read; move them once.
- Added: the MIT license.
- Changed: CI runs on pushes to main and on pull requests. agentconsole
  is public, so a runner fetches it without a token, and the workflow no
  longer sets `GOPRIVATE`.
- Added: a running `bash` command reports its output as progress while
  it goes — the terminal client shows the lines under the call's row,
  and the REPL prints them as they arrive — instead of everything
  appearing when the command ends. The result the model sees is
  unchanged, and progress is not recorded.
- Changed: in the terminal client a function call's row is one line
  while it is collapsed: its arguments as key=value pairs instead of
  raw JSON, and, once it has ended, a hint of how many lines its output
  holds instead of the first of them; Ctrl-O or a second click shows
  the whole call as before. A running call shows the last five lines of
  its progress, not only the first. The session pane refreshes when a
  run ends, which it did not always do. This is agentconsole v0.0.6's.
- Dependencies: agentconsole v0.0.6, up from v0.0.5, for the one-line
  call rows and the running call's last lines.
- Changed: thinking streams as it happens, as assistant text always did.
  Against a server that follows OpenAI's Responses API (OpenRouter among
  them) the reasoning deltas arrive under OpenAI's event names, which
  decoded to nothing, so thinking showed once, whole, at completion.
- Dependencies: openresponses v0.0.15, up from v0.0.14, which decodes
  OpenAI's reasoning event names; providers/anthropic and
  providers/gemini at v0.0.15, the release that requires that root.
- Added: `-effort` and the config's `effort`, the reasoning effort
  `-think` asks for (`minimal`, `low`, `medium`, `high` or `xhigh`);
  `low`, as before, by default. It is fitted to each model like the
  default was, and a project file may not set it.
- Added: a mouse drag in the terminal client selects text anywhere on
  the screen (the conversation, the panes, the top bar) and the
  selection stays drawn until the next key or press; Ctrl-C copies it
  to the terminal's clipboard with OSC 52, over ssh too, as a desktop's
  copy does, and drops the selection so the next Ctrl-C is the abort or
  quit it always was. A click with no drag still selects the row under
  it. This is agentconsole v0.0.5's.
- Dependencies: agentconsole v0.0.5, up from v0.0.4, for the drag
  selection and the ctrl+c copy.
- Added: `-pricing-file` and the config's `pricing_file` load model
  prices. The terminal client's status line shows a running total of
  token usage and, with a price file, the session's cost, and the
  session pane shows the usage split by model. Without a price file it
  shows usage but no cost.
- Changed: the terminal client's prompt wraps over up to five lines,
  then scrolls, instead of scrolling sideways one line at a time; pasted
  newlines stay newlines.
- Fixed: in the terminal client, a sub-agent's call that the policy asks
  about is asked on screen while the run goes, y or n with an optional
  reason that reaches the sub-agent; it was refused, with a note telling
  the main agent to make the call itself. A tool's yes-or-no question
  (MCP elicitation) is asked the same way; a form or a page to visit is
  still cancelled.
- Dependencies: agentconsole v0.0.3, up from v0.0.2, for
  `client.Question` and `native.Backend.Ask`.
- Dependencies: agentconsole v0.0.4, for `client.Cost`, `console.WithCost`,
  the wrapping prompt, and the running usage on the status line.
- Dependencies: agenteval v0.0.10, for `price` and its JSON table.
- Dependencies: agentturn and agentturn/session v0.0.18, up from
  v0.0.17: a forked task's progress shows the sub-agent's own messages,
  not the main agent's last answer at the head of each update.
- Fixed: `/think` and `/model` did not reach the sub-agents, whose model
  and effort were fixed when the session started; each `explore` and
  `task` call now reads them as they stand.
- Added: `task` takes `context` (`fresh`, the default, or `fork`, which
  also sees the conversation so far) and `model` (`subagent`, the
  default, or `main`), so the main agent picks per call. A fork's
  conversation is the child run's opening items, recorded and verified
  in the child session; the main agent's reasoning items are left out.
- Dependencies: agentturn and agentturn/session v0.0.17, up from
  v0.0.16, for `tools/agent.WithCallConfig`.
- Fixed: the session record said a request asked for the effort dax
  configured when the model was sent the fitted one, so the recorded
  request hashes were of requests never sent; `-verify` could not see it,
  since it checks the record against itself. The effort is now fitted
  where each configuration is made, and the model wrapper only reports a
  request that was not.
- Fixed: `/model` kept the effort fitted to the previous model, and
  `/think` was forgotten by the next `/model`.
- Fixed: a sub-agent's session recorded no decision for a call its
  policy allowed.
- Added: the `task` sub-agent. The main agent starts it for a
  self-contained coding task; it runs on the sub-agent model with the file
  tools and bash under the same policy, sees dax's prompt, your
  instructions and AGENTS.md, and reports back. Calls in one turn run in
  parallel. Starting one is on the built-in allow list.
- Changed: the sub-agents are offered by default; `-agents=false` or
  `"agents": false` in the user config turns them off.
- Fixed: two edits of one file running at once, parallel calls in one
  turn, could lose one of them; writes and edits now take a workspace
  lock.
- Added: `subagent_model` and `-subagent-model`, the model the explore
  sub-agent runs.
- Changed: the `openrouter` provider defaults to
  `deepseek/deepseek-v4-pro-0813`, with `deepseek/deepseek-v4.1-flash`
  for sub-agents.
- Fixed: reasoning asked for was fitted to `none` when `none` and the
  nearest effort the model takes were equally near, so `-think` on
  `deepseek/deepseek-v4-pro` (none, high, xhigh) turned reasoning off.
- Fixed: a model, `base_url` or `api_key_env` from the config was kept
  when a flag switched the provider, so `"model": "qwen3-coder:30b"` with
  `-provider openrouter` asked OpenRouter for the Ollama model. They now
  belong to the provider in force where they were set, and a switch
  takes the new provider's defaults.
- Added: dax asks the vendor what the model takes (Anthropic's and
  Gemini's model endpoints, OpenRouter's catalogue, Ollama's
  `/api/show`), shows it under the banner, and fits each request's
  reasoning effort to it, saying once when it changes one.
- Fixed: `-think` (the default) on an Ollama model without thinking,
  `qwen3-coder:30b` say, failed every request with "does not support
  thinking"; reasoning is now sent as none.

- Added: the `openrouter` provider, with its key from `OPENROUTER_API_KEY`
  and `anthropic/claude-sonnet-5.5` as the default model.
- Added: the `openresponses` provider for any other Open Responses
  server: `-base-url` and `-model` are required, and `-api-key-env`
  (`api_key_env` in the user config) names the variable its key is read
  from. The project config may not set it.
- Changed: `openai` no longer takes `-base-url`; it is OpenAI. A config
  that pointed it at another server is refused with an error that says
  to use `openresponses`.
- Security: the variable the provider's key was read from is removed
  from the environment of bash commands and MCP servers even when its
  name does not look like a credential's, unless `pass_env` names it.
  `OPENROUTER_API_KEY` joins the named credentials.

- Added: dax, with the siblings at agentkit v0.0.7, agentturn and
  agentturn/session v0.0.16, agentsession v0.0.21, agenttool v0.0.15,
  openresponses and its anthropic and gemini providers v0.0.14,
  agentpolicy v0.0.11, agentskill v0.0.11, agentmemory v0.0.10 and
  agentsmd v0.0.2.
- Added: `glob`, `grep` and `ls` tools.
- Added: the file tools are confined to the working directory, through
  an `os.Root`: a path outside it, a `..` out, or a symbolic link out is
  refused.
- Added: providers, `-provider ollama|openai|anthropic|gemini`, `-model`
  and `-base-url`, with keys from `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`
  and `GEMINI_API_KEY`. Ollama stays the default.
- Added: a config file, `~/.config/dax/config.json`, and a per-project
  `.dax/config.json`; `-config` names another user file.
- Added: a default policy that is always on: the read-only tools and a
  few safe commands run, writes and every other command ask. `bash` is
  decided per subcommand. `-no-policy` turns it off.
- Security: a bash call is auto-allowed only if it is one simple command in a
  safe subset of the syntax with read-only arguments (git status, diff,
  log, show; ls; pwd; go version; go env NAME); everything else asks. Fixes
  comment, `$'..'` and `>&word` handling in the splitter that serves asked
  commands. Git runs with the repository's fsmonitor, pager, ssh command and
  hooks neutralised and `--no-ext-diff --no-textconv`.
- Security: `go test`, `go build`, `go vet` and `go list` are no longer
  auto-allowed; the README says how to allow them in the user config.
- Security: the project config is tighten-only; it may no longer set the
  provider, model, base_url, think, instructions_file, skills_dirs,
  memory_dir, mcp_servers or pass_env, nor add allow rules or carve-outs.
- Security: path rules match the normalised path; glob, grep and ls are
  matched on where they search.
- Security: AGENTS.md files and `.dax/skills` that link out of the workspace
  are not read; reported as omitted.
- Security: bash commands and MCP servers start without credentials in their
  environment (`pass_env` names exceptions); `max_read_bytes` bounds `read`
  and `edit`; grep and glob skip non-regular files and cannot be made
  exponential; the session store and memory are `0700`; `/mcp add` uses the
  `mcp__<name>__` prefix and names may not contain `__`; terminal control
  sequences are stripped from output.
- Security: git commands run unasked only if the repository's own git config
  names no program (filters, textconv, askpass, editor, proxies,
  credential helpers, drivers ...), the question names the key; the gpg
  programs are `/bin/false` and `%G` in a format asks. The git
  neutralisation environment and `GOTOOLCHAIN=local` apply only to
  auto-allowed commands, not to ones you approve. `..` in an `ls` or git
  path asks and links are resolved segment by segment; a `:` in a git
  revision or pathspec asks; bare `go env` asks. The credential scrub covers
  `_KEY`, `_PAT`, `_PWD`, `_JWT`, `_CREDENTIALS`, `_AUTH`, PASSWORD, SECRET,
  DATABASE_URL, SSH_AUTH_SOCK and URLs with passwords; credential-file paths
  pass through. MCP stderr is cleaned. `-trust-skills` trusts only skills in
  directories you named.
- Added: more read-only commands run unasked: git combined short flags and
  space-separated values, `branch`, `rev-parse`, `ls-files`, `remote -v`,
  `blame`, `stash list`, `tag`, `describe`, `shortlog`, `config` reads;
  trailing `2>&1`, `2>/dev/null`, `>/dev/null`; pipes into `head`, `tail`,
  `wc`, `sort`, `uniq`, `cut`, `grep`; `&&` sequences and `cd` into the
  workspace; `cat`, `head`, `tail`, `wc`, `grep` on named files; `ls` with
  globs. Bash output is capped at 50 KiB in memory.
- Security: a glob word with a quoted part is not in the safe subset (it was
  rendered bare, so `ls ';touch /x;'*` ran touch); a differential test runs
  every auto-allowed line through real bash with each stage's command
  replaced by a probe and compares argv. An auto-allowed bash call is
  stamped with the plan the policy approved and runs only that plan; if the
  line no longer analyses to it the call fails instead of running verbatim.
  git config reads only a named key that holds no secret; URL credentials are
  taken out of auto-allowed output. git asks when `.git` is a file or link,
  git's repository is not the workspace's, `core.worktree`, `core.bare`,
  `extensions.*` are set, or a submodule's config names a program. Reading a
  secret-looking path asks (`read(.env)` in the user config opens one). `ls`
  glob expansions follow a `--`.
- Security: a line with a git stage that is not auto-allowed asks when the
  repository's config names core.fsmonitor, hooksPath, sshCommand, pager or a
  gpg program (it runs without the neutralising environment). The explore
  sub-agent is decided by the parent's policy for every tool, with asks put to
  the user. Secret-path asks apply to what a link leads to and ignore case.
  The Security model says committed secrets and tree-wide searches are not
  caught.
- Added: the terminal client (agentconsole v0.0.1) is the default front on
  a terminal, over the session's kit through kitbackend: permissions answered
  with y/n on screen with the policy's reason and what it asks about; start
  lines and warnings printed before the client takes the screen and notes
  after it. `-front tui|repl`; `-p` unchanged. The explore sub-agent's asks,
  which cannot be a permission, are refused with a reason telling the model
  to make the call itself; tool elicitation is declined. The slash commands
  stay in the REPL.
- Fixed: the terminal client is the default only with a terminal at both ends
  (a real isatty, so /dev/null is not one) and a session store; `-front tui`
  without them fails before a store is opened or a banner printed; the
  pre-client pause needs both ends a terminal; dax's buffered notes and the
  session ID are flushed however the client ends, and warnings held back for
  the screen are shown before an error opening the session.
- Dependencies: agentconsole v0.0.2. In the terminal client, Ctrl-O and
  Ctrl-R expand or collapse the selected row alone (every row with none
  selected), a click selects a row and a second click expands it, bars
  frame the input line, and Ctrl-/ (or F1) lists the keys.
- Changed: CI runs on `workflow_dispatch` only until a `DAX_DEPS_TOKEN`
  secret exists (agentconsole is private).
- Changed: `-confirm` is gone, since the policy is always on; `-key` and
  `DAX_API_KEY` are gone, since a key is read from the provider's own
  variable; `-base` is now `-base-url`; the default `-model` follows the
  provider.
- Changed: the front is chosen in `cmd/dax/front.go`, the place a
  terminal UI plugs in; `-front` names it, and only `repl` exists.
