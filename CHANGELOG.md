# Changelog

All user-visible changes to dex. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break flags and the config file.

## Unreleased

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
