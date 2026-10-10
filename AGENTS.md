# dax

A coding agent: a terminal client, a REPL and a one-shot `-p` mode over
an Open Responses model, under a policy, recording every session. It is a
product that assembles the sibling libraries, and a minimal core meant to
be extended: the session knows no tool, and everything the model can do
comes from an `extension.Extension`. dax's own capabilities are
extensions like any other (dax-coding, dax-agents, dax-skills,
dax-memory), and a Go program runs `dax.Main` with its own. Where the
tools act is a `workspace.Workspace` (the agentworkspace module), and
this machine's directory is one implementation of it: dax should behave the same whether its tools
act here or elsewhere. The design,
who owns what and the extension points are in `docs/design.md`; read it
before changing the shape.

## Module

- Module path: `github.com/ChristopherDavenport/dax`.
- Go 1.26 is the floor (agentconsole, on Bubble Tea v2, needs it; `os.Root`
  and its `MkdirAll`, `ReadFile` and `WriteFile` need 1.25). The binary
  is `./cmd/dax`, which is `dax.Main` with no options.
- This is a product, so unlike the siblings it has no dependency
  boundary: it depends on every sibling it assembles, and on
  `anthropic-sdk-go` and `google.golang.org/genai` through the
  provider adapters, pinned to the versions the adapters require.
- Public API: the root package `dax` (`Main`, `Option`, `WithName`,
  `WithExtension`, `WithoutExtension`), `agent`, `extension`, `policy`,
  `tool`, `toolrender`, and `ext/coding`, `ext/agents`, `ext/skills`,
  `ext/memory`. Where the tools act is agentworkspace's
  `Workspace`, which the public API names directly (imported as
  `workspace`); dax keeps no copy or alias of it. Everything else is in `internal/`. Before v1.0.0 the public API may change in a minor
  version; a change to it gets a `Changed:` line in the changelog. Keep
  it small: export what a program built on dax needs, and say why in the
  doc comment.

## Layout

- `dax.go`: `Main` and its options, the default extensions,
  the package doc. `cli.go`: flags, the admin modes (`-list`, `-verify`,
  `-project`, `-import`, `-gc`, `-repair`), settings, the extensions the
  settings choose, then one front. `execute.go`: `execute`, the
  extensions' tools served over stdio MCP (`executor.NewServer`), with
  flags of its own and no config read. `front.go`: the `front` interface,
  `selectFront`, the REPL and print fronts, which drive the session's
  `agent.Turn`. `tui.go`: the terminal client, agentconsole's
  `console.Run`, the default front on a terminal; `tuiadapter.go`: the
  glue that presents the Turn as agentconsole's `client.Backend` over
  the session's one agent, with what it fakes listed at its top.
  `cmd/dax/main.go`: `dax.Main`, nothing else.
- `extension`: the `Extension` type, `ToolEnv` (what tools are built
  over), `Env` (the session as kit options see it) and `Renderers`.
- `agent`: one `agentkit.New` per session; the two-phase build of the
  extensions and their checks (`agent/extension.go`), the policy built
  from them, the sub-agents' policy hook, and the session-store
  helpers. It names no tool. `agent/plane.go`: the human plane, the
  session's one agent as `Turn` (the contract dax proposes for
  agentturn), its questions hub, `Controls`, and `Drive`, the controller
  that answers by rule. Nothing asks a front through a hook: a new
  question goes through the Turn.
- `policy`: generic. Merges one source per extension
  (`extension:<name>`) with the user's and the project's rules, and
  refuses a shipped rule that names another extension's tool, a pattern
  or a carve-out.
- `ext/coding` (dax-coding): the coding tools, their rules, matchers,
  aliases and bash stamp, the prompt's nudge, and the policy tests:
  `exploit_test.go`, `widen_test.go`, `gitconfig_test.go`.
- `ext/agents` (dax-agents): explore and task, built from every
  extension's tools; the delegation guide.
- `ext/skills` (dax-skills): skill directories, the screening of the
  project's, grants under `-trust-skills`.
- `ext/memory` (dax-memory): the store, the scopes, the memory tools.
- The workspace is not a package here: the `Workspace` interface the
  session and every tool act through (root, file system, writes,
  environment, `Exec`, descriptor), `Starter` for a long-lived process
  with pipes, and `Local`, this machine's directory over an `os.Root`,
  are the agentworkspace module's, imported as `workspace`.
- `tool`: dax-coding's tools; `Files` (`tool/workspace.go`), the tools'
  view of a workspace for model-written paths, with the write lock;
  `view.go`, the workspace as the policy's checks read it (links
  through `fs.ReadLinkFS`); `BashSubjects`, `PathSubjects` and
  `Analyzer`, the analysis each tool's facts claim makes, and the
  git-config check, run through `Exec`.
- `facts/factspolicy`: where a tool's facts claim (agenttool's
  `Factual`: what a call would touch and the rewrite it runs with if
  allowed, bash's stamped plan) becomes agentpolicy's subjects and the
  session's one rewrite hook.
- `toolrender`: the terminal client's renderers of dax-coding's and
  dax-agents' calls (`toolview.Renderer`s), from the record's arguments
  and output alone. It does not import `tool`; its tests run the real
  tools, so a change to a tool's output format fails them.
