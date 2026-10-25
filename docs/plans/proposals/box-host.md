# The box host: a box behaves the same here and on another machine

Repository: dax. No sibling release is needed: the box host is a dax
mode, `dax box serve`, over the executor protocol dax already speaks
(execution-boundary.md), and `internal/box` (#98) is its engine.

## The requirement

Local is remote with more direct connectivity. A session that runs its
tools in a box on this machine and one that runs them in a box on a
build host must be configured the same way, decide the same way, and
see the same box: the same image, limits, internet, units and
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
   ├─ the workspace unit: the project box, dax execute (read, write, edit, bash, …)
   ├─ the MCP units (step 2): one container per server
   └─ the egress sidecars: the proxy #98 built, one per unit
```

- **Control** is the session, as execution-boundary.md has it: the
  model and its key, the policy and every decision, the record. It
  never runs docker and never holds a box's secrets.
- **The box host** is on the execution side of the boundary, outside
  every box. It owns docker on its machine, the box host config (what
  units it makes, at most how large, with which files, internet and
  secrets), and the sources of those secrets. It decides nothing about a
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
| an MCP unit's token (step 2) | the box host's sources (env, command, file), then that unit's container | control, the session's link, the workspace unit, every other unit, the record |
| ssh's agent (`SSH_AUTH_SOCK`) | control, given to the ssh launcher alone (`executor.pass_env`) | the box host's boxes |

## The relay

`dax box serve` speaks stdio MCP on its standard input and output. It
reads the session's first message, its handshake: `initialize`, or the
stateless `server/discover` that go-sdk v1.8's client tries first
(SEP-2575), whose client capabilities are in its `_meta`. From it the
box host reads what the session asks of the box: under the executor's
capability key, beside `maxReadBytes`, a `box` object with the
profile, the MCP units wanted, and tightenings. It resolves that
against its config (below), makes or starts the box with
`internal/box.Ensure`, starts `docker exec -i <box> dax execute`
there, forwards the first line unchanged, and from then on passes
every message through, a line at a time, as it came: requests,
responses, notifications and the server's own requests (pings,
elicitation). It rewrites one kind of message, the executor's answer
to the handshake, to which it adds what it resolved (Units, below), and
answers one kind itself, the process methods of a unit's server.

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
whose arguments are `["dax-unit", NAME]`, for one of the session's MCP
units, starts that unit's server in the unit's container (`docker exec
-i`), and the relay answers that process's methods, with ids of its own
that cannot be the executor's. `dax-unit` is a reserved word, not a
program: a `dax execute` with no box host in front fails it as a
program not found. Every other process start, and every other method,
still passes through.

## The box host config

Read only by the box host: `~/.config/dax/box-host.json` on the
machine it runs on, `-config` to name another. On this machine it is
the user's file; on a build host it is the administrator's. Absent, the
box host runs with built-in defaults that equal #98's: one unit,
`workspace` (`golang:1.26-bookworm` under `runsc`, 4g, 2 CPUs, 512
processes, the project read-write, no internet), and one profile,
`default`, of that unit alone.

```json
{
  "runtime": "runsc",
  "limits": { "memory": "8g", "cpus": 4, "pids": 1024 },
  "images": ["golang:*", "node:*", "python:*", "registry.example.com/mcp/*"],
  "default_profile": "go",
  "units": {
    "workspace": { "image": "golang:1.26-bookworm", "files": { "project": "rw" },
                   "internet": "allow", "allow": ["proxy.golang.org", "sum.golang.org"],
                   "exfiltration": "accepted" },
    "rag":       { "image": "registry.example.com/mcp/rag:1.0",
                   "command": ["rag-mcp"], "files": { "project": "ro", "state": "rw" } },
    "github":    { "image": "registry.example.com/mcp/github:1.0",
                   "command": ["github-mcp", "stdio"], "files": "none",
                   "internet": "allow", "allow": ["api.github.com"],
                   "secrets": { "GITHUB_TOKEN": "env:GITHUB_TOKEN" } }
  },
  "profiles": {
    "go":  { "workspace": "workspace", "mcp": ["rag", "github"] }
  }
}
```

- `runtime` is the default runtime; a unit may name its own. It is
  checked only as the name of a runtime docker has registered, so
  gVisor's `runsc` and Kata's `kata` (or `io.containerd.kata.v2`) are both taken,
  and nothing in the box host behaves differently by runtime.
- `limits` are ceilings: no unit, and no tightening, goes past them.
- `images` are glob patterns an image (a unit's, or a build's base)
  must match.
- `units` are the tool groups the box host makes, each one schema
  (Units, below).
- `profiles` are what a session asks for by name: exactly one
  `workspace` unit, the project box dax's own tools run in, and the
  `mcp` units beside it.

## The session's `box`

In the user's config, user layer only, as #98's:

```json
{ "box": { "host": "local", "profile": "go" } }
{ "box": { "host": "ssh://me@build-host:2222", "profile": "go",
           "project": "/srv/src/app", "remote_dax": "/usr/local/bin/dax" } }
```

- `host`: `local` (the default) or `ssh://[user@]host[:port]`.
- `profile`: one of the box host's; empty is its default.
- `mcp`: which of the profile's MCP units this session gets; empty is
  all of them, and one the profile does not have is refused.
- `tighten`: `memory`, `cpus`, `pids`, `internet`, `allow`, each only
  ever narrowing what the profile's workspace unit gives. `internet` orders
  off < allow < on; `allow` must be within the profile's.
- Local only: `host_config`, the box host config file the local box
  host reads in place of the default.
- ssh only: `project` (the path on that host; default the same path as
  here), `remote_dax` (the box host's dax; default `dax` on the login's
  PATH), `ssh` (more arguments for ssh, before the host).
- Flags: `-box`, `-box=false`, `-box-host`, `-box-profile`, and
  `-box-internet`, a tightening for one run.

The session cannot name an image, a runtime, a unit's files or a
secret: those are the box host's. What it can say only narrows.

## Project files

The workspace unit mounts the box host's own copy of the project, at
its own path, read-write; an MCP unit whose `files.project` is `ro` or
`rw` mounts the same directory at the same path (step 2), so a path one
names is a path the other reads. Two runsc containers sharing
a bind mount see each other's writes at once (`file-access-mounts`
defaults to shared); whether a file watcher in one sees another's
changes is not known. Here the project is the session's directory. On
a build host it is a path there: `box.project`, default the same path,
or a checkout the box host keeps of a repository the session names
(step 3, below).

