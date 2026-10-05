# Changelog

All user-visible changes to dax. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break flags and the config file.

## Unreleased

- Added: the `vertex` provider, Claude and Gemini on Google Vertex AI.
  Each request goes to the Anthropic adapter for a `claude-` model and
  the Gemini adapter for a `gemini-` model, so the main agent and the
  sub-agents can run different families. It authenticates with
  Application Default Credentials (`gcloud auth application-default
  login`) and takes the project from `GOOGLE_CLOUD_PROJECT`, else the
  credentials' project, and the location from `GOOGLE_CLOUD_LOCATION`
  or `GOOGLE_CLOUD_REGION`. Vertex publishes no model capabilities, so
  the reasoning effort is fitted from the model ID's generation.

## v0.0.1 - 2026-10-04

- Fixed: `make release` can cut the first release. With no plain vX.Y.Z
  tag published, the release guard takes any vX.Y.Z as the first one
  instead of refusing for want of a version floor; it also refuses
  prereleases and a major version the module path does not carry, and
  `make check` runs its tests.
- Changed: the project is named dax. The module is
  `github.com/ChristopherDavenport/dax`, the binary `dax`
  (`go install github.com/ChristopherDavenport/dax/cmd/dax@latest`), and
  its state moves with it: `~/.dax` (AGENTS.md, skills, memory,
  sessions), `~/.config/dax` (config.json, prices.json, instructions)
  and a project's `.dax/`. The old paths are not read; move them once.
- Added: the MIT license.
- Changed: CI runs on pushes to main and on pull requests. agentconsole
  is public, so a runner fetches it without a token, and the workflow no
  longer sets `GOPRIVATE`.
- Added: a running `bash` command reports its output as progress while
  it goes — the terminal client shows the lines under the call's row,
  and the REPL prints them as they arrive — instead of everything
  appearing when the command ends. The result the model sees is
  unchanged, and progress is not recorded.
- Changed: in the terminal client a function call's row is one line
  while it is collapsed: its arguments as key=value pairs instead of
  raw JSON, and, once it has ended, a hint of how many lines its output
  holds instead of the first of them; Ctrl-O or a second click shows
  the whole call as before. A running call shows the last five lines of
  its progress, not only the first. The session pane refreshes when a
  run ends, which it did not always do. This is agentconsole v0.0.6's.
- Dependencies: agentconsole v0.0.6, up from v0.0.5, for the one-line
  call rows and the running call's last lines.
- Changed: thinking streams as it happens, as assistant text always did.
  Against a server that follows OpenAI's Responses API (OpenRouter among
  them) the reasoning deltas arrive under OpenAI's event names, which
  decoded to nothing, so thinking showed once, whole, at completion.
- Dependencies: openresponses v0.0.15, up from v0.0.14, which decodes
  OpenAI's reasoning event names; providers/anthropic and
  providers/gemini at v0.0.15, the release that requires that root.
- Added: `-effort` and the config's `effort`, the reasoning effort
  `-think` asks for (`minimal`, `low`, `medium`, `high` or `xhigh`);
  `low`, as before, by default. It is fitted to each model like the
  default was, and a project file may not set it.
- Added: a mouse drag in the terminal client selects text anywhere on
  the screen (the conversation, the panes, the top bar) and the
  selection stays drawn until the next key or press; Ctrl-C copies it
  to the terminal's clipboard with OSC 52, over ssh too, as a desktop's
  copy does, and drops the selection so the next Ctrl-C is the abort or
  quit it always was. A click with no drag still selects the row under
  it. This is agentconsole v0.0.5's.
- Dependencies: agentconsole v0.0.5, up from v0.0.4, for the drag
  selection and the ctrl+c copy.
- Added: `-pricing-file` and the config's `pricing_file` load model
  prices. The terminal client's status line shows a running total of
  token usage and, with a price file, the session's cost, and the
  session pane shows the usage split by model. Without a price file it
  shows usage but no cost.
- Changed: the terminal client's prompt wraps over up to five lines,
  then scrolls, instead of scrolling sideways one line at a time; pasted
  newlines stay newlines.
- Fixed: in the terminal client, a sub-agent's call that the policy asks
  about is asked on screen while the run goes, y or n with an optional
  reason that reaches the sub-agent; it was refused, with a note telling
  the main agent to make the call itself. A tool's yes-or-no question
  (MCP elicitation) is asked the same way; a form or a page to visit is
  still cancelled.
- Dependencies: agentconsole v0.0.3, up from v0.0.2, for
  `client.Question` and `native.Backend.Ask`.
- Dependencies: agentconsole v0.0.4, for `client.Cost`, `console.WithCost`,
  the wrapping prompt, and the running usage on the status line.
- Dependencies: agenteval v0.0.10, for `price` and its JSON table.
- Dependencies: agentturn and agentturn/session v0.0.18, up from
  v0.0.17: a forked task's progress shows the sub-agent's own messages,
  not the main agent's last answer at the head of each update.
- Fixed: `/think` and `/model` did not reach the sub-agents, whose model
  and effort were fixed when the session started; each `explore` and
  `task` call now reads them as they stand.
