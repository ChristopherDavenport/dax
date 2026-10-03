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
| `ollama` (default) | `qwen3.5:9b` | none | `http://localhost:11434/v1` |
| `openai` | `gpt-5` | `OPENAI_API_KEY` | `https://api.openai.com/v1`; any OpenAI-compatible server |
| `anthropic` | `claude-sonnet-5-5` | `ANTHROPIC_API_KEY` | not supported |
| `gemini` | `gemini-2.5-pro` | `GEMINI_API_KEY` | not supported |

`-model` names another model. A missing key is an error before any request
is made: `dex: openai: no API key: set OPENAI_API_KEY in the
environment`. dex reads keys from the environment and nowhere else (not
from a config file, not from a flag, so they stay out of `ps` and out of
a repository), and never prints one.

Ollama and the OpenAI-compatible path use the `openresponses` client;
Anthropic and Gemini use its provider adapters. `-think` (default on)
asks for low reasoning effort with summaries and shows the reasoning;
`-think=false` turns reasoning off.

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

Every field is optional. The files are strict: an unknown field, a bad
provider, a malformed URL or a rule that does not parse is an error that
names the file and the field.

| field | meaning |
|---|---|
| `provider`, `model`, `base_url`, `think` | as above; `base_url` is for `ollama` and `openai` |
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
one of yours. It may **not** set `provider`, `model`, `base_url`, `think`,
`instructions_file`, `skills_dirs`, `memory_dir` or `mcp_servers`: dex
refuses the file with an error naming the field and saying to put it in
your own config. (Where the model runs, what it is told and remembers, and
what programs start are decisions that send your code, your files and your
keys somewhere; a repository does not get to make them.)

| flag | |
|---|---|
| `-provider`, `-model`, `-base-url`, `-think` | override the config |
| `-config path` | the user config file |
| `-memory dir` | memory directory; `off` or empty disables it |
| `-no-policy` | run every tool call without asking; ignores the config's policy |
| `-front repl` | the front end (only `repl`) |
| `-agents-md`, `-skills`, `-trust-skills` | the AGENTS.md chain, skills, and a skill's `allowed-tools` running unasked until the next message |
| `-compact N`, `-compact-server` | fold the transcript above N estimated tokens, locally or through the server |
| `-mcp "cmd"` | one more stdio MCP server, as `mcp__cli__<tool>` |
| `-agents` | offer the read-only `explore` sub-agent |
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
your credentials: every variable whose name ends in `_API_KEY`, `_TOKEN`,
`_SECRET`, `_PASSWORD`, `_SECRET_ACCESS_KEY` or `_ACCESS_KEY_ID`, and the
provider keys (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`,
`GOOGLE_API_KEY`, `GITHUB_TOKEN`, ...), is removed from their environment,
so a test, a build script or a server cannot read them. A command that
needs one (`gh`, a private module proxy) gets it by name from your config:
`"pass_env": ["GITHUB_TOKEN"]`. Only your own config can say that.

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
  `memory_search`, and the commands `git status`, `git diff`, `git log`,
  `git show`, `go version`, `go env NAME`, `ls` and `pwd`, as far as they
  are in the safe subset below. `go test`, `go build`, `go vet` and `go list`
  are not on the list: they run the repository's code (a `TestMain`, cgo,
  a vet tool, a toolchain named in `go.mod`), so a hostile repository
  would run code as you on the model's say-so;
- **asks**: `write`, `edit`, every other command, memory writes, and every
  MCP tool.

**What runs without asking is a safe subset, not a blacklist.** A `bash`
call is auto-allowed only when both hold:

1. It is one simple command made of plain words and simple quotes:
   letters, digits and `_ . / : @ % + , = -` (and `~` or `^` inside a word,
   as in `HEAD~1`), single-quoted strings, and double-quoted strings with
   no `$`, backtick or backslash. No `;`, `&`, `|`, `<`, `>`, `#`, `$`,
   backtick, backslash, newline, glob (`* ? [`), `{`, a word-initial `~`,
   `=` in the command word, or non-ASCII byte. Anything else asks, whatever
   the rules say; it is never denied for that.
2. Its arguments pass a per-command check: `git status|diff|log|show` with
   read-only flags from an allowlist only (not `--output`, `-o`,
   `--ext-diff`, `--textconv`, `-c`, `-C`, `--git-dir`, `--work-tree`,
   `--exec-path`, `--no-index`), `ls` with listing flags, `pwd`, `go version`
   and `go env NAME`; and every path argument stays inside the working
   directory after cleaning and resolving links. Git runs with the
   repository's fsmonitor, pager, ssh command and hooks configuration
   neutralised, and `git diff|log|show` run with `--no-ext-diff
   --no-textconv`.

A command that is not in the subset is still cut into its subcommands, so
a deny or ask rule for `rm` reaches `git status && rm x`, a redirect to a
file is shown as a write to its target, and the question names the part it
is asking about. The cut is for the question and for deny and ask rules.
Nothing is allowed because of it: a command outside the subset is allowed
only by a bare `bash` allow rule or `"fallback": "allow"`.

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
  `glob`, `grep`, `ls`, confined to the working directory) and a bash
  command only if it is one simple command in a safe subset of the
  syntax (no operators, expansions, globs, comments or escapes) whose
  arguments pass a per-command read-only check: `git status|diff|log|show`
  with read-only flags, `ls`, `pwd`, `go version`, `go env NAME`, paths
  inside the working directory. It is an allow-list of what is safe, not a
  list of what is dangerous: anything dex does not recognise asks.
  `go test`, `go build` and `go vet` run the repository's code and are not
  on it.
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
- **`-trust-skills` trusts every skill it can load**, the project's
  included; leave it off for repositories you do not trust.
- **The provider sees what the model reads.** Files and command output go
  to the model's provider (OpenAI, Anthropic, Google, or your Ollama
  host). Choose the provider with that in mind; a path rule is not a read
  ACL for a search that includes the directory from above.
- **Not covered:** a hostile `.git/config` in a directory you did not
  clone (git is run with its program hooks switched off, but a textconv or
  filter driver the repository names under `.gitattributes` is git's
  to run for commands dex does not rewrite), the Go toolchain download a
  `go.mod` can trigger, and denial of service by a model that loops (use
  `Ctrl-C`).

## In the REPL

A line typed while a run is in flight steers it and lands before the next
model call; `/follow text` queues a follow-up that runs once the model
would have stopped; `/abort` aborts. Between runs: `/model name`,
`/think on|off`, `/tools`, `/session`, `/mcp add <name> <command>` (tools are `mcp__<name>__<tool>`; a name has letters, digits, `-` and `_`, and no `__`),
`/mcp remove <label>`, `/quit`.

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
make build    # ./dex
```

In this workspace the module is built outside the go.work:
`GOWORK=off GOFLAGS=-mod=readonly make check`. See [CLAUDE.md](CLAUDE.md)
for the layout and release process, and [CHANGELOG.md](CHANGELOG.md).
