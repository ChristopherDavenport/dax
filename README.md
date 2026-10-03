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
2. `.dex/config.json` in the working directory
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
| `policy` | see below |

A project's `.dex/config.json` comes from a repository, not from you, so it
is held to less. It may not set `mcp_servers` (that starts programs), may
not set `base_url` unless the provider is `ollama` (which takes no key;
anything else would send your key to the repository's host), may not set
`"fallback": "allow"` or `"builtin": false`, and the `allow` rules in its
policy are ignored: it can make dex ask or refuse more, never less.

| flag | |
|---|---|
| `-provider`, `-model`, `-base-url`, `-think` | override the config |
| `-config path` | the user config file |
| `-memory dir` | memory directory; `off` or empty disables it |
| `-no-policy` | run every tool call without asking; ignores the config's policy |
| `-front repl` | the front end (only `repl`) |
| `-agents-md`, `-skills`, `-trust-skills` | the AGENTS.md chain, skills, and a skill's `allowed-tools` running unasked until the next message |
| `-compact N`, `-compact-server` | fold the transcript above N estimated tokens, locally or through the server |
| `-mcp "cmd"` | one more stdio MCP server, as `mcp__mcp__<tool>` |
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
  `git show`, `go test`, `go build`, `go vet`, `go list`, `go version`,
  `ls`, `pwd` and `cd`;
- **asks**: `write`, `edit`, every other command, memory writes, and every
  MCP tool.

A command line is cut into its subcommands before the rules see it, so
`git status && rm -rf /` asks (about the `rm`), `git log > ~/.bashrc`
asks (as a write to `~/.bashrc`), and anything with `$(...)`, backticks or
process substitution asks. The splitter is not a shell parser; it errs
toward asking and blocks a command with an unterminated quote.

Your rules in `policy` add to the default. Because deny beats ask beats
allow, allow what the default asks about (`"allow": ["write(docs/**)"]`)
and ask or deny what it allows. `"builtin": false` drops the shipped allow
list, so only your rules allow anything; `"fallback": "allow"` or `"deny"`
changes what a call no rule names does.

An asked call prints `? allow bash {"command":"..."} (reason) [y/N]`.
Your answer is recorded in the session as a person's.

## In the REPL

A line typed while a run is in flight steers it and lands before the next
model call; `/follow text` queues a follow-up that runs once the model
would have stopped; `/abort` aborts. Between runs: `/model name`,
`/think on|off`, `/tools`, `/session`, `/mcp add <prefix> <command>`,
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