- Added: `task` takes `context` (`fresh`, the default, or `fork`, which
  also sees the conversation so far) and `model` (`subagent`, the
  default, or `main`), so the main agent picks per call. A fork's
  conversation is the child run's opening items, recorded and verified
  in the child session; the main agent's reasoning items are left out.
- Dependencies: agentturn and agentturn/session v0.0.17, up from
  v0.0.16, for `tools/agent.WithCallConfig`.
- Fixed: the session record said a request asked for the effort dax
  configured when the model was sent the fitted one, so the recorded
  request hashes were of requests never sent; `-verify` could not see it,
  since it checks the record against itself. The effort is now fitted
  where each configuration is made, and the model wrapper only reports a
  request that was not.
- Fixed: `/model` kept the effort fitted to the previous model, and
  `/think` was forgotten by the next `/model`.
- Fixed: a sub-agent's session recorded no decision for a call its
  policy allowed.
- Added: the `task` sub-agent. The main agent starts it for a
  self-contained coding task; it runs on the sub-agent model with the file
  tools and bash under the same policy, sees dax's prompt, your
  instructions and AGENTS.md, and reports back. Calls in one turn run in
  parallel. Starting one is on the built-in allow list.
- Changed: the sub-agents are offered by default; `-agents=false` or
  `"agents": false` in the user config turns them off.
- Fixed: two edits of one file running at once, parallel calls in one
  turn, could lose one of them; writes and edits now take a workspace
  lock.
- Added: `subagent_model` and `-subagent-model`, the model the explore
  sub-agent runs.
- Changed: the `openrouter` provider defaults to
  `deepseek/deepseek-v4-pro-0813`, with `deepseek/deepseek-v4.1-flash`
  for sub-agents.
- Fixed: reasoning asked for was fitted to `none` when `none` and the
  nearest effort the model takes were equally near, so `-think` on
  `deepseek/deepseek-v4-pro` (none, high, xhigh) turned reasoning off.
- Fixed: a model, `base_url` or `api_key_env` from the config was kept
  when a flag switched the provider, so `"model": "qwen3-coder:30b"` with
  `-provider openrouter` asked OpenRouter for the Ollama model. They now
  belong to the provider in force where they were set, and a switch
  takes the new provider's defaults.
- Added: dax asks the vendor what the model takes (Anthropic's and
  Gemini's model endpoints, OpenRouter's catalogue, Ollama's
  `/api/show`), shows it under the banner, and fits each request's
  reasoning effort to it, saying once when it changes one.
- Fixed: `-think` (the default) on an Ollama model without thinking,
  `qwen3-coder:30b` say, failed every request with "does not support
  thinking"; reasoning is now sent as none.

- Added: the `openrouter` provider, with its key from `OPENROUTER_API_KEY`
  and `anthropic/claude-sonnet-5.5` as the default model.
- Added: the `openresponses` provider for any other Open Responses
  server: `-base-url` and `-model` are required, and `-api-key-env`
  (`api_key_env` in the user config) names the variable its key is read
  from. The project config may not set it.
- Changed: `openai` no longer takes `-base-url`; it is OpenAI. A config
  that pointed it at another server is refused with an error that says
  to use `openresponses`.
- Security: the variable the provider's key was read from is removed
  from the environment of bash commands and MCP servers even when its
  name does not look like a credential's, unless `pass_env` names it.
  `OPENROUTER_API_KEY` joins the named credentials.

- Added: dax, with the siblings at agentkit v0.0.7, agentturn and
  agentturn/session v0.0.16, agentsession v0.0.21, agenttool v0.0.15,
  openresponses and its anthropic and gemini providers v0.0.14,
  agentpolicy v0.0.11, agentskill v0.0.11, agentmemory v0.0.10 and
  agentsmd v0.0.2.
- Added: `glob`, `grep` and `ls` tools.
- Added: the file tools are confined to the working directory, through
  an `os.Root`: a path outside it, a `..` out, or a symbolic link out is
  refused.
- Added: providers, `-provider ollama|openai|anthropic|gemini`, `-model`
  and `-base-url`, with keys from `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`
  and `GEMINI_API_KEY`. Ollama stays the default.
- Added: a config file, `~/.config/dax/config.json`, and a per-project
  `.dax/config.json`; `-config` names another user file.
- Added: a default policy that is always on: the read-only tools and a
  few safe commands run, writes and every other command ask. `bash` is
  decided per subcommand. `-no-policy` turns it off.
- Security: a bash call is auto-allowed only if it is one simple command in a
  safe subset of the syntax with read-only arguments (git status, diff,
  log, show; ls; pwd; go version; go env NAME); everything else asks. Fixes
  comment, `$'..'` and `>&word` handling in the splitter that serves asked
  commands. Git runs with the repository's fsmonitor, pager, ssh command and
  hooks neutralised and `--no-ext-diff --no-textconv`.
- Security: `go test`, `go build`, `go vet` and `go list` are no longer
  auto-allowed; the README says how to allow them in the user config.