## Step 3: checkouts, and administering a box through its box host

A box on another machine needs two things the first steps leave to
the user: a copy of the project there, and a way to see, remove and
take work back from the box without logging in. Both go through the
box host the session's `box` names, so a box here and a box there are
administered the same way, and a key held to one project (a forced
command) is held to it for these too.

### One entry point, three requests

`dax box serve` takes, as the session's first message in place of
`initialize`, one of three requests of its own, answers it, and exits:

| Request | Params | Answer |
|---|---|---|
| `dax/box.status` | none | the project's box and its units (name, unit, state, runtime, files, internet, image), and, for a checkout, its path, remote, branch, head and whether its worktree has changes |
| `dax/box.down` | `{state}` | removes the project's box, its units, their sidecars, networks and rules files, and with `state` their state directories, all on the box host's machine |
| `dax/box.pull` | none | a git bundle of the project's HEAD, base64, with its branch, head and whether the worktree had changes (which the bundle does not hold); refused above 256 MiB, with the size |

Each acts on the project the box host was started for (`-project` or
`-checkout`) and no other: the request names nothing. On the session's
side `dax box status`, `dax box down` and `dax box pull` start the box
host as `-box` does (this dax as a child, or the ssh launcher) and send
the request. `dax box down NAME` and `dax box ls` stay on this
machine's docker, for whoever administers it; so does `dax box up`. A
`down` over ssh removes the box host's own state directories there,
which a `down` here could not reach.

### Checkouts

The session's `box.checkout` is `{url, ref}`, with an ssh host only (a
local box host's project is the directory dax starts in) and not with
`box.project`. `ref` is a branch or a tag; none is the remote's
default branch. The launcher passes `-checkout URL [-ref REF]` in
place of `-project`, so `-list` keys the project on the URL, and an
administrator holds a key to one repository with
`command="dax box serve -checkout https://…",restrict`.

