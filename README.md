# dex

A coding agent for the terminal. It reads, searches, edits and writes
files in your project and runs commands, asking you before it does
anything it should not do alone, and records every session so you can
list, resume, verify and export it. It runs against a local Ollama with
no setup, or OpenAI, Anthropic or Gemini with a key.

It is a thin product over a set of small Go libraries
(`openresponses`, `agentturn`, `agentkit`, `agentpolicy`,
`agentsession` and friends); [docs/design.md](docs/design.md) says who
owns what.

## Install

```sh
go install github.com/ChristopherDavenport/dex/cmd/dex@latest   # needs GOPRIVATE=github.com/ChristopherDavenport/*
# or, in a checkout
make install
```

Go 1.25 or later. With no flags dex talks to Ollama on
`localhost:11434` and runs `qwen3.5:9b`.

```sh
dex                          # REPL in the current directory; Ctrl-C aborts a run in flight
dex -p "what is in go.mod"   # one prompt, then exit
dex -provider anthropic      # a hosted model
```

## Providers and keys

| `-provider` | default model | key (environment only) | `-base-url` |
|---|---|---|---|
| `ollama` (default) | `qwen3.5:9b` | none | default `http://localhost:11434/v1`; another Ollama host |
| `openai` | `gpt-5` | `OPENAI_API_KEY` | not supported |
| `openrouter` | `deepseek/deepseek-v4-pro-0813`; sub-agents `deepseek/deepseek-v4.1-flash` | `OPENROUTER_API_KEY` | not supported |
| `openresponses` | none; `-model` is required | the variable `-api-key-env` names, or none | required |
| `anthropic` | `claude-sonnet-5-5` | `ANTHROPIC_API_KEY` | not supported |
| `gemini` | `gemini-2.5-pro` | `GEMINI_API_KEY` | not supported |

A provider is a vendor: `openai` is OpenAI, and `openrouter` is
OpenRouter, whose models are named `vendor/model`. Any other server that
speaks the Open Responses (OpenAI Responses) API, such as vLLM, LM Studio
or a proxy, is `openresponses`, with its `-base-url`, a `-model`, and
`-api-key-env` naming the variable its key is in, if it takes one:

```sh
LITELLM_KEY=... dex -provider openresponses -base-url https://llm.internal/v1 -model qwen3-coder -api-key-env LITELLM_KEY
```

`-model` names another model. `-subagent-model` names the one the
sub-agents run; without it a sub-agent runs the provider's default for
sub-agents, where it has one, else the main model.
A missing key is an error before any request
is made: `dex: openai: no API key: set OPENAI_API_KEY in the
environment`. dex reads keys from the environment and nowhere else (not
from a config file, not from a flag, so they stay out of `ps` and out of
a repository), and never prints one. The variable a key was read from is
removed from the environment of bash commands and MCP servers, whatever
it is called, unless `pass_env` names it.

Ollama, OpenAI, OpenRouter and `openresponses` use the `openresponses`
client; Anthropic and Gemini use its provider adapters.

### What the model takes

At start dex asks the vendor what the model supports and shows it under
the banner, `model: reasoning low–max, always on · default high · 1M
context · 128k out (openrouter)`. Anthropic and Gemini are asked through
their model endpoints, OpenRouter through its public catalogue (fetched
once, without the key) and Ollama through `/api/show`; OpenAI and an
`openresponses` server publish nothing, and nothing is shown.

The reasoning effort dex asks for is then fitted to the answer: an
effort the model does not take becomes the nearest one it does, and dex
says so once, `[qwen3-coder:30b: reasoning effort low is not accepted;
asking for none (ollama)]`. So `-think` on a model that cannot reason
turns reasoning off instead of failing, and `-think=false` on one that
always reasons asks for its least effort. The fitting is done where each
configuration is made (the main agent's, the sub-agents', a fold's
summary, `/think` and `/model`), never to a request on its way out, so
the session records the effort that was sent. When the vendor cannot be
asked, or does not know the model, the effort is asked for as
configured. `-think` (default on)
asks for low reasoning effort with summaries and shows the reasoning;
`-think=false` turns reasoning off.

## Sub-agents

The main agent can start sub-agents, each a run of its own on the
sub-agent model, recorded as a child session:

- `explore` investigates without changing anything (read, glob, grep, ls
  and bash) and returns a written answer.
