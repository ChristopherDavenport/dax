# dax

A coding agent for the terminal. It reads, searches, edits and writes
files in your project and runs commands, asking you before it does
anything it should not do alone, and records every session so you can
list, resume, verify and export it. It runs against a local Ollama with
no setup, OpenAI, Anthropic or Gemini with a key, or Claude and Gemini on
Google Vertex AI with your gcloud login.

It is a thin product over a set of small Go libraries
(`openresponses`, `agentturn`, `agentkit`, `agentpolicy`,
`agentsession` and friends); [docs/design.md](docs/design.md) says who
owns what.

dax is a minimal core meant to be extended. The session (the model,
the store, the policy, the workspace, AGENTS.md, MCP servers,
compaction) knows no tool. Everything the model can do comes from an
extension, and dax's own are extensions like any other: `dax-coding`
(the file tools and bash), `dax-agents` (the sub-agents), `dax-skills`,
`dax-memory` and `dax-a2a` (your A2A peers). A Go program runs dax with extensions of its own on
the same terms, or leaves one of dax's out; see
[Building on dax](#building-on-dax).

Where the tools act is a value too. dax is meant to feel the same
whether its tools act on this machine or elsewhere: every tool, and
every check the policy makes of a call, goes through one workspace
interface, and this machine's directory is one implementation of it;
see [Where the tools act](#where-the-tools-act).

## Install

```sh
go install github.com/ChristopherDavenport/dax/cmd/dax@latest
# or, in a checkout
make install
```

Go 1.26 or later. With no flags dax talks to Ollama on
`localhost:11434` and runs `qwen3.5:9b`.

```sh
dax                          # REPL in the current directory; Ctrl-C aborts a run in flight
dax -p "what is in go.mod"   # one prompt, then exit
dax -provider anthropic      # a hosted model
```

## Providers and keys

| `-provider` | default model | key (environment or `api_key_command`) | `-base-url` |
|---|---|---|---|
| `ollama` (default) | `qwen3.5:9b` | none | default `http://localhost:11434/v1`; another Ollama host |
| `openai` | `gpt-5` | `OPENAI_API_KEY` | not supported |
| `openrouter` | `deepseek/deepseek-v4-pro-0813`; sub-agents `deepseek/deepseek-v4.1-flash` | `OPENROUTER_API_KEY` | not supported |
| `openresponses` | none; `-model` is required | the variable `-api-key-env` names, or none | required |
| `anthropic` | `claude-sonnet-5-5` | `ANTHROPIC_API_KEY` | not supported |
| `gemini` | `gemini-2.5-pro` | `GEMINI_API_KEY` | not supported |
| `vertex` | `claude-sonnet-5-5` | none; Google Application Default Credentials | not supported |

A provider is a vendor: `openai` is OpenAI, and `openrouter` is
OpenRouter, whose models are named `vendor/model`. Any other server that
speaks the Open Responses (OpenAI Responses) API, such as vLLM, LM Studio
or a proxy, is `openresponses`, with its `-base-url`, a `-model`, and
`-api-key-env` naming the variable its key is in, if it takes one:

```sh
LITELLM_KEY=... dax -provider openresponses -base-url https://llm.internal/v1 -model qwen3-coder -api-key-env LITELLM_KEY
```

`-model` names another model. `-subagent-model` names the one the
sub-agents run; without it a sub-agent runs the provider's default for
sub-agents, where it has one, else the main model.
A missing key is an error before any request
is made: `dax: openai: no API key: set OPENAI_API_KEY in the
environment`. dax reads keys from the environment and nowhere else (not
from a config file, not from a flag, so they stay out of `ps` and out of
a repository), and never prints one. The variable a key was read from is
removed from the environment of bash commands and MCP servers, whatever
it is called, unless `pass_env` names it.

For a key that expires, such as a short-lived access token, set
`api_key_command` in your config (or `-api-key-command`) to a program
that prints the key, in place of the variable:

```json
{
  "provider": "openresponses",
  "base_url": "https://llm.internal/v1",
  "model": "qwen3-coder",
  "api_key_command": ["gcloud", "auth", "print-access-token"]
}
```

dax runs it without a shell when it starts, again once the key is five
minutes old, and again when the server answers 401 or 403, then sends
the refused request once more with the new key, so a long session
outlives any one key. Its output must be a single token of printable
ASCII, at most 16 KiB. It may take up to five minutes, long enough to
wait on a sign-in in a browser. Concurrent requests share one run. The
key is held in memory only: it is never written down or printed, and
never put in any environment, so bash commands and MCP servers cannot
see it. It is for the providers that take a key (not `ollama` or
`vertex`), and is one source or the other with `api_key_env`.
`-api-key-command ""` turns off a command your config sets.

When the command fails, or the server refuses even a fresh key, the
turn ends with an error that starts `authentication failed` and carries
the last line the command wrote to standard error. A failure is not
retried and the command is not run again for a few seconds, so a
helper that cannot sign in is not run over and over; send the prompt
again once you have signed in. The REPL and `-p` show the command's
standard error as it runs; the terminal client keeps it for the error,
so it does not draw over the screen. To have the error say how to sign
in, set `api_key_login` to a URL or a command:

```json
  "api_key_command": ["my-token-helper", "print"],
  "api_key_login": "my-token-helper login"
```

gives `authentication failed; sign in with my-token-helper login, then
try again (...)`. dax only shows it; it does not open or run it.

A server may group a client's calls by session, or key its prompt cache
on the session, and record which client called. `session_header` names
the header each model call carries its session's ID in, and
`client_header` the header that carries `dax/` and dax's version:

```json
  "session_header": "X-Session-Id",
  "client_header": "X-Client"
```

The ID is that of the session the call is recorded in: a sub-agent's
calls carry the sub-agent's own session, and after `/clear` the new
session's. A request made outside a run, such as asking the vendor what
a model supports, carries none. Each must be a header name of letters,
digits and hyphens, not one the client sets itself (`Authorization`,
`X-Api-Key`, `Content-Type` and the like), and the two must differ.
They are not for `vertex`.

Ollama, OpenAI, OpenRouter and `openresponses` use the `openresponses`
client; Anthropic and Gemini use its provider adapters.

Requests to Claude, with `anthropic` or a `claude-` model on `vertex`,
use Claude's prompt caching: each turn reads the conversation so far
from the cache at a fraction of the input price instead of paying for
all of it again. Claude matches the cache on the prompt's prefix; the
session ID each request carries as `prompt_cache_key` only opts it in.
The cache lasts 5 minutes from its last use.

`vertex` is Google Vertex AI, which serves Claude and Gemini behind one
project and login. Each request goes to the Anthropic adapter for a
`claude-` model and to the Gemini adapter for a `gemini-` model, so the
main agent and the sub-agents can run different families and `/model`
can move between them; any other model name is refused. It takes no
key: it authenticates with Application Default Credentials
(`gcloud auth application-default login`, or
`GOOGLE_APPLICATION_CREDENTIALS`). It reads Google Cloud's usual settings
from the environment: the project from `GOOGLE_CLOUD_PROJECT`, falling
back to the credentials' own project, and the location from
`GOOGLE_CLOUD_LOCATION` (or `GOOGLE_CLOUD_REGION`), such as `global` or
`us-east5`.

```sh
gcloud auth application-default login
export GOOGLE_CLOUD_PROJECT=my-project GOOGLE_CLOUD_LOCATION=global
dax -provider vertex -model claude-opus-5-5 -subagent-model gemini-3.5-flash
```

Vertex publishes no model capabilities, so what dax knows is read off
the model ID's generation, from what Vertex accepted from each model:

| models | efforts | so |
|---|---|---|
| Claude before 4.6 (`claude-haiku-4-5`, `claude-opus-4-5`) | none | reasoning is off; the adapter sends an effort as adaptive thinking, which they do not take |
| Claude 4.6 | none to high | `xhigh` asks for `high` |
| Claude 4.7 to 5 | not described | sent as configured |
| Claude 5.5 and later | minimal to xhigh | `-think=false` asks for `minimal`: they refuse the way the adapter turns thinking off |
| Gemini | low to high | `none` and `minimal` ask for `low`, `xhigh` for `high` |

### What the model takes

At start dax asks the vendor what the model supports and shows it under
the banner, `model: reasoning low–max, always on · default high · 1M
context · 128k out (openrouter)`. Anthropic and Gemini are asked through
their model endpoints, OpenRouter through its public catalogue (fetched
once, without the key) and Ollama through `/api/show`; OpenAI and an
`openresponses` server publish nothing, and nothing is shown.

The reasoning effort dax asks for is then fitted to the answer: an
effort the model does not take becomes the nearest one it does, and dax
says so once, `[qwen3-coder:30b: reasoning effort low is not accepted;
asking for none (ollama)]`. So `-think` on a model that cannot reason
turns reasoning off instead of failing, and `-think=false` on one that
always reasons asks for its least effort. The fitting is done where each
configuration is made (the main agent's, the sub-agents', a fold's
summary, `/think` and `/model`), never to a request on its way out, so
the session records the effort that was sent. When the vendor cannot be
asked, or does not know the model, the effort is asked for as
configured. `-think` (default on)
asks for the `-effort` (`minimal`, `low` by default, `medium`, `high` or
`xhigh`) with summaries and shows the reasoning; `-think=false` turns
reasoning off. The effort is fitted like any other, so `medium` on a model
that takes only `low` and `high` asks for `low`.

## Sub-agents

The main agent can start sub-agents, each a run of its own on the
sub-agent model, recorded as a child session:

- `explore` investigates without changing anything (read, glob, grep, ls
  and bash) and returns a written answer.
- `task` carries out a coding task with the file tools and bash, then
  reports what it changed, what it ran and what is left. Its instructions
  are dax's, your `instructions_file` and the AGENTS.md chain, without
  skills or memory. The main agent chooses, per call:
  - `context`: `fresh` (the default) sees only the brief, so the brief
    must be complete; `fork` also sees the conversation so far, for a
    task that depends on what was said or decided in it.
  - `model`: `subagent` (the default) runs the sub-agent model; `main`
    runs the main model, for a task that needs it.

  Each call reads `/think` and `/model` as they stand: `main` is the
  model `/model` last chose, and with no `subagent_model` configured the
  sub-agents run it too.

  A fork's conversation is recorded in the child session as its own
  opening items, so the child verifies like any session. On OpenRouter
  the defaults were the cheapest in a trial: a fresh Flash sub-agent
  cost about a twentieth of a Pro one at the same pass rate, and a Pro
  fork about twice a fresh Pro task.

Several calls in one turn run at once, so the main agent can hand out
independent pieces of work in parallel. Writes and edits take a
workspace lock, so two sub-agents editing one file cannot lose an edit,
though they can still make changes that do not fit together; the
sub-agent is told to change only the files its task is about.

Both are offered by default; `-agents=false` or `"agents": false`
turns them off. They are the `dax-agents` extension, which ships an
allow rule for starting one, since what a sub-agent then does is decided
call by call by the same policy.

## Peers: other agents over A2A

An agent that serves [A2A](https://a2a-protocol.org) can be offered to
the model as a tool, by a name you give it in your own config:

```json
{
  "peers": {
    "reviewer": { "url": "https://reviewer.example", "token_env": "REVIEWER_TOKEN" },
    "builder": { "url": "http://localhost:9000", "relation": "peer" }
  }
}
```

Each call sends the model's request to the agent as one task and returns
its answer. When a session starts, each peer's agent card is read from
`<url>/.well-known/agent-card.json`, within five seconds and up to 1 MiB;
a peer whose card cannot be read, whose `token_env` is not set, or whose
card names an endpoint at another origin than `url` is not offered, and
the session says why once. The card's name never replaces yours, and
the session's record names the URL each call reached.

- `url` is `https`, or `http` to this machine only, with no credentials,
  query or fragment.
- The card must name its JSON-RPC endpoint at `url`'s origin (scheme,
  host and port): the card, not you, says where the work goes, so a card
  that points elsewhere (another host, or a port on this machine) is
  refused. No redirect to another origin is followed, on the card's fetch
  or on a call.
- `token_env` names the variable holding a bearer token, which is sent
  to that origin alone. dax takes the variable out of the environment
  of bash, MCP servers and an executor's command, whatever its name,
  and refuses a `pass_env` or `executor.pass_env` that names it; the
  token is never printed, logged or recorded.
- `relation` is what an abandoned call does. `subsession` (the default):
  the task is the call's own work, so when the call is given up (an
  abort, say, even while the agent's question waits) dax sends the agent
  `tasks/cancel` and goes on without waiting for it. `peer`: the
  agent's work outlives the call and is never cancelled.
- A question the agent asks during a call is put to you as a question
  for text, naming the peer and its URL first; the agent's own words come
  last, quoted. In the REPL and `-p` a reply that is not an explicit `n`
  or `no` is the answer (`blue`, or `y blue`; a bare `y` sends "yes"); in
  the terminal client `a` and the text answer, `y` sends "yes". The answer
  goes back on the same task, and the call's result tells the model what
  the agent asked and what you answered. `n` refuses: the call ends with the
  question, what you said reaches the model once, as yours, and a
  subsession's task is given up. When nobody is answering questions, or
  `-p` has reached the end of its input, nobody answered (the record
  says so) and the call ends with the question for the model; nothing
  answers in your place. A subsession's task that ends that way is given
  up, with `tasks/cancel`, since nothing would come back to it; a peer's
  is left asking.
- Every call asks unless your config allows the peer by name
  (`"allow": ["reviewer"]`); `dax-a2a` ships no rule. The card's
  description and skills go into the tool's description, which the model
  reads: text the remote agent wrote, worth reading before you allow a
  peer by name.

Peers run where the session runs, never in an executor, and only the
main agent calls them. A repository's `.dax/config.json` may not name
one. `dax attach` shows their calls as raw arguments, since it reads no
config.

## Config

Settings come from three layers, each overriding the one before:

1. `~/.config/dax/config.json` (`$XDG_CONFIG_HOME/dax/config.json`; `-config path` names another)
2. `.dax/config.json` in the working directory, read through the workspace, which can only tighten the policy
3. flags

```json
{
  "provider": "anthropic",
  "model": "claude-sonnet-5-5",
  "base_url": "",
  "think": true,
  "effort": "high",
  "instructions_file": "~/.config/dax/instructions.md",
  "skills_dirs": ["~/skills"],
  "memory_dir": "~/.dax/memory",
  "pricing_file": "~/.config/dax/prices.json",
  "mcp_servers": {
    "fs": { "command": "mcp-server-filesystem /home/me/notes", "in": "host" },
    "gh": { "url": "https://api.githubcopilot.com/mcp/", "token_env": "GITHUB_TOKEN" }
  },
  "policy": {
    "builtin": true,
    "fallback": "ask",
    "allow": ["bash(make:*)", "write(docs/**)"],
    "ask": ["bash(go test -race:*)"],
    "deny": ["bash(git push --force:*)"]
  }
}
```

`model`, `subagent_model`, `base_url`, `api_key_env`, `api_key_command`, `api_key_login`, `session_header` and `client_header` belong to the provider in force
where they are set. A later layer that switches the provider leaves them
behind, so with `"model": "qwen3-coder:30b"` in your config,
`dax -provider openrouter` runs OpenRouter's default model rather than
asking OpenRouter for an Ollama one; `-model` beside `-provider` names
another.

Every field is optional. The files are strict: an unknown field, a bad
provider, a malformed URL or a rule that does not parse is an error that
names the file and the field.

| field | meaning |
|---|---|
| `provider`, `model`, `subagent_model`, `base_url`, `think`, `effort` | as above; `base_url` is for `ollama` and `openresponses` |
| `agents` | offer the explore and task sub-agents; default `true` |
| `api_key_env` | the variable holding the `openresponses` provider's key (the name, never the key) |
| `api_key_command` | a program and its arguments, `["program", "arg", ...]`, that prints the provider's key; run again as the key ages or is refused (see above) |
| `api_key_login` | how to sign in again, a URL or a command, shown when `api_key_command` fails or its key is refused; one line, at most 500 bytes |
| `session_header` | the header each model call carries its session's ID in, for a server that groups calls by session (see above) |
| `client_header` | the header each model call carries `dax/<version>` in, for a server that records which client called |
| `instructions_file` | your own instructions, added to the system prompt after dax's; a relative path is relative to the file that names it |
| `skills_dirs` | more skill directories, after `.dax/skills` and `~/.dax/skills`; one that does not exist is an error |
| `agents_md_global` | your own instruction files for every session, whatever the repository holds, such as an `AGENTS.md` in a directory above your checkouts: read on this machine after `~/.dax/AGENTS.md` and before the repository's `AGENTS.md` chain, in order; a missing one is skipped; a relative path is relative to the file that names it |
| `memory_dir` | where the model's memory lives; `""` turns memory off. Default `~/.dax/memory` |
| `pricing_file` | a JSON file of model prices (`"model": {"input","cached","output"}` in USD per million tokens), used by the terminal client's status line and session pane to show cost; without it the client shows token usage but no cost |
| `mcp_servers` | MCP servers by name; the name prefixes their tools, `mcp__<name>__<tool>`. Each is a `command`, split as `executor`'s string is, or a `url`, not both. A command's `in` is `workspace` (the default: in the workspace, or the executor or box, when it can start a process, here when it cannot) or `host` (on this machine, with its credentials scrubbed, even beside an executor). A `url` is a streamable HTTP server called from this machine, https or http to loopback, and `token_env` names the variable whose token is sent to its origin alone and which no child process gets. A project's file may name servers too (no `token_env`, and no name yours has), which start only once you trust the project's set with `-trust-mcp` |
| `max_read_bytes` | the most bytes of a file `read` scans per call and `edit` will rewrite; default 2 MiB (use `grep` to find a line later in a bigger file); with `executor`, at most `dax execute`'s `-max-read-bytes` |
| `pass_env` | credential-looking variables bash commands and MCP servers may inherit, by name (default none) |
| `executor` | `{"command": "docker exec -i box dax execute -root /work"}`: run the tools, and the MCP servers, in `dax execute` started by that command (see below), or `{"url": "http://127.0.0.1:7777"}` in `dax execute -listen` already serving there; one or the other. `command` is a command line, split as a shell splits words (`'…'`, `"…"`, `\`) with nothing expanded (no `$VAR`, `~` or globs), or an array, `["docker", "exec", "-i", "box", "dax", "execute", "-root", "/my work"]`, used exactly; no shell runs either |
| `box` | `{"host": "local", "profile": "go", "tighten": {"memory": "2g", "internet": "off"}}`, each optional: run the tools in this project's box, which a box host (`dax box serve`, here or over ssh) makes from its own config, `~/.config/dax/box-host.json` on its machine, and relays the session to (see below); `{}` is a local box host's default profile. `host` is `local` or `ssh://[user@]host[:port]`; `profile` names one of the box host's; `mcp` which of its MCP units (each an MCP server the box host runs in a container of its own, with its own files, internet and secrets, its output redacted of those secrets' values; empty is all); `tighten` (`memory`, `cpus`, `pids`, `internet`, `allow`) only narrows its workspace unit. With `local`, `host_config` names another box host config; with ssh, `project` (the path there) or `checkout` (`{"url", "ref"}`: a repository the box host clones and keeps, within its config's `checkouts.allow`; no credentials in the URL), `remote_dax` (dax there) and `ssh` (more ssh arguments). The image, runtime and internet's ceiling are the box host's. `confine` (`off`, `units`, `workspace`) has the policy take the box's tools as confined where their envelope allows, so a bare ask for them does not ask; `workspace` also has the box host hold the repository's `.git` read-only in the box, the git guard (git that writes then fails there; commit outside it), and claims nothing for the workspace without it (docs/sandbox.md, "Fewer questions in a box"). Not with `executor` (its `pass_env` alone may stay, for the box host's launcher) |
| `workspace` | `{"url": "https://box.example:7779", "token_env": "DAX_WORKSPACE_TOKEN", "tls_ca": "ca.pem"}`: run the tools here, in a workspace `dax workspace` serves there (see [A served workspace](#a-served-workspace-dax-workspace)). `url` is https, or http to a loopback host, with no user, password, query or fragment; `token_env` names the variable holding its token (default `DAX_WORKSPACE_TOKEN`), which only the workspace is sent and `pass_env` may not name; `tls_ca`, for https, a CA certificate to verify its certificate against in place of the system's. Your file only; not with `executor` or `box` |
| `workspace.hub` | `{"listen": ":7780", "name": "build", "token_env": "DAX_TUNNEL_TOKEN", "tls_cert": "hub.pem", "tls_key": "hub-key.pem", "wait": "60s"}`, in place of `workspace.url`: host a hub for a workspace that dials in (`dax workspace -connect`, see [A workspace that dials in](#a-workspace-that-dials-in-dax-workspace--connect)), and act in it once it does. `listen` is a loopback address unless `tls_cert` and `tls_key` are given; the upgrade path is `/tunnel`. `name` is the workspace's name and identity, a DNS label; `token_env` the variable holding the one token that enrolls it (default `DAX_TUNNEL_TOKEN`, at least 32 bytes, taken out of the environment before any process starts, and no other credential's variable); `wait` how long to wait for it (default `60s`). Your file only; not with `url`, `executor` or `box` |
| `peers` | remote A2A agents offered as tools, by tool name: `{"reviewer": {"url": "https://…", "relation": "subsession", "token_env": "REVIEWER_TOKEN"}}`; see [Peers](#peers-other-agents-over-a2a). Your file only; a token's variable may not be in `pass_env` |
| `executor.url` | the `http://` or `https://` URL where `dax execute -listen` serves, in place of `command`; no user, password, query or fragment. Nothing authenticates the session to it yet, so keep it on loopback and tunnel to it. It takes no `pass_env` |
| `executor.pass_env` | variables, credentials included, that only the command starting the executor is given, by name: `SSH_AUTH_SOCK` for ssh's agent, the token a kubectl credential plugin reads. bash and MCP servers never get them, here or in the sandbox; the model's key may not be one. It may be set without `command`, for a `-executor` given each time |
| `policy` | see below |

A project's `.dax/config.json` comes from a repository, not from you, so
it can only **tighten**. It may add `ask` and `deny` rules to `policy`
(no `allow`, and no `!` carve-out of any rule), set `"builtin": false` to
drop the shipped allow rules (their asks and denies stay), and set
`"fallback"` to `ask` or `deny` when
that is stricter than yours. It cannot bring back what you dropped or
loosen what you set, and its rules rank below yours so they cannot cancel
one of yours. It may **not** set `provider`, `model`, `subagent_model`, `base_url`, `api_key_env`, `api_key_command`, `api_key_login`, `session_header`, `client_header`, `think`, `effort`, `agents`,
`instructions_file`, `skills_dirs`, `agents_md_global`, `memory_dir`, `pricing_file`, `executor`, `box`, `workspace` or `peers`: dax
refuses the file with an error naming the field and saying to put it in
your own config. (Where the model runs, what it is told and remembers, and
what programs start are decisions that send your code, your files and your
keys somewhere; a repository does not get to make them.)

The one exception is `mcp_servers`: a project may name the servers its
work needs, but they start only once you have trusted that exact set
for that project. Run `dax -trust-mcp` after reading them in the
project's `.dax/config.json`: it starts them and records the trust in
`~/.dax/mcp-trust.json`, keyed by the project's workspace (as `-list`
names it) and a hash of the set, so later sessions start them without
the flag. A set that changes in any way, a command, a URL, a placement
or a server more, is untrusted again; until it is trusted, dax starts
your own servers, says which of the project's it left out, and starts
none of them. A project's server may not name a `token_env` (it would
send your credential where the repository says) or take a name your
config gives a server (nor that name and an underscore, whose tools
your rules for it would cover). dax believes the trust file only while
no other account could have written it.

| flag | |
|---|---|
| `-provider`, `-model`, `-subagent-model`, `-base-url`, `-api-key-env`, `-think`, `-effort` | override the config |
| `-api-key-command 'program arg ...'` | overrides `api_key_command`, split as a shell splits words with nothing expanded; `""` turns it off, and `api_key_login` with it |
| `-api-key-login hint` | overrides `api_key_login` |
| `-session-header name`, `-client-header name` | override `session_header` and `client_header`; `""` turns one off |
| `-config path` | the user config file |
| `-memory dir` | memory directory; `off` or empty disables it |
| `-executor 'command ...'` | overrides `executor`: the command line that starts `dax execute` where the tools act, split as a shell splits words (`-root "/my work"`) with nothing expanded, or the `http://` or `https://` URL where `dax execute -listen` serves; `""` clears it |
| `-executor-pass-env NAME,NAME` | overrides `executor.pass_env`; `""` clears it |
| `-workspace URL` | overrides `workspace.url` (or `workspace.hub`, dialing the URL in its place): the URL where `dax workspace` serves, its token from `$DAX_WORKSPACE_TOKEN` or `workspace.token_env`; `""` clears the config's. Refused with an executor or a box: clear the config's with `-executor ""` or `-box=false` |
| `-box` | run the tools in this project's box (see below), with `box`'s settings or a local box host's default profile; `-box=false` runs here despite the config's `box`. Refused with an executor: clear the config's with `-executor ""` |
| `-box-host local\|ssh://[user@]host[:port]`, `-box-profile NAME` | with `-box` or a `box` in your config, override `box.host` and `box.profile` |
| `-box-confine off\|units\|workspace` | with `-box`, `box.confine` for this run |
| `-box-internet off\|allow\|on` | with `-box`, narrow the profile's internet for this run; `allow` keeps the profile's hosts (or `box.tighten`'s). It is the box's, so it applies to every session in it |
| `-pricing-file path` | JSON file of model prices for the terminal client's session cost |
| `-no-policy` | run every tool call without asking; ignores the config's policy |
| `-front tui\|repl` | the front end; default `tui` when standard input and output are both terminals and a session is recorded, `repl` otherwise (so `-sessions ""` gives the REPL); `-front tui` without a terminal or a session store is refused; `-p` always prints |
| `-v` | the terminal client prints its start lines before it takes the screen and what dax noted during the run after it exits; without it, only the executor's line and warnings before, and warnings that came while it had the screen and the resume command after |
| `-agents-md-global 'a.md:b.md'` | overrides `agents_md_global`, split on `:` as `PATH` is, so a path may hold a space; `""` clears it |
| `-agents-md`, `-skills`, `-trust-skills` | the AGENTS.md files (`~/.dax/AGENTS.md`, `agents_md_global` and the repository's chain), skills, and a skill's `allowed-tools` running unasked until the next message, for skills in `~/.dax/skills` and `skills_dirs` only, never the repository's |
| `-compact N`, `-compact-server` | fold the transcript above N estimated tokens, locally or through the server; without `-compact`, N is three quarters of the model's context window when the vendor reports the window, and compaction is off when it does not; `-compact 0` turns it off |
| `-mcp "cmd"` | one more stdio MCP server, as `mcp__cli__<tool>`; split as `-executor` is |
| `-trust-mcp` | start the MCP servers the project's config names, and trust that set for the project from now on (`~/.dax/mcp-trust.json`) |
| `-agents` | offer the `explore` and `task` sub-agents (default on; `-agents=false` turns them off) |
| `-sessions dir`, `-sync append\|response\|never` | the session store (`-sessions ""` disables recording) and when appends are durable |

## Tools

| tool | what it does |
|---|---|
| `read` | numbered lines of a file, with `offset` and `limit` |
| `write` | create or replace a file, making parent directories |
| `edit` | replace one exact occurrence of a string |
| `glob` | paths matching a doublestar pattern (`**/*.go`, `cmd/*/main.go`, `*.{md,txt}`), sorted |
| `grep` | a regular expression over files: `path`, `include` (a glob), `ignore_case`, `max_results`; prints `path:line:text` |
| `ls` | one directory, sorted, directories with `/`, files with their size |
| `bash` | a command in the working directory with a timeout; runs one at a time, its output reported as progress while it runs |

The file tools are confined to the working directory. A path that is
absolute outside it, climbs out with `..`, or goes out through a
symbolic link is refused. `bash` is not confined (a shell reaches what you
reach); the policy is what stands in front of it. `glob` and `grep` skip
`.git`, `node_modules`, `vendor`, `.venv`, `__pycache__` and similar trees
by name; there is no `.gitignore` support. Search a skipped directory by
naming it as `path`.

## Environment of commands and servers

Commands the `bash` tool runs and the MCP servers dax starts do not get
your credentials. A variable is removed from their environment if its
name ends in `_KEY`, `_KEY_ID`, `_PAT`, `_PWD`, `_JWT`, `_CREDENTIALS`,
`_AUTH`, `_TOKEN`, `_SECRET`, `_PASSWORD` or `_API_KEY`, contains
`PASSWORD` or `SECRET`, is one of `PASSWORD`, `TOKEN`, `API_KEY`,
`SECRET_KEY`, `DATABASE_URL`, `SSH_AUTH_SOCK`, `PGPASSWORD`, `MYSQL_PWD` or
a provider or CI token by name (`OPENAI_API_KEY`, `GITHUB_TOKEN`, ...), or
holds a URL with `user:password@` in it. So a test, a build script or a
server cannot read them, and cannot use your ssh agent.

Variables that hold the **path of a credential file** pass through, since
a program that needs its file needs them: `GOOGLE_APPLICATION_CREDENTIALS`,
`KUBECONFIG`, `DOCKER_CONFIG`, `NETRC`, `AWS_SHARED_CREDENTIALS_FILE`,
`AWS_CONFIG_FILE`, `CLOUDSDK_CONFIG`, `PGPASSFILE`. The files are as
readable to a command as they are to you. dax has no setting to withhold
them; unset one before starting dax to keep it from commands.

A command that needs a scrubbed variable (`gh`, a private module proxy)
gets it by name from your config: `"pass_env": ["GITHUB_TOKEN"]`. Only
your own config can say that.

## The tools in a sandbox: `dax execute`

`dax execute` serves dax's tools over MCP on its standard input and
output, or over HTTP with `-listen`, from inside the place they should
act: a container, a VM, another host. It runs no model, no policy and
no session. A session elsewhere decides each call under its own policy
and sends it there to run: start that session with `-executor` (or
`"executor"` in your config) set to the command that starts `dax
execute`, or to the URL where it listens.

```sh
dax -executor 'docker exec -i box dax execute -root /work -kind container -ref box'
```

```sh
docker exec -i box dax execute -root /work -kind container -ref box
ssh build-host dax execute -root /home/me/src/app
kubectl exec -i pod/agent -- dax execute -root /work -kind container -ref pod/agent
```

`-box` does all of that for one project. It starts a box host, `dax
box serve`: this dax as a child, or the dax on another machine over
ssh. The box host makes, or reuses, a container named for the project
(`dax-<base>-<hash>`) from a profile of its own config, under the
runtime that profile names (gVisor's `runsc` by default; any runtime
docker has registered, Kata's among them), and relays the session to
the `dax execute` in it. Local is remote with a shorter connection:
the same config, the same box, the same decisions. The project is
mounted at its own path and the box host's dax read-only as the box's;
the box has no capabilities, a read-only root file system, memory for
`/tmp` and its home, and limits on memory, processors and processes.
The image needs bash, sh, git and glibc. What the box is (image,
runtime, limits, internet) is the box host's config's; the session
picks a profile and may only narrow it. A profile is units: the
workspace, where dax's tools act, and MCP units, each an MCP server in
a container of its own with its own image, files (none, or the project
read-only or read-write), internet and secrets, which the box host
resolves and gives to that container alone; the workspace's bash never
sees them. On another machine the project is a path there or a
checkout the box host keeps of a repository you name (`box.checkout`),
cloned and moved on only when nothing of a session's would be lost.
`dax box status`, `dax box down` and `dax box pull` (the checkout's
head into a local branch) go through the box host your `box` names,
here or there; `dax box ls` lists this machine's boxes and units, and
`dax box up` makes this directory's and prints its executor's command. The box limits what an
approved call can reach, not how often you are asked: the policy is
the same as here. It has no internet unless its profile gives it some,
and then only HTTP and HTTPS through its sidecar's proxy, never the
box host's machine or a private network.

```sh
dax -box                               # this directory's box, made on first use
dax -box -box-profile big              # another of the box host's profiles
dax -box -box-internet=off             # this run, no internet whatever the profile gives
dax -box -box-host ssh://me@build-host # the box on another machine
dax box status                         # the box, its units and its checkout, through the box host
dax box pull                           # the box host's checkout's head into dax-box/<repository> here
dax box down
dax box ls
```

[docs/sandbox.md](docs/sandbox.md) has recipes for daily use: the box,
the box host config and its units, a box over ssh, runtimes (gVisor, Kata), an image
with dax in it, each launcher, the config, what to
check and what does not work yet.

The pipe is the credential: whoever started the process is its one
client. There is no listener, no port and no token. Since no policy
runs in it, anyone who can start it can run whatever its tools run,
which is no more than anyone who can already `docker exec` or `ssh`
there.

With `-listen` it is a server instead, started once and left running,
which sessions connect to by URL:

```sh
dax execute -listen 127.0.0.1:7777 -root ~/src/app -kind remote -ref build-host   # there
dax -executor http://127.0.0.1:7777                                                # here
```

It speaks MCP's streamable HTTP transport, and each client is a session
of its own, with its own processes, over the one workspace. **Nothing
authenticates a client yet**: whoever reaches the address runs the
tools. So it listens on a loopback address only (`127.0.0.1`, `::1`,
`localhost`), and another machine reaches it through a tunnel, `ssh -o ExitOnForwardFailure=yes -L
7777:127.0.0.1:7777 build-host`; `-insecure-no-auth` takes any address,
and says on standard error that anyone who can reach it can run
commands. A request from a web page is refused (another origin, or a
host name other than the loopback address it reached). A session that
starts no request for five minutes is closed and its processes end; a
connected client pings every minute, so only one that went away is
closed. A signal stops it, and every session's processes with it.

| Flag | Default | |
|---|---|---|
| `-root` | the current directory | where the tools act |
| `-max-read-bytes` | 2 MiB | the most a session's `max_read_bytes` may be: each session's tools read what its config says, up to this; a dax older than the setting sends none and gets this |
| `-pass-env` | none | comma-separated variables to pass to commands though they look like credentials, as `pass_env` |
| `-kind` | `remote` | the workspace's kind as the session records it: `local`, `container` or `remote` |
| `-ref` | this host's name | the workspace's ref as the session records it: an image, a host, a pod. With `-kind` and `-root` it is the project's identity for `-list` and memory, so give one that outlives a recreated container |
| `-listen` | none: standard input and output | serve over HTTP at this address, a loopback one unless `-insecure-no-auth`; the address is printed on standard error |
| `-insecure-no-auth` | off | with `-listen`, take an address that is not loopback; nothing authenticates a client |

It reads no config file: the sandbox holds the files it would read,
and the policy is the session's. Commands get its environment with
credentials removed, as under a session; the model's key is never sent
to it.

It serves the tools of every extension that has them: dax-coding's,
and a program built on `dax.Main` serves its own as `<name> execute`,
less those it leaves out with `WithoutExtension`. Sub-agents, skills
and memory are the session's and stay where it runs. The tools served
are the tools themselves, so what a call would touch is read there, in
the sandbox's files, and the stamp that holds an allowed call to what
was decided is made and checked there, under that process's own key: a
stamp made anywhere else is refused, and a file tool's path that
became a link since the decision is caught where the file is. What MCP has no field for
crosses beside the tools: agenttool's `_meta` (facts and replay claims,
sequential, barrier, resource), its `execution/facts` method, and the
experimental capability `io.github.christopherdavenport.dax/executor`
with the descriptor, the extensions, each tool in order with its
extension, read-only, facts and strict, and where its files are read. A
running command's output arrives as progress, and a tool's question
reaches the session.

It also serves the workspace's files, read-only, so the session reads
the project where the project is: MCP resources under the template
`agentworkspace:///{op}{?path}` (agentworkspace's `mcpworkspace`), where `op` is `stat`, `lstat`,
`readdir`, `readlink` or `read` and `path` a name relative to the root
(`.` for the root itself), percent-encoded. Every name goes through the
workspace's own confinement: one that is absolute, climbs out with `..`
or leads out through a link is refused, and nothing is written. A read
gives at most 1 MiB of a regular file (the bound on a project's config
and a skill's file) and refuses a larger one, a directory or a FIFO; a
listing gives at most 10,000 entries. The reply is one JSON document,
with a refusal's kind (`notexist`, `outside`, `invalid`, `permission`
or `other`) in it, so the session sees the same errors a read of its
own directory would give.

It can start a process for the session, a process that lives longer
than one call with pipes to it, as the session's own machine starts an
MCP server: custom JSON-RPC methods (agentworkspace's `mcpworkspace`),
`agentworkspace/process.start`, `.write`, `.read`, `.closeStdin`,
`.signal`, `.wait` and `.close`,
named in the capability's `start` field. They are not tools, so another
harness connected to `dax execute` is offered no tool that runs a
command, and nothing in the sandbox (its files, a project's config, a
tool) makes it start one: it starts only what its one client asks for
and keeps no list of its own. A process starts at the root (or a
directory under it) with the environment commands get; each session has
its own processes, at most 32 open at once, and an id another session
started is refused; a write is at most 1 MiB and a read's reply at most
64 KiB; standard error it holds unread is bounded, and past the bound
dropped. When the session's connection closes, or a signal stops `dax
execute`, every process it started for the session ends with it. A
session started with `-executor` runs its MCP servers this way.

With `-executor`, the session starts the command with this machine's
environment less its credentials, but those `pass_env` and
`executor.pass_env` name, and never the model's key's variable, so the
key stays here. What the command and `dax execute` write to standard
error is cleaned; while the terminal client has the screen it is held
with dax's other warnings and printed when the client exits. It refuses a
program that is not a dax executor (no facts method, no capability, a
capability of another version, or tools the capability does not name),
and checks the tools against what it would have built: every extension
with tools must be one the executor runs. Each call is decided here, on
what the executor says it would touch; when the executor cannot say (its
claim fails, a reading takes longer than 30 seconds, the executor is
gone) the call is blocked, never allowed. The banner and the record name
the executor's workspace, and the model is told its root.

The MCP servers run in the sandbox too: every command in your config's
`mcp_servers` (but those with `"in": "host"`), `-mcp` and `/mcp add`
starts in `dax execute`, at its root and with the environment its
commands get, never on this machine. A server with `"in": "host"` or a
`url` is called independently of the sandbox, from this machine.
Which servers start is said here: the executor keeps no list and reads
no config, a box host offers its units, and a project's
`.dax/config.json` names servers only you have trusted (`-trust-mcp`). Their tools are `mcp__<name>__<tool>` as anywhere, decided here by
name under your rules (they ask by default; an MCP server's tools make
no claim of what they would touch), and recorded here. Their standard
error comes back cleaned, and `/mcp remove`, the session's end or the
executor's end stops them. An executor too old to start a process (dax
v0.0.8 or earlier) fails a session that has an MCP server, and `/mcp
add`, with "update dax execute where it runs"; the server is not started
here in its place.

The project's files are the executor's, read through it and not from
this directory: the root's `AGENTS.md`, `.dax/skills` and
`.dax/config.json`. They are screened as a local project's are: an
`AGENTS.md` that is a link out of the workspace, and a skills directory
with a link out of it, are left out with an `omitted:` line. Nothing
above the executor's root is read, from either machine, even when it
says it is a local directory. The project's config is read once the
executor is connected (your config and the flags say which executor; a
project's file cannot) and, as anywhere, can only tighten the policy;
one that cannot be read (a link out, over 1 MiB, the executor gone or
answering none of it within 30 seconds) stops the start, since leaving
it out would drop rules that only tighten. An executor whose files
cannot be read at all when the session starts stops it too; a single
`AGENTS.md` or skills directory that cannot be read is left out and
reported, as on this machine. Your own `~/.dax/AGENTS.md`,
`agents_md_global` and skills are read here as usual. An executor from
dax v0.0.6, which does not serve its files, is refused: update `dax`
where it runs.

An executor at a URL is already running, so nothing here starts it:
no environment goes to it, `executor.pass_env` and
`-executor-pass-env` do not apply (naming either with a URL is an
error), and no standard error comes back; it is on the terminal where
`dax execute -listen` runs. The URL carries no user or password, since
the executor takes no credential yet.

What `-executor` does not do yet:

- A `unix:` address is refused.
- No restart of a call that ran over a broken connection: dax connects
  again on the next request (the command run again, the URL reached
  again) if it is the same executor, but a call in flight then fails.
  The MCP servers started there end with the connection and start again
  on the next call to one of their tools, without what they held, with
  a warning and a line in the record. See docs/sandbox.md, When the
  connection drops.

Standard output carries MCP and nothing else. While it serves,
`os.Stdout` is standard error, so a stray print from a tool cannot
corrupt the stream; commands get no standard input and their output is
captured. Logs and errors go to standard error.

## A served workspace: `dax workspace`

`dax workspace` serves a directory, whole, over HTTP: its files, its
writes, its environment, its commands and its long-lived processes,
each crossing the wire on its own (agentworkspace's `remote`). A
session started with `-workspace` (or `"workspace"` in your config)
acts in it, while its tools, their checks and the policy run here.
`dax execute` cuts at the tool calls, so the tools run where they act;
this cuts at the primitives, so the tools run here and only what they
touch is there. [docs/sandbox.md](docs/sandbox.md#a-served-workspace-dax-workspace)
compares the two.

```sh
dax workspace -dir ~/src/app                    # there
# dax workspace: serving /home/me/src/app at http://127.0.0.1:7779
# token: /home/me/.dax/serve/workspace-7779-1a2b3c4d.token
# connect: DAX_WORKSPACE_TOKEN="$(cat /home/me/.dax/serve/workspace-7779-1a2b3c4d.token)" dax -workspace http://127.0.0.1:7779

ssh -N -o ExitOnForwardFailure=yes -L 7779:127.0.0.1:7779 build-host &      # here
DAX_WORKSPACE_TOKEN=... dax -workspace http://127.0.0.1:7779
```

| `dax workspace` flag | |
|---|---|
| `-dir DIR` | the directory to serve; default the current one |
| `-listen ADDR` | where to serve; default `127.0.0.1:7779`, port 0 picks one. A loopback address unless `-tls-cert` and `-tls-key` are given |
| `-tls-cert FILE`, `-tls-key FILE` | serve HTTPS with this certificate and key (PEM); then `-listen` may be any address |
| `-pass-env NAME,NAME` | credential-looking variables the workspace's commands keep anyway, as `dax execute`'s |

It reads no config. Every request needs its one token, whose holder may
read and write the directory and run any command as its user there:
`$DAX_WORKSPACE_TOKEN` (at least 32 bytes, and not one `-pass-env`
may name), or one it makes and writes to a file only you can read
under `~/.dax/serve`, removed when it stops; it prints the file's path,
never the token.
Authentication is always on: there is no `-insecure-no-auth`. Without
TLS it listens on loopback only, and another machine tunnels to it.
A request from a web page is refused. A signal stops it, and every
process a session started in it ends first.

The session sends the token from `workspace.token_env`
(`DAX_WORKSPACE_TOKEN` by default), takes the variable out of its own
environment as soon as its settings are read, before it starts any
process (the key command first), so no child here gets it, and never
prints or records it. The URL is https, verified against the system's roots or
`workspace.tls_ca`, or http to a loopback host; only your config and
the flags can name it, never a project's, and not beside an executor
or a box. The session's header records the workspace as `remote`, the
URL as its ref and the served root, and `-list -workspace URL` lists
its sessions without connecting. The project's `AGENTS.md`,
`.dax/skills` and `.dax/config.json` are the served directory's, read
through it; your MCP servers start there, through the workspace (a
token without `exec` cannot start them, and then your own run here and
the project's not at all). It
serves this machine's directory alone: MCP servers a workspace offers
of its own (a box host's units) do not cross the wire yet.

### A workspace that dials in: `dax workspace -connect`

A host behind NAT or a firewall, which the session cannot dial, dials
the session instead. The session hosts a hub (`workspace.hub` in your
config), and `dax workspace -connect` on the workspace's host dials out
to it over a WebSocket, serving the directory through that connection
(agentworkspace's `tunnel`, which carries `remote` unchanged). From
there it is a served workspace like any other: the tools, their checks
and the policy run in the session, and what they touch is on the host.

On the session's machine, `build.example` with a certificate, in
`~/.config/dax/config.json`:

```json
{
  "workspace": {
    "hub": { "listen": ":7780", "name": "build", "tls_cert": "/home/me/.config/dax/hub.pem", "tls_key": "/home/me/.config/dax/hub-key.pem" }
  }
}
```

```sh
export DAX_TUNNEL_TOKEN="$(openssl rand -base64 32)"   # here, and the same value there
dax
# workspace.hub: waiting up to 1m0s for workspace build at wss://build.example:7780/tunnel; on its host run: DAX_TUNNEL_TOKEN=... dax workspace -connect wss://build.example:7780/tunnel
```

On the host behind NAT:

```sh
DAX_TUNNEL_TOKEN=... dax workspace -dir ~/src/app -connect wss://build.example:7780/tunnel
# dax workspace: serving /home/me/src/app to the hub at wss://build.example:7780/tunnel, which may read, write, exec
```

| `dax workspace -connect` flag | |
|---|---|
| `-connect URL` | dial the session's hub at this URL in place of `-listen`: `wss://`, or `ws://` to a loopback host alone, no user, password, query or fragment. Not with `-listen`, `-tls-cert` or `-tls-key` |
| `-tls-ca FILE` | for a `wss://` hub, a CA certificate (PEM) to verify the hub's certificate against, in place of the system's roots, as `workspace.tls_ca` and `attach -tls-ca` do; refused with `ws://` |
| `-scopes read,write,exec` | what the session may do here; default all three, as `-listen`'s token grants. Without `exec`, bash and every command fail as forbidden, and the session's MCP servers run on its own machine |
| `-dir`, `-pass-env` | as without `-connect` |

The token is `$DAX_TUNNEL_TOKEN` at both ends, at least 32 bytes:
`dax workspace -connect` sends it in the upgrade's `Authorization`
header, takes it out of its environment before it starts anything,
never prints it, and refuses `-pass-env` naming it; the hub takes the
one token from `workspace.hub.token_env` and refuses any other. The
hub holds its workspace to the first `dax workspace -connect` run that
connects: another, on another host or in another directory, is refused
for the session's life and stops, saying the workspace is held. The
host dials again whenever the connection drops, and a session's
processes there outlive a drop shorter than their lease; it stops at
once on what no redial would change (a token refused, a certificate
`-tls-ca` does not verify, no hub at the path). The session shows the
hub's connects, drops and refusals among its warnings, and a request
made while the host is away fails after 10 seconds.

Without a certificate the hub listens on loopback only, and the host
reaches it through a tunnel of its own (`ssh -o ExitOnForwardFailure=yes -R 7780:127.0.0.1:7780 host`, then
`-connect ws://127.0.0.1:7780/tunnel` there). **A `ws://` URL
authenticates no hub: whoever holds that port on the workspace's host
gets the token and the workspace, with every scope you gave it.** On a
host others share, give the hub a certificate and dial `wss://` with
`-tls-ca`; `-connect` warns on standard error whenever it dials
`ws://`. The session's record names
the workspace `remote` at `http://build.tunnel.invalid`, and
`-list` with the config's hub lists its sessions without listening.
If the workspace does not connect within `wait`, the session stops,
saying the command to run on its host. [docs/sandbox.md](docs/sandbox.md#a-workspace-that-dials-in)
has who dials whom, scopes and the limits.

## Policy

Each tool call is decided by rules: **deny**, then **ask**, then
**allow**, then the fallback (**ask**). A rule is a tool name, or a name
with a specifier: `read`, `bash(go test:*)`, `write(docs/**)`. `Bash`,
`Read` and `Edit` are accepted as the reference's names. For `bash` the
specifier is matched against the command; `go test:*` means `go test` and
anything after it at a word boundary, so it matches `go test ./...` and
not `go testing`.

dax ships this default, as the rules of its extensions (a verdict names
the one whose rule decided it: `extension:dax-coding`, say):

- **runs without asking**: `read`, `glob`, `grep`, `ls` and the read-only
  bash commands listed below (dax-coding's), `explore` and `task`
  (dax-agents'), `skill` (dax-skills') and `memory_search` (dax-memory's);
- **asks**: `write`, `edit`, every other command, memory writes, every
  peer (dax-a2a's) and every MCP tool. `go test`, `go build`, `go vet` and `go list` ask: they run the
  repository's code (a `TestMain`, cgo, a vet tool), so a hostile
  repository would run code as you on the model's say-so.

**What runs without asking is a safe subset, not a blacklist.** A `bash`
call is auto-allowed only when it parses in a strict subset of the syntax
and every part of it is a command dax knows to be read-only, with
arguments that are.

*The syntax*: words of letters, digits and `_ . / : @ % + , = -` (and `~`
or `^` inside a word, as in `HEAD~1`), single-quoted strings, and
double-quoted strings with no `$`, backtick or backslash; the operators
`&&` and `|`; at the end of a command `2>&1`, `2>/dev/null` or
`>/dev/null` as a separate word; and `*` or `?` in the arguments of `ls`.
Anything else, `;`, `&`, `||`, `<`, any other `>`, `#`, `$`, backtick,
backslash, parentheses, braces, `[`, a newline, `=` in the command word, a
non-ASCII byte, asks. It is never denied for that.

*The commands* (`&&` joins any of them; after a `|` only `head`, `tail`,
`wc`, `sort`, `uniq`, `cut` and `grep`, with flags from a short list and no
file arguments):

- `git status`, `diff`, `log`, `show`, `branch` (listing forms only, a name
  only with `--list`), `rev-parse`, `ls-files`, `remote` (`-v` only), `blame`,
  `stash list`, `tag` (listing forms only), `describe`, `shortlog` and
  `config --get` of one key that holds no secret (`user.name`, `user.email`,
  `core.autocrlf`, `branch.*.remote`, `remote.*.url`, ...; not `--list`,
  `--get-regexp`, `--global`, `--system`, `http.*` or `credential.*`), with read-only
  flags from an allowlist, including combined short flags (`-sb`) and
  space-separated values (`-n 5`, `--author x`, `-S foo`). Not
  `--output`, `-o`, `--ext-diff`, `--textconv`, `-c`, `-C`, `--git-dir`,
  `--no-index`, or `%G` in a format.
- `ls` (listing flags; a glob is expanded and checked), `pwd`, `go version`,
  `go env NAME`.
- `cat`, `head`, `tail`, `wc` and `grep` (not recursive) on named files.
  Each file must be a regular file inside the working directory no larger
  than `max_read_bytes` (default 2 MiB), checked when the call is decided,
  and what bash returns is capped at 50 KiB however big it is; `grep -r`
  asks, use the `grep` tool.
- `cd <directory inside the workspace> && ...`: the directory is checked,
  and what follows is checked relative to it.

*The arguments*: every path stays inside the working directory with links
resolved and no `..` component at all, and no git revision or pathspec
contains a `:` (`HEAD:file` and `:/file` name what is in the repository,
which may be above the working directory).

*git*: before a git command runs unasked, dax asks git for the config it
would use (`git config --list --show-scope`). If the repository's own
config, or anything it includes, names a program (`filter.*.clean`,
`smudge` or `process`, `diff.*.textconv` or `command`, `core.askPass`,
`editor`, `gitProxy`, `attributesFile`, `remote.*.uploadpack` or
`receivepack`, `credential.helper`, `merge.*.driver`, `pager.*`,
`protocol.*.allow = always`, ...), moves the work tree (`core.worktree`),
is bare, sets an extension, or runs a submodule update command, or if a
submodule's own config names a program, the call asks and the question
names the key. It also asks when `.git` is a file or a link, or when git's
own answer for the repository (`rev-parse`) is not the workspace's
ancestor chain: a hand-built `.git` can point git at another repository or
at your home directory. Your own global and system config are trusted. The
auto-allowed run switches off the rest: no fsmonitor, pager, ssh command or
hooks, the gpg programs are `/bin/false`, `--no-ext-diff --no-textconv`
are added to `diff`, `log` and `show`, and `go` runs with
`GOTOOLCHAIN=local`. What an auto-allowed command prints has the
`user:token@` of any URL replaced by `***@`. A command you approve runs as
you would run it, hooks and `GIT_CONFIG_*` included.

*What was decided is what runs*: the policy stamps an auto-allowed call with
the plan it approved and the files it was decided on, and the bash tool runs
a stamped call only if the line still analyses to that plan on those files.
If a file, where a path leads (`notes.txt` is now a link to `.env`) or the
repository's config changed in between, the call fails with "the command
changed since it was allowed; ask again", and the original line is never
run instead. A line outside the subset that writes through a redirect
(`echo x > notes`) is never auto-allowed, but once you approve it (or a
rule allows it) it is stamped with the files its redirects write, and it
runs as typed only while they are still those files: a target that
became a link to `.env` after your yes is refused the same way. Only dax
can stamp a call; one the model stamps is refused. `ls` with a glob runs with the
expansion after a `--`, so a file called `-n` is a name.

*Secret-looking files ask* (any case, and under any name: a link to one is
decided under its target as well): reading `.env*`, `*.pem`, `*.key`, `id_*`,
`*.p12`, `*.pfx`, `.npmrc`, `.netrc`, `.pgpass`, `.git-credentials`,
`credentials*`, `*secret*`, `.aws/**`, `.ssh/**`, `.kube/config` or
`.docker/config.json` (at the root or in any directory) asks, whether by
`read`, `grep`, `glob` or `ls` or by `cat`, `head`, `tail`, `wc`, `grep`
and `git show`, `log`, `diff` or `blame` of the path: the stage is decided
as a read of it. Name the path to open one, in your own config:
`"allow": ["read(.env)"]` (or `Read(config/*.pem)`) allows it for every
tool, `cat .env` included, and only that. A bare `"allow": ["read"]` does
not open them: an ask beats an allow. This is a guard on naming a file, not
a boundary: a search of the whole tree (`grep -r`, the `grep` tool with no
path) can still read one, and so can anything you approve.

*A line with git in it asks if the repository names a program, whatever
else is on it*: when a line is not auto-allowed (a stage your own rule
allowed, say `echo`, beside `git status`) it runs as typed, without the
auto-allow environment, so the repository's `core.fsmonitor`, `hooksPath`,
`sshCommand`, `pager` and gpg program count as well, and the question names
the key.

A command outside the subset is still cut into its parts, so a deny or ask
rule for `rm` reaches `git status; rm x`, a redirect to a file is decided
as a write of the file it opens, and the question names the part it is
asking about. The target is read as a file tool's path is: normalised from
the directory the line is in (after a plain `cd DIR`, and from the root as
well), and through its links, so `"deny": ["write(.env)"]` refuses
`echo x > notes` when `notes` is a link to `.env`, and
`cd sub && echo x > ../.env`. A target whose links the workspace cannot
read, or that leads out of it, adds a subject no rule names; one bash
expands (`$HOME/x`, `~/x`, a glob) is decided as its text.
The cut is for the question and for deny and ask rules. Nothing is allowed
because of it: a command outside the subset is allowed only by a bare
`bash` allow rule or `"fallback": "allow"`. Inside the subset a command
the list above does not govern (`make`, `rm`) is for your rules, one stage
at a time.

**Trusting `go test` for your own repositories.** Put the rule in your
**user** config, `~/.config/dax/config.json`, never a repository's (a
project file's `allow` rules are ignored):

```json
{ "policy": { "allow": ["bash(go test:*)", "bash(go vet:*)", "bash(go build:*)"] } }
```

It applies to every repository you open, so only add what you would run
by hand in any of them. Each call is still one simple command: `go test
./... && rm -rf x` asks, as does `go test ./... > out`, and an arbitrary
`-exec`, `-toolexec` or `-vettool` is yours to decide: tighten with
`"ask": ["bash(go test -exec:*)", "bash(go build -toolexec:*)", "bash(go vet -vettool:*)"]`
(the `-o` and `-coverprofile` flags write files and are worth asking about
too).

Your rules in `policy` add to the default. Because deny beats ask beats
allow, allow what the default asks about (`"allow": ["write(docs/**)"]`)
and ask or deny what it allows. An ask the extensions ship, such as the
one for a secret-looking path, holds against a plain allow of yours: lift
it with a carve-out (`"ask": ["deploy(!staging)"]`) or, for the file
tools, an allow that names the path (`"allow": ["read(.env)"]`).
`"builtin": false` drops the shipped allow rules, so only your rules
allow anything; the shipped asks and denies stay. `"fallback": "allow"`
or `"deny"` changes what a call no rule names does.

Path rules (`read(.env)`, `write(docs/**)`, `Read(secrets/**)`) are written relative to the working directory and match the path after cleaning, so `./.env`, `a/../.env` and the absolute path all meet the rule for `.env`, and `docs/../.git/x` is `.git/x`. Links are not followed for matching; the tools' own confinement still refuses one that leaves. `glob`, `grep` and `ls` are matched on the directory they search (default `.`), not on their pattern, and a rule for a directory should name `dir` and `dir/**`. A path rule is not a read ACL for a search that includes the directory from above.

An asked call prints `? allow bash {"command":"..."} (reason) [y/N]`.
Your answer is recorded in the session as a person's.

## Security model

dax gives a model the ability to read your files, change them and run
commands, so what it does and does not stand between the model and your
machine matters.

**What dax does**

- **Asks by default.** Every call that is not on a short allow list asks
  you first: writes, edits, every command that is not one simple read-only
  command, memory writes and every MCP tool. Your answer is recorded. In
  a box, `box.confine` lets the box's own confinement stand in for some
  of those questions ([docs/sandbox.md](docs/sandbox.md), "Fewer
  questions in a box").
- **Auto-allows a small, checked set.** The read-only tools (`read`,
  `glob`, `grep`, `ls`, confined to the working directory) and bash lines
  that parse in a safe subset of the syntax (simple commands joined by
  `&&` and `|`, with a few trailing redirects) and whose every part is a
  read-only git, `ls`, `cat`/`head`/`tail`/`wc`/`grep` on named files, `pwd`,
  `go version`/`go env NAME` or `cd` into the workspace, with arguments
  that pass a per-command check and paths that stay inside the working
  directory (no `..`, links resolved). It is an allow-list of what is safe,
  not a list of what is dangerous: anything dax does not recognise asks.
  git is asked what its config would run first. `go test`, `go build` and
  `go vet` run the repository's code and are not on it.
- **Confines the file tools.** Paths outside the working directory,
  `..`, and symbolic links that lead out are refused, by the operating
  system's rooted open rather than by string checks.
- **Treats the repository as untrusted.** Its `.dax/config.json` can only
  tighten the policy; it cannot choose the provider, model or endpoint,
  add instructions, skills, memory or MCP servers, or allow anything.
  Its `"builtin": false` drops only allow rules, never an ask or a deny.
  Its `AGENTS.md`, `.dax/skills` and `.dax/config.json` are read through
  the workspace, so one that is a symbolic link out of it is refused as
  a tool's read would be: an `AGENTS.md` or `.dax/skills` is left out
  and reported, and a `.dax/config.json` is an error. A `.dax/skills`
  holding a link that leads outside it, even into the workspace, is left
  out too, since the skill tool reads without asking. A read of a file
  of the repository's skills, the instructions included, is held to
  `read`'s asks and denies on the file's name in the workspace and where
  its links lead, so a `.dax/skills` that is itself a link elsewhere in
  the workspace (`-> ..`, `-> ../config`) does not read a `.env` without
  `read(.env)`'s question, and your `deny read(config/**)` refuses it.
  An allow of `read(...)` never allows such a read; the skill tool's own
  rule does. The skills you installed are read unasked. Path rules match
  the path after normalisation, so `docs/../.git/x` is not under `docs/**`.
- **Keeps credentials away from what it starts.** Bash commands and MCP
  servers get your environment without `*_API_KEY`, `*_TOKEN`,
  `*_SECRET` and the like unless your config names a variable. Keys are
  read from the environment or from your own `api_key_command`, and
  never printed.
- **Bounds resource use.** `read` scans at most 2 MiB a call, `grep`
  skips big and non-regular files, search patterns cannot run away, and
  git runs with the repository's fsmonitor, pager and diff programs
  switched off.
- **Keeps its records private.** The session store and memory are
  created `0700`; terminal control sequences in tool output and model text
  are stripped.

**What dax does not do**

- **There is no sandbox.** A command you approve runs with all your
  privileges, and a command that is auto-allowed is only as safe as the
  checks above. `bash` reaches files outside the working directory
  (approved commands are not confined, and the read-only allow list
  checks paths but cannot see what a program does with them). If you
  need a boundary, run dax in a container or VM.
- **A prompt injection can still ask.** A file, a web page, an MCP
  tool's output or an `AGENTS.md` can tell the model what to do. dax
  makes the dangerous steps ask; it cannot make you read the question.
  Read what you approve, especially a compound command, a write to
  `.git/hooks`, `.dax/`, `AGENTS.md` or `.github/`, and anything that
  sends data out.
- **Your own `allow` rules are yours.** `"allow": ["write"]` lets a
  model write `.git/hooks/pre-commit`; `bash(go test:*)` runs a hostile
  repository's tests with your privileges. Prefer narrow rules, and put
  deny or ask rules for the sensitive paths beside them.
- **Committed secrets are not caught.** The secret-path asks fire when a
  file is named. `git log -p`, `git show` and `git diff` without a path,
  `grep` over the tree and `git show HEAD~5` print what a repository
  holds, and a `.env` or key that was ever committed is in it. Keep
  secrets out of history (and rotate one that got in); dax cannot tell
  which lines of a diff are keys.
- **`-trust-skills` trusts the skills in directories you named**
  (`~/.dax/skills` and your config's `skills_dirs`) and never the
  repository's: a skill in `.dax/skills` is text from the repository, and
  its `allowed-tools` are withheld, so it cannot run anything unasked.
- **The provider sees what the model reads.** Files and command output go
  to the model's provider (OpenAI, OpenRouter and whichever upstream it
  routes to, Anthropic, Google, your Ollama host, or the `openresponses`
  server you named). Choose the provider with that in mind; a path rule is not a read
  ACL for a search that includes the directory from above.
- **The sub-agents are governed like the parent**: every call `explore`
  or `task` makes is decided by the same rules (your denies, the
  secret-path asks, path rules, the auto-allow list), and one that asks is
  put to you, from inside the sub-agent's run. A `task` can write, so with
  `"fallback": "allow"` it writes unasked, as the main agent would.
- **Not covered:** programs the *user's own* git config names (it is
  trusted), a race between dax checking a path and the command using it,
  credential-file path variables such as `KUBECONFIG` (they pass through to
  commands), and denial of service by a model that loops (use `Ctrl-C`).

## The terminal client

On a terminal, `dax` opens the terminal client
([agentconsole](https://github.com/ChristopherDavenport/agentconsole)) over
the same session: the conversation rendered from the session's record, with
the run's live deltas on top. When it exits it leaves one line on the
terminal, the command that resumes the session:

```
To resume this session: dax -resume 01a112e5-b4df-7081-bd11-baac746b29cc
```

Any warning, such as a store whose permissions were fixed, is printed
before the client takes the screen, and dax waits for Enter, since the
client uses the alternate screen. A warning that comes while the client
has the screen, such as what the executor's launcher writes to standard
error, is held and printed when it exits, above the resume command.
With `-executor`, the executor's line (its name and version, kind and
ref, and root) is printed before the client either way, and stays on
the terminal when it exits. With `-v` dax also prints its start lines
(provider and model, the session, the policy in force, the tools, any
`omitted:` line) and waits for Enter on an omission too, and when the
client exits it prints what it noted during the run (skill grants,
compactions, denied calls) above the resume command.

Each of dax's tool calls is drawn as what it does, on one line, with a
short body under it:

| tool | the line | collapsed | Ctrl-O |
|---|---|---|---|
| `edit` | the path, the lines it adds and removes (`+3 −1`) | the change as a diff, up to 8 lines | the whole diff |
| `write` | the path and the content's lines | | the content |
| `bash` | `$` and the command's first line, its timeout if set | how many lines it printed; a failure's exit status and last 3 lines | the whole output and the exit status |
| `read` | the path and the lines asked for (`a.go:120-199`) | how many lines it read | the output |
| `grep`, `glob`, `ls` | the pattern or directory, where and how | the matches and files, the files or the entries counted | the output |
| `explore`, `task` | the brief's first line, `fork` and `main model` when set | the report's first 2 lines | the report |

A call that failed shows its error in place of the body. A call waiting on
a permission shows what it would do: an edit its diff, a command of several
lines the rest of it. Any other tool (`skill`, memory, an MCP server's) is
drawn as the client draws any call, its name and arguments.

| key | |
|---|---|
| Enter | send a prompt, or steer the run in flight |
| Ctrl-C | copy the selection when one is drawn; otherwise abort the run, quit when idle (a second one quits at once) |
| `y` / `n` | approve / refuse the permission asked; `n` then takes an optional reason and Enter |
| `y` / `a` / `n` | on a question: allow, allow with a note, refuse with an optional reason; the note or the reason is typed, then Enter |
| PgUp, PgDn, Ctrl-Up/Down, Ctrl-Home/End, mouse wheel | scroll the conversation |
| Ctrl-R / Ctrl-O | show reasoning / tool arguments and output in full: the selected row's, or with no row selected every row's |
| Ctrl-T | the tree of the session's branches; Enter views one, `c` continues from it |
| Ctrl-P, Ctrl-N, Ctrl-B, Tab | move over the rows, continue from a row, open its detail (policy decision, who decided, verification) |
| click | select the row under the pointer; a click on the selected row expands or collapses it |
| drag | select text anywhere on the screen; it stays drawn until the next key or press |
| Ctrl-/ or F1 | list every key; Esc, q or Ctrl-/ goes back |

A call the policy asks about is a permission: the panel shows the call and
the reason, which is the policy's with what it is asking about added: the
rule that fired, the secret-looking path, the git config key that names a
program, the part of a command line. A call the policy only held beside
the one it asks about is not shown: your answer to the batch decides it.
A call that was started before it was held says it may have run. A
session that stopped with a call held for approval opens with it on the
panel, and a prompt waits until it is answered. Everything that can ask is answered on
screen; nothing reads standard input once the client has the terminal. Two
things cannot be a permission, which a run that ended answers, since the
call that needs the answer is still running; the client asks them as
questions while the run goes, ahead of any permission:

- A call a **sub-agent** (`explore` or `task`) makes that the policy asks
  about: `y` allows it, `a` allows it with a note, `n` refuses it with an
  optional reason, and `Esc` goes back. The sub-agent sees the note with
  the call's result, and the reason in place of it.
- A **question a tool asks mid-call** (MCP elicitation) that is yes or no.
  dax's own question for a line of text (a peer's question) is answered
  with `a` and the text, or `y` for "yes"; any other form, an MCP
  server's included, or a page to visit, has no screen in the client,
  and is cancelled.

A call stamped as auto-allowed that changed before it ran fails with "the
command changed since it was allowed ... ask again", which shows in the
transcript.

## Commands

A line that starts with a slash is a command to dax, in the terminal
client and the REPL alike; the model never sees it. Start a line with
`//` to send the model a line that begins with a slash (`//etc/hosts`
sends `/etc/hosts`). In the terminal client a command's output shows over
the input until the next line, and an error gives the line back to edit.

- `/model name`, `/think on|off`: the model and reasoning from the next
  request.
- `/mcp add <name> <command>` (tools are `mcp__<name>__<tool>`; a name has
  letters, digits, `-` and `_`, and no `__`), `/mcp remove <label>`.
- `/tools`, `/session`, `/help`.
- `/follow text` queues a follow-up that runs once the model would have
  stopped; `/abort` aborts the run; `/quit` leaves.

Any other line typed while a run is in flight steers it and lands before
the next model call. `/model`, `/think` and `/mcp` wait for the run to
end; the rest work during one. An unknown command is an error, not a
prompt. `/compact` does not exist (use `-compact N`).

## Serving a session: `dax serve` and `dax attach`

`dax serve` runs a session with no front of its own and serves it, so a
terminal elsewhere can drive it with `dax attach`. It takes the same
flags as `dax` (the provider, the model, `-resume`, `-executor` and the
rest), `-listen`, the address (default `127.0.0.1:0`, a free port), and
`-a2a` and `-a2a-escalate`, which serve the agent to other agents
([Serving peers](#serving-peers-dax-serve--a2a)):

```sh
dax serve -resume 01a112e5-b4df-7081-bd11-baac746b29cc
# serving at http://127.0.0.1:41753
# token: /home/you/.dax/serve/01a112e5-....token
# attach: dax attach -token-file /home/you/.dax/serve/01a112e5-....token http://127.0.0.1:41753

dax attach -token-file ~/.dax/serve/01a112e5-....token http://127.0.0.1:41753
```

- **Loopback, unless it has a certificate.** Without TLS, dax serve
  refuses an address that is not `127.0.0.1`, `::1` or `localhost`: the
  session runs tools and answers what its policy asks, so it is not
  offered to the network in the clear. The simplest way in from another
  machine is still a tunnel: `ssh -o ExitOnForwardFailure=yes -L 7878:127.0.0.1:7878 host`, then
  attach to `http://127.0.0.1:7878`.
- **HTTPS with a certificate.** Given a certificate and its key (PEM),
  dax serve serves HTTPS and may listen on any address:

  ```sh
  dax serve -listen 0.0.0.0:7878 -tls-cert host.pem -tls-key host-key.pem
  dax attach -token-file host.token https://host.example:7878
  ```

  dax attach verifies the certificate against the system's roots, or
  against the CA you give it with `-tls-ca ca.pem` for a certificate of
  your own; nothing skips the check. It refuses an `http://` URL to a
  host that is not loopback, since the token would cross the network in
  the clear. The certificate must name the host you attach to.
- **One token.** Every request needs it. It is `$DAX_SERVE_TOKEN` if
  set; otherwise dax serve makes one and writes it to a file under
  `~/.dax/serve` that only you can read, removed when it stops. The
  token is never printed; attach reads it from `-token-file` or
  `$DAX_SERVE_TOKEN`.
- **One conversation.** Every view attached to a session drives the same
  conversation: what one asks, the others show, and a permission or a
  question is answered from whichever view answers first. Questions and
  held calls wait for whoever attaches. For independent work, fork the
  session instead.
- **Leaving is not stopping.** `/quit` in an attached view leaves the
  session's run going; ctrl+c in a view aborts it. dax serve stops on
  ctrl+c or SIGTERM, aborting a run still going, and prints the command
  that resumes the session.
- **Commands.** The slash commands run against the served session:
  `/model`, `/think`, `/mcp` and `/session` act there, through the
  server's commands.
- **The tree.** Continuing from an entry (Ctrl-B on a row, `c` in the
  tree) moves the served session's head, as it does locally: the next
  prompt, from any view, continues from there. A move the session
  refuses, while a run goes or from a call whose output is not on the
  path, is shown in the view that asked.

### Serving peers: `dax serve -a2a`

`-a2a` serves the session's agent over [A2A](https://a2a-protocol.org)
as well, so another agent (a dax with this one in its `peers`, or any
A2A client) can call it. It is off by default: it lends the agent's
tools, in this workspace, to whoever holds the A2A token.

```sh
dax serve -a2a -listen 127.0.0.1:7878
# serving at http://127.0.0.1:7878
# a2a: http://127.0.0.1:7878/a2a, card at http://127.0.0.1:7878/.well-known/agent-card.json
# a2a token: /home/you/.dax/serve/01a112e5-....a2a.token
```

| Flag | What it does |
|---|---|
| `-a2a` | mount A2A's JSON-RPC at `/a2a` and the agent card at `/.well-known/agent-card.json`, on serve's listener and TLS, behind the A2A token |
| `-a2a-escalate` | with `-a2a`: put a call the model makes that the policy asks about to the caller instead of the attached views, for a host nobody attaches to |
| `-a2a-url URL` | with `-a2a`: the base URL callers reach serve at, which the card names (https, or http to loopback), for serve behind a proxy that ends TLS; without it the card names the origin each request reached, `https` when serve itself serves TLS |

- **Two tokens.** serve's token (`<id>.token`, or `$DAX_SERVE_TOKEN`)
  is the control and the store: whoever holds it drives the session,
  answers its questions, adds MCP servers (which start commands on this
  machine) and reads every session in the store. The A2A token
  (`<id>.a2a.token` beside it, only you can read it, or
  `$DAX_SERVE_A2A_TOKEN`, which must differ) reaches `/a2a` and the card
  and nothing else; it is what you give a peer. Neither works in the
  other's place: 401. A2A calls are made under the principal `a2a`.
- **The card is behind the token.** It names the bearer scheme, the
  endpoint, the agent's name and description and its tools as they are
  when it is asked (an MCP server added since serve started included),
  and no token; it is sent with no cross-origin headers, so a page in a
  browser cannot read it.
- **A conversation is a session.** Each A2A context is a conversation
  of its own, recorded in a dax session of its own in the store, with
  the served session's working directory: `dax -list` in the project
  shows it, and each run names the caller (a trigger `a2a/a2a`).
  Nothing of it is written into the served session, and a view attached
  to that session does not see its transcript. A conversation belongs to
  the principal whose message started it; another cannot continue it or
  read its tasks. The conversations, who owns each, the calls a task put
  to its caller and the task each waits on are kept under `~/.dax/a2a/`
  (one directory per session store, working directory and workspace,
  only you can read it), so a restart in the same place continues them
  in the same sessions. One `dax serve -a2a` at a time keeps a
  directory: a second in the same place refuses to start. When dax
  serve stops it refuses new A2A requests (503) before it ends the
  conversations' runs. The tasks themselves are not kept:
  after a restart a task's ID is unknown, and the caller goes on with a
  new message in the same context.
- **The same agent, the same workspace.** A conversation runs under the
  served session's model and reasoning as they stood when serve
  started, and its extensions, policy and MCP servers as they are: an
  MCP server added or removed in an attached view is added or removed
  for the conversations too. Its tools act in the same workspace. The
  conversations and the served session run at the same time, on the
  same files, as parallel sub-agents do; nothing orders them. One
  conversation runs one task at a time: a message while one is in
  flight is refused, and the caller sends it again.
- **Who answers.** A question in a conversation, a call the policy asks
  about, a sub-agent's asked call or a tool's own question, is put to
  the views attached to the served session (`dax attach`), naming the
  caller and the conversation (`[A2A caller a2a, conversation "<id>"
  (session <id>)] Allow bash ...`). Only a view attached when it is
  asked is asked: with none attached the question is not asked and the
  call is refused at once. When the last view detaches while it waits,
  it waits five seconds for a view to come back, as `dax attach` does
  after a broken connection, and is shown again when one does; past that
  it is refused. The conversation's record holds the refusal, and a
  reply given first stands. Nothing is allowed in anyone's place. With
  `-a2a-escalate` a call the model makes that the policy asks about goes
  to the caller instead: the task waits for input, listing the call and
  the policy's reason, and the caller's approval runs the call as the
  policy decided it, its refusal refuses it, both on the record under
  the caller's principal. Escalation covers only those calls: a
  sub-agent's asked call and a tool's own question still go to an
  attached view, by the same rule. A skill's grant lasts for the task
  that read it, so a call it would have let run after an approval
  resumes the task is asked again. A dax calling with dax-a2a cannot
  answer an approval yet; a host nobody attaches to and a caller that
  can answer are what it is for.

To call it from another dax, name it as a peer in that dax's config,
with the A2A token in a variable:

```json
{ "peers": { "builder": { "url": "http://127.0.0.1:7878", "token_env": "BUILDER_TOKEN", "relation": "peer" } } }
```

```sh
BUILDER_TOKEN=$(cat ~/.dax/serve/<id>.a2a.token) dax
```

dax-a2a holds the card's endpoint to the configured `url`'s origin, so
the `url` is the one dax serve printed or, from another machine, the
tunnel's end, the name the certificate is for, or `-a2a-url` behind a
proxy. With `-a2a-url` every caller, one on this machine too, is given
that URL on the card, so configure the peer with it. Set
`DAX_SERVE_A2A_TOKEN` on the serving side to keep one A2A token across
restarts.

## In the REPL

`dax -front repl` (the default when not on a terminal) is a line REPL
with the same commands; a permission or a question is answered with `y`
or `n` on the next line, and what follows the first word is what you say
with the answer, which the model is told: `y keep it short` allows with a
note, `n not that file` refuses with a reason. Any other line refuses,
the whole line its reason. `-p` reads its answers the same way.

## Sessions

Every run is recorded in one content-addressed store,
`~/.dax/sessions`, for every project. Admin commands:

```sh
dax -list                          # sessions of this project: this directory, or the executor's (docs/sandbox.md)
dax -resume <id>                   # continue one, answering any call an abort cut off
dax -verify <id>                   # rebuild every request and check its hash
dax -project <id> -out dir         # write one as a JSONL file (RFC 0001)
dax -import old.jsonl              # bring a JSONL session into the store
dax -gc pack                       # pack loose objects; -gc sweep also drops what no session needs
dax -repair <id>                   # rewrite a damaged log from what still reads
```

`-list`, `-verify` and `-project` open the store read-only, so they work
beside a running dax. `-sessions ""` disables recording.

Other state lives in `~/.dax`: `AGENTS.md` (read before the project's),
`skills/`, `memory/`. A project's `.dax/skills` and its `AGENTS.md` files
are read too, through the workspace. The `AGENTS.md` chain follows the
convention (https://agents.md): from the repository's root, the nearest
directory at or above the start that holds a `.git`, down to where dax
starts, the nearest file last; a directory above the repository is not
read. With no repository only the start directory's file is read. A
file you want in every session whatever the repository, such as one in a
directory above your checkouts, goes in your config's
`agents_md_global`.

## Building on dax

dax is a Go module as well as a command. `cmd/dax` is one line,
`dax.Main` with no options; a program of your own is the same line with
an extension:

```go
func main() {
	os.Exit(dax.Main(context.Background(), os.Args[1:],
		dax.WithName("acme", ""),
		dax.WithExtension(extension.Extension{
			Name:         "acme-deploy",
			Tools:        deployTools, // func(extension.ToolEnv) []agenttool.Tool
			Matchers:     extension.FixedMatchers(map[string]agentpolicy.ToolMatcher{"deploy": {Match: agentpolicy.GlobMatcher("env")}}),
			Policy:       policy.Rules{Allow: []string{"deploy(staging)"}},
			Instructions: "Never deploy to prod unless the user asks.",
			Renderers:    func(string) toolview.Renderers { return toolview.Renderers{"deploy": deployRenderer{}} },
		})))
}
```

dax's own extensions are built the same way, from the same fields:

| Extension | Package | What it offers |
|---|---|---|
| `dax-coding` | `ext/coding` | read, write, edit, glob, grep, ls and bash; the allow list, the secret-path asks, the matchers and the aliases (`Bash`, `Read`, `Edit`, `Write`) for them; the stamp that holds an auto-allowed bash call to the plan and the facts the policy approved |
| `dax-agents` | `ext/agents` | the `explore` and `task` sub-agents, given every extension's tools (the read-only ones for `explore`) |
| `dax-skills` | `ext/skills` | the `skill` tool and catalogue; grants under `-trust-skills` |
| `dax-memory` | `ext/memory` | the memory tools and block |
| `dax-a2a` | `ext/a2a` | a tool per configured peer, added through its kit as memory's are; their renderers; no rules |

`dax-coding` and `dax-a2a` are always on (`dax-a2a` offers nothing
without `peers`); the others follow `agents`, `skills` and
`memory` in the settings. `WithoutExtension` leaves one of the five out
whatever the settings say (any other name is an error), and
`WithExtension` adds the program's after them.

| `extension.Extension` field | What it adds |
|---|---|
| `Name` | the extension's name; its rules are recorded as `extension:<Name>` |
| `Tools` | tools (`agenttool.Tool`), in order, for the main agent and for `task` |
| `ReadOnly` | the names of its tools that only look, which `explore` gets too; one annotated destructive is refused |
| `Owns` | names of tools it adds through `Kit` (`skill`, `memory_save`), so its rules may name them |
| `Matchers` | how a rule's specifier reads a call of one of its tools: `deploy(staging)` above. Built over the session's `ToolEnv`, so one that looks at files looks at the session's workspace; `extension.FixedMatchers` wraps ones that read only the arguments |
| `Aliases` | names a rule may use for several of its tools: `Read` for read, grep, glob and ls |
| `Policy` | the allow, ask and deny rules it ships for its tools |
| `Lifts` | its tools whose asks a user's allow rule with a specifier lifts: `read(.env)` |
| `HeldTo` | its tools whose calls are held to another extension's tools' ask and deny rules (never their allows), by what the tool's facts claim names: dax-skills holds `skill` to `read` |
| `BeforeToolCall` | a hook, built over the session's `ToolEnv`, that decides or rewrites a call, folded with the policy, for the main agent and the sub-agents; `extension.FixedHook` wraps one that needs nothing of the session |
| `Instructions` | text in the main agent's system prompt, in extension order, before your `instructions_file` |
| `Kit` | agentkit options for anything else (skills, memory, child agents, guards), given every extension's tools |
| `Renderers` | how the terminal client draws its tools' calls (`toolview.Renderer`) |

A session builds its extensions in two phases: every extension's tools,
then each one's `Kit` options, so a sub-agent built in the second phase
gets a tool an extension listed after it adds. Names are checked across
the whole session, without regard to case: two extensions may not offer
a tool, an alias or an owned name that differ only in case, `mcp__`
belongs to MCP servers, and an alias may differ from its own tool only
in case (`Bash` and `bash`). The session's own kit options (the name,
the model, the instructions, the policy, the session) go after the
extensions', so where an option replaces another the session's is the
one in force; a `Kit` that sets a policy on a session with the policy
off is an error. The session closes the tools it built. One tool value
is shared by every agent that has it, the main agent, `explore` and
`task`, and may be called by them at once, so a tool that keeps state
guards it.

An extension's tools are under the same policy as dax's. A call no rule
allows asks first. Each extension's rules are a source of their own,
`extension:<Name>`, which every verdict one of them decides names in the
session's record, and the start line counts them. A rule may name only
the extension's own tools and aliases, not a pattern and not a
carve-out, so one extension cannot loosen another's tools or cancel its
rules. Nor can it borrow them: a call of another extension's tool that
its tool's facts claim (`agenttool.Factual`) or its matcher's own
subjects name is decided as a tool no rule names, so it asks, unless
`HeldTo` holds the claiming tool to that tool, whose asks and denies
then apply to the call and whose allows never do. Deny beats ask beats allow whatever the source: an extension's
ask or deny holds against a plain allow in your config, and a carve-out
in your config (`"ask": ["deploy(!staging)"]`) or, for a tool in
`Lifts`, an allow with a specifier lifts it. A project's config can add
asks and denies, and `"builtin": false` drops the extensions' allow
rules but keeps their asks and denies.

An extension's tools get an `extension.ToolEnv`: the session's
`Workspace`, `Files` over it, and `MaxReadBytes`. A tool that takes a
path from the model reads and writes through `Files` (`ReadFile`,
`WriteFile`, `Update`, `Stat`, `ReadDir`, `Rel`), which turns the path
the model wrote, absolute in the workspace's root or relative to it,
into the workspace's name and refuses one that leaves, as dax's file
tools do. `Update` is a read-modify-write under the lock dax's `write`
and `edit` take, so a tool's change and an `edit` beside it cannot lose
each other's; a FIFO or a device is refused rather than waited on. A
tool that runs a process uses `Workspace.Exec`, whose processes start
with `Workspace.Env()`, the environment with credentials removed (a
copy each call); the workspace types are the agentworkspace module's
(`github.com/ChristopherDavenport/agentworkspace`, which dax imports
as `workspace`). Written that way, a tool runs unchanged wherever the
session's workspace is. A tool is built over `ToolEnv` rather than
agentkit's kit because what it needs is dax's, the workspace and its
confinement, which the kit does not hold.

A renderer reads only the record. It declines (returns ok false) a call
whose arguments or schema it was not written for, such as one an older
version recorded, and the client draws that call as raw text. Two
extensions that draw one tool are an error.

`WithName` puts the program's name in the banner, the resume command,
the client header, the session header and the prompt; the config files
and the store are still dax's (`~/.config/dax`, `.dax`, `~/.dax`). The
program keeps the flags, the config files, the providers, the session
store, MCP, and the three fronts.

For a front of your own, build the session with `agent.New` and
`Options.Extensions`, and drive it as agentturn's control contract
(`*agent.Session` is an `agentturn.Control`): prompt, queue a steer or a
follow-up, answer the permissions a run stops on (`Session.Permissions`,
`Resume`), reply to the questions asked while a call runs (the
`agentturn.Question` events, `Reply`, after `Session.Answering`), and
subscribe to the agent's events. Name who answered on `Reply`'s
context with `agentturn.ContextWithAnswerer` (`agent.AtTerminal` for
the person at the terminal), and only for a reply that person gave:
the record writes it beside the answer, and a reply that names nobody
is unattributed. `Session` is also `agent.Controls`
(model, reasoning, MCP servers, `Info`); `Session.Record` is the store
to follow for a view, and `Session.Recorder` what agentconsole's
`client.RecordOf` takes.
`agent.Drive` is a controller that answers by rule, for running without
a person. The terminal client is agentconsole; hand it
`extension.Renderers(dir, exts)`. The public packages are `dax`,
`agent`, `extension`, `policy`, `tool`, `toolrender` and the four
under `ext/`, over the agentworkspace module's `Workspace`; the configuration, providers, model metadata, prompt frame and
renderer for the REPL stay internal. Everything is pre-1.0: the exported
API may change in a minor version, and the changelog says when.

### Where the tools act

A session acts in one `workspace.Workspace`, from the agentworkspace
module (`github.com/ChristopherDavenport/agentworkspace`): a root, a
file system that never blocks on a FIFO or a device, writes and
removes, an environment, one-shot execution with its output streamed as
it arrives, and a descriptor the session records; a workspace that can
also run a long-lived process with pipes is a `workspace.Starter`.
`workspace.Local` is this machine's directory, opened as an `os.Root`
so no name leaves it, with each command in its own process group. A
container or a remote runtime is another implementation of the same
interface, and the rest of dax cannot tell them apart:

- dax-coding's tools act through it: `read`, `write`, `edit`, `glob`,
  `grep` and `ls` through `tool.Files`, and `bash` through
  `Workspace.Exec`, with the same output wherever it runs.
- The policy's checks of a call read the same workspace: a path rule
  follows links through it, and the bash analyzer stats files, follows
  links, finds `.git` and reads git's configuration there (git is run
  through `Exec`, with paths passed as arguments). A workspace whose
  file system cannot tell a link from its target leaves those checks
  unable to follow links, and they ask.
- The project's instructions are read through it: the `AGENTS.md`
  chain (agentsmd's `Options.FS`), `.dax/skills` (an `agentskill.Source`
  over its file system) and `.dax/config.json`, each screened there, so
  a container gives its own and nothing comes from the directory dax
  started in. Only when the workspace is that directory on this machine
  and dax started below its repository's root are the `AGENTS.md` files
  between the two read from this machine, screened the same way.
- MCP stdio servers run in it when it is a `Starter` (`Local` is, and
  so is an executor's), at its root and with its environment, and end
  with the session; in a workspace that cannot start a process they run
  on this machine, with its environment scrubbed.
- The session records the workspace: the header's and the env entry's
  `cwd` are its root, and the env entry names its kind (`local`,
  `container`, `remote`) and ref. The model is told the root as the
  working directory.

`agent.Options.Workspace` takes one; without it the session opens a
`workspace.Local` over `Dir` and closes it. `agent.Options.Executor`
takes an executor in its place (`agent.DialExecutor`, which starts `dax
execute` as `-executor` does), whose tools act where it runs; the
caller closes it. `agent.Options.Store` takes
the session store the same way: an `agentsession.Store` the caller
opened and closes, or, without it, the content-addressed store at
`Root`. A remote store client (agentsession's RFC 0003) is meant to fit
there.

What is not there yet:

- dax serves `workspace.Local` and dials agentworkspace's `remote`
  (`dax workspace`, `-workspace`); a container workspace is a
  program's to provide until agentworkspace ships one. `dax execute`
  serves the tools from inside a sandbox and `-executor`
  (`agent.Options.Executor`, `agent.DialExecutor`) runs a session's
  tools and MCP servers there and reads the project's files through
  it.
- RFC 0003's client is not a drop-in `Store`: opening takes a lease,
  an append returns a different result, and a lost lease needs
  handling. `Options.Store` takes today's interface; the lease handling
  waits for agentsession's remote client. `Session.Path` is empty for a
  store that is not local.
- A front over a wire (the terminal client driving a session elsewhere,
  ACP) is not built. The contract it carries is agentturn's, which the
  session implements, served by `agentturn/front/control`; what waits
  is the record over the wire (agentsession's RFC 0003 read and follow)
  for the view.

## Develop

```sh
make check    # gofmt, go mod tidy -diff, go vet, staticcheck, govulncheck, go test -race
make build    # ./dax
```

In this workspace the module is built outside the go.work:
`GOWORK=off GOFLAGS=-mod=readonly make check`. See [AGENTS.md](AGENTS.md)
for the layout and release process, and [CHANGELOG.md](CHANGELOG.md).

## License

MIT. See `LICENSE`.
