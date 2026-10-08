# Proposals to the siblings

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
| [execution-boundary.md](execution-boundary.md) (primary) | dax first (an `executor` with an in-process and a remote form); agenttool (`mcpserver`/`mcpclient`: a facts call beside MCP's tool calls), agentpolicy (subjects with a context) | `dax execute` inside a sandbox and `-executor <url>`: the in-process form needs no sibling release; the remote form waits on the facts call |
| [agentturn-control.md](agentturn-control.md) (optional, later) | agentturn (`Control`, questions as events, `front/control`), agentkit (`Kit.Control`) | `dax serve` / `dax attach`: every front over the human plane's control on another machine, with dax's model, think and MCP controls as commands |
| [agentconsole-view.md](agentconsole-view.md) (optional, later) | agentconsole (narrows to the view; `client/native` and `client/kitbackend` leave) | dax's fronts hand the terminal client `kit.Control` and a record instead of a `client.Backend`; blocked on agentturn-control first |
| [agentworkspace-module.md](agentworkspace-module.md) | new: agentworkspace | the workspace interface an executor runs over, `dax/workspace` replaced by a rename; its remote workspace is the primitive cut, for storage-only hosts (see execution-boundary.md) |
| [instruction-sources-fs.md](instruction-sources-fs.md) | agentsmd (`Options.FS`), agentkit (a release that pins it) | AGENTS.md read through the workspace; skills need no sibling change and the project config is dax's own |
| [agentturn-peers.md](agentturn-peers.md) | agentturn (`tools/a2a`, `front/a2a`), agentsession (a `peer` link relation) | an `ext/a2a` extension for calling peers, and `front/a2a` in `dax serve` |