- `internal/executor`: the execution plane: the `Executor` the
  session runs the extensions' `Tools` through (facts, replay, calls,
  descriptor, close), `InProcess` (the tools in this process), and
  `Set`, the tools bound as adapters for the kit, which holds each
  model response's facts for its decisions (two requests: the calls,
  then their rewrites) so one reading decides and stamps a call; and
  `NewServer` (`serve.go`), the in-process tools served over MCP for
  `execute`, with what MCP cannot carry in a capability, and its
  workspace's files served read-only as MCP resources and read back as
  an `fs.FS` (`files.go`), and processes started for the session
  through custom methods, not tools (`process.go`); `Remote`
  (`remote.go`), the client of such a server, which refuses one that
  does not give the claims (`agent/executor.go` wraps it as the public
  `agent.Executor` for `-executor`). Kit tools are control and never
  go through it.
- `internal/config`: the JSON config layers, validation, `Resolve`.
- `internal/cmdline`: `Split`, a command line as a program and its
  arguments, as a shell splits words with nothing expanded; every
  command dax runs from a line (`-executor`, MCP servers, the key
  command) goes through it.
- `internal/provider`: provider setting to `openresponses.Streamer`, and
  the vendor's `modelinfo.Describer`.
- `internal/modelinfo`: asks the vendor what a model takes and fits each
  request's reasoning effort to it; a trial of a shape meant to move to
  openresponses.
- `internal/prompt`: the session's part of the system prompt (role line,
  extensions' and user's instructions, working directory).
  `internal/render`: the REPL's event printer. `internal/private`: the
  private-directory check and its warnings.

## Siblings

Peer repositories, each independently versioned; bump them by hand with
an `Unreleased` changelog line:

- `../open-responses` (openresponses and `providers/*`): the model.
- `../agentturn`, `../agentkit`, `../agenttool`: the loop, the assembly,
  the tool contract.
- `../agentpolicy`, `../agentsession`, `../agentskill`, `../agentsmd`,
  `../agentmemory`.
- `../agentconsole`: the terminal client, a dependency.
- `../agentworkspace`: where the tools act, `Workspace` and `Local`.

## Conventions

- CI runs on pushes to main and on pull requests; release.yml publishes
  a GitHub release from a tag.
- `make check` (fmt, tidy-check, vet, staticcheck, govulncheck, race
  tests) must pass before any commit. In a workspace with a go.work,
  run `GOWORK=off GOFLAGS=-mod=readonly make check`.
- Tests are table-driven and offline. A model is the `echo` adapter or a
  scripted streamer; nothing in `go test` needs Ollama or a key.
- Keep the config small and strict. A new field needs validation, a test
  of its precedence, and a line in the README's table; a field that a
  repository could abuse is refused in the project layer.
- Keys come from the environment and are never printed, logged or
  written to a config file.
- A bash command is auto-allowed only through the Analyzer's safe
  subset, package tool's unexported `safeWords` and `readOnlyArgs`;
  widening either needs a test with the review's exploit strings
  (ext/coding/exploit_test.go) and a reason.
- A project config field is refused unless it can only tighten.
- Tools, dax's and any extension's, act through `tool.Files` over the
  session's `workspace.Workspace` (`ReadFile`, `WriteFile`, `Update`,
  `Stat`, `ReadDir`) and its `Exec`; never `os` or `os/exec` on a
  model-supplied path or command, so the tool runs on any workspace. A
  read-modify-write uses `Update`, which holds the lock dax's `write`
  and `edit` hold. A new file tool, or a new exported `Files` method,
  gets a case in the confinement test; `workspace.Local`'s own
  confinement is agentworkspace's, whose tables every backend runs.
- A check the policy makes of a call is the tool's facts claim
  (`agenttool.Factual`, set with `agenttool.WithFacts`): the tool says what the call would touch, and the
  policy decides on that and reads no machine itself. The claim
  inspects the workspace the call acts in, through its file system and
  `Exec`, never this machine's; where the workspace cannot answer (a
  file system that cannot read links), the claim says so with a call
  no rule names, and the policy asks. A rewrite a tool needs when
  allowed (bash's stamp) is in its claim, never in a hook of the
  extension's. A claim names only its own extension's tools (one that
  names another's asks, unless the extension's `HeldTo` holds the
  claiming tool to it: then that tool's asks and denies apply, never
  its allows), and so does a matcher's own subjects. A claim that reads a field of the arguments
  refuses a key that is that field in another case, which the tool's
  decoder would take (package tool's `exactKeys`), and the tool refuses
  it again when it runs. The cross-workspace table in
  `tool/remote_test.go` runs a new check on `Local` and on a stand-in
  container.
- Nothing is special: a capability dax ships is an extension built only
  from what package `extension` offers any extension. If dax's own needs
  something more, add it to `extension` for everyone.
- An extension is compile-time and goes through the same policy, record
  and workspace as any other: its tools ask by default, its rules are its
  own source and name only its own tools (no patterns, no carve-outs),
  and nothing a repository's config says can add one.
- A new shipped allow rule, dax's or any extension's, needs a policy
  test that says what a compound command with it does.
- `CHANGELOG.md` keeps an `## Unreleased` section in Keep a Changelog
  form; `make release VERSION=vX.Y.Z` dates it, runs `make check`,
  commits, guards and writes an annotated tag whose message is the
  section, and pushes branch and tag atomically.
  `scripts/release-guard.sh` refuses a tag that exists or does not sort
  above the published ones. The release workflow publishes the tag
  message as the GitHub release.
