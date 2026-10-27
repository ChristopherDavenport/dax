# dax host: a long-lived host of many conversations

Repository: dax (dax#113, its decisions of 2026-10-27). No sibling
release is needed: the host is a dax mode over what dax already
assembles (agentturn's `front/control` and `front/a2a`, agentsession's
`remote`, agentworkspace's `tunnel`), each mounted once per session or
once per host.

## The requirement

A session that hosts its own `workspace.hub` lives and dies with it: a
workspace host behind NAT dials again for every session, two sessions
cannot share one listening address, and a restarted `dax workspace
-connect` is refused for the session's life. `dax serve` is the
long-lived process the user reaches from elsewhere, but it serves one
conversation.

So dax has two modes that share most of their machinery (decision 1):

- **One conversation:** today's `dax`, and `dax serve` serving that one
  session.
- **A host:** `dax host`, a long-lived process offering many
  conversations, each a session with its own kit, which owns the
  tunnel hub its sessions' workspaces dial into.

What stays fixed whatever the mode: one session is one conversation; a
session's workspace is fixed at its start and recorded in its header
(decision 3); its A2A conversations act in it (decision 2).

## `dax host`

A subcommand with a config of its own, as `dax box serve` has: `dax
host [-host-config FILE] [-config FILE]`. It reads no project and opens
no session at start. `-config` is the user's layer, as for any run
(`~/.config/dax/config.json`); `-host-config` is the host's,
`~/.config/dax/host.json` by default.

```json
{
  "listen": "0.0.0.0:7878",
  "tls_cert": "/home/me/.config/dax/host.pem",
  "tls_key": "/home/me/.config/dax/host-key.pem",
  "token_env": "DAX_HOST_TOKEN",
  "sessions": "/home/me/.dax/sessions",
  "dirs": ["/home/me/src"],
  "default_workspace": "build",
  "max_sessions": 8,
  "idle": "30m",
  "hub": { "token_env": "DAX_TUNNEL_TOKEN", "names": ["build", "laptop", "secrets"],
           "wait": "60s", "max_workspaces": 16 },
  "a2a": { "token_env": "DAX_HOST_A2A_TOKEN", "url": "https://agents.example.com", "escalate": false }
}
```

| Field | |
|---|---|
| `listen`, `tls_cert`, `tls_key` | one listener for everything: loopback unless both TLS files are given, as `dax serve -listen` |
| `token_env` | the control token (principal `attach`); default `DAX_HOST_TOKEN`, or one made and written to a private file, as serve's |
| `sessions` | the session store every session records in; default `~/.dax/sessions`, the user's |
| `dirs` | directories a session may name as its local workspace, each with what is below it; default the directory the host started in |
| `default_workspace` | a connected workspace's name for a session that names none; absent, the directory the host started in |
| `max_sessions`, `idle` | sessions open at once (default 8); how long one with no view, run or A2A task stays open (default 30m; `0` keeps it) |
| `hub` | the tunnel hub at `/tunnel`: the one token (decision 5), the names it enrolls (empty: any valid name), the longest wait a session may ask for, the workspaces connected at once |
| `a2a` | serves sessions opened with `a2a` to A2A callers, behind a token of their own (principal `a2a`); `url` and `escalate` are `-a2a-url` and `-a2a-escalate` |

What is the host's and what is each session's:

| The host's | Each session's |
|---|---|
| the listener, its TLS, the three tokens | its workspace and servers workspace, fixed at its start |
| the hub, its names, connected workspaces | its kit: extensions, policy engine, tools, MCP servers, sub-agents |
| the store, the private directory, the limits | its model and reasoning (within the host's provider), its policy tightenings |
| the provider and its key (the user's layer) | its views, its questions, its A2A conversations |

The host takes every token out of its environment at start, before it
starts any process, as `takeTunnelToken` does now.

## Opening, listing, attaching, closing

The host serves, behind the control token:

| Route | |
|---|---|
| `POST /host/sessions` | open a session from a request (below); answers its ID, name and attach URL |
| `GET /host/sessions` | every session the host has opened: ID, name, state (open, running, idle-closed), workspace, views attached, A2A conversations running |
| `DELETE /host/sessions/{id}` | close it: abort a run, end its kit; its record stays |
| `GET /host/workspaces` | the names connected: instance, root, scopes, the sessions using each |
| `/store/` | the store, read-only, as serve's |
| `/s/{id}/control/` | the session's control, with serve's commands (`serveCommands`) |

and behind the A2A token, for a session opened with `a2a`,
`/s/{id}/a2a` and its card at `/s/{id}/.well-known/agent-card.json`.

The control token holder is the user, as for `dax serve`: it opens,
attaches to and closes any session. An A2A caller reaches the A2A
routes of the sessions offered to it and nothing else: it opens no
session, names no workspace (decision 4), and reads neither the store
nor any control.

A session's URL holds across an idle close and a host restart: a
request to a session that is not open (an attach, a control call, an
A2A call) reopens it from its record (resume), its workspace the one
its header names, within `max_sessions`.

On the command line:

- `dax host ls URL`, `dax host open URL [-workspace NAME | -dir D]
  [-servers-workspace NAME] [-name N] [-model M] [-a2a] [-request FILE]`,
  `dax host close URL ID`: the routes above, the token from
  `$DAX_HOST_TOKEN` or `-token-file`, `-tls-ca` as attach's.
- `dax attach -session ID|NAME URL`: attach's code with
  `URL/s/{id}` as its base, unchanged below that. Without `-session`,
  `dax attach` asks `GET /host/sessions`: with one session open it
  attaches to it; with more it lists them and says to pass `-session`;
  a 404 is a `dax serve`, attached as now.

### The open request

```json
{ "name": "triage", "workspace": { "connected": "build", "wait": "2m" },
  "servers_workspace": { "connected": "secrets" },
  "model": "MODEL", "think": true, "effort": "medium",
  "policy": { "deny": ["bash(git push*)"] }, "agents": false,
  "a2a": true, "resume": "" }
```

The request takes the flags' place among the session's layers: the
user's (`-config`), then the project's `.dax/config.json` read through
the session's workspace, then the request. It is parsed by `config` as
a layer of its own kind, held to the project layer's rules (it may only
tighten) with these exceptions, each a choice among what the host
already has:

- `workspace.connected` (a name the hub enrolls) or `dir` (under one of
  `dirs`); `workspace.wait`, at most `hub.wait`; neither given is
  `default_workspace`.
- `servers_workspace.connected`.
- `model`, `subagent_model`, `think`, `effort`: the host's provider's
  models, as `/model` and `/think` choose them over control.
- `name` (a DNS label, unique on the host), `a2a`, `resume`.

It may not set what a project may not (the provider, an endpoint, the
instructions, skills, memory, peers, `mcp_servers`), any secret
(`api_key_env`, `api_key_command`, `pass_env`, a `token_env` or
`secret_env`), an allow rule, a carve-out, the extensions' allow rules
back on (`builtin`), `fallback: allow`, the policy off, `-trust-mcp`
or `-trust-skills`; each refusal names the field. MCP servers are added
after the open with `mcp-add` over control, as from any attached view.
A project's MCP set is trusted with a new control command,
`trust-mcp`, which is the user acting on the human plane, not a field
a request carries.

Where a session's tools act is the request's or the host's: the user
layer's `executor`, `box`, `workspace` and `servers_workspace` are not
read on a host, and the host says so once at start. A session in a box
or at an executor is not in this proposal.

`resume` names a recorded session: its workspace is the one its header
names, and a request that names another is refused (decision 3: fork,
or open a new session). A connected workspace is found by its recorded
ref, `http://<name>.tunnel.invalid`, and the root must match.

## What sessions share and keep apart

| | Shared | Per session |
|---|---|---|
| Store | one, the host's `sessions`; a session's A2A conversations are sessions in it | its record |
| Policy | the user's rules and the extensions' | the engine, its project's and its request's tightenings, its skill grants |
| MCP servers | none | every server, `"in": "host"` included, started by its own kit and restarted and recorded in its record; sharing one would mix two conversations' state in one process and one record |
| Memory | the store at `memory_dir`, as concurrent dax runs share it now (agentmemory's lock and anchored writes) | the project scope, its workspace's identity |
| Extensions | the compiled set (`dax.Main`'s) | which are on, and their tools, built over its workspace |
| Secrets | the host's environment, read on the host alone | none in any child: each kit's children lose them, as now |
| The provider | the client and the key command | the model and reasoning |
| Notes | the host's log | each line prefixed with its session's ID; a note is not an event, so an attached view does not see it, as with serve now |

Sessions that share a connected workspace (decision 6) act in one
directory with nothing ordering their writes against each other, as
two dax runs in one directory do now; each has its own `tool.Files`
lock and its own remote session, and so its own process table.

Limits: `max_sessions` open at once, the rest idle-closed and reopened
on demand. A session opens a remote session on its workspace only at
its first `Start` (an MCP server or other process there), and `dax
workspace -connect` holds at most 4 remote sessions by default; it
takes `-sessions N`, lowering the processes per session so that
sessions × processes × 4 plus transfers stays under the tunnel's
`MaxStreams` (8 sessions of 16 processes, say), which `tunnel.Serve`
enforces. The hub holds at most `hub.max_workspaces` connections. No
per-session spend or run budget.

## The hub

The host mounts one `tunnel.Hub` at `/tunnel` on its listener, beside
control, the store and A2A.

- **Names.** Each workspace host dials with `-name`; the hub enrolls
  any valid name (`tunnel.ValidName`), or only those in `hub.names`.
  The upgrade now carries `Dax-Workspace-Name`, and `authn.Hub` checks
  its MAC under the one token for the name the upgrade claims, where
  today it checks its one configured name. A `-connect` of an earlier
  dax sends no name and is refused, saying to upgrade.
- **One token** (decision 5), `hub.token_env`, proved both ways as now
  (`internal/authn`'s `Prover` and `Hub`, a nonce the hub issues, the
  instance id); never sent.
- **Newest replaces** (decision 6). `pin`'s rule, the first instance
  held for the session's life, becomes the library's: a second
  connection for a name replaces the first. dax adds one rule beside
  it: an instance that was replaced is retired for that name, and its
  later upgrades are refused 403 ("replaced by a newer run"), so two
  hosts dialling one name do not take it from each other at every
  redial; the replaced `-connect` stops and says why. The retired ids
  are the host's memory alone, so a host restart forgives them.
- **A replacement, seen by a session.** The enrollment notices a new
  instance before the library takes its connection. From then, each
  session on that name holds its workspace's new requests, dials the
  name again (`Hub.Workspace`, a new remote session on the new run),
  and compares what the new run serves with what it recorded: the root,
  the scopes, and whether it starts processes and offers servers. The
  same: the session goes on through the new client, closes the old,
  and records a `dax.workspace.replaced` entry with the new instance
  id; its processes on the old run are gone (a call cut off is
  answered as having possibly run, and MCP servers there start again
  at their next call, each a `dax.mcp.restart`). Different: every tool
  call of the session fails, saying the workspace changed and to fork
  or open a new session (decision 3). The library documents that a
  workspace dialled before a replacement must not be written through;
  the hold and the re-dial are dax keeping to that.
- **A drop without a restart** is as now: requests wait up to 10
  seconds for the host (`hubRequestTimeout`), streams their own bounds,
  and processes outlive a drop shorter than the 30-second lease.
- **Wait** (decision 7). A session naming a name that is not connected
  waits up to its `wait` (default `hub.wait`, 60 seconds) at its open
  or reopen, then fails the open with the line to run on the
  workspace's host. It never acts in the host's directory instead.
- **The record.** The header records the workspace as now (`remote`,
  `http://<name>.tunnel.invalid`, the served root) and, under
  `dax.host`, the name and the instance id it started with. `dax -list`
  with that name lists by the ref as now; `dax host ls` and
  `/host/workspaces` say which sessions use which name.
- **Servers-only workspaces** (decision 8). The same hub enrolls a
  workspace connected with `-scopes servers -servers FILE`. A session
  names it with `servers_workspace.connected`, and
  `checkServersWorkspace` refuses one granted more than servers, as
  now; a name whose grant is servers alone is refused as a session's
  `workspace` (`config.ServersOnly`).

## What `dax serve` keeps, and what moves

Recommendation: **`dax serve` stays the one-conversation mode and
shares its code with the host; it does not become a host of one
session.**

- Decision 1 puts `dax serve` in the one-conversation mode.
- Its inputs are a run's: the session is built from the same flags and
  config as a front's (`-resume`, `-model`, `-workspace`, the user's
  `box`), which a host never reads from its command line.
- Its URLs and tokens are one session's: an A2A peer configured with
  `https://host:7878` would break if the routes moved under `/s/{id}`.
- A host adds an administrative surface (open, close, enrollment of
  any allowed name) that a one-off serve should not expose.

Shared, factored out of today's code so the two cannot drift:

- the session's build, now inline in `run` (settings, tokens taken,
  workspace dialled or waited for, `agent.Options`, `openSession`),
  into one function both call with their layers;
- the served session: `serveHandler` mounted under a prefix, the
  `viewed` session, `Answering` held while it is open, `serveCommands`;
- `a2aPeers` per session, with its conversations under the host's
  private directory (one lock for the host, a subdirectory per
  session) where serve keeps its own `a2aDir`;
- `listen`, `loadTLS`, `makeToken`, `closeSession`;
- the hub's code: `startHub` split into its listener and its handler,
  so the host mounts it on its own mux.

`workspace.hub` on a single session stays, with the new pin rule, so a
restarted `-connect` no longer costs a new session there either.

## Lifecycle

- **Shutdown** (a stop signal): the host stops taking opens, A2A work
  and enrollments; aborts each session's run and waits a drain for its
  end to be recorded, as serve does; closes each session (its MCP
  servers, its remote session, so its processes on the workspace's host
  end) within `closeSession`'s deadline; then the hub, then the
  listener. The workspace hosts dial again until the host is back,
  under the lease rule `-connect` has now.
- **Idle:** a session with no view, no run and no A2A task for `idle`
  is closed: its kit ends, its record and A2A conversations stay, and
  the next request to it reopens it.
- **Ended:** `DELETE /host/sessions/{id}` closes it the same way. It
  stays listed and reopenable; removing a record is the store's (`-gc`).
- **Restart:** the host keeps which sessions it opened (ID, name, the
  open request, whether A2A is on) in its private directory,
  `~/.dax/host/<hash of the store's root and the host config's path>`,
  locked while it runs. After a restart every session is idle-closed,
  and reopens on its first request, waiting for its workspace.

## Security notes

**What the host trusts.** Its own config and environment: the provider
and its key, every `secret_env`, the three tokens. The user's layer,
as any run does. The holder of the control token. And every workspace
host that holds the tunnel token, alike: with one token any of them
can enroll as any allowed name, and since the newest connection
replaces the older, take that name's sessions' file reads, writes and
commands. Retiring the replaced instance makes that loud (the real
`-connect` stops and says why, and the sessions record the
replacement) but does not prevent it, and a host that serves the same
root passes the re-check. Give one host's token only to workspace
hosts trusted alike, list their names in `hub.names`, and see #134 for
enrollment per workspace. In particular a servers-only host, which may
hold a secret, holds the token that enrolls the build host's name
too; today's single session refuses one token for both connections,
which one host token cannot keep.

**The control token** is the user's agent on the host. Its holder opens
sessions in any allowed directory or name, prompts, answers every
question the policy asks, adds MCP servers (commands on the host or in
the workspace, with the scrubbed environment), trusts a project's MCP
set, reads every session in the store, and closes sessions. It does not
get a secret: none is in a child's environment or any answer. It does
not widen a session's policy through a request, nor name an endpoint
or a provider; those are the host's config. Off loopback it crosses
only TLS. A leaked one is revoked by restarting the host with another.

**An A2A caller** reaches the sessions opened with `a2a`, by URL, and
nothing else: no store, no control, no open, no choice of workspace or
model. Its conversations run under its session's kit and workspace;
the policy's questions go to a view attached (with none, refused), or
to the caller under `escalate`. A call to an idle-closed session
reopens it, so a caller can make the host load sessions it was
offered, within `max_sessions`.

**Between sessions** there is no boundary but separate kits: one OS
user, one store, one memory directory. A session's bash on the host
reaches the other sessions' directories; sessions on one connected
workspace share its directory and its user there. The host is one
user's, as dax is.

**The hub** is as now: the token never sent, the proof binding the name
and a nonce the hub issued but not the connection (docs/sandbox.md,
"Proving the hub"), so off loopback it wants `tls_cert` and `wss://`.

## Steps

Each is one pull request that ships alone. None needs a sibling
release.

1. **The hub's names and the new pin rule**, on today's `workspace.hub`:
   `Dax-Workspace-Name` and `authn.Hub` checking the claimed name;
   newest replaces, the replaced instance retired; the session's hold,
   re-dial and re-check, `dax.workspace.replaced`; `-connect`'s message
   for a retired run. A restarted `-connect` then keeps the session.
2. **Factor the shared code** out of `run`, `serve.go` and
   `serve_a2a.go` (the session's build, the served session under a
   prefix, the hub's handler apart from its listener), with no change
   in behaviour.
3. **`dax host` with local sessions**: the host config, the listener
   and tokens, `/host/sessions`, `/s/{id}/control`, `/store`, the open
   request as a config layer and its refusals, `trust-mcp`, idle close
   and reopen, the private directory, shutdown; `dax host ls|open|close`
   and `dax attach -session`.
4. **The hub on the host**: `/tunnel` on the host's listener,
   `workspace.connected` and `servers_workspace.connected`, `wait`,
   `hub.names`, the scopes checks, `/host/workspaces`, `dax.host` in
   the header, and `-connect -sessions N`.
5. **A2A on the host**: `/s/{id}/a2a` and its card, the host's A2A
   token, each session's conversations in its own directory under the
   host's, a call reopening its session.

Each step carries its docs: docs/sandbox.md (a section on the host
beside "A workspace that dials in", its limits updated), docs/design.md
(Planes and Fronts), the README's tables, AGENTS.md's layout.

A follow-up, not needed for any step: agentworkspace's tunnel could
give a workspace held to the connection it was dialled over, failing
its requests after a replacement instead of sending them to the new
host; dax's hold would then be that.

## Open questions

- Keep `workspace.hub` on a single session once the host ships, or
  retire it and send hub users to `dax host`?
- The default store: the user's `~/.dax/sessions`, so a project's `dax
  -list` shows its host sessions and the control token reads local
  ones, or a store of the host's own?
- Several hosts each enrolling a `build` record one identity (the ref
  is the name, the root beside it), so one `-list`, memory scope and
  MCP trust. Should the host's identity enter the ref, at the cost of
  changing it for sessions already recorded?
- Does a host need per-session budgets (model spend, runs at once)
  before it is left running unattended?
