# Proposals to the siblings

The plan they serve, its order and how each fits its repository, is
[../README.md](../README.md).

Drafts of issues for the libraries dax assembles, written in each
repository's terms. None is filed. Each says what dax does once it ships;
dax's step waits on its release, as the workspace's rules require
(siblings are depended on by published tag).

The primary one is the execution boundary: everything that touches the
world can run in a sandbox, and the rest of a turn is orchestration whose
placement is a choice. The others are either what a remote executor
builds on or choices about where the orchestration runs.

| Proposal | Repository | dax's next step blocked on it |
|---|---|---|
| [execution-boundary.md](execution-boundary.md) (primary) | dax first (an `executor` with an in-process and a remote form); agenttool (facts as a per-call claim in the contract, carried over MCP), agentpolicy (subjects from facts, with a context) | `dax execute` inside a sandbox and `-executor <url>`: the in-process form needs no sibling release; the remote form waits on the facts call |
| [agentturn-control.md](agentturn-control.md) (built; dax serve and dax attach, #64) | agentturn (`Control`, questions as events, `front/control`), agentkit (`Kit.Control`) | `dax serve` / `dax attach`: every front over the human plane's control on another machine, with dax's model, think and MCP controls as commands |
| [agentconsole-view.md](agentconsole-view.md) (built) | agentconsole (the view: record and events in, intents out; `client/native` and `client/kitbackend` left) | dax hands the terminal client its session as source and controller (`client.Direct`) and a record, instead of a `client.Backend` (#62) |
| [agentworkspace-module.md](agentworkspace-module.md) | new: agentworkspace | the workspace interface an executor runs over, `dax/workspace` replaced by a rename; its remote workspace is the primitive cut, for storage-only hosts (see execution-boundary.md) |
| [instruction-sources-fs.md](instruction-sources-fs.md) | agentsmd (`Options.FS`), agentkit (a release that pins it) | AGENTS.md read through the workspace; skills need no sibling change and the project config is dax's own |
| [box-host.md](box-host.md) | dax only (`dax box serve`, the box host config) | a box that behaves the same here and on a build host: step 1 is the relay and ssh, step 2 units (every tool group an envelope of files, connectivity and secrets), step 3 checkouts the box host keeps and `dax box status|down|pull` through it; HTTPS follows |
| [mcp-placement.md](mcp-placement.md) | agentworkspace (`ServerProvider`, `mcpworkspace.CommandTransport`, `ServerTransport`, the server methods; v0.0.3) | MCP servers provided by the workspace (started in it, or offered by it: a box host's units) or called independently (`"in": "host"`, `url`), and a project's servers once trusted (`-trust-mcp`) |
| [agentturn-peers.md](agentturn-peers.md) | agentturn (`tools/a2a`, `front/a2a`), agentsession (a `peer` link relation) | an `ext/a2a` extension for calling peers, and `front/a2a` in `dax serve` |
| [conversation-host.md](conversation-host.md) (a candidate library; nothing built) | new: a host of many live sessions above agentkit, incubated in dax first; gaps in agentworkspace (`tunnel`'s replaced signal), agentsession (`Releaser`, claiming a ref, a key as a ref name), agentturn (`Deliver` on `Control`) and agentkit (an unattended control) | none: `dax host` (PR #135, #113) can be built without it; it asks whether that host's registry and dexclaw's should be one library |
