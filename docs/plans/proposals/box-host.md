# The box host: a box behaves the same here and on another machine

Repository: dax. No sibling release is needed: the box host is a dax
mode, `dax box serve`, over the executor protocol dax already speaks
(execution-boundary.md), and `internal/box` (#98) is its engine.

## The requirement

Local is remote with more direct connectivity. A session that runs its
tools in a box on this machine and one that runs them in a box on a
build host must be configured the same way, decide the same way, and
see the same box: the same image, limits, internet, MCP servers and
secrets handling. Only how the session reaches the box differs: a
child process here, ssh there, HTTPS once an executor connection can
be authenticated.

#98 makes the box from the session's process: it runs docker on the
machine the session runs on, from the session's own config. That
cannot be the remote shape, since the remote machine's docker, its
secrets and its limits are not the session's to drive. So the work
that makes boxes moves to a process that runs where docker runs, and
the session talks to that process the way it talks to any executor.

## Three roles

```
control (the session: model, key, policy, record)
   │  the executor protocol: MCP plus the facts call, over stdio
   │  (a child here, ssh there; HTTPS once authenticated)
   ▼
box host: `dax box serve`, on the docker machine; its own config
   ├─ the project box: dax execute (read, write, edit, bash, …)
   ├─ the MCP boxes (step 2): one per server
   └─ the egress sidecars: the proxy #98 built, one per box
```

- **Control** is the session, as execution-boundary.md has it: the
  model and its key, the policy and every decision, the record. It
  never runs docker and never holds a box's secrets.
- **The box host** is on the execution side of the boundary, outside
  every box. It owns docker on its machine, the box host config (what
  boxes it makes, at most how large, with which internet), and the
  sources of the MCP servers' secrets. It decides nothing about a
  call: it makes boxes, and passes the executor protocol through.
- **The executors** are what they are today: `dax execute` in the
  project box serves the tools, makes each call's facts and checks its
  stamp, under its own per-process key. The box host does not see the
  key and does not make or read stamps.

This extends execution-boundary.md's split rather than changing it.
Control still trusts an executor's facts only about the box itself; a
box host that lies can only make another box than the one asked for,
which the box's descriptor (kind, ref, root) and the record show. The
box host is trusted the way docker on that machine is trusted: by
whoever runs it, the user here, the machine's administrator there.

Where each secret sits:

| Secret | Where it is | Never in |
|---|---|---|
| the model's key | control | the box host, every box |
| an MCP server's token (step 2) | the box host's sources (env, command, file), then that server's MCP box | control, the session's link, the project box, the record |
| ssh's agent (`SSH_AUTH_SOCK`) | control, given to the ssh launcher alone (`executor.pass_env`) | the box host's boxes |

## The relay

`dax box serve` speaks stdio MCP on its standard input and output. It
reads the session's first message, its handshake: `initialize`, or the
stateless `server/discover` that go-sdk v1.8's client tries first
(SEP-2575), whose client capabilities are in its `_meta`. From it the
box host reads what the session asks of the box: under the executor's
capability key, beside `maxReadBytes`, a `box` object with the
profile, the MCP servers wanted, and tightenings. It resolves that
against its config (below), makes or starts the box with
`internal/box.Ensure`, starts `docker exec -i <box> dax execute`
there, forwards the first line unchanged, and from then on copies
bytes both ways. It never rewrites, reorders or answers a message but
the first: requests, responses, notifications and the server's own
requests (pings, elicitation) pass through as they came.

A request the box host refuses (a profile it does not have, a
tightening that widens, an MCP server in step 1) is answered as the
first request's JSON-RPC error with the reason, and so is every
request after it until the session hangs up: a client whose
`server/discover` fails falls back to `initialize`, and must read the
reason there too. When the in-box executor exits, the box host exits with
it, so the session's redial (agent/executor.go) runs the launcher
again: the box host, again.

Why a relay and not a second executor server: the facts and stamps
are the in-box executor's, and a relay cannot change them. A server
that re-served the tools would sit between the facts and the call.

In step 2 the relay answers one more thing itself: a `dax/process.start`
that names a catalog MCP server (below) starts that server in its MCP
box, and the relay answers that process's methods. Every other process
start, and every other method, still passes through.

## The box host config

Read only by the box host: `~/.config/dax/box-host.json` on the
machine it runs on, `-config` to name another. On this machine it is
the user's file; on a build host it is the administrator's. Absent, the
box host runs with built-in defaults that equal #98's: one profile,
`default`, `golang:1.26-bookworm` under `runsc`, 4g, 2 CPUs, 512
processes, no internet.

```json
{
  "runtime": "runsc",
  "limits": { "memory": "8g", "cpus": 4, "pids": 1024 },
  "images": ["golang:*", "node:*"],
  "default_profile": "go",
  "profiles": {
    "go": { "image": "golang:1.26-bookworm", "internet": "allow",
            "allow": ["proxy.golang.org", "sum.golang.org"] },
    "vm": { "image": "golang:1.26-bookworm", "runtime": "kata" }
  },
  "mcp": {
    "github": {
      "build": { "base": "node:24-bookworm", "run": "npm i -g @modelcontextprotocol/server-github" },
      "command": "mcp-server-github",
      "secrets": { "GITHUB_TOKEN": "env:GITHUB_TOKEN" },
      "internet": "allow", "allow": ["api.github.com"],
      "files": "none"
    }
  }
}
```

- `runtime` is the default runtime; a profile may name its own. It is
  checked only as the name of a runtime docker has registered, so
  gVisor's `runsc` and Kata's `kata` (or `io.containerd.kata.v2`) are both taken,
  and nothing in the box host behaves differently by runtime.
- `limits` are ceilings: no profile, and no tightening, goes past them.
- `images` are glob patterns an image (a profile's, an MCP server's
  base) must match.
- `profiles` are the boxes the box host makes. A field a profile leaves
  out takes the built-in default.
- `mcp` is the catalog (step 2): each server's image (`image`, or
  `build`: a base and a command whose result is cached by a hash of the
  two), its `command`, its `secrets` as references resolved on the box
  host (`env:NAME`, `command:LINE`, `file:PATH`), its own internet
  (`internet`, `allow`, through its own sidecar), and its access to the
  project (`files`: `none`, the default, `ro` or `rw`). Step 1 reads and
  validates it, and refuses a session that asks for a server.