The box host clones only what its config's `checkouts.allow` matches:
glob patterns (`path.Match`) on the URL with its scheme, `https://`,
`ssh://` (`git@host:path` is read as `ssh://git@host/path`) or
`file://`, each with a trailing `/` and `.git` dropped. No `allow`
refuses every checkout, saying why. A URL with a password, or any user
on https (a token), is refused: credentials are the box host's own,
its git config, ssh agent or credential helper, never the session's.
The checkout lives at `<checkouts.dir>/<repository name>-<12 hex of
the SHA-256 of the URL>` (`dir` defaults to `<user cache>/dax/box-checkouts`)
and is then the project, as `-project` would make it: the same mount
rules, the box named for that path.

At each session's start the box host clones it if it is not there,
else fetches; it fast-forwards the worktree to the ref only when the
worktree is clean and on that ref with nothing of its own. A worktree
with changes, commits the remote has not, or another branch checked
out is left as it is, and the box host says so: a session's work is
never discarded. Git runs on the box host, with its environment and
credentials and `GIT_TERMINAL_PROMPT=0`, never in the box. The URL
(without any user), the ref and the head reach the session in the
handshake's `box` and the record's `dax.box` header, beside the units.

### Taking work back

`dax box pull [-branch NAME] [-force]`, in the local repository the
work should come to, fetches the bundle the box host gives into
`refs/heads/NAME` (default `dax-box/<repository name>`), after `git
bundle verify`. A branch already there is moved only forward unless
`-force`. Uncommitted changes in the box host's checkout are not in a
bundle; pull says when there were. A local box host's project is
already here, so pull is refused there.

## Units

Every tool group is a unit, and a unit is an envelope on three axes:
what files it has, what it can reach, and what secrets it holds. The
box host makes each unit's container from the envelope and enforces
it; the session's policy decides each call, as for any tool. The
workspace is a unit like the others, the one whose tools are dax's
own, clustered around the project's files.

| Unit | Files | Connectivity | Secrets |
|---|---|---|---|
| workspace: read, write, edit, bash | project `rw` | the profile's (`off` by default) | none |
| a local RAG | project `ro`, its own state `rw` | `off` | none |
| GitHub's MCP server | `none` | `allow api.github.com` | `GITHUB_TOKEN` |
| a docs search | `none` | `allow docs.example.com` | none |

One schema serves every unit:

- `kind`: `workspace`, or `mcp` (the default when `command` is set).
- `image`, or `build`: `{"base": IMAGE, "run": [LINE, …]}`, made once
  as `docker build` of `FROM base` and a `RUN` per line, and tagged
  `dax-unit/<unit>:<a hash of the declaration>`, so a changed
  declaration is a new image and an unchanged one is not built again.
- `runtime`, `memory`, `cpus`, `pids`: as the workspace's, within the
  config's `limits`. An MCP unit that names none gets 1g, 1 CPU and 256
  processes.
- `files`: `"none"`, or `{"project": "ro"|"rw", "state": "rw"}`. The
  project is mounted at its own path; `state` is a directory the box
  host keeps for the unit, at `/state` (`DAX_UNIT_STATE`), which
  outlives the container: a RAG's index. Nothing else is mounted. A
  workspace unit has the project `rw`.
- `internet`, `allow`: the unit's own, through a sidecar of its own on
  an internal network of its own, so no unit reaches another unit or
  the workspace.
