# Running dax's tools in a sandbox

dax can run its tools in a container, a pod or another host while the
session stays on your machine: start `dax execute` there, and start dax
here with `-executor` set to the command that starts it, or to the URL
where `dax execute -listen` serves. These are
recipes for daily use; what `dax execute` serves and how is in the
README's [The tools in a sandbox](../README.md#the-tools-in-a-sandbox-dax-execute).

## A box: `dax -box`

For a container on this machine, dax does the recipes below itself:

```sh
cd ~/src/app
dax -box
```

The first time, it pulls the image and makes `dax-app-<hash>`, the
box of this directory: the hash is of the directory's path, with links
resolved, so the name is the same however often the box is made and
two projects called `app` have two boxes. After that `-box` reuses it,
starting it if it stopped; two sessions that start it at once share the
one box. A directory that is `/`, your home or above it is refused,
since its mount would hand the tools every key and config in your home,
and so is a path with a colon, which docker's `-v` cannot take. The
session's executor is

```sh
docker exec -i dax-app-<hash> /usr/local/bin/dax execute -root ~/src/app -kind container -ref dax-app-<hash>
```

so everything under [Docker](#docker) holds for it: the pipe is the
credential, there is no port, and `-list` and memory know the project
by the box's name and the directory. `-list` with `-box` (or with a
`box` in your config) finds its sessions without running docker.

What the box is made with:

| | |
|---|---|
| `--runtime runsc` | gVisor (see [gVisor](#gvisor)); `box.runtime` or `-box-runtime` names another. A runtime docker does not know fails the session; dax never falls back to `runc` |
| `--user` your uid and gid | the files the tools write in the project are yours |
| `--cap-drop ALL`, `--security-opt no-new-privileges` | no capabilities, and no setuid program can gain any |
| `--read-only`, `--tmpfs /tmp`, `--tmpfs /home/dax` | nothing outside the project survives the container; the home (`HOME=/home/dax`, go's caches among it) and `/tmp` are memory, counted against the limit |
| `--memory`, `--memory-swap`, `--cpus`, `--pids-limit` | `box.memory` (4g, no swap beyond it), `box.cpus` (2), `box.pids` (512) |
| `--network` | `box.network`: `bridge` by default, so commands reach the internet; `none` cuts it off (the executor needs no port); `host` is refused |
| `-v DIR:DIR`, `-w DIR` | the project at its own path, the one mount you can write |
| `-v <this dax>:/usr/local/bin/dax:ro` | this machine's dax, so the executor is the session's version |

The container's own process is `sleep infinity`; the tools run in the
`dax execute` each session's `docker exec` starts.

The image (`box.image`, `-box-image`; default `golang:1.26-bookworm`)
needs bash, sh, git, `sleep`, and a glibc at least as new as the one
this dax was built against: a dax built by `go install` links glibc.
An image without it (alpine) needs a dax built with `CGO_ENABLED=0`.
dax runs `dax execute -h` in a new box and says so when it fails.

A box is made from its settings once. Change `box` in your config and
the next `-box` refuses the box it finds, since making it again would
end any other session's tools in it: `dax box down` and start again.
A dax that changed since the box was made (an upgrade writes a new
file, which the box's mount does not see) makes the box again, with a
warning, for the same reason a newer executor is wanted.

```sh
dax box ls        # every box: name, state, runtime, directory
dax box down      # remove this directory's box; or dax box down NAME
dax box up        # make or start it, and print the -executor line
```

`dax box up` takes `-config`, `-image` and `-runtime`, and prints
`dax -executor '...'`, for a program that starts the executor itself.
Only your config can name a box; a project's `.dax/config.json` may
not, and `box` is refused beside an `executor` that names a command or
a URL. Its `executor.pass_env` alone may stay: the box's `docker exec`
is the command it applies to (`DOCKER_HOST`, say).

## gVisor

A box runs under gVisor's `runsc` by default: the box's processes talk
to gVisor's kernel, not this machine's. Install Docker Engine and
runsc from their apt repositories ([docker](https://docs.docker.com/engine/install/ubuntu/),
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

`-box-runtime runsc-ptrace` then names the second.

What the box does not do: it limits what an approved call can reach,
not how often you are asked. dax claims no confinement for an
executor's tools, so every rule and question is as on your machine.
Its network is open unless `box.network` is `none`. And anything
mounted is reachable: the project, read-write.

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
  mid-session. The sandbox keeps no list of its own and a project's
  config cannot name one.
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
their stamps) however many calls it makes, and each call is one more.
The project's `AGENTS.md`, config and skills are read over the pipe at
the start, and a project skill's files when it loads. Over `docker
exec` this is quick; over ssh each response pays a few round trips.

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

## Limits today

- A `unix:` executor is refused ("not supported yet").
- An executor at a URL authenticates no one, and is served over plain
  HTTP; keep it on loopback behind a tunnel.
- The sandbox does not loosen the policy. dax does not claim
  confinement for the executor's tools, so every rule and question is as
  on your machine; the sandbox limits what an approved call can reach,
  not how often you are asked.
- A dropped connection is made again (see
  [When the connection drops](#when-the-connection-drops)), but a call
  in flight fails, and your MCP servers in the sandbox start again
  without what they held: an open handle or a subscription is gone.
- Your own skills are read on your machine; a skill that runs a script
  from its directory finds no such file in the sandbox unless you put
  it there.
- Memory stays on your machine; the sandbox cannot read or write it.
- The terminal client's own screen does not name the executor: its
  line is printed above the client, and the client (agentconsole) has
  no header for it yet.
