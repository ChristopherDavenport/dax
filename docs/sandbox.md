# Running dax's tools in a sandbox

dax can run its tools in a container, a pod or another host while the
session stays on your machine: start `dax execute` there, and start dax
here with `-executor` set to the command that starts it. These are
recipes for daily use; what `dax execute` serves and how is in the
README's [The tools in a sandbox](../README.md#the-tools-in-a-sandbox-dax-execute).

## What runs where

In the sandbox: dax-coding's tools (`read`, `write`, `edit`, `glob`,
`grep`, `ls`, `bash`) and every check of what a call would touch, the
project's `AGENTS.md`, `.dax/skills` and `.dax/config.json` (read from
the sandbox, not from your checkout), and your MCP servers. On your
machine: the model and its key, the policy and every decision, the
questions you answer, memory, your own `~/.dax/AGENTS.md`,
`agents_md_global` and skills, and the session record. The key never
enters the sandbox: the command that starts the executor gets your
environment less its credentials, the key's variable among them. The
connection is that command's standard input and output (a `docker
exec -i`, an `ssh`, a `kubectl exec -i`); there is no port, listener or
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
dax -executor "docker exec -i app-box dax execute -root $PWD -kind container -ref app-box"
```

`-i` and not `-t`: the pipe carries MCP. `-root` must exist in the
container; it is where every tool acts and nothing above it is read.
`-kind` and `-ref` are only what the record and the banner call the
workspace.

The tools' commands and MCP servers get `dax execute`'s environment,
which is the container's (the image's `ENV`, `docker run -e`), less
anything that looks like a credential. To give them one, set it on the
container and name it: `-e GITHUB_TOKEN` on `docker run`, and
`-pass-env GITHUB_TOKEN` on `dax execute`. Nothing from your machine's
environment reaches them.

## ssh

Install dax on the host with the same `go install`, then:

```sh
dax -executor "ssh -T -o BatchMode=yes build-host bash -lc 'exec dax execute -root /home/me/src/app -kind remote -ref build-host'"
```

ssh joins its arguments with spaces and the remote user's shell parses
the result, so quotes and `~` in the command work on that side. Here
they run `dax execute` under a login shell (`bash -l`): a
non-interactive ssh session often lacks the `PATH` with `~/go/bin` and
the Go toolchain, and the tools' commands inherit `dax execute`'s
environment. The tools run as the user
you log in as, with that user's files and rights on the host.

`-T` asks for no terminal, and `BatchMode=yes` makes ssh fail rather
than ask for a password, a passphrase or a new host key, since dax
holds the terminal. Connect by hand once first. dax removes
`SSH_AUTH_SOCK` from the command's environment along with the other
credentials, so ssh cannot reach your agent. Either use a key file
without a passphrase for that host, or let dax's ssh ride a connection
you opened yourself:

```
# ~/.ssh/config
Host build-host
  ControlMaster auto
  ControlPath ~/.ssh/cm-%C
  ControlPersist 8h
  ServerAliveInterval 30
```

then `ssh build-host true` in your terminal before starting dax. (Adding
`SSH_AUTH_SOCK` to `pass_env` also works, but `pass_env` hands it to
bash and MCP servers too whenever you run dax without an executor.)

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
it unless `pass_env` names it.

## Config

Put the executor in your own config (`~/.config/dax/config.json`):

```json
{
  "executor": {
    "command": "docker exec -i app-box dax execute -root /home/me/src/app -kind container -ref app-box"
  }
}
```

The command is split on spaces and run directly, with no shell: no
`$PWD`, no `~` and no quotes on this side, so a path with a space cannot
be passed to `docker exec` or `kubectl exec`. Since the root is fixed,
a shell function suits several projects better:

```sh
daxbox() { dax -executor "docker exec -i $(basename "$PWD")-box dax execute -root $PWD -kind container -ref $(basename "$PWD")-box" "$@"; }
```

- `-executor ""` runs one session on this machine despite the config.
- A project's `.dax/config.json` may not set `executor`; dax refuses the
  file. Only your config and the flags say where the tools run.
- `max_read_bytes` in your config does not reach the sandbox's tools;
  give `dax execute` `-max-read-bytes`.
- MCP servers are configured as usual (`mcp_servers`, `-mcp`) and start
  in the sandbox, at its root, so their commands must exist there.
  `/mcp add <name> <command>` (in `-front repl`) starts one there
  mid-session. The sandbox keeps no list of its own and a project's
  config cannot name one.
- Your `pass_env` applies only to the command that starts the executor;
  what the sandbox's commands get is `dax execute`'s `-pass-env`.
- `-config ~/.config/dax/sandbox.json` keeps a second config for
  sandboxed sessions, with the executor and rules of their own. It
  replaces your config rather than adding to it.

## Checking it

`dax -v` (or `-front repl`) prints the banner, with a line for the
executor: its name and version, kind and ref, and root,

```
executor: dax v0.0.9 · container app-box · /home/me/src/app
```

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
to your terminal, cleaned of control characters; keep the launcher
quiet (ssh's `-o LogLevel=ERROR`) so nothing draws over the terminal
client.

Speed: each model response costs two facts requests (its calls, then
their stamps) however many calls it makes, and each call is one more.
The project's `AGENTS.md`, config and skills are read over the pipe at
the start, and a project skill's files when it loads. Over `docker
exec` this is quick; over ssh each response pays a few round trips.

## Sessions and memory

The record names the executor's root as the session's directory, and
`dax -list` lists the sessions of the directory you run it in, matched
exactly, so mounting at the same path keeps `-list` working from your
checkout and `-resume` from noting a different directory. With a fixed path like `/work`, sandboxed
sessions do not show in `-list` there (`-resume <id>` still works). The
project's memory is keyed by the directory you started dax in on your
machine, not by the executor's root, so start dax from the project's
checkout.

## Limits today

- Only a command: an `http:`, `https:` or `unix:` executor is refused
  ("not supported yet").
- The sandbox does not loosen the policy. dax does not claim
  confinement for the executor's tools, so every rule and question is as
  on your machine; the sandbox limits what an approved call can reach,
  not how often you are asked.
- No reconnect: if the pipe breaks, the session's calls are blocked
  until you restart it.
- Your own skills are read on your machine; a skill that runs a script
  from its directory finds no such file in the sandbox unless you put
  it there.
- Memory stays on your machine; the sandbox cannot read or write it.