## The session's `box`

In the user's config, user layer only, as #98's:

```json
{ "box": { "host": "local", "profile": "go" } }
{ "box": { "host": "ssh://me@build-host:2222", "profile": "go",
           "project": "/srv/src/app", "remote_dax": "/usr/local/bin/dax" } }
```

- `host`: `local` (the default) or `ssh://[user@]host[:port]`.
- `profile`: one of the box host's; empty is its default.
- `mcp`: catalog server names (step 2).
- `tighten`: `memory`, `cpus`, `pids`, `internet`, `allow`, each only
  ever narrowing what the profile gives. `internet` orders
  off < allow < on; `allow` must be within the profile's.
- Local only: `host_config`, the box host config file the local box
  host reads in place of the default.
- ssh only: `project` (the path on that host; default the same path as
  here), `remote_dax` (the box host's dax; default `dax` on the login's
  PATH), `ssh` (more arguments for ssh, before the host).
- Flags: `-box`, `-box=false`, `-box-host`, `-box-profile`, and
  `-box-internet`, a tightening for one run.

The session cannot name an image, a runtime or a secret: those are the
box host's. What it can say only narrows.

## Project files

The box mounts the box host's own copy of the project, at its own
path, read-write; an MCP box with `files` other than `none` mounts the
same directory at the same path (step 2). Two runsc containers sharing
a bind mount see each other's writes at once (`file-access-mounts`
defaults to shared); whether a file watcher in one sees another's
changes is not known. Here the project is the session's directory. On
a build host it is a path there: `box.project`, default the same path;
cloning a repository into a directory the box host keeps is step 3.

## Identity

The project's identity for `-list` and memory stays the workspace's
descriptor: kind `container`, ref the box's name, root the project's
path, as the in-box executor reports it. The session's header keeps a
hash of the executor's address, which is the launcher's words: the
local box host's command or the ssh command line, each naming the
project, so two projects on one host are two addresses.

## Transports

- **Here:** the session runs its own binary as `dax box serve` with
  the project, over stdio. No port, no token.
