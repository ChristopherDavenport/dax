# Changelog

All user-visible changes to dex. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break flags and the config file.

## Unreleased

- Added: dex, with the siblings at agentkit v0.0.7, agentturn and
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
- Added: a config file, `~/.config/dex/config.json`, and a per-project
  `.dex/config.json`; `-config` names another user file.
- Added: a default policy that is always on: the read-only tools and a
  few safe commands run, writes and every other command ask. `bash` is
  decided per subcommand. `-no-policy` turns it off.
- Changed: `-confirm` is gone, since the policy is always on; `-key` and
  `DEX_API_KEY` are gone, since a key is read from the provider's own
  variable; `-base` is now `-base-url`; the default `-model` follows the
  provider.
- Changed: the front is chosen in `cmd/dex/front.go`, the place a
  terminal UI plugs in; `-front` names it, and only `repl` exists.
