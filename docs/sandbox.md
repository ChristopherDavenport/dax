# Running dax's tools in a sandbox

dax can run its tools in a container, a pod or another host while the
session stays on your machine: start `dax execute` there, and start dax
here with `-executor` set to the command that starts it, or to the URL
where `dax execute -listen` serves. Or keep the tools here and serve
only the directory they act in: `dax workspace` there, `-workspace`
here (see [A served workspace](#a-served-workspace-dax-workspace)).
These are recipes for daily use; what `dax execute` serves and how is in the
README's [The tools in a sandbox](../README.md#the-tools-in-a-sandbox-dax-execute).

## A box: `dax -box`

For a container, dax does the recipes below itself:

```sh
cd ~/src/app
dax -box
```

`-box` starts a **box host**, `dax box serve`, and connects to it as
its executor. The box host makes or reuses the project's box from its
own config, then relays the session to the `dax execute` inside it.
Here the box host is this dax, run as a child; on another machine it
is that machine's dax, over ssh (see [A box on another
machine](#a-box-on-another-machine)). Either way the box is made the
same way, from the same kind of config, and the session sees the same
box: only the connection differs. The design is
[docs/plans/proposals/box-host.md](plans/proposals/box-host.md).

The first time, the box host pulls the image and makes
`dax-app-<hash>`, the box of this directory: the hash is of the
directory's path, with links resolved, so the name is the same however
often the box is made and two projects called `app` have two boxes.
After that `-box` reuses it, starting it if it stopped; two sessions
that start it at once share the one box. A directory that is `/`, the
box host's home or above it is refused, since its mount would hand the
tools every key and config there, and so is a path with a colon, which
docker's `-v` cannot take. The box host's executor is

```sh
docker exec -i dax-app-<hash> /usr/local/bin/dax execute -root ~/src/app -kind container -ref dax-app-<hash>
```

so everything under [Docker](#docker) holds for it: the pipe is the
credential, there is no port, and `-list` and memory know the project
by the box's name and the directory. `-list` with `-box` (or with a
`box` in your config) finds its sessions without starting the box host.

### What the session says, and what the box host decides

What a box is made of, its image, runtime, limits, files, internet
and secrets, is the box host's: its config,
`~/.config/dax/box-host.json` on the machine it runs on. The session
names one of its profiles, which of the profile's MCP units it wants,
and may only narrow the rest. Your config's `box`:

```json
{
  "box": {
    "host": "local",
    "profile": "go",
    "tighten": { "memory": "2g", "internet": "allow", "allow": ["proxy.golang.org"] }
  }
}
```

| `box.` | |
|---|---|
| `host` | `local` (the default) or `ssh://[user@]host[:port]` |
| `profile` | one of the box host's profiles; empty is its default |
| `tighten` | `memory`, `cpus`, `pids`, `internet` (`off` < `allow` < `on`), `allow` (hosts within the profile's): each only narrows; the box host refuses a widening and says why |
| `mcp` | which of the profile's MCP units this session gets; empty is all of them, and one the profile does not have is refused (see [Units](#units)) |
| `host_config` | with `local`: the box host config the local box host reads in place of `~/.config/dax/box-host.json` (an absolute path) |
| `project`, `remote_dax`, `ssh` | with an ssh host: see below |

| Flag | |
|---|---|
| `-box`, `-box=false` | run in a box, or here despite the config's `box` |
| `-box-host local\|ssh://…` | in place of `box.host` |
| `-box-profile NAME` | in place of `box.profile` |
| `-box-internet off\|allow\|on` | narrow the profile's internet for this run |

Only your config can name a box; a project's `.dax/config.json` may
not, and `box` is refused beside an `executor` that names a command or
a URL. Its `executor.pass_env` alone may stay: it applies to the box
host's launcher (`SSH_AUTH_SOCK` for ssh's agent, `DOCKER_HOST`).

### The box host config

With no file, the box host makes one unit, `workspace`
(`golang:1.26-bookworm` under `runsc`, 4g, 2 CPUs, 512 processes, the
project read-write, no internet), and one profile, `default`, of that
unit alone. A file says more:

```json
{
  "runtime": "runsc",
  "limits": { "memory": "8g", "cpus": 4, "pids": 1024 },
  "images": ["golang:*", "node:*", "python:*", "registry.example.com/mcp/*"],
  "default_profile": "go",
  "units": {
    "workspace": { "image": "golang:1.26-bookworm" },
    "go-net":    { "image": "golang:1.26-bookworm", "allow": ["proxy.golang.org", "sum.golang.org"],
                   "exfiltration": "accepted" },
    "files":     { "build": { "base": "node:24-bookworm-slim",
                              "run": ["npm install -g @modelcontextprotocol/server-filesystem@2026.8.31"] },
                   "command": ["mcp-server-filesystem", "/srv/src/app"],
                   "files": { "project": "ro" } },
    "fetch":     { "build": { "base": "python:3.12-slim-bookworm",
                              "run": ["pip install --no-cache-dir mcp-server-fetch==2026.8.18"] },
                   "command": ["mcp-server-fetch", "--ignore-robots-txt"], "files": "none",
                   "allow": ["docs.example.com"] },
    "github":    { "image": "registry.example.com/mcp/github:1.0",
                   "command": ["github-mcp", "stdio"], "files": "none",
                   "allow": ["api.github.com"],
                   "secrets": { "GITHUB_TOKEN": "env:GITHUB_TOKEN" } }
  },
  "profiles": {
    "go":      { "workspace": "go-net", "mcp": ["files", "fetch", "github"] },
    "offline": { "workspace": "workspace" }
  }
}
```

| | |
|---|---|
| `runtime` | the runtime a unit that names none runs under: any runtime docker has registered (see [Runtimes](#runtimes)) |
| `limits` | ceilings no unit and no tightening goes past |
| `images` | glob patterns every image, and every build's base, must match; none means any |
| `default_profile` | the profile a session that names none gets; needed when there are several and none is `default` |
| `units` | the tool groups, each an envelope (see [Units](#units)) |
| `profiles` | what a session asks for: `workspace`, the one unit dax's own tools run in, and `mcp`, the MCP units beside it |
| `checkouts` | `{"allow": [...], "dir": "/abs/path"}`: the repositories a session may name for this box host to clone and keep (see [A checkout the box host keeps](#a-checkout-the-box-host-keeps)); none is no checkouts |

A field it does not know, a value docker would misread, a unit past
the limits or outside the images, an internet that says two things,
and a unit that breaks one of the [three rules](#the-three-rules) are
refused when the box host starts, before anything is made.

### Units

Every tool group is a unit: the workspace, where dax's own tools act
on the project, and each MCP server the box host runs for a session.
A unit is an envelope on three axes, what files it has, what it can
reach, and what secrets it holds, and the box host makes each unit's
container from it and holds it there. The policy is the session's, as
for any tool: a unit's tools ask, as `mcp__<unit>__<tool>`, until a
rule says otherwise.

| `units.NAME.` | |
|---|---|
| `kind` | `workspace` or `mcp`; `mcp` when `command` is set, `workspace` otherwise |
| `image`, `build` | the image, or `{"base": IMAGE, "run": [LINE, …]}`: built once (`FROM base` and a `RUN` per line) as `dax-unit/<unit>:<12 hex digits of the declaration's hash>`, and built again only when the declaration changes |
| `runtime`, `memory`, `cpus`, `pids` | as the workspace's, within `limits`; an MCP unit's default is 1g, 1 CPU, 256 processes |
| `files` | `"none"` (an MCP unit's default), or `{"project": "ro"\|"rw", "state": "rw"}`. The project is mounted at its own path; `state` is a directory the box host keeps for the unit, mounted at `/state` (`DAX_UNIT_STATE`), which outlives the container. The workspace has the project `rw` |
| `internet`, `allow` | the unit's own, through a sidecar of its own on an internal network of its own |
| `secrets` | MCP units only: `{"NAME": "env:VAR" \| "command:LINE" \| "file:/abs/path"}`, resolved on the box host when the unit's container is made |
| `command` | MCP units only: the server's argv in its container |
| `exfiltration` | `"accepted"`: see rule 2 |

Each MCP unit is a container of its own beside the box,
`<box>-<unit>`, made with the box when a session connects (its image
built first, once), so a unit that cannot be made, a secret that is
not set, refuses the session with the reason: the
same hardening as the box (no capabilities, a read-only root, the box
host's user, the limits, `--ulimit nproc` under a sandbox), its own
image and none of dax, its own internal network `<box>-<unit>-net`
and its own sidecar. No unit can reach another unit, or the box, or
their files but through what both mount. Its image needs `sh` and
`sleep` (a distroless image has neither: build one from a base that
does); its sidecar runs dax in the workspace's image. The session
starts each unit's server as an MCP server named after the unit: the
box host offers the units as the workspace's own servers
(`agentworkspace/servers.list` and `servers.start`, which it answers
itself), so its tools are `mcp__<unit>__<tool>`, and a server that ends
is started again on the next call.

#### The three rules

1. **A secret needs an allowlist.** A unit with `secrets` has
   `internet` `allow` or `off`, never `on`: a token reaches only the
   hosts it is for, and a server that misbehaves cannot send it
   anywhere else.
2. **Files and connectivity together are declared.** A unit with the
   project (`ro` or `rw`) and any internet is the one way the
   project's contents can leave without the session seeing them, so it
   needs `"exfiltration": "accepted"`. The workspace is held to it too:
   a workspace unit with an allowlist needs it, and its default
   internet, `off`, keeps it on the safe side.
3. **A secret is a reference.** Its name is a variable's name, its
   value `env:VAR`, `command:LINE` or an absolute `file:` path; a value
   written in the config is refused.

Data moves between units only through files both mount, or through
the session: a unit with `files: none` cannot read the project, so for
the model to send it code the code is in a call's arguments, where the
policy and the record see it.

#### Secrets

A unit's secrets are resolved on the box host when its container is
made, and given to that container alone, through docker's own
environment: its command line names them (`-e NAME`) and never holds a
value, and no label, log, error or message to the session does. Docker
itself keeps it in the container's configuration, which `docker
inspect` shows on the box host's machine to whoever can run docker
there, as it does any container's environment; whoever can is root
there already. `env:`
reads the box host's own environment; `command:` runs the line with
`sh` there and takes its output, trimmed; `file:` reads the file. A
secret that cannot be resolved fails the unit's start, naming the
secret and its reference, before anything is made. The workspace's
bash never sees a unit's secret: they are in different containers.

A local box host is started from your session's environment less its
credentials, so a variable an `env:` secret reads must be named for
the launcher: `"executor": {"pass_env": ["GITHUB_TOKEN"]}` beside your
`box`. It reaches the box host alone; the session's tools never get
it. On another machine the box host's own environment is its login's.

#### A server that returns its secret

A server can hand its own secret back as output: a tool that prints its
environment, an error that quotes the token it was refused with. The
session would then hold it as conversation, send it to the model's
provider, and record it. So the box host redacts what a unit's server
writes, standard output and error, before any of it is passed on: each
value the unit's container holds for a variable its `secrets` name,
read with `docker inspect` when the server starts (so a container made
before a secret was rotated is redacted by the value it has), becomes
`[REDACTED NAME]`, and the first replacement of each secret in a
session is said on the terminal:

```
dax: unit env: redacted DAX_TEST_TOKEN in its output; a server that returns its secret is redacted by the box host, see docs/sandbox.md
```

A value is matched as written and as JSON encoders write it inside a
string: Go's, with and without HTML escaping; with `/` as `\/`;
Python's `json.dumps` (non-ASCII as `\uXXXX`, lower-case hex, astral
runes as surrogate pairs) and PHP's `json_encode` (that, and `\/`); and
JavaScript's `JSON.stringify`. Longer values go first, so a value
holding another is not half replaced, and the output does not depend on
the order the secrets are declared in. Output is redacted a line at a
time; a line over 1 MiB without a newline is passed on in pieces that
hold back what could begin a value the next piece ends.

It is a backstop, not a guarantee. A value the server encodes another
way (base64, upper-case hex escapes, reversed, hashed), splits across
two messages or two calls, or transforms is not found, and a server
that means to leak its secret can. What holds then is the unit's
envelope: a secret only reaches the hosts its `allow` names (rule 1).
agentredact's pass over a recorded store is the other half: it finds
secrets nobody declared (patterns, home paths, identities) in a
session being published, and cannot know a unit's values, which never
leave the box host; the box host knows only those.

#### What the session sees

The box host tells the session what it resolved, in its answer to the
handshake: the profile and each unit's envelope (its image, runtime,
files, internet and hosts, and its secrets' names). The session's
header records the same (`dax.box`), so a session file says what each
unit could reach; dax writes no secret's value in it, and a server's
own output is redacted of its values (above). The executor's line
at the start names the profile and the units:

```
executor: dax 0.1.0 · container dax-app-1a2b3c4d · /home/me/src/app · profile go, units files, github
```

What the box is made with:

| | |
|---|---|
| `--runtime` | the profile's runtime (see [Runtimes](#runtimes)). A runtime docker does not know fails the session; dax never falls back to another |
| `--user` the box host's uid and gid | the files the tools write in the project are that user's |
| `--cap-drop ALL`, `--security-opt no-new-privileges` | no capabilities, and no setuid program can gain any |
| `--read-only`, `--tmpfs /tmp`, `--tmpfs /home/dax` | nothing outside the project survives the container; the home (`HOME=/home/dax`, and `GOPATH=/home/dax/go`, so go's module and build caches are in it) and `/tmp` are memory, counted against the limit |
| `--memory`, `--memory-swap`, `--cpus`, `--pids-limit` | the profile's, narrowed by the session's `tighten` |
| `--ulimit nproc=<pids>` | under a sandbox (anything but `runc`), the profile's process limit, enforced by the sandbox's own kernel for the box's user: `--pids-limit` is a cgroup on this machine, which gVisor's and Kata's processes are not counted in |
| `--network <box>-net` | the box's own internal network, with no route out; its sidecar is the only other thing on it (see [The box's internet](#the-boxs-internet)) |
| `-e HTTP_PROXY=…`, `HTTPS_PROXY`, lower case too | the sidecar's proxy, at a fixed address on that network; `NO_PROXY` is localhost |
| `-v DIR:DIR`, `-w DIR` | the project at its own path, the one mount you can write |
| `-v <the box host's dax>:/usr/local/bin/dax:ro` | the box host's dax, so the executor is its version |

The container's own process is `sleep infinity`; the tools run in the
`dax execute` each session's `docker exec` starts. A workspace unit
with `state` also mounts its state directory at `/state`.

The image needs bash, sh, git, `sleep`, and a glibc at least as new as
the one the box host's dax was built against: a dax built by `go
install` links glibc. An image without it (alpine) needs a dax built
with `CGO_ENABLED=0`. The box host runs `dax execute -h` in a new box
and says so when it fails.

A box is made from its settings once. When a session asks for other
settings (another profile, other limits), the box host refuses the box
it finds, since making it again would end any other session's tools in
it: `dax box down` and start again. A unit's container likewise: a
changed declaration (its image, command, files, secrets' references or
limits) is refused, while its internet, which only its sidecar's rules
hold, changes without making it again. A dax that changed since the box
was made (an upgrade writes a new file, which the box's mount does not
see) makes the box again, with a warning.

```sh
dax box ls        # this machine's boxes and units: name, unit, state, runtime, files, internet, directory
dax box down      # remove this directory's box and its units, each one's sidecar, network and rules; or dax box down NAME
dax box down -state  # the units' state directories too, kept by default
dax box up        # make or start it from this machine's box host config, and print its executor's command
```

`dax box up` takes `-config`, `-profile` and `-internet`. These act on
this machine's docker; against a box host elsewhere is the design's
step 3.

### A box on another machine

Install dax on that machine (`go install`), with Docker Engine and the
runtime its config names, and give it a `~/.config/dax/box-host.json`
if the built-in profile is not what you want: on that machine it is
the administrator's file, and no session can widen it. Then:

```json
{
  "box": {
    "host": "ssh://me@build-host:2222",
    "profile": "go",
    "project": "/srv/src/app",
    "remote_dax": "/home/me/go/bin/dax"
  },
  "executor": { "pass_env": ["SSH_AUTH_SOCK"] }
}
```

dax runs

```sh
ssh -T -o BatchMode=yes -p 2222 -- me@build-host '/home/me/go/bin/dax' box serve -project '/srv/src/app'
```

and the session is otherwise as here. `project` is the project's path
on that machine, a checkout there (default: the same path as here); or,
in its place, `checkout` names a repository the box host clones and
keeps for you (below). `remote_dax` is the
dax there (default `dax` on the login's PATH, which a non-interactive
ssh often lacks). `ssh` adds arguments before the host, such as
`["-F", "/home/me/.ssh/build.conf"]`, for a port forward, a jump host
or a known-hosts file of its own. As for any ssh executor, `BatchMode`
means ssh fails rather than asks, so connect by hand once first.

The box host there resolves the profile against its own config, makes
the box with its own docker, and runs it as the user you log in as.
`-list` keys the project by the ssh command's words, which name the
host and the project, and by the box's descriptor.

#### A key that can only start the box host

The user you log in as makes boxes with that machine's docker, and
whoever can run docker there can make themselves root there. On a
machine you share, give each person a key that can do nothing but
start the box host, with sshd's own forced command, in that user's
`~/.ssh/authorized_keys` on the box host's machine:

```
command="/home/me/go/bin/dax box serve -project /srv/src/app",restrict ssh-ed25519 AAAA… alice
```

sshd then runs that command whatever the session asks for: the words
dax sends (its `remote_dax` and `-project`) are not run, `restrict`
refuses a terminal, port and agent forwarding, and the project is the
one the line names, so a key stands for one project. A login with the
key that asks for a shell, or for `id; cat ~/.ssh/id_ed25519`, gets the
box host, which refuses anything but a session's first message
(`dax: box host: the session's first message: EOF`); a dax session with
the key works as with any other. The administrator's `box-host.json`
still decides the box; the key only decides who may ask for it. For a
checkout the line names the repository instead,
`command="/home/me/go/bin/dax box serve -checkout https://github.com/me/app",restrict`,
and the key reaches that checkout and nothing else, for sessions and
for `dax box status`, `down` and `pull` alike.

#### A checkout the box host keeps

Rather than a path you cloned there yourself, a session can name a
repository, and the box host clones it into a directory it keeps:

```json
{ "box": { "host": "ssh://me@build-host", "checkout": { "url": "https://github.com/me/app", "ref": "main" } } }
```

`url` is `https://`, `ssh://`, `git@host:path` or `file://` (a path on
the box host), with no credentials in it: a URL with a password, or
any user on https (a token), is refused. `ref` is a branch or a tag,
by default the remote's default branch. dax runs `dax box serve
-checkout <url> [-ref <ref>]` there in place of `-project`, so `-list`
knows the project by its URL.

The box host clones only what its config's `checkouts.allow` takes:
glob patterns on the URL as it writes it (`git@github.com:me/app.git`
is `ssh://git@github.com/me/app`, and a trailing `/` or `.git` is
dropped), so `"https://github.com/me/*"` takes every repository of
`me` there and nothing below them. A box host with no `allow` takes no
checkout, and says so. Each lives in `checkouts.dir` (default
`~/.cache/dax/box-checkouts` on that machine), as
`<repository>-<12 hex digits of the URL's hash>`, and is then the
project as a `project` path is: the box is named for it and mounts it
at its own path.

At each session's start the box host clones it if it is not there,
else fetches, and moves the worktree to the ref only when that loses
nothing: the worktree is clean, on that ref, and holds no commit the
remote lacks. Otherwise it leaves it as it is and says why on the
terminal (`checkout https://github.com/me/app at …: fetched; it has
commits origin/main does not; left as it is`): a session's work is
never discarded. Git runs on the box host, not in the box, with that
user's own credentials (its git config, ssh agent or credential
helper), and never prompts; the box's internet does not come into it.
The record's `dax.box` header and the executor line name the
repository, the ref and the head the session started on.

#### Status, removing and taking work back

These go through the box host your config's `box` names, here or over
ssh, and act on its one project:

```sh
dax box status            # the box, its units, and the checkout: branch, head, uncommitted changes
dax box down [-state]     # remove the box and its units there, and the box host's state for them
dax box pull              # in a local clone: fetch the checkout's head into dax-box/<repository>
dax box pull -branch work # into another branch; -force to replace one with commits the head lacks
```

`pull` asks the box host for a git bundle of the project's HEAD (256
MiB at most), checks it with `git bundle verify`, and fetches it into
the branch: a new branch, or one the head is ahead of, unless
`-force`. It will not move the branch checked out where you run it.
Changes the checkout there has not committed are not in a bundle;
pull says when there were, so commit them in a session first. With a
local box host the project is already here, so pull is refused.

`dax box down NAME` and `dax box ls` still act on this machine's
docker directly, for whoever administers it; `dax box down` with no
name asks the box host, which is the only way to remove what a box
host on another machine keeps (its rules files, the units' state). The
checkout itself is left: it holds the session's work.

### The box's internet

A box has no internet unless its profile gives it some. Each run says
so:

```
dax: box dax-app-1a2b3c4d has no internet; a box host profile with internet (-box-profile) opens it
```

| a profile's `internet` | |
|---|---|
| `off` (the default) | every request refused |
| `allow` | only the hosts its `allow` names: `proxy.golang.org`, or `*.githubusercontent.com` for every name below it (not the domain itself) |
| `on` | any host |

An `allow` list alone means `allow`; `internet: "allow"` with no hosts,
or `on` or `off` beside a list, is refused. A session narrows it with
`tighten` or `-box-internet`: `off` under any profile, `allow` with
hosts the profile's list covers (any hosts under `on`), `on` only under
`on`. Hosts are names, never addresses, ports or URLs.

How it holds. The box sits alone on an internal network of its own,
`<box>-net`, with no route anywhere. The only other container on it is
the box's sidecar, `<box>-proxy`, which is also on `dax-box-egress`, an
ordinary docker network all boxes' sidecars share. The sidecar runs
`dax box proxy`: the box host's dax, mounted read-only as in the box,
serving HTTPS through `CONNECT` and plain HTTP to the hosts its rules
take, on ports 80 and 443, listening on its address on the box's
network alone. Its rules are a file on the box host's machine,
`~/.cache/dax/box/<box>/egress.json`, mounted read-only into the
sidecar and read again whenever it changes, so a run changes the mode
without making either container again. In every mode the proxy refuses
a destination that resolves to that machine, a private network
(including docker's own), a link-local address (a cloud's metadata
service, 169.254.169.254) or shared address space, checked on the
address it connects to, so no name that resolves there gets through. A
refusal is a 403 whose body says which host and which setting; curl
prints `CONNECT tunnel failed, response 403` for HTTPS. The sidecar
logs each one (`docker logs <box>-proxy`).

So from a box the internet is HTTP and HTTPS through that proxy, even
`on`: no DNS (a name resolves only in the proxy), no ssh, no other
TCP, no UDP. git over https, `go get` through the module proxy, curl,
pip and npm read the proxy variables; a program that ignores them has
no way out at all. Push from your machine, not from the box.

The mode is the box's, for every session in it: the latest run sets
it, and says so when it changes another's
(`box dax-app-1a2b3c4d internet: off -> allow proxy.golang.org; applies
to every session in the box`). `dax box ls` shows it.

The sidecar runs under `runc`, whatever the box's runtime: under
runsc, docker's own name server on a user-defined network does not
answer, and the proxy must resolve the hosts it is asked for. It runs
dax's proxy and nothing the model wrote, with no capabilities, a
read-only root, 128 MiB and 64 processes, as the box host's user. Its
image is the box's, for the glibc dax needs.

## Runtimes

A box runs under whichever runtime its profile names, any docker has
registered; nothing in dax behaves differently by runtime. What
differs is what has been verified.

### gVisor (`runsc`)

The default, and the one verified: the box's processes talk to
gVisor's kernel, not the machine's. Install Docker Engine and runsc
from their apt repositories
([docker](https://docs.docker.com/engine/install/ubuntu/),
[gVisor](https://gvisor.dev/docs/user_guide/install/)), then register
it with docker:

```sh
sudo runsc install
sudo systemctl restart docker
docker run --rm --runtime=runsc busybox dmesg   # "Starting gVisor..."
```

On some kernels the default platform, systrap, cannot start a
sandbox. Under WSL2 (kernel 6.6) every container fails with

```
docker: Error response from daemon: OCI runtime start failed: starting container: starting root container: urpc method "containerManager.StartRoot" failed: EOF
```

and runsc's boot log says `panic: failed to create a syscall thread`.
Find out which platforms work with runsc alone, as yourself:

```sh
runsc --rootless --network=none --platform=kvm --debug --debug-log=/tmp/runsc/ do true
```

On WSL2 `kvm` (when `/dev/kvm` is there) and `ptrace` both work. Point
docker's `runsc` at the faster one, and keep the other as a runtime of
its own:

```sh
sudo runsc install --runtime=runsc -- --platform=kvm
sudo runsc install --runtime=runsc-ptrace -- --platform=ptrace
sudo systemctl restart docker
```

A profile with `"runtime": "runsc-ptrace"` then names the second.

### Kata Containers (`io.containerd.kata.v2`)

A VM per container, with a kernel of its own. Verified with Kata 4.2.0
(the static bundle: runtime-rs, QEMU, virtiofsd) under WSL2, kernel
6.6, where it runs nested on `/dev/kvm`. Install the bundle and
register the shim with docker:

```sh
sudo tar --zstd -xf kata-static-4.2.0-amd64.tar.zst -C /
sudo ln -sf /opt/kata/runtime-rs/bin/containerd-shim-kata-v2 /usr/local/bin/
# /etc/docker/daemon.json, beside runsc's entries:
#   "runtimes": { "kata": { "runtimeType": "io.containerd.kata.v2" } }
sudo systemctl restart docker
docker run --rm --runtime kata busybox uname -r   # the guest's kernel, not this machine's
```

The 4.x static bundle carries only the Rust runtime; its shim finds
`/opt/kata/share/defaults/kata-containers/runtime-rs/configuration.toml`
(QEMU, virtio-fs) without more setup. A profile names it `kata`, as
registered, or `io.containerd.kata.v2`, which docker also takes.

What a box under Kata does, as checked here:

- The hardening holds as under gVisor: the box's user, no
  capabilities, a read-only root, the project writable, and the dax
  binary's single-file mount, read-only. A box starts in about two
  seconds.
- Docker's name server does not answer inside a Kata box on a
  user-defined network, as under gVisor (it answers on `127.0.0.11` in
  the container's namespace, and the guest has a loopback of its own),
  so the sidecar stays under `runc` for Kata too. The box's internal
  network holds: no route out, the sidecar reached at its address, and
  the three internet modes as under gVisor.
- Two containers sharing the project see each other's writes at once,
  and the host's: a new file, a new directory, an append, a file
  rewritten under an open descriptor. A read-only mount refuses writes.
  virtio-fs's `cache = "auto"` (the default) is enough.
- `--memory 4g` gives the guest about 3.9 GiB; QEMU itself takes about
  360 MiB of this machine's memory on top while idle. `--cpus 2` shows
  as three processors, Kata's own vCPU and two.
- The process limit is the box's user's (`--ulimit nproc`), as under
  gVisor; see [What a box does not do](#what-a-box-does-not-do).

### Fewer questions in a box

A box limits what an approved call can reach; on its own it does not
change how often you are asked. `box.confine` in your config (or
`-box-confine` for a run) lets it:

```json
{ "box": { "confine": "workspace" } }
```

- `off`, the default: every rule and question is as on your machine.
- `units`: a unit's tools run unasked when the box host holds the unit
  to no files of yours beyond a read-only project (`files: none`,
  `project ro`, its own `state`), no internet, no secrets, and a
  runtime with a kernel of its own (not `runc`). A unit with internet
  or a secret keeps asking: the box keeps its token in, not what an
  API call does with it.
- `workspace`: those, and the workspace's `bash`, `write` and `edit`
  when the workspace unit has no internet and runs under gVisor or
  Kata.

What it does, in the policy's terms: the session takes those calls as
confined (agentpolicy's `ConfinedBy`) and adds a bare ask rule for each
such tool, of source `dax:box`, which a confined call skips. A deny
rule still denies, an ask rule with a specifier (`bash(git push:*)`,
`Read(ci:.env*)`) still asks, and nothing the policy allows without
`box.confine` starts asking with it. Your own bare ask for one of these
tools (`"ask": ["bash"]`) is skipped the same way; to be asked every
time in a box, write `bash(*)`. The verdict in the record says what
confined the call: `confined by box:runsc:workspace, so bash does not
ask`.

The workspace is your project, mounted read-write, so a confined call
can still change its files on your disk; that is the point, and the
reason not every call is taken as confined:

- **Protected paths ask.** A `write` or `edit` of, or a bash line that
  names, `.git` (what git on your machine trusts), `.dax`, an
  environment file (`.env*`, `.envrc`), CI (`.github`, `.gitlab-ci.yml`,
  `.circleci`), hooks (`.husky`, `.pre-commit-config.yaml`) or editor
  settings (`.vscode`, `.idea`) asks as it does without a box, in any
  directory and any case, links followed for the file tools. A Makefile
  is not protected: it is source, as everything a build reads is.
- **A bash line is confined only when it can be read.** The analyzer
  must read the whole line (no subject it cannot name), and the line
  must hide nothing: no `$`, backquote, `<(`, `>(` or backslash; no
  program that runs another or code given as text (a shell, `eval`,
  `env`, `xargs`, `find`, an interpreter); no `git`, which writes
  `.git`; no word that is the project's root (`.`), a parent, an
  absolute path, `~` or a glob over dot files. Anything else asks as
  without a box. `go test ./...`, `rm -rf build` and `cp src/a.go
  build/` run; `sh -c '...'`, `git commit` and `rm -rf .` ask.
- **A redirect asks.** What a line writes through `>` is decided as a
  write, which the line's confinement does not cover.
- **Sub-agents get nothing from it.** The explore agent's read-only
  promise rests on the analyzer, so a sub-agent's call runs unasked
  only when the policy allows it without `box.confine`.
- **A repository cannot use it.** Only your config can set `confine`.
  A project's `.dax/config.json` bare ask for a tool `box.confine`
  covers keeps that tool asking: `bash`, `write` and `edit` are then
  not confined at all, and a unit a project's ask names is not either.

#### The git guard

The protected paths stop the model's own commands naming them; they do
not stop a program a confined command runs. A confined `write` of a
Makefile whose rule writes a hook, then a confined `make`, would leave
something git on your machine runs, and `go test`, `go generate`, `npm
test` or a `tar x` can do the same. Holding chosen files read-only is
not enough, as three ways round it show:

- **Renaming `.git`.** A program can move the repository aside and make
  a new `.git` with hooks of its own, which your git then uses.
- **Removing it.** `rm -rf .git` takes the history with it.
- **Pointing it elsewhere.** git reads files in `.git` that send it to
  another directory for its config and hooks (`commondir` is one), and
  a program can create them.

So with `workspace` the box host mounts the project's `.git` read-only,
whole, over the read-write project. It is a mount point in the box, so
it cannot be renamed or removed, and nothing under it can be written,
whatever runs:

- the project's `.git` directory; or a `.git` file (a worktree or a
  submodule checkout) and its git dir when that is in the project (one
  outside it the box cannot reach anyway);
- every checked-out submodule's `.git`, nested ones too, as git lists
  them when the box is made, since a rewritten `gitdir:` line would send
  your git to a directory the box wrote;
- `core.hooksPath` (yours, global config included) when it is in the
  project, and `.husky`.

The box host changes nothing in the repository to do this. A guard
path that is a symbolic link, or leads out of the project, refuses the
box with the reason, since a link would stay writable; so does a
`core.hooksPath` in the project that is not there yet, or that is the
project's root.

In the box git that reads works: `status`, `log`, `diff`, `show`,
`blame`. Git that writes fails with `Read-only file system`: `commit`,
`add`, `branch`, `stash`, `fetch`, `checkout` of another branch. Commit
on your side, outside the box; `dax box pull` against such a box is
refused, since it has no commits of its own. Each session says so when
it starts: `dax: box <name>: the repository is read-only in this box
(box.confine workspace); commit outside it`.

The workspace's envelope in the session's header says `"guard": "on:
.git read-only"`, and the session takes the workspace's tools as
confined only when it does. A box made before the guard, or without it,
is refused with `dax box down` to make it again, and so is one whose
guarded paths changed since. A project that is not a git repository has
nothing to guard, and a `git init` in the box would make one your git
trusts: the box host says `"guard": "none: not a git repository"`, the
workspace's tools ask as without `box.confine` (units are unchanged),
and dax says why when the session starts. Run `git init` before boxing
such a project.

Not guarded: a repository a program in the box creates under the
project, or one there that git does not list as a submodule. Your git,
run inside such a directory, would trust its hooks and config as it
trusts any repository's; do not run git in a directory a confined box
made without looking at it first.

What it does not do: a confined command's own program is otherwise the
box's business, not the policy's. A test can rewrite any source in the
project, an environment file or CI config included, and `cp` follows a
link the repository already has; none of that runs on your machine
until you run it. Turn `box.confine` on for projects whose code you
would run on your own machine without reading it first.

### What a box does not do

It limits what an approved call can reach, not how often you are
asked: unless your `box.confine` says otherwise (see [Fewer questions
in a box](#fewer-questions-in-a-box)), dax claims no confinement for a
box's tools, so every rule and question is as on your machine. Its internet is what [The box's
internet](#the-boxs-internet) gives it, and `on` is any host on the
internet. Its process limit is per user: under gVisor and Kata,
`--pids-limit` does not hold (both started 1000 processes under a limit
of 100 here), and `--ulimit nproc` does (95 and 97); the processes of
every session in a box, all the box's user's, count against it
together. And anything mounted is reachable: the project, read-write.

## What runs where

In the sandbox: dax-coding's tools (`read`, `write`, `edit`, `glob`,
`grep`, `ls`, `bash`) and every check of what a call would touch, the
project's `AGENTS.md`, `.dax/skills` and `.dax/config.json` (read from
the sandbox, not from your checkout), and your MCP servers. On your
machine: the model and its key, the policy and every decision, the
questions you answer, memory, your own `~/.dax/AGENTS.md`,
`agents_md_global` and skills, and the session record. The key never
enters the sandbox: the command that starts the executor gets your
environment less its credentials, but those you name for it, and never
the key's variable. The
connection is that command's standard input and output (a `docker
exec -i`, an `ssh`, a `kubectl exec -i`); there is no port, listener or
token. With `-listen` (see [Network](#network)) the connection is HTTP
to a server left running there, and there is a port, still with no
token.

## Getting dax into the sandbox

Releases are tags with notes, not binaries, so build it with `go
install`. The sandbox needs dax v0.0.9 or later for MCP servers (an
older one fails the session with "update dax execute where it runs"),
and at least v0.0.7 to connect at all. The tools need `bash`, `sh` and `git` there.

## Docker

A Go image has all of that:

```dockerfile
FROM golang:1.26
RUN go install github.com/ChristopherDavenport/dax/cmd/dax@v0.0.9 \
 && mv /go/bin/dax /usr/local/bin/dax
# A writable home for any user the container runs as (go's build cache).
RUN mkdir -m 1777 /home/dax
ENV HOME=/home/dax
```

For a project that is not Go, copy the binary into its own image:

```dockerfile
FROM golang:1.26 AS dax
RUN CGO_ENABLED=0 go install github.com/ChristopherDavenport/dax/cmd/dax@v0.0.9

FROM your-project-image
COPY --from=dax /go/bin/dax /usr/local/bin/dax
```

Start one long-lived container per project, with the checkout mounted
at the same path as on your machine and running as you, so the files
it writes stay yours and git does not call the checkout's ownership
dubious:

```sh
docker build -t dax-sandbox .
cd ~/src/app
docker run -d --init --name app-box --user "$(id -u):$(id -g)" \
  -v "$PWD:$PWD" -w "$PWD" dax-sandbox sleep infinity
dax -executor "docker exec -i app-box dax execute -root '$PWD' -kind container -ref app-box"
```

Your shell fills in `$PWD` inside the double quotes; the single quotes
reach dax, which splits the line as a shell splits words, so a
checkout whose path has a space stays one argument (one whose path has
a single quote needs the array form, under Config).

`-i` and not `-t`: the pipe carries MCP. `-root` must exist in the
container; it is where every tool acts and nothing above it is read.
`-kind` and `-ref` are what the record and the banner call the
workspace, and with `-root` they are the project's identity: the
sessions `-list` finds and the memory scope are keyed by them (see
[Sessions and memory](#sessions-and-memory)). Give `-ref` explicitly, a
name that outlives the container: its default is the host's name, and
a container's changes when it is recreated.

The tools' commands and MCP servers get `dax execute`'s environment,
which is the container's (the image's `ENV`, `docker run -e`), less
anything that looks like a credential. To give them one, set it on the
container and name it: `-e GITHUB_TOKEN` on `docker run`, and
`-pass-env GITHUB_TOKEN` on `dax execute`. Nothing from your machine's
environment reaches them.

## ssh

Install dax on the host with the same `go install`, then:

```sh
dax -executor-pass-env SSH_AUTH_SOCK \
  -executor "ssh -T -o BatchMode=yes build-host \"bash -lc 'exec dax execute -root ~/src/app -kind remote -ref build-host'\""
```

dax splits the line as a shell would split it, so the remote command,
in double quotes, is one argument to ssh. ssh sends it to the host,
whose shell parses it again, so the single quotes, `~` and `$HOME`
inside it work on that side. Here they run `dax execute` under a login
shell (`bash -l`): a non-interactive ssh session often lacks the
`PATH` with `~/go/bin` and the Go toolchain, and the tools' commands
inherit `dax execute`'s environment. The tools run as the user you log
in as, with that user's files and rights on the host. In your config
the array form needs one layer of quoting less:

```json
{
  "executor": {
    "command": ["ssh", "-T", "-o", "BatchMode=yes", "build-host", "bash -lc 'exec dax execute -root ~/src/app -kind remote -ref build-host'"],
    "pass_env": ["SSH_AUTH_SOCK"]
  }
}
```

`-T` asks for no terminal, and `BatchMode=yes` makes ssh fail rather
than ask for a password, a passphrase or a new host key, since dax
holds the terminal. Connect by hand once first. dax removes
`SSH_AUTH_SOCK` from the command's environment along with the other
credentials unless you name it for the launcher, as above
(`-executor-pass-env`, or `pass_env` under `executor`). Only the
command that starts the executor gets it: bash and the MCP servers do
not, in the sandbox or, when you run dax without an executor, on your
machine. (The top-level `pass_env` would hand it to them too.)

## kubectl

Run the same image, pushed where your cluster pulls from, as a pod, and
put the project in it (`git clone` inside, or `kubectl cp`):

```sh
kubectl run app-box --image=dax-sandbox --command -- sleep infinity
kubectl exec app-box -- git clone https://github.com/me/app /home/dax/app
dax -executor "kubectl exec -i app-box -c app-box -- dax execute -root /home/dax/app -kind container -ref app-box"
```

`kubectl run` names the container after the pod; name yours with `-c`.
`KUBECONFIG` reaches kubectl, but an exec credential plugin that reads
a token from your environment (`AWS_SESSION_TOKEN`, say) does not get
it unless `executor.pass_env` (or `-executor-pass-env`) names it.

## Network

Rather than starting `dax execute` for each session, leave it running
where the project is and connect to it by URL. On the host:

```sh
dax execute -listen 127.0.0.1:7777 -root ~/src/app -kind remote -ref build-host
```

It prints where it serves (`dax execute: serving /home/me/src/app at
http://127.0.0.1:7777`) and runs until a signal. Here, tunnel to it and
connect:

```sh
ssh -N -L 7777:127.0.0.1:7777 build-host &
dax -executor http://127.0.0.1:7777
```

or in your config:

```json
{
  "executor": { "url": "http://127.0.0.1:7777" }
}
```

**Nothing authenticates a session to it yet.** Anyone who reaches the
address can run commands as the user it runs as, so it listens on a
loopback address only, and ssh's tunnel is what decides who reaches
it. `-insecure-no-auth` takes any address (`-listen :7777`) and prints
a warning; use it only on a network where everyone who can reach the
port may already run commands there. A web page cannot reach it: a
request from another origin, or one that came to the loopback address
under another host name, is refused.

Each session is a session of its own on the server, with its own MCP
servers and processes, over the one workspace: two sessions write the
same files. A session that starts no request for five minutes is
closed and its processes end; dax pings every minute while it runs, so
that is only a session whose dax went away without closing it. The
launcher's environment does not apply: `executor.pass_env` and
`-executor-pass-env` are refused with a URL, and the tools get the
environment `dax execute` was started with. Its standard error stays on
the host's terminal.

## Config

Put the executor in your own config (`~/.config/dax/config.json`):

```json
{
  "executor": {
    "command": "docker exec -i app-box dax execute -root /home/me/src/app -kind container -ref app-box",
    "pass_env": ["SSH_AUTH_SOCK"]
  }
}
```

`command` is a command line, split as a shell splits words (`'...'`,
`"..."` and `\` quote; `-root "/home/me/my app"` is one argument) and
run directly, with no shell: nothing is expanded, so `$PWD`, `$HOME`,
`~` and `*` reach the program as those characters. `-executor`, `-mcp`,
`mcp_servers` and `-api-key-command` split the same way. Or give the
program and its arguments as an array, used exactly:

```json
{
  "executor": {
    "command": ["docker", "exec", "-i", "app-box", "dax", "execute", "-root", "/home/me/my app"]
  }
}
```

`pass_env` names the variables only that command gets (see ssh); it
may stand alone, without `command`, when you give `-executor` each
time. Since the root is fixed, a shell function suits several projects
better:

```sh
daxbox() { dax -executor "docker exec -i $(basename "$PWD")-box dax execute -root '$PWD' -kind container -ref $(basename "$PWD")-box" "$@"; }
```

- `-executor ""` runs one session on this machine despite the config.
- A project's `.dax/config.json` may not set `executor`; dax refuses the
  file. Only your config and the flags say where the tools run.
- `max_read_bytes` in your config applies to the sandbox's tools too,
  up to the ceiling whoever runs the sandbox sets with `dax execute
  -max-read-bytes` (dax's default, 2 MiB, when unset). dax sends the
  setting when it connects, each session gets its own, and a session
  that asks for more than the ceiling gets the ceiling, with a warning
  naming the flag that would allow more. A `dax execute` older than
  this takes no setting and applies its own flag; dax warns when your
  setting is not the default. A dax older than this sends none, and
  gets the flag's value.
- MCP servers are configured as usual (`mcp_servers`, `-mcp`) and start
  in the sandbox, at its root, so their commands must exist there.
  `/mcp add <name> <command>` (in `-front repl`) starts one there
  mid-session. A server with `"in": "host"`, or a `url`, is called from
  this machine instead, beside the sandbox; its tools are never taken
  as confined. The sandbox keeps no list of its own (a box host offers
  its units), and a project's config names servers only you trusted
  with `-trust-mcp`.
- Your `pass_env` and `executor.pass_env` apply only to the command
  that starts the executor (and `pass_env` to the tools when you run
  dax without one); what the sandbox's commands get is `dax execute`'s
  `-pass-env`.
- A project's `.dax/config.json` may not set `executor.pass_env`
  either; it is part of `executor`.
- `-config ~/.config/dax/sandbox.json` keeps a second config for
  sandboxed sessions, with the executor and rules of their own. It
  replaces your config rather than adding to it.

## Checking it

With an executor, the terminal client and the REPL print a line for
it at the start: its name and version, kind and ref, and root,

```
executor: dax v0.0.9 · container app-box · /home/me/src/app
```

The terminal client prints it before it takes the screen, and it is on
the terminal again when the client exits; `dax -v` prints the rest of
the banner with it.

Ask the model to run `hostname`; the answer should be the sandbox's (a
container's ID, under docker).

When it fails:

| What you see | What it means |
|---|---|
| `-executor: executor: ...` and the launcher's own error above it | the command did not start `dax execute`: the container is not running, `dax` is not on the sandbox's `PATH`, ssh could not log in, or `-root` does not exist there |
| `... not a dax executor: ...` | the command started something else that speaks MCP, or a dax whose protocol this one does not read; the rest of the line says what is missing |
| `... it does not serve its workspace's files ...; update dax execute where it runs` | the sandbox has dax v0.0.6 |
| `... cannot start a process ...; update dax execute where it runs` | the sandbox has dax v0.0.8 or older and the session has an MCP server |
| `the executor's workspace cannot be read: ...` | it connected, then did not answer a read of its root within 30 seconds, or went away |
| `-executor: config <root>/.dax/config.json: ...` | the sandbox's project config is invalid or sets a field a project may not; fix it there. dax will not start without it, since it can only tighten |
| a call blocked mid-session | the executor did not answer within 30 seconds or is gone; quit, bring it back, `dax -resume <id>` |

Anything the launcher or `dax execute` writes to standard error comes
to your terminal, cleaned of control characters. Under the terminal
client nothing draws over the screen: what comes before it takes the
screen is printed above it (dax waits for Enter on what came while the
session opened), and what comes while it runs is held and printed when
it exits, above the resume command. In the
REPL and with `-p` it is printed as it comes. Keep the launcher quiet
(ssh's `-o LogLevel=ERROR`) so a start does not stop for a warning.

Speed: each model response costs two facts requests (its calls, then
their stamps) however many calls it makes, then its calls: those
between two shell commands go out together, and each command runs
alone, after the calls before it and before the calls after it. So a
response of five reads and one command costs at most five round trips
one after another (two for the facts, then the reads before the
command, the command, and the reads after it), and four when the
command comes first or last, where dax v0.0.23 ran every call of such
a response one at a time, eight in all. A `dax execute` of v0.0.23 or older
advertises `bash` as sequential only, and a session reaching one runs
such a response one call at a time, as before. The project's
`AGENTS.md`, config and skills are read over the pipe at the start, and
a project skill's files when it loads. Over `docker exec` this is
quick; over ssh each response pays a few round trips.

## Sessions and memory

A session's project is its workspace: the kind, ref and root `dax
execute` names (`-kind`, `-ref`, `-root`). The record keeps it in the
session's header, and, for a session run through an executor, a hash of
the address that reached it (the `-executor` command's words, or its
URL); the address itself is not written, since a command line can carry
what you would not want in a session file you share.

`dax -list` lists the project you would work on if you started dax
here. With no executor configured it is this directory on this
machine, as before; a sandboxed session whose root happens to be the
same path is not one of them. With one configured, by `-executor` or
your config's `executor`, it takes the latest session that address
reached, and lists every session recorded in that session's workspace,
whichever address reached them (a `docker exec` one day, a URL the
next). It does not start or connect to the executor: an address no
session used yet lists nothing. A command given as a string and as an
array is one address when they split into the same words.

One limit: an address stands for the machine it reached last. If one
address leads to different machines over time, say the same
`127.0.0.1:7777` tunnel pointed at another box, `-list` shows only the
latest box's project; the others are still there, and `dax -resume
<id>` opens any of them. Sessions recorded through an executor before
dax kept the workspace (v0.0.18 and earlier) have neither, so `-list`
does not find them from the executor either; `-resume <id>` still
works.

`-resume` notes when a session continues in another workspace than the
one it was recorded in, naming both (`recorded in container
app-box:/work, continuing in /home/me/src/app`), even when the two
roots are the same path.

The project's memory scope follows the workspace too. On this machine
it is the directory's, as before; in a sandbox it is named for the base
of the root with a hash of the kind, ref and root, so two sandboxed
projects started from one directory here keep two memories, and one
sandbox keeps one memory wherever you start dax.

All of this is as stable as `-kind`, `-ref` and `-root`. A container's
default ref is its hostname, which changes when the container is
recreated, and with it the project; the recipes here pass `-ref`, and
yours should too.

## When the connection drops

If the executor's connection breaks (the `docker exec` or `ssh` dies,
the container restarts, `dax execute -listen` restarts or closes the
session as idle, the tunnel drops), dax connects again on the next
request: it runs the `-executor` command again, as it ran it first, or
connects to the URL again, and prints `executor connection lost;
reconnected to …`. There is no reconnect loop in the background; if the
executor cannot be reached, that request fails saying so, and the next
one tries again.

What survives: the session, its record and its policy, and the tools,
since the new connection must be the same executor. What does not:

- A call that was running when the connection broke fails and is not
  sent again, since it may have run; the model sees the error. A check
  of what a call would touch, or a read of the project's files, has no
  effect, and is read again once on the new connection.
- Processes started in the sandbox for the session end with the old
  connection. Your MCP servers among them are started again, on the new
  connection, by the next call to one of their tools, with a warning
  (`dax: MCP server <name> ended and was started again …`) and an entry
  in the session's record (`dax.mcp.restart`). Whatever a server held
  (open handles, subscriptions) is gone. The restarted server's tools
  are offered from then on, even when they differ, and the warning says
  which were added or removed; a call the model made of a tool the
  restarted server no longer lists as it was is not sent, and fails
  saying so. A server that cannot be started again warns with the
  reason and fails the call, and the next call tries again. dax does
  the same for a server on your machine that exits.
- A call allowed before the drop and sent after it may be refused by the
  new executor (its stamp no longer matches); ask again.

The new connection must name the same workspace (kind, ref and root),
run the same extensions and offer the same tools with the same
definitions and claims, since the model was offered those tools and the
record names that workspace. The new connection asks for the same
`max_read_bytes` and must give the same: a `dax execute` restarted with
a lower `-max-read-bytes` is another executor. One that does not
(another container, a `dax execute` with other tools) is refused with a
warning, and every
call after fails, without connecting again, until you restart dax and
resume the session (`dax -resume <id>`).

## A served workspace: `dax workspace`

An executor is one cut between the session and the place its tools
act: at the tool calls. `dax execute` runs the tools there, makes their
facts claims there, and the session sends it each call it allowed. A
served workspace is the other cut: at the primitives. `dax workspace`
serves a directory as a whole workspace over HTTP, every stat, read,
write, command and process a request of its own (agentworkspace's
`remote`), and the session runs the tools here over it.

| | `-executor` (`dax execute`) | `-workspace` (`dax workspace`) |
|---|---|---|
| The tools and their facts claims | there | here |
| What crosses the wire | one call, its facts, its output | every file operation and process the tools and the checks make |
| The policy, the model, its key, the record, memory | here | here |
| bash, git, the MCP servers' processes | there | there |
| The project's `AGENTS.md`, `.dax/skills`, `.dax/config.json` | read through it | read through it |
| Authentication | none yet (`-listen` is loopback only) | one bearer token, always |
| TLS | no | `-tls-cert`, `-tls-key` |
| A program's own extensions | must be the executor's too | run here, unchanged |

A check that reads the project (a path rule following a link, the bash
analyzer's stats and globs, the git-config check) is a few round trips
over a served workspace where an executor's is one; a call that only
runs a command costs the same. Pick the executor when the link is slow
or the tools should never leave the sandbox; pick a served workspace
when the session's own tools (a program's extensions) should act there
unchanged, or when you want a token and TLS between the two.

### Serving and connecting

On the machine with the project:

```sh
dax workspace -dir ~/src/app
```

```
dax workspace: serving /home/me/src/app at http://127.0.0.1:7779
token: /home/me/.dax/serve/workspace-7779-1a2b3c4d.token
connect: DAX_WORKSPACE_TOKEN="$(cat /home/me/.dax/serve/workspace-7779-1a2b3c4d.token)" dax -workspace http://127.0.0.1:7779
or in your config: {"workspace": {"url": "http://127.0.0.1:7779", "token_env": "DAX_WORKSPACE_TOKEN"}}
```

It runs until a signal, reads no config, and says this on standard
error. The token is `$DAX_WORKSPACE_TOKEN` when set; otherwise it makes
one and writes it to a file only you can read, removed when it stops,
and prints the file's path, never the token. Copy the token to the
machine you run dax on, by whatever you trust, and put it in the
variable there. The token's holder may read and write the directory
and run any command as the user `dax workspace` runs as; there is no
`-insecure-no-auth`. The served directory's commands get its
environment less anything that looks like a credential, unless
`-pass-env` names it, as `dax execute`'s do.

Without a certificate it listens on loopback only (`127.0.0.1`, `::1`,
`localhost`; port 0 picks one), and another machine tunnels:

```sh
ssh -N -L 7779:127.0.0.1:7779 build-host &
DAX_WORKSPACE_TOKEN=... dax -workspace http://127.0.0.1:7779
```

With `-tls-cert` and `-tls-key` it serves HTTPS on any address, and the
session verifies the certificate against the system's roots or the CA
your config names:

```json
{
  "workspace": { "url": "https://build-host:7779", "token_env": "BUILD_WS_TOKEN", "tls_ca": "/home/me/.config/dax/build-ca.pem" }
}
```

| `workspace.` | |
|---|---|
| `url` | https, or http to a loopback host; no user, password, query or fragment. `-workspace URL` replaces it, `-workspace ""` clears it |
| `token_env` | the variable holding the token; default `DAX_WORKSPACE_TOKEN`. dax takes it out of its own environment as soon as the settings are read, before it starts any process (the key command first), so no child here inherits it, and never prints or records it. `pass_env` and `executor.pass_env` may not name it |
| `tls_ca` | for https: a CA certificate (PEM) in place of the system's roots; nothing skips the verification |

Only your config and the flags can name a workspace: a project's
`.dax/config.json` that sets `workspace` is refused, since it would send
the session's files and commands to a machine the repository chose.
`workspace` with `executor` (a command or a URL) or `box` is refused,
saying which to clear; `executor.pass_env` alone may stay.

### What runs where

Here: the model and its key, the policy and every decision, the
questions you answer, dax-coding's tools and every extension's, their
facts claims, memory, your own `~/.dax/AGENTS.md`, `agents_md_global`
and skills, and the session record. There: every file the tools read,
write or list, every command `bash` runs, every check's stat, glob and
`git config`, and the MCP servers (`mcp_servers`, `-mcp`, `/mcp add`),
started through the served workspace at its root with its environment
(a server with `"in": "host"`, or a `url`, is called from here, as
always). The project's `AGENTS.md`, `.dax/skills` and
`.dax/config.json` are the served directory's, read through it, and
the config's layer can only tighten, as anywhere.

A token whose principal lacks `exec` (a program serving `remote` with
read and write alone) still connects: reads and writes work, and
`bash`, the git-config check and every command fail as forbidden.
Your own MCP servers, which the workspace cannot start, run on this
machine with its environment scrubbed, as in any workspace that cannot
start a process. The project's command servers do not: they run in
its workspace or not at all, so a trusted one fails the session's start,
saying so, as with an executor that cannot start a process. The MCP
trust records where the servers run, so a set trusted while they ran
in the workspace is not trusted to run here. The start line says so
(`starts no process there`). `dax workspace`'s own token always holds
`exec`.

### Sessions and memory

The session's header records the workspace as `remote`, with the URL
as its ref and the served root as its root; the token is in no part of
it. The URL is normalised: its scheme and host in lower case, no
default port (80 for http, 443 for https), no query, fragment or
trailing slash, so `http://LOCALHOST:7779/` and `http://localhost:7779`
are one workspace (`http://127.0.0.1:7779` is another: dax does not
resolve names). `-list -workspace URL` (or
with your config's `workspace`) lists the sessions recorded in the
workspace the latest session at that URL recorded, without connecting;
like an executor's address, a URL stands for whatever served there
last. `-resume` notes a session continuing in another workspace, and
the memory scope is the workspace's, as for an executor: two served
directories keep two memories, and so do two URLs. The identity is the
URL and the root alone, so two machines reached in turn through the
same tunnel URL, each serving a directory at the same path (`/work`),
are one workspace: one `-list`, one memory scope (what dax remembered
of one project is offered in the other's prompt) and one MCP trust.
Give each its own local port (`ssh -L 7780:127.0.0.1:7779 other-host`)
to keep them apart.

### Limits

- No reconnect beyond the lease. Each file request stands alone, so a
  restarted `dax workspace` answers the next one. A started process
  lives in a session the client renews every 10 seconds on a 30-second
  lease: a session that lapses (the server restarted, the network gone
  for a whole lease) ends every process it started, and their reads
  and waits fail with `no such session`. MCP servers among them start
  again on the next call to one of their tools, as when an executor's
  connection drops, without what they held.
- A call in flight when the connection breaks fails, and is not sent
  again.
- No offered MCP servers: `dax workspace` serves a `workspace.Local`,
  and a workspace's own servers (`workspace.ServerProvider`, a box
  host's units) are not carried over HTTP yet.
- The server's bounds: a write of at most 32 MiB, a listing of at most
  10,000 entries (a directory with more fails to list, so `glob`,
  `grep` and `ls` over it fail), 16 MiB of each stream of a command
  whose output is not streamed, and at most 32 processes in a session.
  A dax session opens its session there at its first MCP server, and
  the one token is one principal, which holds at most 8 sessions at
  once: a ninth dax session with an MCP server fails to start it until
  another ends.
- Your own skills are read here; a skill that runs a script from its
  directory finds no such file there unless you put it there.

## Limits today

- A `unix:` executor is refused ("not supported yet").
- An executor at a URL authenticates no one, and is served over plain
  HTTP; keep it on loopback behind a tunnel.
- The sandbox does not loosen the policy unless you ask it to. dax
  claims no confinement for an executor's tools but a box's that your
  `box.confine` names ([Fewer questions in a box](#fewer-questions-in-a-box)),
  so every other rule and question is as on your machine.
- A dropped connection is made again (see
  [When the connection drops](#when-the-connection-drops)), but a call
  in flight fails, and your MCP servers in the sandbox start again
  without what they held: an open handle or a subscription is gone.
- Your own skills are read on your machine; a skill that runs a script
  from its directory finds no such file in the sandbox unless you put
  it there.
- Memory stays on your machine; the sandbox cannot read or write it.
- A served workspace (`-workspace`) reconnects only as its lease
  allows, carries no MCP servers a workspace offers of its own, and
  ends its processes when its session is lost (see [its
  limits](#limits)).
- The terminal client's own screen does not name the executor: its
  line is printed above the client, and the client (agentconsole) has
  no header for it yet.