- `secrets`: `{"NAME": "env:VAR" | "command:LINE" | "file:/path"}`, MCP
  units only. Each is resolved on the box host when the unit's
  container is made and given to that container alone, through
  docker's environment, never on a command line, in a label, a log, an
  error or the session. (Docker keeps it in the container's
  configuration, as any container's environment, where `docker
  inspect` on the box host shows it to whoever can run docker there.)
- `command`: the server's argv in its container, MCP units only.
- `exfiltration`: `"accepted"`, rule 2.

The box host refuses, when it reads its config, a unit that breaks one
of three rules, naming the unit and the rule:

1. **A secret needs an allowlist.** A unit with `secrets` has
   `internet` `allow` or `off`, never `on`: the token reaches only the
   hosts it is for, and a server that misbehaves cannot send it
   anywhere else.
2. **Files and connectivity together are declared.** A unit with the
   project (`ro` or `rw`) and any internet is the one way the
   project's contents can leave without the session seeing them, so it
   needs `"exfiltration": "accepted"` from the administrator. The
   workspace unit is held to it too; its default internet, `off`,
   keeps it on the safe side.
3. **A secret is a reference.** Its name is a variable's name; its
   value is `env:`, `command:` or an absolute `file:` path; a value
   written in the config is refused.

Data moves between units only through files both are given, or through
the session. A GitHub unit with `files: none` cannot read the project:
for the model to send it code, the code is in a call's arguments,
where the policy and the record see it.

The box host tells the session what it resolved, in its answer to the
handshake, under the executor's capability key:
`box: {profile, units: [{name, kind, image, runtime, files, internet,
allow, secrets: [names]}]}`. The units are made with the box, before
that answer, so an image build is not held to a process start's
timeout and a unit that cannot be made refuses the session with the
reason. The session starts each MCP unit as an MCP
server named after it (`dax-unit NAME`, through the executor, so it
restarts as any MCP server does, its tools `mcp__<unit>__<tool>`), and
records the envelopes in the session's header (`dax.box`): what each
unit could reach, with its secrets' names and never their values.

A server can still hand its own secret back as output (an env tool, an
error quoting its token), which would put the value in the
conversation, the provider's request and the record. So the box host
redacts each value the unit's container holds (read with `docker
inspect` when its server starts) from the server's standard output and
error before any of it is passed on, in every spelling a JSON encoder
writes it in, as `[REDACTED NAME]`, and says so once per unit and
secret. It is a backstop: an encoded or split value passes, and the
envelope (a secret reaches only its `allow` hosts) is what holds then.
agentredact's pass over a recorded store is the complement: it finds
undeclared secrets in a session being published, and cannot know a
unit's values, which never leave the box host.

The envelope informs the policy when the user says so.
execution-boundary.md lets dax claim confinement for an executor's
tools only when the user configured the sandbox; `box.confine` in the
user's config is that: `units` claims a unit whose envelope keeps it
to no files beyond a read-only project, no internet, no secrets and a
sandboxing runtime; `workspace` adds the workspace's bash, write and
edit when the workspace has no internet, off protected paths
(`.git`, `.dax`, environment files, CI, hooks, editor settings) and
for bash lines the analyzer reads whole and that hide nothing. The
claim is the session's, from the box host's envelope, never the
executor's or a unit's own; a SourceBox bare ask makes it matter, and
the session also claims whatever its policy allows without SourceBox,
so nothing that ran unasked starts asking. Because a confined call is
judged by its line and not by the program it runs, the workspace's
claim also needs the git guard: the session asks for it (`guard` in
the request), the box host mounts the project's `.git` read-only,
whole (each checked-out submodule's `.git`, an in-project
`core.hooksPath` and `.husky` too), over the project, and says so in
the workspace's envelope (`"guard": "on: .git read-only"`, or why
not), and the session claims the workspace's tools only behind it. Git
that writes then fails in the box: commits are made outside it. docs/sandbox.md, "Fewer questions in a box", has
the rules.

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
  <remote_dax> box serve -project <path>` (or `-checkout <url>`), over stdio. The pipe is the
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
    `cache = "auto"`: units can share the project.
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
   MCP servers in a box are refused.
2. **Units:** the config's `units` and `profiles`, the three rules,
   image builds cached by a hash of their declaration, secrets resolved
   on the box host into one unit's container, each unit's internet
   through its own sidecar and network, `files` and `state` mounts,
   `dax/process.start` of `dax-unit NAME` answered by the relay, the
   envelopes in the handshake and the record.
3. **Checkouts and administration** (above): `box.checkout` and the
   box host's `checkouts`, cloned and kept current only when clean;
   `dax box status`, `down` and `pull` through the box host, here or
   over ssh, each held to the box host's one project.
4. **HTTPS**, after executor connections authenticate.

## Open questions

- Should a session be able to bring a unit of its own (a declaration
  the box host builds), within the administrator's images and rules,
  or are units always the administrator's?
- One box host per session (each a process, as now) or one long-lived
  box host per machine that sessions attach to? Over ssh the former is
  free; over HTTPS the latter is the shape.
- A box host serving more than one user: each user's boxes are named
  by their project's path today; a multi-user host needs the user in
  the name and in the box host config's limits.
- The record holds each unit's envelope (step 2). Should a resumed
  session whose box host now gives another envelope warn, as a changed
  workspace does?