- **ssh, now:** `ssh -T -o BatchMode=yes [-p PORT] [user@]host
  <remote_dax> box serve -project <path>`, over stdio. The pipe is the
  credential, as for `dax execute` over ssh today; ssh's agent reaches
  the launcher through `executor.pass_env`.
- **HTTPS, later:** `dax box serve -listen`, once an executor
  connection authenticates its client (execution-boundary.md,
  Security). Until then a box host listens nowhere.

## Runtimes

The box host takes any runtime docker has registered; what differs
between them is what has to be verified before a runtime is called
supported, not code.

- **gVisor (`runsc`).** Here it runs on the KVM platform
  (`runsc install -- --platform=kvm`): the default platform, systrap,
  panics on WSL2's 6.6 kernel at the first exec. Under runsc, Docker's
  embedded DNS (`127.0.0.11`) does not answer on user-defined networks,
  which is why the egress sidecar runs under `runc`; hot `docker network
  connect` does not reach a running sandbox. Verified on this machine.
- **Kata Containers (`io.containerd.kata.v2`, registered as `kata`).**
  A VM per container; needs `/dev/kvm`. Verified here with Kata 4.2.0's
  static bundle (runtime-rs, QEMU, virtio-fs), nested under WSL2:
  - Docker's embedded DNS does not answer in a Kata box either (it is
    on `127.0.0.11` in the container's namespace; the guest's loopback
    is its own), so the sidecar stays under `runc` for every runtime.
    The internal network, the sidecar at a fixed address and the three
    internet modes behave as under gVisor.
  - Writes to a shared bind mount are seen by another Kata container
    and by the host at once (new files and directories, appends, a
    rewrite under an open descriptor) with virtio-fs's default
    `cache = "auto"`: MCP boxes can share the project.
  - The dax binary's single-file mount works, read-only.
  - `--memory 4g` gives the guest about 3.9 GiB, and QEMU takes about
    360 MiB more on the host while idle; `--cpus 2` is three vCPUs.
- **Process limits, both sandboxes.** `--pids-limit` is a cgroup on the
  host, which neither gVisor's nor Kata's processes are counted in:
  under each, 1000 processes started with `--pids-limit 100`. The box
  adds `--ulimit nproc=<pids>` under any runtime but `runc`, which the
  sandbox's kernel enforces for the box's user (95 and 97 started).
  Under `runc` the ulimit would count the user's every process on the
  host, so `--pids-limit`, which holds there, is the limit.

## What moves from #98

- `internal/box` is the box host's engine, unchanged in what it makes:
  the hardened run, the internal network, the sidecar and its rules.
- The settings that make a box (image, runtime, limits, internet)
  move from the session's `box` to the box host config's profiles;
  `-box-image` and `-box-runtime` go, since the image and the runtime
  are the box host's. #98 is unreleased, so nothing released changes.
- `dax box up|ls|down` keep acting on this machine's docker in step 1;
  against a box host elsewhere is step 3.

## Steps

1. **The relay, profiles, here and over ssh** (this change): the box
   host config, `dax box serve` over stdio, the session's `box` with
   `host`, `profile`, `tighten`, the local child and the ssh launcher.
   The catalog is validated and refused.
2. **MCP boxes:** the catalog; image builds cached by a hash of base
   and command; secrets resolved on the box host into the server's box
   alone; each server's internet through its own sidecar; `files`
   mounts; `dax/process.start` for a catalog server answered by the
   relay.
3. **Remote projects and administration:** a box host that clones a
   repository into a directory it keeps; `dax box ls` and `down`
   against a box host elsewhere.
4. **HTTPS**, after executor connections authenticate.

## Open questions

- Should a session be able to ask for a profile's image to be built
  from a declaration, as the catalog's servers are, or is the image
  always the administrator's choice?
- One box host per session (each a process, as now) or one long-lived
  box host per machine that sessions attach to? Over ssh the former is
  free; over HTTPS the latter is the shape.
- A box host serving more than one user: each user's boxes are named
  by their project's path today; a multi-user host needs the user in
  the name and in the box host config's limits.
- Should the box host's resolved settings (profile, limits, internet)
  reach the session's record, beside the workspace's descriptor?
