# Changelog

All user-visible changes to dax. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break flags and the config file.

## Unreleased

- Added: a served or tunnelled workspace's offered MCP servers reach
  the session. `-workspace URL` and `workspace.hub` keep what the
  workspace is (a `Starter`, a `ServerProvider`, both), and the servers
  it offers are connected beside yours, as a box host's units are,
  under the same names, rules, restarts and secret handling. Every
  server, configured or offered, is connected through one path.
- Added: `dax workspace -servers FILE`, MCP servers the workspace
  offers, in the config's own `{"mcp_servers": {...}}` shape, checked
  by the same code: each a command run in the served directory, named
  1 to 32 lowercase letters, digits and inner hyphens; an entry with a
  `url` or `"in": "host"` is refused. The start line names each and the
  variables it is given, never a value.
- Added: `mcp_servers.<name>.description`, what the server is for,
  shown where servers are listed, and `mcp_servers.<name>.secret_env`,
  variables a command's server is given from the environment of the
  process that starts it (dax, or `dax workspace` for its offered
  ones), which every other child loses (bash, the other servers, the
  key command, the box host's launcher), and whose values are redacted
  from its standard output and standard error (agentredact's `secrets`,
  through agentworkspace's `Offer`; standard error cleaned of terminal
  controls, then redacted again) before the session sees any of it. A
  secret stays on the machine whose environment holds it: in your
  config `secret_env` needs `"in": "host"`, and a server that would
  run in a workspace (this directory's, a served one, a hub's, an
  executor's or a box's) is refused. Your file only; not for a `url`;
  not the model's key's variable, a `token_env`, an extension's
  secret, `DAX_WORKSPACE_TOKEN`, `DAX_TUNNEL_TOKEN` or a variable
  `pass_env` or `executor.pass_env` names; a value shorter than 8
  bytes is refused at start.
- Changed: `agent.MCPServer` has `Description` and `SecretEnv`, and
  `agent.Secrets` counts each server's `SecretEnv`.
- Changed: `dax workspace -scopes` applies to `-listen` as well as
  `-connect`, and takes `servers`; the default stays `read,write,exec`,
  with `servers` added when `-servers` offers any. `servers` with
  nothing offered, and `-servers` without `servers` granted, are
  refused, as are `write` or `exec` without `read`, a grant holding
  `exec` or `write` beside a server that names `secret_env` (with
  `exec` a session reads a running server's environment through
  `/proc`, and with `write` it changes the server to print its secret
  past the redaction), and a secret holder offered beside any other
  server. `-servers` refuses a key given twice. The start line of
  `-listen` names the grant too.
- Changed: a served workspace granted its servers alone
  (`-scopes servers`) starts a session: its project's `AGENTS.md` and
  config are not read (an `omitted:` line says so for each), and the
  file tools and bash fail as forbidden. One that refuses the session
  its project's config while letting it run commands or write fails
  the start. A workspace that offers one server name twice is refused.
- Changed: the API-key command no longer inherits the session's other
  credentials (each MCP server's `token_env` and `secret_env`, each
  peer's token), and `dax box status|down|pull`'s launcher loses them
  too.
- Dependencies: agentworkspace, agentworkspace/mcpworkspace and
  agentworkspace/tunnel v0.0.7, up from v0.0.6, whose `remote` and
  `tunnel` carry a workspace's offered MCP servers under a new
  `servers` scope, and whose `Offer` takes the secrets a server holds
  and a filter for its output (`WithOutputFilter`).

## v0.0.29 - 2026-10-26

- Added: `dax workspace -connect URL`, the served workspace for a host
  the session cannot reach: in place of listening, it dials out to a
  session's hub at a `wss://` URL (`ws://` to a loopback host alone)
  and serves the directory through that connection (agentworkspace's
  `tunnel`), dialling again whenever it drops; a session's processes
  there outlive a drop shorter than their lease. The token is
  `$DAX_TUNNEL_TOKEN`, at least 32 bytes, sent in the upgrade's header,
  taken out of the environment before anything starts and never
  printed; `-pass-env` may not name it. Each run sends an instance id
  of its own, which the hub holds its workspace to. `-scopes` (default
  `read,write,exec`) is what the session may do there, `-tls-ca` a CA
  to verify a `wss://` hub with in place of the system's roots, as
  `workspace.tls_ca` does. Not with `-listen`, `-tls-cert` or
  `-tls-key`. It warns on standard error when it dials `ws://`, which
  authenticates no hub, and stops at once on a refused token, a
  workspace held by another host, a certificate not verified or no hub
  at the path (404), and keeps dialling a hub not up yet. A signal ends
  every process the session started there before it stops.
- Added: the config's `workspace.hub` (`listen`, `name`, `token_env`,
  default `DAX_TUNNEL_TOKEN`, `tls_cert`, `tls_key`, `wait`, default
  `60s`), in place of `workspace.url`: the session hosts a hub at
  `/tunnel`, on loopback unless it has a certificate, enrolls the one
  token as the one name and refuses any other, waits up to `wait` for
  that workspace to dial in, and then acts in it as in a served one
  (tools, policy checks, the project's files, MCP servers). One that
  never connects fails the start with the `dax workspace -connect` line
  to run on its host. The hub holds the workspace to the first
  `dax workspace -connect` run that connects and refuses any other
  (403) for the session's life; its connects, drops and refusals are
  among dax's warnings; a request to a host that is away fails after
  10 seconds; and the session's close and the hub's are bounded by one
  deadline. Your file only; not with `url`, `executor` or
  `box`; its `token_env` is no other credential's variable. The record
  names the workspace `remote` at `http://<name>.tunnel.invalid`, and
  `-list` with the hub's config lists its sessions without listening.
  `-workspace URL` dials that URL in the hub's place.
- Added: `agent.Warnings`, where dax warns at the time of each write
  (standard error, or what `CaptureWarnings` holds while a front has
  the screen), for a program's own warnings beside a session.
- Changed: `dax workspace -listen` serves at most 256 transfers (Execs
  and file transfers) at once, agentworkspace `remote`'s new default;
  past it a request fails as over its limit until one ends.
- Changed: the start line cleans the workspace's root, which a served
  workspace reports, as other text from elsewhere is cleaned.
- Dependencies: agentworkspace and agentworkspace/mcpworkspace v0.0.6,
  up from v0.0.5, whose `remote` adds `WithMaxTransfers` and
  `Handler.Lease()` and drops `BearerTokens` and `WithBearerToken`;
  agentsession v0.0.29, up from v0.0.28, and agentturn,
  agentturn/session, agentturn/front/a2a and agentturn/tools/a2a
  v0.0.33, up from v0.0.32, which drop their own bearer-token helpers
  too (dax never used them: `internal/authn` is its check). front/a2a's
  `New` now refuses an `Authenticator` with a scheme and no name; dax's
  names it `bearer`.

## v0.0.28 - 2026-10-25

- Added: `dax workspace`, a mode with flags of its own that reads no
  config, serves a directory over HTTP as a whole workspace
  (agentworkspace's `remote`): its files, writes, environment, commands
  and processes, for a session elsewhere to run its tools in. One
  bearer token, `$DAX_WORKSPACE_TOKEN` or one made and written to a
  private file whose path alone is printed, may read, write and run;
  authentication is always on. `-dir` (default the current directory),
  `-listen` (default `127.0.0.1:7779`, loopback unless `-tls-cert` and
  `-tls-key` are given), `-pass-env`. A signal ends every process a
  session started there before it stops. It serves `workspace.Local`
  alone: a workspace's offered MCP servers are not carried yet.
- Added: `-workspace URL` and the config's `workspace` (`url`,
  `token_env`, default `DAX_WORKSPACE_TOKEN`, and `tls_ca`): the
  session's tools, their checks and the policy run here and act in the
  served workspace; the project's `AGENTS.md`, `.dax/skills` and
  `.dax/config.json` are read through it, and stdio MCP servers start
  there when the token holds `exec`; without it your own run here and
  a project's are refused, never moved to this machine, and the MCP
  trust records where a project's servers run, so a trust given while
  they ran there does not hold here. The URL is https, or http to a
  loopback host, with no user, password, query or fragment; the token
  is never printed or recorded, and its variable is taken out of dax's
  environment before dax starts any process, the key command first. Your config only: a project's file may not
  set `workspace`, and it is refused beside an executor or a box. The
  header records the workspace as `remote` with the URL as ref (its
  host in lower case, no default port), and `-list` finds its sessions
  by the URL without connecting. See
  docs/sandbox.md, "A served workspace".
- Changed: `dax serve` and `dax execute` stop as cleanly on a hangup
  (SIGHUP, as when the ssh session they ran in drops) and on SIGQUIT as
  on SIGINT and SIGTERM, ending what they started, where a hangup
  killed them before; and their HTTP servers close a keep-alive
  connection left idle for two minutes. `dax workspace` does the same.
- Changed: `dax serve` refuses a `$DAX_SERVE_TOKEN` or
  `$DAX_SERVE_A2A_TOKEN` shorter than 32 bytes, which could be guessed;
  leave it unset and serve makes one, as before. `dax workspace` takes
  the same rule for `$DAX_WORKSPACE_TOKEN`, refuses a `-pass-env` that
  names it, and `workspace.token_env` may not be `api_key_env`, an MCP
  server's or a peer's `token_env`.
- Changed: dax checks the bearer tokens of `dax serve` (its control,
  its record and A2A) and `dax workspace` itself, and its clients
  (`dax attach`, `-workspace`) send them through an HTTP client of its
  own (`internal/authn`), held to the server's origin; the libraries'
  `BearerTokens` and `WithBearerToken` helpers are no longer used. The
  tokens accepted, the scopes, the principals and the refusals are as
  before.
- Added: `agent.ListWorkspaceURL`, the sessions of a workspace served
  at a URL, without dialing it.
- Dependencies: agentworkspace and agentworkspace/mcpworkspace v0.0.5,
  up from v0.0.4, whose new `remote` package serves a workspace over
  HTTP and dials one.

## v0.0.27 - 2026-10-25

- Fixed: a rewind (the head move) on a session whose store reported
  an append with `Reopen`, as a remote record or the cas store does
  when the session it held could not follow an append, went on from
  the session the store had replaced: the rewind's mark put the head
  back where it started, or the move was refused. The recorder now
  writes through the store's `agentsession.Writer` and opens the
  session again on `Reopen` (agentturn#268), and agentkit's head move
  follows it, and undoes a move cancelled partway in the store as
  well as in the agent.
- Dependencies: agentturn, agentturn/session, agentturn/front/a2a and
  agentturn/tools/a2a v0.0.32, up from v0.0.31; agentkit v0.0.15, up
  from v0.0.14; agentworkspace and agentworkspace/mcpworkspace v0.0.4,
  up from v0.0.3, whose `Offer` takes an output filter for an offered
  server's secrets, which dax does not use yet.

## v0.0.26 - 2026-10-25

- Fixed: a unit's secret could reach the session when its server wrote
  a line longer than a megabyte: where the box host split the line, a
  value whose end is the start of another value (one secret's suffix
  another's prefix) was cut inside the first, so most of it passed
  unredacted, since v0.0.22. The split now moves back past every value
  that reaches across it (agentredact#16).
- Changed: the box host redacts a unit's output with agentredact's
  `secrets` module (v0.0.1), which dax's own redactor moved to;
  `internal/box/redact.go` keeps reading the container's values and
  wrapping the unit's server. The output is the same placeholders,
  `[REDACTED NAME]`, in the same spellings.
- Added: an MCP server is provided by the workspace or called
  independently (docs/plans/proposals/mcp-placement.md). In
  `mcp_servers`, a command's `in` is `workspace` (the default, as
  before) or `host`, which runs it on this machine even beside an
  executor or a box; and a server may be a `url`, a streamable HTTP
  server called from this machine (https, or http to loopback), with
  `token_env` naming the variable whose token is sent to its origin
  alone, no redirect off it followed, the variable kept from every
  child process. The servers the workspace offers of its own
  (agentworkspace's `ServerProvider`) are connected beside the config's,
  named after themselves; the executor's capability says whether its
  workspace offers any (`servers`).
- Added: a project's `.dax/config.json` may name MCP servers, the one
  field a project may set that does not only tighten. They start only
  once you trust the project's exact set: `-trust-mcp` starts them and
  records the trust in `~/.dax/mcp-trust.json` (private), keyed by the
  project's workspace and a hash of the set, so later sessions start
  them without the flag; a changed set is untrusted again, and an
  untrusted one is left out with a warning naming it. A project's
  server may name no `token_env`, and none may take a name your config
  gives a server, nor that name and an underscore, or the other way
  round; the trust file is believed only while no other account could
  have written it or its directory.
- Added: two servers whose tools would be named alike (one name the
  other, or it and an underscore: the `-mcp` flag's `cli` beside a
  configured `cli` among them) are refused when the session opens, and
  so is a `/mcp add` named like a configured server, an offered one or
  a box host's unit.
- Added: `agent.MCPServer`'s `Host`, `URL`, `TokenEnv` and `Project`;
  `agent.Options`' `TrustMCP` and `MCPTrustFile`; `agent.DefaultMCPTrustFile`;
  `agent.Secrets`, the variables every child of a session loses (the
  extensions', the key's and each MCP server's token variable), for a
  program that builds the session's workspace or starts its executor,
  as dax's command line does.
- Changed: the box host offers a session's MCP units as the workspace's
  own servers (`agentworkspace/servers.list` and `servers.start`, from
  agentworkspace v0.0.3), in place of answering a
  `agentworkspace/process.start` of the reserved `["dax-unit", NAME]`,
  which is gone. A session and the box host it reaches must both be
  this version or later: behind an older box host, which offers none of
  the units its handshake names, the session refuses to open ("the box
  host offers no server for unit X; it is older than this dax, update
  it"), and an older session's `dax-unit` start reaches the box's
  executor, which has no such program. A box is made again for another
  dax anyway. `box.confine` claims a unit's tools only when the unit is
  a server the box host offered and the session connected, and no other
  server may take a unit's name.
- Changed: agentworkspace v0.0.3 and its `mcpworkspace` v0.0.3; a
  command's server in the workspace goes through
  `mcpworkspace.CommandTransport`.

## v0.0.25 - 2026-10-25

- Changed: the executor's process methods and workspace files are
  agentworkspace's `mcpworkspace` (agentworkspace v0.0.2), no longer
  dax's own code, and are renamed on the wire: `dax/process.*` is
  `agentworkspace/process.*`, and the files' template
  `dax-workspace:///{op}{?path}` is `agentworkspace:///{op}{?path}`.
  A session and the `dax execute` (or `dax box serve`) it reaches must
  both be this version or later. Either way round the session refuses
  the executor at the handshake, over the files template: a newer
  session says to update dax execute where it runs, and an older one
  that it serves its files under a template it does not read. Update
  dax where the executor runs together with dax here; a box is made
  again for another dax anyway.

## v0.0.24 - 2026-10-25

- Added: `box.confine` (`-box-confine`): `units` or `workspace` has the
  policy take a box's tools as confined where their envelope allows,
  so a bare ask for them does not ask. `units` covers an MCP unit with
  no internet, no secrets, no files beyond a read-only project and a
  runtime other than runc; `workspace` adds the workspace's bash, write
  and edit when the workspace has no internet. Protected paths (`.git`,
  `.dax`, environment files, CI, hooks, editor settings) still ask, a
  bash line is covered only when the analyzer reads it whole and it
  hides nothing, a sub-agent's call gets nothing from it, deny rules
  and ask rules with a specifier still apply, nothing the policy allows
  without it starts asking, and a project's bare ask for a tool keeps
  it asking. The rules are a new policy source, `dax:box`. User config
  only.
- Added: the git guard. With `box.confine workspace` the box host
  mounts the project's `.git` read-only in the box, whole (a `.git`
  file and its in-project git dir, each checked-out submodule's `.git`,
  an in-project `core.hooksPath` and `.husky` too), so no program a
  confined call runs (a `make`, a test) can leave a hook, a config or a
  `commondir` your git trusts outside the box, rename `.git`, or remove
  the history. Git that writes fails in such a box; commit outside it.
  Each session says so, and `dax box pull` is refused against it. The
  workspace's envelope says `"guard": "on: .git read-only"`, and the
  workspace's tools are taken as confined only then; a project that is
  not a git repository, or an older box host, keeps them asking. A box
  made without the guard, or whose guarded paths changed, is refused
  until `dax box down`. `dax box up -guard` makes a guarded box by
  hand.
- Changed: `/mcp add` and a config MCP server may not take a box
  unit's name, or that name and an underscore, whose tools would be
  named as the unit's.
- Changed: `bash` is a barrier (agenttool's `Barrier`) instead of
  sequential: a command runs alone in its place in a response's batch,
  after every call before it and before every call after it, and the
  calls on either side of it run together. Before, one command made the
  whole batch run one call at a time. Five reads and a command now take
  four or five round trips one after another over an executor's link,
  not eight, which matters over ssh. A command still never overlaps a
  `write` or `edit` of the same response. A `dax execute` of v0.0.23 or
  older advertises `bash` as sequential only, and a session reaching
  one runs such a batch one call at a time, as before; a new executor
  advertises both, and the session reads the barrier.
- Dependencies: agenttool, mcpclient and mcpserver v0.0.24, up from
  v0.0.23 (`Barrier`, `Executor.Preceding`, the barrier in the MCP
  listing's `_meta`), and agentturn, its `session`, `front/a2a` and
  `tools/a2a` v0.0.31, up from v0.0.30 (a batch ordered by
  `Executor.Preceding`).

## v0.0.23 - 2026-10-24

- Fixed: a box unit's server could lose the last of its output, standard
  error most of all, when it exited: its pipes were exec's own, which
  `Wait` closes as the process ends, before they were read. They are
  now pipes the box host reads to their end.

## v0.0.22 - 2026-10-24

- Added: `-box` runs the tools in this project's box. It starts a box
  host, `dax box serve` (this dax as a child, or the dax on another
  machine over ssh, `box.host: "ssh://[user@]host[:port]"`), which makes
  or reuses the project's container from a profile of its own config
  and relays the session to the `dax execute` in it, so a box here and
  a box on a build host are configured, made and used the same way. The
  box runs under the profile's runtime (`runsc` by default; any runtime
  docker has registered, Kata's `io.containerd.kata.v2` among them),
  with no capabilities, a read-only root file system, limits on memory,
  processors and processes, the project mounted at its own path and the
  box host's dax mounted read-only as the box's. A project at `/`, at
  the box host's home or above it is refused, since the box would mount
  every key and config there. A box made from other settings is refused
  (`dax box down` removes it); one whose dax changed since it was made
  is made again, with a warning. `-list` finds a box's sessions without
  starting the box host. Config `box` (user layer only; not with
  `executor`) names the host, the profile and tightenings that only
  narrow it (`memory`, `cpus`, `pids`, `internet`, `allow`); its
  presence makes `-box` the default; `-box-host`, `-box-profile` and
  `-box-internet` override for a run. With ssh, `box.project`,
  `box.remote_dax` and `box.ssh` say where the project and dax are there
  and what more ssh takes; `executor.pass_env` gives the launcher ssh's
  agent.
- Added: the box host config, `~/.config/dax/box-host.json` on the box
  host's machine (`dax box serve -config`, or `box.host_config` for a
  local one): the default runtime, `limits` no unit or tightening goes
  past, `images` patterns, `units`, `profiles` and `default_profile`.
  Every tool group is a unit, an envelope of files, connectivity and
  secrets: the workspace, where dax's own tools act, and MCP units, each
  an MCP server the box host runs in a container of its own (`image`, or
  `build` from a base and lines, built once and tagged by the
  declaration's hash; `files` none, or the project `ro`/`rw` and a
  `state` directory that outlives the container; its own `internet`
  and `allow` behind its own sidecar and network; `secrets` as `env:`,
  `command:` or `file:` references resolved on the box host and given
  to that container alone, through docker's environment). A profile is
  a workspace unit and its MCP units; a session takes all of them, or
  those `box.mcp` names. The box host refuses a unit that holds
  secrets with internet `on`, one with the project and any internet
  without `"exfiltration": "accepted"` (the workspace too), and a
  secret that is not a reference. The session starts each MCP unit as
  an MCP server through the executor (`dax-unit NAME`, which the box
  host answers), its tools `mcp__<unit>__<tool>`, and records each
  unit's envelope, with its secrets' names and never their values, in
  its header (`dax.box`). The box host redacts each value a unit's
  container holds from its server's output, standard output and error,
  as `[REDACTED NAME]`, in every spelling Go's, Python's, PHP's and
  JavaScript's JSON encoders write it in, and says so once per unit and
  secret, so a server that returns its own secret does not put it in
  the conversation or the record; an encoded or split value is not
  found. With no file, one unit, `workspace`
  (`golang:1.26-bookworm` under `runsc`, 4g, 2 CPUs, 512 processes, the
  project read-write, no internet), in one profile, `default`.
- Added: a box has no internet unless its profile gives it some:
  `internet` is `off` (the default), `allow` (the hosts `allow` names,
  `*.domain` for those below one) or `on`, which a session narrows with
  `box.tighten` or `-box-internet`. The box sits alone on an internal
  network of its own; its only way out is a sidecar container running
  dax's egress proxy (`dax box proxy`, under runc), HTTP and HTTPS on
  ports 80 and 443, never to the box host's machine, a private network
  or a link-local address in any mode. Its rules are a file the proxy
  reads again, so a run changes the box's internet without making it
  again; the change applies to every session in the box, and the run
  says so.
- Added: `dax box status`, `dax box down` and `dax box pull`, through
  the box host the config's `box` names, here or over ssh, each acting
  on that box host's one project: the box, its units (with their files,
  internet and image) and its checkout's branch, head and uncommitted
  changes; removing the box, its units and each one's sidecar, network
  and rules there (`-state` the units' state directories too); and a
  git bundle of the project's head (256 MiB at most), verified and
  fetched into a local branch (`-branch`, default
  `dax-box/<repository>`; one that is not behind it only with
  `-force`). `dax box serve` answers these as its first message in
  place of a session. `dax box up`, `dax box ls` and `dax box down
  NAME` act on this machine's docker: make this directory's box and
  print its executor's command, list the boxes and units, remove one.
- Added: `box.checkout` (`{"url", "ref"}`, with an ssh `box.host`, in
  place of `box.project`): a repository the box host clones into a
  directory it keeps, within its config's `checkouts.allow` (glob
  patterns on the URL; `checkouts.dir`), which is then the project. At
  each session's start it is fetched and moved to the ref only when
  its worktree is clean, on that ref and holds no commit of its own;
  otherwise it is left as it is, and the box host says why. Git runs on
  the box host with its own credentials, never prompting; a URL with a
  password, or a user on https, is refused. The launcher passes
  `-checkout URL [-ref REF]`, so `-list` and an sshd forced command
  key on the repository. The repository, ref and starting head are in
  the handshake's `box`, the record's `dax.box` header and the
  executor line.
- Added: `agent.ExecutorOptions.Box`, what a session asks of a box host
  when it dials it, sent beside `max_read_bytes` in the executor's
  capability; an executor that is not a box host ignores it.
- Docs: docs/sandbox.md has the box, the box host config and its
  units (the three rules, secrets, what the session sees), a box over
  ssh, the box's internet and how it holds, and runtimes: gVisor
  (installing runsc, registering it on the KVM platform where the
  default, systrap, cannot start, as on WSL2) and Kata 4.2.0 (installing
  the static bundle, registering the shim, and what was checked: the
  box's hardening, its internet, shared files between containers, the
  VM's memory). docs/plans/proposals/box-host.md is the design and its
  steps.
- Added: under a sandboxing runtime (anything but `runc`) a box's
  process limit is also `--ulimit nproc`, which the sandbox's kernel
  enforces for the box's user: `--pids-limit`, a cgroup on this
  machine, does not count gVisor's or Kata's processes.

- Dependencies: agentsession v0.0.28, up from v0.0.27, and
  agentconsole v0.0.14, agenteval v0.0.11 and agentkit v0.0.14, up from
  v0.0.13, v0.0.10 and v0.0.13, which take agentsession v0.0.27 and
  agentturn v0.0.30 themselves. A redacted session now hashes
  independently: a redactor gives each redacted response the
  `request_hash` of the request the redacted session rebuilds for it,
  so readers verify and replay it with no exception for redacted
  entries. `-verify`, which has none, verifies a redacted session made
  under that rule whole, and fails a mismatch on a redacted response as
  on any other. This answers #93, and agentconsole#38 for the inspect
  pane dax embeds.

## v0.0.21 - 2026-10-18

- Fixed: `-resume` refuses a session written before format 0.13 whose
  header or entries hold a redaction's `redacted_from` or `redacted`
  member, saying it cannot be continued (agentsession's `ErrNoRaise`)
  and leaving it as it was. agentsession never raises such a session,
  since 0.13 gives those members a meaning they were not written with,
  so it took no entry: the resume opened, and the first prompt failed
  on its append. The warning for a session of an earlier format no
  longer says readers before agentsession v0.0.18 refuse it once
  raised, which stopped being true at format 0.11; it says readers that
  know only earlier formats do.
- Dependencies: agentsession v0.0.27, up from v0.0.25, and agentturn,
  agentturn/session, agentturn/front/a2a and agentturn/tools/a2a
  v0.0.30, up from v0.0.29. agentsession's format is now 0.13: dax
  raises every session it appends to, a resumed one included, to 0.13,
  so readers on agentsession v0.0.26 or older, and a dax before this
  one, refuse those files. With it `-import` takes a redacted session's
  projection, as the store now holds redacted sessions like any other.
  agentturn v0.0.30 clears an entry's `redacted` member when the
  recorder copies an entry it read (a callee link copied onto another
  branch, the env a child is written with), so a redacted session dax
  resumes takes its run as native entries.

## v0.0.20 - 2026-10-15

- Changed: an MCP server whose session ended, a process that exited or,
  with `-executor`, one that ended with the executor's lost connection,
  is started again by the next call to one of its tools, in the
  workspace or on this machine as before, the config's servers and
  `/mcp add`'s alike. Each attempt is a warning (`dax: MCP server <name>
  ended and was started again (<server> <version>); whatever it held is
  gone, …`, naming the tools added and removed when they changed, or
  the reason it could not start) and an entry in the session's record,
  `dax.mcp.restart`: the server's name, why, whether it started, the
  error, its implementation and version, and whether its tools changed
  and which came and went. The restarted server's tools are offered
  from then on. A call in flight when the server ended is not sent
  again, a call of a tool the restarted server no longer lists as it
  was fails unsent, and a restart that fails fails the call, with the
  next call trying again; the model reads each as the call's error.
  Closes #87.
- Dependencies: agenttool v0.0.23 (mcpclient reconnect), up from
  v0.0.22, for `mcpclient.WithReconnect` and `WithReconnected`.

## v0.0.19 - 2026-10-15

- Changed: a session's project is its workspace, where its tools act
  (`dax execute`'s `-kind`, `-ref` and `-root`), for `-list` and for
  memory alike. Every new session's header records the workspace
  (`dax.workspace`: kind, ref, root) and, when its tools run through an
  executor, a SHA-256 of the address that reached it (`dax.executor`:
  the command's words, so a string and an array that split alike are
  one, or the URL); the address itself is never written. `-list` with
  an executor configured (`-executor` or `executor` in your config)
  lists the workspace the latest session at that address recorded,
  whichever address reached its sessions, without starting or dialing
  the executor; an address no session used lists nothing. With none it
  lists this directory's sessions as before, less those whose tools
  ran in an executor whose root is the same path. Sessions recorded
  through an executor before this release record neither and are not
  listed; `-resume <id>` still opens them. `-resume` notes a session
  continued in another workspace, naming both, even when the roots are
  one path. dax-memory's project scope follows the workspace: this
  machine's directory keeps the scope it had, so existing memory stays
  the project's, and a sandbox's is named for its root's base with a
  hash of its kind, ref and root, so two sandboxed projects started
  from one directory no longer share one. Give `dax execute` a `-ref`
  that outlives the container. See docs/sandbox.md, Sessions and
  memory. Closes #56, #57.
- Changed: `agent.List` lists the sessions whose tools acted in the
  directory on this machine (its workspace, or for older sessions its
  cwd), and no longer one whose executor's root is the same path.
- Changed: `max_read_bytes` reaches the executor's tools. dax sends the
  session's setting when it connects to `dax execute`, and each session
  there has its tools built at its own value, up to the ceiling
  `dax execute -max-read-bytes` sets (dax's default, 2 MiB, when
  unset); before, every session's tools read what that flag said and
  the setting had no effect. A session that asks for more than the
  ceiling gets the ceiling, and dax warns, naming the flag that would
  allow more; one that asks for nothing gets the default, capped
  likewise. A dax older than this sends nothing and gets the flag's
  value, as before; against a `dax execute` older than this, dax warns
  when the setting is not the default, since it does not reach the
  executor. A reconnection asks for the same value, and one to an
  executor whose ceiling now gives less is refused. Closes #58.
- Added: `agent.ExecutorOptions.MaxReadBytes`, the session's read limit
  as `DialExecutor` and `DialExecutorURL` send it to the executor.
- Added: `agent.ListExecutor` and `agent.ListExecutorURL`, a project's
  sessions and its workspace by the executor address that reached it,
  as `DialExecutor` and `DialExecutorURL` take it, for a program built
  on dax to list as `-list` does without dialing; and
  `memory.WorkspaceScope`, the project scope of a session's workspace.

## v0.0.18 - 2026-10-15

- Changed: when the executor's connection breaks (`-executor`'s command
  exits, its container restarts, `dax execute -listen` restarts or
  closes the session, the tunnel drops), dax connects again on the next
  request, running the command again or reaching the URL again, and
  warns "executor connection lost; reconnected to …"; before, every
  later call failed until you restarted dax. A call in flight when it
  broke fails and is not sent again, since it may have run; a check of
  a call's facts, its replay or a file read is read again once on the
  new connection. The new connection must be the same executor (the
  same workspace, extensions and tools, each with the same definition
  and claims); another is refused with a warning, and every call after
  fails until you restart dax and resume. Processes started in the
  executor (your MCP servers there) end with the old connection and are
  not started again. A failed reconnect fails that request only; the
  next tries again. See docs/sandbox.md, When the connection drops.
  Closes #59.

- Added: `dax execute -listen ADDR` serves the executor over MCP's
  streamable HTTP transport, left running for sessions to connect to;
  `dax -executor http://127.0.0.1:7777` (or `executor.url` in your
  config, which a project's file may not set) connects to it. Each
  client is a session of its own, with its own processes, over the one
  workspace. Nothing authenticates a client yet, so it listens on a
  loopback address only, reached from elsewhere through a tunnel (`ssh
  -L`); `-insecure-no-auth` takes any address and warns. A request from
  a web page (another origin, or a rebound host name) is refused. A
  session that starts no request for five minutes is closed and its
  processes end; dax pings every minute while connected.
  `executor.pass_env` and `-executor-pass-env` are refused with a URL,
  since nothing here starts the executor. See docs/sandbox.md, Network.
- Added: `agent.DialExecutorURL`, for a program built on dax to start
  its session against `dax execute -listen` as `-executor` does with a
  URL.
- Changed: an executor command that is an `http:` or `https:` URL is
  now refused with "give it as executor.url" in a config file, and
  taken as the URL from `-executor`; `unix:` is still refused.

## v0.0.17 - 2026-10-13

- Added: `dax serve -a2a` serves the session's agent over A2A beside its
  control: agentturn's `front/a2a` at `/a2a` and the agent card at
  `/.well-known/agent-card.json`, on serve's listener and TLS. Both are
  behind an A2A token of their own (`<id>.a2a.token` beside serve's, or
  `$DAX_SERVE_A2A_TOKEN`, which must differ from serve's), under the
  principal `a2a`, which the control and the store do not take, nor does
  A2A take serve's token: a caller is lent the agent, not the human
  plane or the store. The card names the bearer scheme, the endpoint at
  `-a2a-url` or else at the origin the caller reached (`https` when
  serve serves TLS; forwarded headers are not taken), and the tools as
  they are when it is asked, and no token, with no cross-origin headers.
  Each A2A context is a conversation of its own, run under the served
  agent's model, extensions, policy, MCP servers and workspace, and
  recorded in a dax session of its own (listed by `-list`, each run
  naming the caller), never in the served session; a skill's grant lasts
  for the task that read it. A conversation is its creator's;
  conversations, owners, escalated calls and waiting tasks are kept
  under `~/.dax/a2a/` (private, one directory per session store, working
  directory and workspace, locked: a second `dax serve -a2a` there
  refuses to start), so a restart continues them. A question from a
  conversation goes only to a view attached when it is asked, naming the
  conversation: with none it is refused at once, and when the last view
  detaches while it waits it waits five seconds for one to come back,
  which is shown it, and is refused after, never allowed; a reply given
  first stands. `-a2a-escalate` puts the model's own calls the policy
  asks about to the caller instead (front/a2a's escalation); a
  sub-agent's asked call and a tool's own question still go to a view.
  When serve stops it refuses new A2A requests before it ends the
  conversations' runs, and writes nothing to the directory once it lets
  go of it. The conversations and the served session share the workspace
  at the same time. Off by default.
- Changed, public API: `agent.Session.PeerConfig`,
  `agent.Session.OpenPeer`, `agent.PeerRecord`, `agent.Session.Viewing`
  and `agent.Session.SubscribeView` are new, the configuration, the
  record and the attached views of an A2A caller's conversation on a
  host serving the session; `agent.Options.ViewGrace` (default
  `agent.DefaultViewGrace`, five seconds) is how long such a question
  waits for a view to come back; and `agent.Info.Workspace` says where
  the session's tools act.
- Changed: a question about a call that front/a2a puts to the session
  (`agentturn.Ask` with no parent) is no longer prefixed "a tool asks:",
  and a question from an A2A caller's conversation starts with `[A2A
  caller <principal>, conversation "<id>" (session <id>)]`, the caller's
  context ID quoted and cut to 64 bytes.
- Dependencies: agentturn, `agentturn/session` and `agentturn/tools/a2a`
  v0.0.29, `agentturn/front/a2a` v0.0.29 (new), agentsession v0.0.25 and
  agentconsole v0.0.13. With them a peer's call is recorded as the
  call's `subsession` or `peer` link (agentsession's `Callees` reads
  it), not a custom entry, and the terminal client's tree and a call's
  detail pane show it, its URL and task, and "cancel sent" when one was;
  a subsession's call that ends input-required gives its task up with
  `tasks/cancel`, where it left it waiting; and under `dax serve -a2a` a
  caller's cancel of a task waiting on it refuses the calls the task
  left pending, on the conversation's record, so nothing stays held.
- Added: dax-a2a (`ext/a2a`), a default extension that offers the A2A
  agents named in the user's config (`peers`) as tools, one per peer by
  the configured name, over agentturn's `tools/a2a`. Each peer's card is
  read when the session starts, within five seconds; a peer whose card
  cannot be read, or whose `token_env` is unset, is not offered, and the
  session says why once. A bearer token comes from the variable
  `token_env` names, goes only to the configured URL's origin, and is
  never printed, logged or recorded; `url` is https, or http to
  loopback. `relation` is `subsession` (the default: an abandoned call
  sends `tasks/cancel` and does not wait) or `peer` (never cancelled).
  A remote agent's question during a call is put to whoever answers the
  session's questions; the answer goes on the same task, and the call's
  output says what the agent asked and what was answered; a no ends the
  call with the question (the note reaches the model once, as the
  person's), an abort while it waits gives a subsession's task up, and
  with nobody answering the call ends with the question for the model
  (a subsession's task given up with `tasks/cancel`, a peer's left
  asking). The record
  names the URL each call reached. Every call asks unless the user's
  config allows it; dax-a2a ships no rule. A project's config may not
  set `peers`. The terminal client draws a peer's call as its request
  and the start of its answer (`toolrender.Peers`).
- Security: a peer's card must name its endpoint at the configured
  origin, or the peer is not offered; no redirect to another origin is
  followed, on the card's fetch or a call; a card is read up to 1 MiB.
  The peers' token variables are taken out of the environment of bash,
  MCP servers and an executor's command whatever their names, and a
  `pass_env` or `executor.pass_env` naming one is refused.
- Added: a tool's question for a line of text. `extension.TextQuestion`
  is dax's own form of one string field, marked as dax's, which the
  session asks rather than cancelling as a form; every front answers
  it: in the REPL and `-p` a line that is not an explicit no is the
  answer and a bare yes is "yes", and the terminal client answers with
  `a` and the text (the question says so). A peer's question is one.
  Any other form, an MCP server's included, is cancelled as before.
- Changed: `-p` at the end of its input answers a tool's own question
  with a cancel (nobody answered), where it declined in the person's
  place; a sub-agent's call it is asked about is still declined.
- Changed: an extension's notes at start (`extension.Env.Log`) are
  cleaned of escape sequences and control characters, as tool output is.
- Changed, public API: `ext/a2a` is new; `toolrender.Peers`,
  `extension.Extension.Secrets`, `extension.SecretsOf`,
  `extension.TextQuestion`, `IsTextQuestion`, `TextAnswer`,
  `TextContent` and
  `agent.ExecutorOptions.Secrets` are new; `WithoutExtension` takes
  `dax-a2a`.
- Dependencies: agentturn and agentturn/session v0.0.28, up from
  v0.0.27, and agentturn/tools/a2a v0.0.28 (with a2a-go v0.3.15);
  agentkit v0.0.13, up from v0.0.12; agentpolicy v0.0.17, up from
  v0.0.16; agentsession v0.0.24, up from v0.0.22. agentsession's format
  is now 0.12: a session dax appends to is raised, and a dax before this
  one refuses to read it.

## v0.0.16 - 2026-10-11

- Dependencies: agentturn and agentturn/session v0.0.27, up from
  v0.0.22; agentpolicy v0.0.16, up from v0.0.15; agentkit v0.0.12, up
  from v0.0.11; agentconsole v0.0.12, up from v0.0.11. With them, step 5
  of the plan reaches dax: a held call's subject, held mark and the
  policy's note survive a restart; whatever a person says reaches the
  model once, after the outputs of the calls it is about, in their own
  voice, and survives a crash; a held call is released from the
  decision that held it; and the record says who answered a question.
- Changed, public API: `agent.Session.Reply` takes a context, as
  agentturn's `Control.Reply` does. Who answered is the answerer the
  context names (`agentturn.ContextWithAnswerer`), which the record
  writes beside the answer as its source. The new `agent.AtTerminal`
  names the person at this terminal: the terminal client, the REPL and
  `-p` name it on every reply they take from that person, and only on
  replies, since a run's own context names who started it. An answer
  given from `dax attach` names the served session's principal, as
  front/control does. `agent.Rules` has a `By` func, asked after each
  reply for who gave it, which `Drive` names; nil, or a zero trigger,
  names nobody. `-p` names the person only for an answer they typed:
  the decline it gives when the input has ended is unattributed.
- Fixed: `Drive` answers a question off the event's delivery, so an
  interrupt stops a `-p` run while a question waits on a line.
- Fixed: after a restart, the REPL and `-p` ask only about the held
  calls that wait on a person, each with the question and the part of
  the call the restored decision is about (`agent.HeldPermissions`, new),
  as the terminal client does; the calls held only beside an answered
  question are released with no answer.
- Changed: what a person says approving a sub-agent's call is their own
  message to the sub-agent, after the outputs of its batch, where it
  was a line of the policy's note ("The user said: ..."). The question
  is put through `agentturn.Ask`, with dax's own text.
- Note: `dax serve` gives its token no scope to choose who decided a
  resumed call (front/control's `WithChosenBy`): an attached view
  answers as the person, and nothing dax sends over the wire answers
  as the policy, so every answer through it is recorded as a person's.
- dax keeps its own question elicitor (`agentkit.WithToolElicitor`),
  which puts a tool's question to the fronts answering as a question
  event and cancels what no front here can show, so agentkit v0.0.12's
  opt-in `WithQuestionEvents` is not needed (and is refused beside it).

## v0.0.15 - 2026-10-10

- Fixed: a sub-agent's question is on the record (#74). When a call a
  sub-agent makes is put to the person, the question (its text, the
  call it is about) is recorded before anyone is asked, and the answer,
  the person's note and that the human gave it after, through the
  kit's recorded question elicitor, as a tool's own question is. It
  was never recorded before; only the call's decision and, since
  v0.0.14, the policy's verdict were.

## v0.0.14 - 2026-10-10

- Changed: a sub-agent's question is the policy's own text ("the
  explore sub-agent asks: <reason>"); "; about: <subject>" is no longer
  appended (#70). The terminal client shows the subject from the asked
  call's decision, and the REPL now does too.
- Added: a sub-agent's policy verdict is recorded on its run's session
  under `agentpolicy:verdict`, as the main agent's are, so the rule,
  the reason and what a question was about are on the record for a
  sub-agent's calls too (#70).
- Added: an attached view moves the served session's head (#68).
  `dax serve` serves a `continue-from` command, and `dax attach` sends
  the terminal client's continue (Ctrl-B on a row, `c` in the tree)
  through it, so the next prompt, from any view, continues from that
  entry, with the move on the record as a leaf label. A move the
  session refuses is shown in the view that asked.
- Added: TLS for `dax serve` (#69). With `-tls-cert` and `-tls-key`
  (PEM) it serves HTTPS, TLS 1.2 at least, and `-listen` may be any
  address; without them it stays on loopback, and one of the two alone
  is refused. The token is needed either way. `dax attach` verifies the
  certificate against the system's roots, or the CA given with
  `-tls-ca`, with no way to skip the check, and refuses an `http://`
  URL to a host that is not loopback, since the token would cross the
  network in the clear.

## v0.0.13 - 2026-10-10

- Changed: a deferred call's recorded decision keeps the policy's
  reason as it gave it; "; about: <subject>" is no longer appended
  (#66). What the question is about travels on the decision
  (`ToolDecision.Subject`), which the terminal client, the REPL and
  `Drive` show, and is recorded with the policy's verdict under
  `agentpolicy:verdict`.

## v0.0.12 - 2026-10-10

- Added: `dax serve` and `dax attach` (#64). `dax serve [flags]` runs a
  session with no front of its own and serves its control (agentturn's
  `front/control`, with the model, think, MCP and session info as named
  commands) and its record (agentsession's `remote`, RFC 0003's read
  side) on a loopback address (`-listen`, default `127.0.0.1:0`; any
  other is refused, tunnel instead) behind one bearer token, from
  `$DAX_SERVE_TOKEN` or made and written to a private file under
  `~/.dax/serve`, never printed. `dax attach [-token-file f] URL` is the
  terminal client over it, its slash commands run against the served
  session. Every view attached to a session drives one conversation;
  leaving a view leaves the session's run going.
- Changed: requires agentsession v0.0.22.
- Changed: every front drives the session as agentturn's control
  contract (#62). `*agent.Session` is an `agentturn.Control` over
  agentkit's `Kit.Control` (the run context, the engine's release of
  held calls, the queued entries, the head move with its skill grants),
  with dax's rules on top: the calls an abort or a restart left
  unanswered are answered on the next prompt, a prompt while calls held
  for approval wait is refused with `agent.ErrHeld`, and a question no
  front here can answer (a form, a page to visit, or any with no front
  answering) is not asked.
- Changed: `agent.Turn`, `Session.Turn`, `agent.Question` and
  `agent.Reply` are removed. Drive the `Session` itself: `Resume` in
  place of `Answer`, `Queue` with `agentturn.QueueSteer` or
  `QueueFollowUp` in place of `Steer` and `FollowUp`. A question asked
  while a call runs is an `agentturn.Question` event, answered with
  `Reply(id, agenttool.Answer)`, what the user said in its `Note`; a
  front that answers says so with `Session.Answering`.
  `agent.Rules.Reply` takes an `agentturn.Question` and returns an
  `agenttool.Answer`; `agent.Drive` takes any `agentturn.Control`.
- Added: `Session.Permissions` reads the question and the part of the
  call it is about from the decision that deferred the call (agentkit
  v0.0.11); `agent.PermissionsOf` reads them from a run's end for a
  control behind a wire. `agent.Refusal` is a person's refusal with its
  reason. `Session.Recorder`, `Session.ContinueFrom`.
- Changed: the terminal client is agentconsole's view of the session's
  record and events, and sends what you ask to the session as intents
  (`client.Direct`); `tuiadapter.go`, the glue over agentconsole's
  native backend, is gone, and with it everything it faked. The client
  no longer asks about a call the policy only held beside the one it
  asks about; the question names the part of a command it is about from
  the decision; a call started before it was held says it may have run;
  the status line says why the last run stopped; a session resumed with
  a call held for approval opens with it on the panel, and a prompt is
  refused until it is answered.
- Added: what you say with an answer reaches the model. `a` on a
  question in the terminal client allows with a note; in the REPL and
  under `-p`, `y keep it short` allows with a note and `n wrong file`
  refuses with a reason (any other line refuses, the line its reason).
  A sub-agent's call you allow with a note is run with "The user said:
  …" beside its result; a rule's note under `agent.Drive` goes with an
  approval as well as a refusal.
- Fixed: a question a running call gave up (an abort) no longer waits
  on a line in the REPL.
- Changed: agentconsole v0.0.11, agentkit v0.0.11, agentpolicy v0.0.15,
  agenttool v0.0.22, agentturn v0.0.22.

## v0.0.11 - 2026-10-10

- Added: the terminal client takes the REPL's commands. A line that
  starts with a slash is a command to dax in both fronts and never
  reaches the model: `/model`, `/think`, `/tools`, `/session`, `/mcp add`,
  `/mcp remove`, `/follow`, `/abort`, `/quit` and the new `/help`. In the
  terminal client a command's output shows over the input, an error
  gives the line back, and `/quit` leaves as ctrl+c does when idle.
- Changed: an unknown slash command is an error in the REPL, where it
  used to be sent to the model as a prompt; start a line with `//` to
  send one that begins with a slash. During a run, `/model`, `/think`
  and `/mcp` are refused until it ends, where they used to be steered
  into the run, and `/tools`, `/session` and `/quit` work.
- Changed: agentconsole v0.0.10 (`console.WithSubmit`).

## v0.0.10 - 2026-10-09

- Added: `docs/sandbox.md`, recipes for running the tools in a sandbox
  with `-executor` (Docker, ssh, kubectl), the config, what to check
  when it fails, and its limits today; linked from the README's
  `dax execute` section.
- Added: `executor.pass_env` and `-executor-pass-env NAME,...`: variables,
  credentials included, that only the command starting the executor is
  given, such as `SSH_AUTH_SOCK` for ssh's agent or the token a kubectl
  credential plugin reads. bash and MCP servers never get them, in the
  sandbox or on this machine; naming the model's key's variable is
  refused. It may be set without `executor.command`, for an `-executor`
  given each time, and a project's config may not set it.
- Added: `executor.command` may be an array of the program and its
  arguments, used exactly: `["docker", "exec", "-i", "box", "dax",
  "execute", "-root", "/my work"]`.
- Changed: a command line, the string form of `executor.command`,
  `-executor`, `mcp_servers`' commands, `-mcp`, `/mcp add` and
  `-api-key-command`, is split as a shell splits words (`'...'`, `"..."`
  and `\` quote, so `-root "/my work"` is one argument), with nothing
  expanded (`$VAR`, `~` and globs are those characters) and no shell
  run. It was split on spaces, a quote an ordinary character, so a
  line that relied on that, such as an ssh remote command in single
  quotes, now needs the quoting a terminal would: `ssh host "bash -lc
  '...'"`. An unclosed quote is an error.
- Changed: `agent.DialExecutor` takes the command as argv (`[]string`),
  used exactly, not a line split on spaces, and `agent.ExecutorOptions`
  has `LauncherEnv`, the launcher's own variables.
- Changed: the terminal client prints the executor's line before it
  takes the screen without `-v` too, so where the tools act is on the
  terminal when it exits.
- Fixed: what the executor's launcher and `dax execute` write to
  standard error no longer draws over the terminal client: it is held,
  cleaned, with dax's other warnings while the client has the screen
  and printed when it exits, above the resume command (the last 64 KiB
  of it). dax's own warnings during the run are held the same way. In
  the REPL and with `-p` it is printed as it comes.
- Fixed: the model's key's variable never reaches the executor's
  launcher, even when `pass_env` names it, as the docs said.

## v0.0.9 - 2026-10-09

- Added: with `-executor`, MCP servers run in the sandbox. Every server
  in `mcp_servers`, `-mcp` and `/mcp add` starts in `dax execute`, at
  its root and with the environment its commands get, over the process
  methods; it used to be refused, since it would have run on this
  machine. Only your config, the flags and `/mcp add` say which servers
  start: a project's config still may not name one, and the executor
  keeps no list. Their tools are `mcp__<name>__<tool>` as anywhere,
  asked about by default under your rules, recorded here, and make no
  facts request of the executor; their standard error comes back
  cleaned, and `/mcp remove` or the session's end stops them. An
  executor too old to start a process (dax v0.0.8 or earlier) fails a
  session with an MCP server, and `/mcp add`, with "update dax execute
  where it runs", and the server is not started here instead.
- Changed: `agent.Executor.Workspace` is a `workspace.Starter` whose
  `Start` starts the process in the executor, and `agent.Options` no
  longer refuses `MCP` beside `Executor`, nor `Session.AddMCP` with one.
- Added: `dax execute` starts a process for the session that asks, one
  that lives longer than a call with pipes to it, through custom
  JSON-RPC methods (`dax/process.start`, `.write`, `.read`,
  `.closeStdin`, `.signal`, `.wait`, `.close`) named in its
  capability's new `start` field; `executor.Remote.Start` is their
  client, a `workspace.Starter`'s `Start` over the connection. They are
  not tools, so no client of `dax execute` is offered one that runs a
  command. A session's processes are its own (another session's id is
  refused), at most 32 at once, with writes of at most 1 MiB, reads of
  at most 64 KiB and standard error bounded, and they end with the
  session's connection and at a signal to `dax execute`. A session
  with `-executor` runs its MCP servers this way.
- Changed: agenttool, mcpclient and mcpserver v0.0.21, for
  `mcpclient.WithClientSetup`, with which the executor's client
  registers the process methods.

## v0.0.8 - 2026-10-09

- Security: a read of a file of a project's skill through the skill
  tool, the skill file included when the instructions are loaded, is
  held to `read`'s asks and denies: it is decided as a read of
  `.dax/skills/<skill>/<file>` in the workspace and of what its links
  lead to, as `read`'s own call is, beside the skill tool's own rules.
  A `.dax/skills` that is a link elsewhere in the workspace
  (`-> ..`, `-> ../config`) offered that directory to the skill tool,
  which runs unasked, so a `.env` there was read without the question
  `read(.env)` gets, and a user's `deny read(config/**)` did not reach
  it. Now the secret-path asks and the user's and the project's asks
  and denies of `read` reach it; an allow of `read(...)` never allows
  it (the skill tool's own rule does), and the skills the user
  installed (`~/.dax/skills`, `skills_dirs`) are not held. A workspace
  that cannot read links asks about every project skill read.
  `-> ../.claude/skills` loads unasked as before. With `-executor` the
  claim reads the executor's files. What is left is the moment between
  the decision and the read. Closes #39.
- Security: an extension's matcher's own subjects may name only the
  extension's tools, as a facts claim's calls may. A subject that names
  another extension's tool is decided as a tool no rule names, so it
  asks; it was decided under that tool's rules, so a matcher could
  borrow, say, dax-coding's allow of `read` for a tool of its own.
- Added: `extension.Extension.HeldTo`, the other extensions' tools whose
  ask and deny rules hold one of the extension's tools, by what its
  facts claim names: such a call is a constraint (agentpolicy's
  `Subject.Constrain`), which can make the call ask or refuse it and
  never allows it. dax-skills holds `skill` to `read`.
- Added: `tool.PathCalls`, what a read of a path would touch as the
  file tools claim it, for an extension's claim of a read by another
  name.
- Changed: a tool an extension `Owns`, which its kit options add, is
  decided on its facts claim when it makes one, looked up in the kit at
  each decision, and the facts hook applies its rewrite; a matcher that
  gives subjects of its own for such a tool is an error at start, as
  for a tool of `Tools`.
- Changed: `factspolicy.Subjects`, `SubjectsOf` and `Matchers` take
  the tools a claiming tool is held to (`held`, by tool for
  `Matchers`); nil holds none, as before. `factspolicy.Matchers` holds
  a matcher's own subjects to the extension's names
  (`factspolicy.Confine`), and `factspolicy.HookFor` is the facts hook
  over tools looked up at each call.
- Dependencies: agentpolicy v0.0.13, up from v0.0.12, for
  `Subject.Constrain`; agentskill v0.0.12, up from v0.0.11, for the
  skill tool's facts claim (`WithFileClaim`).
- Changed: the facts of a model response's calls are read together for
  the decisions about them. With `-executor`, a response costs two
  facts requests, one for its calls and one for the rewrites (stamps)
  they ask for, however many calls it has and whichever agent makes
  them: three reads cost the main agent two requests where they cost
  twelve, and a sub-agent two where they cost six. A call's verdict,
  the stamp it runs with, the decision about that stamp and the batch
  hold's reading of its siblings are one reading of the response. A
  request that fails blocks every call of the response. A sub-agent's
  question lets the reading go, so the calls after it are read again.
- Security: a bash redirect is decided as a write of the file it opens,
  as a file tool's path is: normalised from the directory the line is in
  (after a plain `cd`, and from the root as well) and through the links
  on its way, read through the workspace. Its target was its raw text,
  so a deny of `write(.env)` did not reach `echo x > notes` with `notes`
  a link to `.env`, nor `cd sub && echo x > ../.env`: the line asked
  (or ran, with `"fallback": "allow"`), and a yes wrote `.env`. A target
  whose links the workspace cannot read adds a subject no rule names, so
  the line asks; one whose links lead out of the workspace adds a write
  no rule names. Such a line is still never auto-allowed; once a person
  approves it or a rule allows it, it is stamped with what its redirects
  write and runs as typed only while they lead there, so a target that
  becomes a link to `.env` after the yes fails with "ask again" instead
  of writing it. Closes #47.

## v0.0.7 - 2026-10-09

- Added: with `-executor`, the project's files are the executor's,
  read through it: the root's `AGENTS.md`, `.dax/skills` and
  `.dax/config.json`, screened as a local project's are (a link out of
  the workspace, or out of the skills directory, is left out and
  reported). Nothing above the executor's root is read, from either
  machine. `dax execute` serves its workspace's files read-only as MCP
  resources under `dax-workspace:///{op}{?path}` (`stat`, `lstat`,
  `readdir`, `readlink`, `read`), every name through the workspace's
  confinement, a read bounded at 1 MiB and a listing at 10,000
  entries, and names the template in its capability as `files`. The
  project's config is read once the executor is connected and can only
  tighten, as anywhere; one that cannot be read (a link out, over 1
  MiB, the executor gone or silent for 30 seconds) stops the start, as
  does an executor whose files cannot be read at all. A single
  `AGENTS.md` or skills directory that cannot be read is left out and
  reported.
- Changed: a session refuses an executor that does not serve its
  files, which an executor of dax v0.0.6 does not: update `dax` where
  it runs. The capability's version stays 1; a v0.0.6 session ignores
  the new field.
- Changed: `agent.Executor.Workspace` reads the executor's files
  (`FS` implements `fs.StatFS`, `fs.ReadDirFS`, `fs.ReadFileFS` and
  `fs.ReadLinkFS`) instead of being empty, so a program built on dax
  reads the project's config through it as the command line does.
- Fixed: a project's skills directory that cannot be read for a reason
  other than a link is reported with that reason, not as a link
  outside the workspace.

## v0.0.6 - 2026-10-09

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
