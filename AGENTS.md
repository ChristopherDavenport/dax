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
  `ext/memory`, `ext/a2a`. Where the tools act is agentworkspace's
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
  extensions' tools served over stdio MCP (`executor.NewServer`), or
  over HTTP with `-listen` (`executor.Handler`; loopback unless
  `-insecure-no-auth`, since nothing authenticates a client yet), with
  flags of its own and no config read. `workspace.go`: the served
  workspace, both ends: `dax workspace`, a directory served as a
  `workspace.Local` through agentworkspace's `remote` behind one bearer
  token (`$DAX_WORKSPACE_TOKEN` or a private file, as serve's) granting
  `-scopes` (read, write, exec by default; servers too with
  `-servers`), an `Offering` of the servers a `-servers` file names in
  the config's own shape (`config.ParseOffered`, `internal/offer`), a
  grant that would give a secret-holding server's secret away refused
  (`settle`), loopback unless `-tls-cert`/`-tls-key`, flags of its own
  and no config read; and `dialWorkspace`, `-workspace`'s client
  (https verified against the system's roots or `workspace.tls_ca`, no
  redirect followed, the token taken and its variable unset, by `takeWorkspaceToken`, as
  soon as the settings are resolved, before the key command runs), whose
  `remote.Workspace` is the session's `Options.Workspace`, under
  `identify`, which keeps what the client is (a `Starter`, a
  `ServerProvider`, both) so each assertion tells the truth; the
  same dial (or `hostHub`) reaches `servers_workspace`, the
  session's `Options.ServersWorkspace`, beside its own.
  `tunnel.go`: the served workspace turned around, both ends of
  agentworkspace's `tunnel` (imported as `wstunnel`): `dax workspace
  -connect`, the directory served through `tunnel.Serve` to a session's
  hub it dials (wss, or ws to loopback; `$DAX_TUNNEL_TOKEN`, taken
  from the environment first, never sent: `-name`, the hub's
  workspace name, required; before each upgrade a nonce fetched from
  the hub (`authn.FetchNonce`), the upgrade carrying the MAC under the
  token of the name, that nonce and the run's instance id, and nothing
  served to a hub whose 101 lacks its own MAC (`internal/authn`'s
  `Prover`; a hub that fails, or issues no nonce, ends the run in any
  phase, through `dialWatch.halt`); `-scopes` and `-servers`, as `-listen`'s; `-tls-ca` in place of the system's roots;
  `dialWatch`, which ends the run before it connects at a certificate
  not verified or a 404); and `hostHub`, the config's
  `workspace.hub`, a `tunnel.Hub` at `/tunnel` (loopback unless
  `tls_cert`/`tls_key`) whose enrollment is `internal/authn`'s
  `Hub` for the one token and name (a nonce it did not issue, older
  than 30 seconds or taken before refused), which `pin` also answers a
  plain GET with a nonce from, its 101 carrying the hub's MAC, held by
  `pin` to the first instance that
  connects (any other 403 for the session's life), its log among
  dax's warnings (`agent.Warnings`), and whose `Hub.Workspace`, waited
  for up to `wait`, with a 10-second request timeout, is the session's
  `Options.Workspace` as `-workspace`'s is, recorded by
  `config.TunnelRef`; `closeSession`, the session's close and the
  hub's within one deadline.
  `front.go`: the `front` interface,
  `selectFront`, the REPL and print fronts, which drive the session as
  agentturn's control contract. `tui.go`: the terminal client,
  agentconsole's `console.Run`, the default front on a terminal: the
  session's record (`client.RecordOf`) and the session as its source of
  events in, and the session as its controller (`client.Direct`) for
  the intents it sends out; `runConsole`, the client's run with dax's
  slash input, which `attach.go` shares. `serve.go`: `dax serve`, a
  session served behind one bearer token, on loopback or over HTTPS
  with a certificate (`-tls-cert`, `-tls-key`), its control
  (agentturn's `front/control`, `agent.Controls` and the head move as
  named commands) and its record (agentsession's `remote`).
  `serve_a2a.go`: `dax serve -a2a`, agentturn's `front/a2a` and its card
  beside them behind a token of their own (principal `a2a`), each A2A
  context a session of its own (`agent.Session.OpenPeer`) under the
  served agent's configuration (`PeerConfig`), `-a2a-escalate` for
  `WithEscalation`, `-a2a-url` for the card, and the conversations,
  their owners, escalated calls and waiting tasks kept in a private
  directory, locked while serve runs (`a2aStore`; `lock_unix.go`).
  `viewed` in `serve.go` counts each follower of the control's events as
  a view attached (`agent.Session.SubscribeView`).
  `attach.go`: `dax attach`, a mode with flags of its own, the terminal
  client over that wire (https verified against the system's roots or
  `-tls-ca`), a record over the store client, an `agent.Controls` over
  the served commands, and a controller that sends the head move
  through its command.
  `box.go`: `-box`, whose executor command is the box host's
  launcher (this dax as `box serve`, or ssh to the dax on another
  machine); `dax box serve`, the box host, which makes the box from its
  own config (a `-project` path, or a `-checkout` it clones and keeps)
  and relays the session to the box's `dax execute`, or answers one of
  its own requests about that project (status, down, pull); `dax box
  status|down|pull`, which ask it; and `dax box up|ls|down NAME|proxy`
  on this machine's docker.
  `cmd/dax/main.go`: `dax.Main`, nothing else.
- `extension`: the `Extension` type, `ToolEnv` (what tools are built
  over), `Env` (the session as kit options see it), `Renderers`,
  `SecretsOf` (the variables every child environment loses), and
  `TextQuestion`, `IsTextQuestion`, `TextAnswer` and `TextContent`,
  dax's own marked form asking for a line of text, which every front
  answers (anyone else's form is cancelled).
- `agent`: one `agentkit.New` per session; the two-phase build of the
  extensions and their checks (`agent/extension.go`), the policy built
  from them, the sub-agents' policy hook, the MCP servers it connects,
  each started again by the next call after it ends, with a warning and
  a `dax.mcp.restart` entry (`mcpStart`, the one path every placement
  takes): a command's in the workspace or here, one placed on the host,
  one at a URL (`agent/mcpurl.go`, its token held to the URL's origin),
  those the workspace offers of its own (`workspace.ServerProvider`, a
  box host's units, a served or tunnelled workspace's `-servers`) or a
  servers workspace beside it offers (`Options.ServersWorkspace`,
  refused unless its grant is servers alone, `checkServersWorkspace`), and
  a command holding secrets (`secret_env`, on the host alone) started
  through `internal/offer` as an offered one is (`mcpHolding`); a project's
  only once the user trusts its set (`agent/mcptrust.go`, `-trust-mcp`,
  `~/.dax/mcp-trust.json`); each listed in `Info.MCP` with its placement,
  chosen where `mcpDialOf` chooses its dial, which `/mcp` shows (`mcpLines`
  in `front.go`); and the session-store helpers. It names
  no tool. `agent/identity.go`: the project's
  identity, the workspace and the executor's hashed address in the
  session's header, a servers workspace's identity and servers beside
  them (`ServersWorkspaceOf`), and the listings by it (`List`, `ListExecutor`,
  `ListWorkspaceURL`, a served workspace's by the URL its descriptor
  records as ref, or a hub's workspace by its tunnel's ref).
  `agent/confine.go`: `box.confine`, the session's claim that a box's
  tools run confined (agentpolicy's `WithConfinement`), from the box
  host's envelopes and the user's opt-in alone, with the `dax:box`
  bare asks that make it matter and a baseline engine without them, so
  nothing allowed today asks; sub-agents' decisions are not claimed.
  `agent/plane.go`: the human plane, the
  session as `agentturn.Control` over agentkit's `Kit.Control` with
  dax's controller rules on top (`ErrHeld`, the calls an abort left
  answered on the next prompt, the questions no front here can answer
  not asked, `Answering`), `Permissions`, `Controls`, and `Drive`, the
  controller that answers by rule. Nothing asks a front through a hook:
  a new question is an `agentturn.Question` event, answered with
  `Reply`. `agent/peers.go`: an A2A caller's conversation on a host
  serving the session, `PeerConfig` and its record, `OpenPeer`, whose
  questions name the conversation and are asked only of a view
  attached (`Viewing`, `SubscribeView`), waiting `Options.ViewGrace`
  for one to come back.
- `policy`: generic. Merges one source per extension
  (`extension:<name>`) with the user's and the project's rules, and
  refuses a shipped rule that names another extension's tool, a pattern
  or a carve-out; `dax:box` is `box.confine`'s bare asks, bash, write,
  edit and `mcp__<unit>__*` alone.
- `ext/coding` (dax-coding): the coding tools, their rules, matchers,
  aliases and bash stamp, the prompt's nudge, and the policy tests:
  `exploit_test.go`, `widen_test.go`, `gitconfig_test.go`.
- `ext/agents` (dax-agents): explore and task, built from every
  extension's tools; the delegation guide.
- `ext/skills` (dax-skills): skill directories, the screening of the
  project's, grants under `-trust-skills`.
- `ext/memory` (dax-memory): the store, the scopes (the project's is
  its workspace's), the memory tools.
- `ext/a2a` (dax-a2a): the user's A2A peers (config `peers`, the user's
  layer only), each agentturn's `tools/a2a` over a client of its card,
  added through the kit (control, as memory's tools are); the card's
  endpoint held to the configured origin, the client's guard (that
  origin only, no redirect off it) and `ObserveEndpoint`; the token
  variables as `Secrets`; a remote agent's question put through the
  call's elicitor as a `TextQuestion`. It is in the root module: dax is a
  product, with no dependency boundary to keep a2a-go out of.
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
  git-config check, run through `Exec`; `confine.go`, which of their
  calls a box's confinement may cover (protected paths, a bash line
  read whole that hides nothing), from the facts alone.
- `facts/factspolicy`: where a tool's facts claim (agenttool's
  `Factual`: what a call would touch and the rewrite it runs with if
  allowed, bash's stamped plan) becomes agentpolicy's subjects and the
  session's one rewrite hook.
- `toolrender`: the terminal client's renderers of dax-coding's,
  dax-agents' and dax-a2a's calls (`toolview.Renderer`s), from the record's arguments
  and output alone. It does not import `tool`; its tests run the real
  tools, so a change to a tool's output format fails them.
- `internal/executor`: the execution plane: the `Executor` the
  session runs the extensions' `Tools` through (facts, replay, calls,
  descriptor, close), `InProcess` (the tools in this process), and
  `Set`, the tools bound as adapters for the kit, which holds each
  model response's facts for its decisions (two requests: the calls,
  then their rewrites) so one reading decides and stamps a call; and
  `NewServer` (`serve.go`), the in-process tools served over MCP for
  `execute`, built for each session on an MCP server of its own at the
  read limit it asks for in its capabilities, capped at the
  executor's, with what MCP cannot carry in a capability, and its
  workspace's files served read-only as MCP resources and read back as
  an `fs.FS` (`files.go`), and processes started for the session
  through custom methods, not tools (`process.go`): both are
  agentworkspace's `mcpworkspace` (`ServeFiles` and `NewFS`,
  `Processes` and `Start`, the names in its `wire` package), and what
  is dax's is the connection they ride, its capability and its redial; `Handler`
  (`http.go`), such a server over streamable HTTP, each client a
  session of its own, closed when idle with its processes ended first;
  `Remote`
  (`remote.go`), the client of such a server, which asks it for the
  session's read limit and warns when it gives less, refuses one that
  does not give the claims, and dials again (`Dial`) before the next
  request when its connection is lost, refusing an executor that is not
  the first one, and never sending a call twice (`agent/executor.go` wraps it as the public
  `agent.Executor` for `-executor`). Kit tools are control and never
  go through it.
- `internal/box`: a project's box over the docker command line
  (`Runner`, a fake in the tests): the hardened `docker run`, `Ensure`
  (reuse, start, refuse other settings, make again for another dax),
  `List`, `Remove`, the exec command; the box's internal network and
  its sidecar, and `proxy.go`, the egress proxy the sidecar runs
  (`dax box proxy`) under the box's rules file, which refuses this
  machine and private networks on the address it dials; `host.go`, the
  box host config (units, each an envelope of files, connectivity and
  secrets held to three rules; profiles; limits; images) and `Resolve`,
  a session's request narrowed, never widened; `units.go`, an MCP
  unit's container beside the box (`EnsureUnit`, its secrets resolved
  into docker's environment alone); `unitproc.go` and `hostproc.go`, a
  unit's server under `docker exec` and the process methods the box
  host answers for it, named by `mcpworkspace/wire`; `redact.go`, the redaction of a unit's secret
  values from its server's output, in every JSON spelling, before the
  box host passes any of it on, through agentredact's `secrets` module; `relay.go`, the box host's relay, which reads
  the session's handshake, passes the rest through a message at a
  time, adds what it resolved to the handshake's answer, and offers the
  session's units as the workspace's own servers
  (`agentworkspace/servers.list` and `servers.start`) and answers their
  process methods itself, or, for a first message of the box
  host's own (`dax/box.status|down|pull`), answers that and returns;
  `checkout.go`, a repository's URL as checkouts match it, the box
  host's checkout of it, cloned and moved on only when nothing would be
  lost, and the git bundle a pull takes back. Runtime-agnostic: any runtime docker has
  registered. Standard library only but for agentworkspace's
  `mcpworkspace/wire`, which names the process methods it answers, and
  agentredact's `secrets`, the redactor (each standard library only
  itself), so it can move to agentworkspace once a second product
  wants it. The design is
  `docs/plans/proposals/box-host.md`.
- `internal/config`: the JSON config layers, validation, `Resolve`,
  and `ParseOffered`, `dax workspace -servers`' file in the layers'
  `mcp_servers` shape.
- `internal/offer`: an MCP server that holds secrets, and the servers
  `dax workspace -servers` offers: commands in a workspace offered
  through agentworkspace's `Offer`, given their `secret_env` from the
  starting process's environment, their output read through
  agentredact's `secrets` filter (`Filter`).
- `internal/authn`: dax's own bearer-token check, which the libraries
  leave to their program: `Tokens`, static tokens compared through
  SHA-256 in constant time, its `Authenticator` the shape of every
  library's hook (agentworkspace's and agentsession's `remote`,
  front/control) and `Principal` front/a2a's; `Challenge`, the
  `Bearer realm="dax"` every handler behind them names on a 401
  (each library's `WithChallenge`); `Client`, an
  `http.Client` that sends one token to one origin, for each library's
  `WithHTTPClient`; and `proof.go`, the tunnel's proof both ways
  (`Prover` at `-connect`, `Hub` at the hub: an HMAC-SHA256 under
  the token of the name, a nonce the hub issued statelessly under a
  secret of its own, and the instance id, labelled per direction, the
  token never sent). Standard library only; no dax code uses a
  library's `BearerTokens` or `WithBearerToken`.
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
- `../agentworkspace`: where the tools act, `Workspace` and `Local`;
  its nested module `mcpworkspace`, a workspace's processes and files
  over MCP, which the executor serves and the session reaches; and
  `tunnel`, `remote` through a connection the workspace dials, which
  `dax workspace -connect` and `workspace.hub` use.

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
- `CHANGELOG.md` keeps an `## Unreleased` section in Keep a Changelog
  form; `make release VERSION=vX.Y.Z` dates it, runs `make check`,
  commits, guards and writes an annotated tag whose message is the
  section, and pushes branch and tag atomically.
  `scripts/release-guard.sh` refuses a tag that exists or does not sort
  above the published ones. The release workflow publishes the tag
  message as the GitHub release.
