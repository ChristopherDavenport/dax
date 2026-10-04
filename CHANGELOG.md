# Changelog

All user-visible changes to dex. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break flags and the config file.

## Unreleased

- Added: `-effort` and the config's `effort`, the reasoning effort
  `-think` asks for (`minimal`, `low`, `medium`, `high` or `xhigh`);
  `low`, as before, by default. It is fitted to each model like the
  default was, and a project file may not set it.
- Added: a mouse drag in the terminal client selects text anywhere on
  the screen (the conversation, the panes, the top bar) and the release
  copies it to the terminal's clipboard with OSC 52, over ssh too; a
  terminal that does not take it copies nothing. A click with no drag
  still selects the row under it; this depends on the next agentconsole
  release.
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
  the wrapping prompt, and the running usage on the status line; this
  depends on the next agentconsole release.
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
- Fixed: the session record said a request asked for the effort dex
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
  tools and bash under the same policy, sees dex's prompt, your
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
- Added: dex asks the vendor what the model takes (Anthropic's and
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

- Added: dex as its own repository, extracted from the agent studies
  with the siblings at agentkit v0.0.7, agentturn and agentturn/session
  v0.0.16, agentsession v0.0.21, agenttool v0.0.15, openresponses and its
  anthropic and gemini providers v0.0.14, agentpolicy v0.0.11,
  agentskill v0.0.11, agentmemory v0.0.10 and agentsmd v0.0.2.
- Added: `glob`, `grep` and `ls` tools.
- Added: the file tools are confined to the working directory, through
  an `os.Root`: a path outside it, a `..` out, or a symbolic link out is
  refused.
- Added: providers, `-provider ollama|openai|anthropic|gemini`, `-model`
  and `-base-url`, with keys from `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`
  and `GEMINI_API_KEY`. Ollama stays the default.
- Added: a config file, `~/.config/dex/config.json`, and a per-project
  `.dex/config.json`; `-config` names another user file.
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
- Security: AGENTS.md files and `.dex/skills` that link out of the workspace
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
  pre-client pause needs both ends a terminal; dex's buffered notes and the
  session ID are flushed however the client ends, and warnings held back for
  the screen are shown before an error opening the session.
- Dependencies: agentconsole v0.0.2. In the terminal client, Ctrl-O and
  Ctrl-R expand or collapse the selected row alone (every row with none
  selected), a click selects a row and a second click expands it, bars
  frame the input line, and Ctrl-/ (or F1) lists the keys.
- Changed: CI runs on `workflow_dispatch` only until a `DEX_DEPS_TOKEN`
  secret exists (agentconsole is private).
- Changed: `-confirm` is gone, since the policy is always on; `-key` and
  `DEX_API_KEY` are gone, since a key is read from the provider's own
  variable; `-base` is now `-base-url`; the default `-model` follows the
  provider.
- Changed: the front is chosen in `cmd/dex/front.go`, the place a
  terminal UI plugs in; `-front` names it, and only `repl` exists.