- Security: the project config is tighten-only; it may no longer set the
  provider, model, base_url, think, instructions_file, skills_dirs,
  memory_dir, mcp_servers or pass_env, nor add allow rules or carve-outs.
- Security: path rules match the normalised path; glob, grep and ls are
  matched on where they search.
- Security: AGENTS.md files and `.dax/skills` that link out of the workspace
  are not read; reported as omitted.
- Security: bash commands and MCP servers start without credentials in their
  environment (`pass_env` names exceptions); `max_read_bytes` bounds `read`
  and `edit`; grep and glob skip non-regular files and cannot be made
  exponential; the session store and memory are `0700`; `/mcp add` uses the
  `mcp__<name>__` prefix and names may not contain `__`; terminal control
  sequences are stripped from output.
- Security: git commands run unasked only if the repository's own git config
  names no program (filters, textconv, askpass, editor, proxies,
  credential helpers, drivers ...), the question names the key; the gpg
  programs are `/bin/false` and `%G` in a format asks. The git
  neutralisation environment and `GOTOOLCHAIN=local` apply only to
  auto-allowed commands, not to ones you approve. `..` in an `ls` or git
  path asks and links are resolved segment by segment; a `:` in a git
  revision or pathspec asks; bare `go env` asks. The credential scrub covers
  `_KEY`, `_PAT`, `_PWD`, `_JWT`, `_CREDENTIALS`, `_AUTH`, PASSWORD, SECRET,
  DATABASE_URL, SSH_AUTH_SOCK and URLs with passwords; credential-file paths
  pass through. MCP stderr is cleaned. `-trust-skills` trusts only skills in
  directories you named.
- Added: more read-only commands run unasked: git combined short flags and
  space-separated values, `branch`, `rev-parse`, `ls-files`, `remote -v`,
  `blame`, `stash list`, `tag`, `describe`, `shortlog`, `config` reads;
  trailing `2>&1`, `2>/dev/null`, `>/dev/null`; pipes into `head`, `tail`,
  `wc`, `sort`, `uniq`, `cut`, `grep`; `&&` sequences and `cd` into the
  workspace; `cat`, `head`, `tail`, `wc`, `grep` on named files; `ls` with
  globs. Bash output is capped at 50 KiB in memory.
- Security: a glob word with a quoted part is not in the safe subset (it was
  rendered bare, so `ls ';touch /x;'*` ran touch); a differential test runs
  every auto-allowed line through real bash with each stage's command
  replaced by a probe and compares argv. An auto-allowed bash call is
  stamped with the plan the policy approved and runs only that plan; if the
  line no longer analyses to it the call fails instead of running verbatim.
  git config reads only a named key that holds no secret; URL credentials are
  taken out of auto-allowed output. git asks when `.git` is a file or link,
  git's repository is not the workspace's, `core.worktree`, `core.bare`,
  `extensions.*` are set, or a submodule's config names a program. Reading a
  secret-looking path asks (`read(.env)` in the user config opens one). `ls`
  glob expansions follow a `--`.
- Security: a line with a git stage that is not auto-allowed asks when the
  repository's config names core.fsmonitor, hooksPath, sshCommand, pager or a
  gpg program (it runs without the neutralising environment). The explore
  sub-agent is decided by the parent's policy for every tool, with asks put to
  the user. Secret-path asks apply to what a link leads to and ignore case.
  The Security model says committed secrets and tree-wide searches are not
  caught.
- Added: the terminal client (agentconsole v0.0.1) is the default front on
  a terminal, over the session's kit through kitbackend: permissions answered
  with y/n on screen with the policy's reason and what it asks about; start
  lines and warnings printed before the client takes the screen and notes
  after it. `-front tui|repl`; `-p` unchanged. The explore sub-agent's asks,
  which cannot be a permission, are refused with a reason telling the model
  to make the call itself; tool elicitation is declined. The slash commands
  stay in the REPL.
- Fixed: the terminal client is the default only with a terminal at both ends
  (a real isatty, so /dev/null is not one) and a session store; `-front tui`
  without them fails before a store is opened or a banner printed; the
  pre-client pause needs both ends a terminal; dax's buffered notes and the
  session ID are flushed however the client ends, and warnings held back for
  the screen are shown before an error opening the session.
- Dependencies: agentconsole v0.0.2. In the terminal client, Ctrl-O and
  Ctrl-R expand or collapse the selected row alone (every row with none
  selected), a click selects a row and a second click expands it, bars
  frame the input line, and Ctrl-/ (or F1) lists the keys.
- Changed: CI runs on `workflow_dispatch` only until a `DAX_DEPS_TOKEN`
  secret exists (agentconsole is private).
- Changed: `-confirm` is gone, since the policy is always on; `-key` and
  `DAX_API_KEY` are gone, since a key is read from the provider's own
  variable; `-base` is now `-base-url`; the default `-model` follows the
  provider.
- Changed: the front is chosen in `cmd/dax/front.go`, the place a
  terminal UI plugs in; `-front` names it, and only `repl` exists.
