# Where an MCP server runs: provided by the workspace, or called independently

The user's requirement (2026-10-25): "The MCP servers would need to be
able to be either provided by the workspace, or able to be called
independently." This is dax's design for it; agentworkspace v0.0.3
(`ServerProvider`, `mcpworkspace.CommandTransport`, `ServerTransport`
and the server methods) is the sibling half.

## Before

A configured server was a stdio command, and where it ran was implied:
in the workspace when the workspace could start a process, otherwise on
this machine. With `-executor` every server ran in the executor or not
at all, so nothing could be called beside a sandbox. A box host's MCP
units were the one case of servers the workspace provided, reached
through a dax-only route: the handshake listed them, and the session
started `["dax-unit", NAME]`, a reserved command the box host
intercepted. A project's config could name no server.

## The two kinds

**Provided by the workspace.** Two ways:

- A command the config names, started in the workspace: `in:
  "workspace"`, the default and the old behaviour (in the workspace when
  it is a `Starter`, here when it is not; with `-executor`, in the
  executor or failing). The transport is
  `mcpworkspace.CommandTransport`.
- A server the workspace offers of its own (`agentworkspace.
  ServerProvider`): listed with `agentworkspace/servers.list` and
  started by name with `servers.start`. A box host answers both for the
  session's MCP units, which replaces `dax-unit`. The session lists the
  workspace's servers when it opens and connects to every one, as it
  connected to every unit; its tools are `mcp__<name>__*`, and a name a
  configured server also has is an error, as it was for units. A box's
  confinement (`box.confine`) still covers these servers' tools, and
  only these.

**Called independently**, whatever the workspace:

- `in: "host"`: a command started on this machine, with the scrubbed
  environment a server here always gets, even with `-executor`.
- `url`: a streamable HTTP server called from this machine. A token,
  when the server needs one, is read from the variable `token_env`
  names and sent to the URL's origin only: no redirect off it is
  followed, and the variable is taken out of every child's environment,
  as dax-a2a's are. https, or http to a loopback host only.

Their tools are never confined: nothing a sandbox says covers them.
They ask by default, as every MCP tool does.

## The project's servers

A project's config may name servers now, of either kind, which is the
one exception to "a project config field is refused unless it can only
tighten". It may not name a `token_env`, since that would send the
user's credential wherever the repository says; and a name the user's
own config gives is an error, so a repository cannot stand in for a
server the user trusts.

A project's servers start only when the user trusts that project's
exact set: `-trust-mcp` trusts it and records the trust in
`~/.dax/mcp-trust.json` (private, the directory made 0700), keyed by
the project's identity as `-list` has it (the workspace's descriptor:
kind, ref and root) and holding the SHA-256 of the set's canonical
JSON. A later session of the project with the same set starts them
without the flag; a set that changed, in any server's name, command,
URL or placement, is untrusted again. An untrusted set does not start,
and the session says which servers and how to trust them; the user's
own servers start as ever.

## What changes on the wire

The box host no longer intercepts `agentworkspace/process.start` of
`["dax-unit", NAME]`; it answers `servers.list` and `servers.start`, and
every process method on the ids it gave, as before. A session and a box
host must therefore be the same side of this change, as with every box
host change: a box is made again for another dax anyway.
