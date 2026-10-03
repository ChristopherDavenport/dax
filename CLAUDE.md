# dex

A coding agent: a REPL and a one-shot `-p` mode over an Open Responses
model, with read, write, edit, glob, grep, ls and bash tools under a
policy, recording every session. It is a product that assembles the
sibling libraries; the design and who owns what is in
`docs/design.md`; read it before changing the shape.

## Module

- Module path: `github.com/ChristopherDavenport/dex`.
- Go 1.25 is the floor (`os.Root` and its `MkdirAll`, `ReadFile` and
  `WriteFile` need it). The binary is `./cmd/dex`.
- This is a product, so unlike the siblings it has no dependency
  boundary: it depends on every sibling it assembles, and on
  `anthropic-sdk-go` and `google.golang.org/genai` through the
  provider adapters, pinned to the versions the adapters require.
- The code is in `internal/`: nothing is a public API.

## Layout

- `cmd/dex/main.go`: flags, the admin modes (`-list`, `-verify`, `-project`,
  `-import`, `-gc`, `-repair`), settings, then one front.
  `cmd/dex/front.go`: the `front` interface, `selectFront`, the REPL and
  print fronts. `cmd/dex/tui.go`: the terminal client, `console.Run` over a
  `kitbackend` on the session's kit; the default front on a terminal. The
  session for it is built with `Options.NoAgent` (the client builds its
  own agent over the kit).
- `internal/config`: the JSON config layers, validation, `Resolve`.
- `internal/provider`: provider setting to `openresponses.Streamer`.
- `internal/policy`: the default rules, matchers, merge of the user's and
  the project's.
- `internal/tool`: the tools, the `Workspace` they are confined to, and
  `BashSubjects`, the command splitter the policy uses.
- `internal/agent`: one `agentkit.New` per session, prompt and approval
  plumbing, and the session-store helpers.
- `internal/prompt`, `internal/render`: dex's part of the system prompt;
  the REPL's event printer.

## Siblings

Peer repositories, each independently versioned; bump them by hand with
an `Unreleased` changelog line:

- `../open-responses` (openresponses and `providers/*`): the model.
- `../agentturn`, `../agentkit`, `../agenttool`: the loop, the assembly,
  the tool contract.
- `../agentpolicy`, `../agentsession`, `../agentskill`, `../agentsmd`,
  `../agentmemory`.
- `../agentconsole`: the terminal client, a dependency (private module).

## Conventions

- CI is manual (`workflow_dispatch` only) until the repository has a
  `DEX_DEPS_TOKEN` secret: agentconsole is private, and a runner needs a
  fine-grained personal access token with read access to it to fetch it
  (the workflow sets `GOPRIVATE` and rewrites github.com URLs with the
  token, and fails with a clear message when the secret is empty). Do not
  create the secret without the owner; `GOFLAGS=-mod=readonly make check`
  locally is the gate. release.yml only publishes a GitHub release from a
  tag and fetches no module, so it needs no token.
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
- A bash command is auto-allowed only through `tool.SafeWords` and
  `tool.ReadOnlyArgs`; widening either needs a test with the review's
  exploit strings (internal/policy/exploit_test.go) and a reason.
- A project config field is refused unless it can only tighten.
- File tools go through `tool.Workspace`; never `os.ReadFile` a
  model-supplied path. A new file tool gets a case in the confinement
  test.
- A new default allow rule needs a policy test that says what a
  compound command with it does.
- `CHANGELOG.md` keeps an `## Unreleased` section in Keep a Changelog
  form; `make release VERSION=vX.Y.Z` dates it, runs `make check`,
  commits, guards and writes an annotated tag whose message is the
  section, and pushes branch and tag atomically.
  `scripts/release-guard.sh` refuses a tag that exists or does not sort
  above the published ones. The release workflow publishes the tag
  message as the GitHub release.
