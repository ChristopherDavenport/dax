# dax

A coding agent: a terminal client, a REPL and a one-shot `-p` mode over
an Open Responses model, under a policy, recording every session. It is a
product that assembles the sibling libraries, and a minimal core meant to
be extended: the session knows no tool, and everything the model can do
comes from an `extension.Extension`. dax's own capabilities are
extensions like any other (dax-coding, dax-agents, dax-skills,
dax-memory, dax-a2a), and a Go program runs `dax.Main` with its own. Where the
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
  `ext/memory`, `ext/a2a`, `ext/hooks`, `ext/plan`. Where the tools act is agentworkspace's
  `Workspace`, which the public API names directly (imported as
  `workspace`); dax keeps no copy or alias of it. Everything else is in `internal/`. Before v1.0.0 the public API may change in a minor
  version; a change to it gets a `Changed:` line in the changelog. Keep
  it small: export what a program built on dax needs, and say why in the
  doc comment.

## Layout

One line a package. `docs/design.md` (the planes, who owns what, the
rules that hold, the fronts, the workspaces, extending) and each
package's doc comment have the detail; read them before changing a
package's shape.

- `dax` (root): `dax.go`, `Main`, its options and the default
  extensions; `cli.go`, flags, admin modes, settings, the front; the
  fronts `front.go` (REPL, `-p`), `tui.go` (agentconsole), `acp.go`
  (`-front acp`), `attach.go`, with `commands.go` (slash commands) and
  `status.go`; the served modes `serve.go` and `serve_a2a.go` (`dax
  serve`), `execute.go` (`dax execute`), `workspace.go` (`dax
  workspace`, `-workspace`), `tunnel.go` (`-connect`, `workspace.hub`)
  and `box.go` (`-box`, `dax box`).
- `cmd/dax`: `dax.Main` with no options, nothing else.
- `extension`: the `Extension` type and `Env`, what every extension,
  dax's own included, is built from.
- `agent`: the session: one `agentkit.New`, the extensions, the
  policy, MCP servers, workspace, record, modes, grants, the human
  plane (`plane.go`) and A2A peers.
- `policy`: the permission modes, and one source per extension merged
  with the user's and the project's rules.
- `ext/coding` (dax-coding): the coding tools, their rules and the
  policy tests (`exploit_test.go`, `widen_test.go`, `gitconfig_test.go`).
- `ext/agents` (dax-agents): the explore and task sub-agents.
- `ext/skills` (dax-skills): skill directories, the project's screened,
  and `skill_manage`.
- `ext/memory` (dax-memory): the memory store, its tools and the
  background pass.
- `ext/plan` (dax-plan): plan mode's `exit_plan_mode`.
- `ext/hooks` (dax-hooks): the user's PreToolUse and PostToolUse hooks.
- `ext/a2a` (dax-a2a): the user's A2A peers as tools.
- `tool`: dax-coding's tools, `Files` (the tools' view of a
  workspace) and the analysis their facts claims make.
- `facts/factspolicy`: a tool's facts claim as the policy's subjects
  and the session's one rewrite hook.
- `toolrender`: the terminal client's renderers of dax's tool calls,
  from the record alone.
- `internal/executor`: the execution plane: `InProcess`, `Set`, the
  MCP server `dax execute` serves, and `Remote`, its client.
- `internal/box`: a project's box over docker, the box host and its
  relay (`docs/plans/proposals/box-host.md`).
- `internal/config`: the JSON config layers, validation, `Resolve`.
- `internal/offer`: MCP servers that hold secrets, and those `dax
  workspace -servers` offers.
- `internal/authn`: dax's bearer tokens and the tunnel's proof.
- `internal/acp`: ACP's JSON-RPC wire and v1 messages.
- `internal/cmdline`: `Split`, a command line as a program and its
  arguments.
- `internal/provider`: the provider setting as an
  `openresponses.Streamer`.
- `internal/modelinfo`: what a model takes, and each request's effort
  fitted to it.
- `internal/prompt`: dax's part of the system prompt.
- `internal/render`: the REPL's event printer.
- `internal/private`: the private-directory check.

## Siblings

Peer repositories, each independently versioned; bump them by hand with
an `Unreleased` changelog line:

- `../open-responses` (openresponses and `providers/*`): the model.
- `../agentturn`, `../agentkit`, `../agenttool`: the loop, the assembly,
  the tool contract.
- `../agentpolicy`, `../agentsession`, `../agentskill`, `../agentsmd`,
  `../agentmemory`.
- `../agentconsole`: the terminal client, a dependency.
- `../agentworkspace`: where the tools act, `Workspace` and `Local`;
  its nested module `mcpworkspace`, a workspace's processes and files
  over MCP, which the executor serves and the session reaches; and
  `tunnel`, `remote` through a connection the workspace dials, which
  `dax workspace -connect` and `workspace.hub` use.

## Conventions

- `make check` (fmt, tidy-check, vet, staticcheck, govulncheck, race
  tests) must pass before any commit. In a workspace with a go.work,
  run `GOWORK=off GOFLAGS=-mod=readonly make check`. CI runs it on
  pushes to main and on pull requests.
- The changelog, commits, pull requests and releases follow the
  `release` skill (`.dax/skills/release/SKILL.md`): load it, or read
  the file, before any of them.
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
- A secret stays on the machine whose environment holds it, and its
  server runs and is filtered there: a session's `secret_env` server
  is `"in": "host"` alone (refused in validation, `Resolve` and
  `agent.New` otherwise), and `dax workspace -servers` gives its own
  servers their values on its own host, offers a secret holder alone,
  under a grant without `exec` or `write`. No secret is sent to a
  workspace; every other child (bash, the key command, the box host's
  launcher) loses the variables.
- A project config field is refused unless it can only tighten. The one
  exception is `mcp_servers`: a project's servers start only once the
  user trusts that project's exact set (`-trust-mcp`), and a project's
  server may name no `token_env`, no `secret_env` and no name the
  user's config gives.
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
  `tool/remote_test.go` runs a new check on `Local`, on a stand-in
  container, and on that container served over `remote` and through a
  tunnel.
- Nothing is special: a capability dax ships is an extension built only
  from what package `extension` offers any extension. If dax's own needs
  something more, add it to `extension` for everyone.
- An extension is compile-time and goes through the same policy, record
  and workspace as any other: its tools ask by default, its rules are its
  own source and name only its own tools (no patterns, no carve-outs),
  and nothing a repository's config says can add one.
- A new shipped allow rule, dax's or any extension's, needs a policy
  test that says what a compound command with it does.
- Package boundaries: `agent` names no tool, and `policy` is generic.
  `toolrender` does not import `tool`; its tests run the real tools, so
  a change to a tool's output format fails them. Kit tools are control
  and never go through the executor. Every command dax runs from a line
  (`-executor`, MCP servers, the key command) goes through
  `internal/cmdline`'s `Split`. `internal/authn` and `internal/acp` are
  standard library only, and `internal/box` is too but for
  agentworkspace's `mcpworkspace/wire` and agentredact's `secrets`, so
  it can move to agentworkspace. Bearer tokens are `internal/authn`'s:
  no dax code uses a library's `BearerTokens` or `WithBearerToken`.