- `task` carries out a coding task with the file tools and bash, then
  reports what it changed, what it ran and what is left. Its instructions
  are dex's, your `instructions_file` and the AGENTS.md chain, without
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
  the defaults were the cheapest in a trial
  (`evals/task-prefix` in the agent studies): a fresh Flash sub-agent
  cost about a twentieth of a Pro one at the same pass rate, and a Pro
  fork about twice a fresh Pro task.

Several calls in one turn run at once, so the main agent can hand out
independent pieces of work in parallel. Writes and edits take a
workspace lock, so two sub-agents editing one file cannot lose an edit,
though they can still make changes that do not fit together; the
sub-agent is told to change only the files its task is about.

Both are offered by default; `-agents=false` or `"agents": false`
turns them off. Starting one is on the built-in allow list, since what a
sub-agent then does is decided call by call by the same policy.

## Config

Settings come from three layers, each overriding the one before:

1. `~/.config/dex/config.json` (`$XDG_CONFIG_HOME/dex/config.json`; `-config path` names another)
2. `.dex/config.json` in the working directory, which can only tighten the policy
3. flags

```json
{
  "provider": "anthropic",
  "model": "claude-sonnet-5-5",
  "base_url": "",
  "think": true,
  "instructions_file": "~/.config/dex/instructions.md",
  "skills_dirs": ["~/skills"],
  "memory_dir": "~/.dex/memory",
  "mcp_servers": {
    "fs": { "command": "mcp-server-filesystem /home/me/notes" }
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

`model`, `subagent_model`, `base_url` and `api_key_env` belong to the provider in force
where they are set. A later layer that switches the provider leaves them
behind, so with `"model": "qwen3-coder:30b"` in your config,
`dex -provider openrouter` runs OpenRouter's default model rather than
asking OpenRouter for an Ollama one; `-model` beside `-provider` names
another.

Every field is optional. The files are strict: an unknown field, a bad
provider, a malformed URL or a rule that does not parse is an error that
names the file and the field.

| field | meaning |
|---|---|
| `provider`, `model`, `subagent_model`, `base_url`, `think` | as above; `base_url` is for `ollama` and `openresponses` |
| `agents` | offer the explore and task sub-agents; default `true` |
| `api_key_env` | the variable holding the `openresponses` provider's key (the name, never the key) |
| `instructions_file` | your own instructions, added to the system prompt after dex's; a relative path is relative to the file that names it |
| `skills_dirs` | more skill directories, after `.dex/skills` and `~/.dex/skills`; one that does not exist is an error |
| `memory_dir` | where the model's memory lives; `""` turns memory off. Default `~/.dex/memory` |
| `mcp_servers` | stdio MCP servers by name; the name prefixes their tools, `mcp__<name>__<tool>` |
| `max_read_bytes` | the most bytes of a file `read` scans per call and `edit` will rewrite; default 2 MiB (use `grep` to find a line later in a bigger file) |
| `pass_env` | credential-looking variables bash commands and MCP servers may inherit, by name (default none) |
| `policy` | see below |

A project's `.dex/config.json` comes from a repository, not from you, so
it can only **tighten**. It may add `ask` and `deny` rules to `policy`
(no `allow`, and no `!` carve-out of any rule), set `"builtin": false` to
drop the built-in allow list, and set `"fallback"` to `ask` or `deny` when
that is stricter than yours. It cannot bring back what you dropped or
loosen what you set, and its rules rank below yours so they cannot cancel
one of yours. It may **not** set `provider`, `model`, `subagent_model`, `base_url`, `api_key_env`, `think`, `agents`,
`instructions_file`, `skills_dirs`, `memory_dir` or `mcp_servers`: dex
refuses the file with an error naming the field and saying to put it in
your own config. (Where the model runs, what it is told and remembers, and
what programs start are decisions that send your code, your files and your
keys somewhere; a repository does not get to make them.)

| flag | |
|---|---|
| `-provider`, `-model`, `-subagent-model`, `-base-url`, `-api-key-env`, `-think` | override the config |
| `-config path` | the user config file |
| `-memory dir` | memory directory; `off` or empty disables it |
| `-no-policy` | run every tool call without asking; ignores the config's policy |
| `-front tui\|repl` | the front end; default `tui` when standard input and output are both terminals and a session is recorded, `repl` otherwise (so `-sessions ""` gives the REPL); `-front tui` without a terminal or a session store is refused; `-p` always prints |
| `-agents-md`, `-skills`, `-trust-skills` | the AGENTS.md chain, skills, and a skill's `allowed-tools` running unasked until the next message, for skills in `~/.dex/skills` and `skills_dirs` only, never the repository's |
| `-compact N`, `-compact-server` | fold the transcript above N estimated tokens, locally or through the server; without `-compact`, N is three quarters of the model's context window when the vendor reports the window, and compaction is off when it does not; `-compact 0` turns it off |
| `-mcp "cmd"` | one more stdio MCP server, as `mcp__cli__<tool>` |
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
| `bash` | a command in the working directory with a timeout; runs one at a time |

The file tools are confined to the working directory. A path that is
absolute outside it, climbs out with `..`, or goes out through a
symbolic link is refused. `bash` is not confined (a shell reaches what you
reach); the policy is what stands in front of it. `glob` and `grep` skip
`.git`, `node_modules`, `vendor`, `.venv`, `__pycache__` and similar trees
by name; there is no `.gitignore` support. Search a skipped directory by
naming it as `path`.

## Environment of commands and servers

Commands the `bash` tool runs and the MCP servers dex starts do not get
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
readable to a command as they are to you. dex has no setting to withhold
them; unset one before starting dex to keep it from commands.

A command that needs a scrubbed variable (`gh`, a private module proxy)
gets it by name from your config: `"pass_env": ["GITHUB_TOKEN"]`. Only
your own config can say that.

## Policy

Each tool call is decided by rules: **deny**, then **ask**, then
**allow**, then the fallback (**ask**). A rule is a tool name, or a name
with a specifier: `read`, `bash(go test:*)`, `write(docs/**)`. `Bash`,
`Read` and `Edit` are accepted as the reference's names. For `bash` the
specifier is matched against the command; `go test:*` means `go test` and
anything after it at a word boundary, so it matches `go test ./...` and
not `go testing`.

dex ships this default:

- **runs without asking**: `read`, `glob`, `grep`, `ls`, `skill`,
  `memory_search`, and the read-only bash commands listed below;
- **asks**: `write`, `edit`, every other command, memory writes, and every
  MCP tool. `go test`, `go build`, `go vet` and `go list` ask: they run the
  repository's code (a `TestMain`, cgo, a vet tool), so a hostile
  repository would run code as you on the model's say-so.

**What runs without asking is a safe subset, not a blacklist.** A `bash`
call is auto-allowed only when it parses in a strict subset of the syntax
and every part of it is a command dex knows to be read-only, with
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

*git*: before a git command runs unasked, dex asks git for the config it
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
the plan it approved, and the bash tool runs a stamped call only if the line
still analyses to that plan. If a file or the repository's config changed in
between, the call fails with "the command changed since it was allowed; ask
again", and the original line is never run instead. Only dex can stamp a
call; one the model stamps is refused. `ls` with a glob runs with the
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
rule for `rm` reaches `git status; rm x`, a redirect to a file is shown as
a write to its target, and the question names the part it is asking about.
The cut is for the question and for deny and ask rules. Nothing is allowed
because of it: a command outside the subset is allowed only by a bare
`bash` allow rule or `"fallback": "allow"`. Inside the subset a command
the list above does not govern (`make`, `rm`) is for your rules, one stage
at a time.

**Trusting `go test` for your own repositories.** Put the rule in your
**user** config, `~/.config/dex/config.json`, never a repository's (a
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
and ask or deny what it allows. `"builtin": false` drops the shipped allow
list, so only your rules allow anything; `"fallback": "allow"` or `"deny"`
changes what a call no rule names does.

Path rules (`read(.env)`, `write(docs/**)`, `Read(secrets/**)`) are written relative to the working directory and match the path after cleaning, so `./.env`, `a/../.env` and the absolute path all meet the rule for `.env`, and `docs/../.git/x` is `.git/x`. Links are not followed for matching; the tools' own confinement still refuses one that leaves. `glob`, `grep` and `ls` are matched on the directory they search (default `.`), not on their pattern, and a rule for a directory should name `dir` and `dir/**`. A path rule is not a read ACL for a search that includes the directory from above.

An asked call prints `? allow bash {"command":"..."} (reason) [y/N]`.
Your answer is recorded in the session as a person's.

## Security model

dex gives a model the ability to read your files, change them and run
commands, so what it does and does not stand between the model and your
machine matters.

**What dex does**

- **Asks by default.** Every call that is not on a short allow list asks
  you first: writes, edits, every command that is not one simple read-only
  command, memory writes and every MCP tool. Your answer is recorded.
- **Auto-allows a small, checked set.** The read-only tools (`read`,
  `glob`, `grep`, `ls`, confined to the working directory) and bash lines
  that parse in a safe subset of the syntax (simple commands joined by
  `&&` and `|`, with a few trailing redirects) and whose every part is a
  read-only git, `ls`, `cat`/`head`/`tail`/`wc`/`grep` on named files, `pwd`,
  `go version`/`go env NAME` or `cd` into the workspace, with arguments
  that pass a per-command check and paths that stay inside the working
  directory (no `..`, links resolved). It is an allow-list of what is safe,
  not a list of what is dangerous: anything dex does not recognise asks.
  git is asked what its config would run first. `go test`, `go build` and
  `go vet` run the repository's code and are not on it.
- **Confines the file tools.** Paths outside the working directory,
  `..`, and symbolic links that lead out are refused, by the operating
  system's rooted open rather than by string checks.
- **Treats the repository as untrusted.** Its `.dex/config.json` can only
  tighten the policy; it cannot choose the provider, model or endpoint,
  add instructions, skills, memory or MCP servers, or allow anything.
  `AGENTS.md` files and `.dex/skills` that are symbolic links out of the
  workspace are not read into the prompt. Path rules match the path
  after normalisation, so `docs/../.git/x` is not under `docs/**`.
- **Keeps credentials away from what it starts.** Bash commands and MCP
  servers get your environment without `*_API_KEY`, `*_TOKEN`,
  `*_SECRET` and the like unless your config names a variable. Keys are
  read from the environment only and never printed.
- **Bounds resource use.** `read` scans at most 2 MiB a call, `grep`
  skips big and non-regular files, search patterns cannot run away, and
  git runs with the repository's fsmonitor, pager and diff programs
  switched off.
- **Keeps its records private.** The session store and memory are
  created `0700`; terminal control sequences in tool output and model text
  are stripped.

**What dex does not do**

- **There is no sandbox.** A command you approve runs with all your
  privileges, and a command that is auto-allowed is only as safe as the
  checks above. `bash` reaches files outside the working directory
  (approved commands are not confined, and the read-only allow list
  checks paths but cannot see what a program does with them). If you
  need a boundary, run dex in a container or VM.
- **A prompt injection can still ask.** A file, a web page, an MCP
  tool's output or an `AGENTS.md` can tell the model what to do. dex
  makes the dangerous steps ask; it cannot make you read the question.
  Read what you approve, especially a compound command, a write to
  `.git/hooks`, `.dex/`, `AGENTS.md` or `.github/`, and anything that
  sends data out.
- **Your own `allow` rules are yours.** `"allow": ["write"]` lets a
  model write `.git/hooks/pre-commit`; `bash(go test:*)` runs a hostile
  repository's tests with your privileges. Prefer narrow rules, and put
  deny or ask rules for the sensitive paths beside them.
- **Committed secrets are not caught.** The secret-path asks fire when a
  file is named. `git log -p`, `git show` and `git diff` without a path,
  `grep` over the tree and `git show HEAD~5` print what a repository
  holds, and a `.env` or key that was ever committed is in it. Keep
  secrets out of history (and rotate one that got in); dex cannot tell
  which lines of a diff are keys.
- **`-trust-skills` trusts the skills in directories you named**
  (`~/.dex/skills` and your config's `skills_dirs`) and never the
  repository's: a skill in `.dex/skills` is text from the repository, and
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
  trusted), a race between dex checking a path and the command using it,
  credential-file path variables such as `KUBECONFIG` (they pass through to
  commands), and denial of service by a model that loops (use `Ctrl-C`).

## The terminal client

On a terminal, `dex` opens the terminal client
([agentconsole](https://github.com/ChristopherDavenport/agentconsole)) over
the same session: the conversation rendered from the session's record, with
the run's live deltas on top. Before it takes the screen dex prints its start
lines (provider and model, the session, the policy in force, the tools, any
`omitted:` line and any warning such as a store whose permissions were fixed)
and, if there are warnings or omissions, waits for Enter, since the client
uses the alternate screen; what dex noted during the run is printed when it
exits.

| key | |
|---|---|
| Enter | send a prompt, or steer the run in flight |
| Ctrl-C | abort the run; quit when idle (a second one quits at once) |
| `y` / `n` | approve / refuse the permission asked; `n` then takes an optional reason and Enter |
| PgUp, PgDn, Ctrl-Up/Down, Ctrl-Home/End, mouse wheel | scroll the conversation |
| Ctrl-R / Ctrl-O | show reasoning / tool arguments and output in full: the selected row's, or with no row selected every row's |
| Ctrl-T | the tree of the session's branches; Enter views one, `c` continues from it |
| Ctrl-P, Ctrl-N, Ctrl-B, Tab | move over the rows, continue from a row, open its detail (policy decision, who decided, verification) |
| click | select the row under the pointer; a click on the selected row expands or collapses it |
| Ctrl-/ or F1 | list every key; Esc, q or Ctrl-/ goes back |

A call the policy asks about is a permission: the panel shows the call and
the reason, which is the policy's with what it is asking about added: the
rule that fired, the secret-looking path, the git config key that names a
program, the part of a command line. Everything that can ask is answered on
screen; nothing reads standard input once the client has the terminal. Two
things cannot be a permission, which a run that ended answers, since the
call that needs the answer is still running; the client asks them as
questions while the run goes, ahead of any permission:

- A call a **sub-agent** (`explore` or `task`) makes that the policy asks
  about: `y` allows it, `n` refuses it with an optional reason, which the
  sub-agent sees, and `Esc` goes back.
- A **question a tool asks mid-call** (MCP elicitation) that is yes or no.
  One that is a form or a page to visit has no screen in the client, and
  is cancelled.

A call stamped as auto-allowed that changed before it ran fails with "the
command changed since it was allowed ... ask again", which shows in the
transcript.

## In the REPL

`dex -front repl` (the default when not on a terminal) is a line REPL with
slash commands the terminal client does not have yet, since the client
takes the input line itself. A line typed while a run is in flight steers
it and lands before the next model call; `/follow text` queues a follow-up
that runs once the model would have stopped; `/abort` aborts. Between runs:
`/model name`, `/think on|off`, `/tools`, `/session`, `/mcp add <name>
<command>` (tools are `mcp__<name>__<tool>`; a name has letters, digits,
`-` and `_`, and no `__`), `/mcp remove <label>`, `/quit`. In the terminal
client use the flags and the config for the model, thinking and MCP servers
(`-model`, `-think`, `mcp_servers`), and Ctrl-C to quit; `/compact` does not
exist in either (use `-compact N`).

## Sessions

Every run is recorded in one content-addressed store,
`~/.dex/sessions`, for every project. Admin commands:

```sh
dex -list                          # sessions recorded for this directory
dex -resume <id>                   # continue one, answering any call an abort cut off
dex -verify <id>                   # rebuild every request and check its hash
dex -project <id> -out dir         # write one as a JSONL file (RFC 0001)
dex -import old.jsonl              # bring a JSONL session into the store
dex -gc pack                       # pack loose objects; -gc sweep also drops what no session needs
dex -repair <id>                   # rewrite a damaged log from what still reads
```

`-list`, `-verify` and `-project` open the store read-only, so they work
beside a running dex. `-sessions ""` disables recording.

Other state lives in `~/.dex`: `AGENTS.md` (read before the project's),
`skills/`, `memory/`. A project's `.dex/skills` and its `AGENTS.md` files
are read too.

## Develop

```sh
make check    # gofmt, go mod tidy -diff, go vet, staticcheck, govulncheck, go test -race

# CI is manual (workflow_dispatch) until the repository has a DEX_DEPS_TOKEN secret:
# agentconsole is private, so a runner needs a fine-grained token with read
# access to it. The gate is `GOWORK=off GOFLAGS=-mod=readonly make check` locally.
make build    # ./dex
```

In this workspace the module is built outside the go.work:
`GOWORK=off GOFLAGS=-mod=readonly make check`. See [CLAUDE.md](CLAUDE.md)
for the layout and release process, and [CHANGELOG.md](CHANGELOG.md).
