# Read AGENTS.md chains from an fs.FS, so a workspace's instructions are read where the project is

Repositories: agentsmd (the change), agentkit (an option that passes it
through). agentskill needs nothing. Draft for filing as two issues, the
agentkit one blocked on the agentsmd release.

## The problem

A product whose tools act in a workspace that is not the host's
directory (a container, a remote runtime: see the agentworkspace
proposal) reads the project's instructions from the wrong machine.
`agentsmd.Chain(path, opts)` walks the OS file system from `path` up to
`opts.Root` with `os.Stat` and `os.ReadFile` (`agentsmd/agentsmd.go:140-230`,
`:184`, `:238`, `:252`), and `agentkit.WithAgentsMD(path, opts)` hands it
an OS path (`agentkit/options.go:248-252`, called at
`agentkit/instructions.go:383` and `:397`). The project's AGENTS.md files
are in the workspace, which on a remote machine has no OS path here.

dax now runs every tool and every policy check through a workspace
(`dax/workspace`), and records the workspace in the session. Its
AGENTS.md chain is the one instruction source still read from the host,
and it is the one the repository controls.

## What each library needs

### agentskill: nothing

Skills already read through `fs.FS`: `agentskill.Source{FS, Location,
Qualifier}` (`agentskill/skill.go:28-48`), `Load(fsys, location)`
(`agentskill/load.go:36`), and `Discover(sources...)`; `Dir` is only the
OS convenience (`load.go:218-234`). agentkit already takes them:
`WithSkillSources(...agentskill.Source)` (`agentkit/options.go`, after
`WithOptionalSkills`). A product passes `fs.Sub(workspace.FS(),
".dax/skills")` with the workspace's root as `Location`, and the skill
tool reads bodies and files through the same `fs.FS`. (dax's own part is
to do that, and to screen links through the workspace's `fs.ReadLinkFS`
instead of the host's.)

### agentsmd: a walk over an fs.FS

Add the file system to `Options`, keeping `Chain`'s signature:

```go
type Options struct {
	// FS, when set, is where the walk reads: path, Root and the chain's
	// directories are fs.ValidPath names in it ("." its root), and the
	// walk never leaves it. Extra are still OS paths (the user's own
	// file in their home directory is not the project's). Nil is the OS
	// file system, as now.
	FS fs.FS
	...
}
```

With `FS` set, `Chain` stats and reads through `fs.Stat`/`fs.ReadFile`,
walks with `path.Dir` instead of `filepath.Dir`, stops at `Root` or `"."`,
and reports `File.Path` as the name in the FS (the product maps it to
what it shows). `MaxBytes`, `Budget`, `Names` and `Omitted` are unchanged.
Root stays standard-library-only (`agentsmd/AGENTS.md`, Module).

Whether `Extra` should also be able to come from an FS is left open
below; the case that needs it (a user's file on a remote machine) has not
come up.

### agentkit: pass it through

`WithAgentsMD(path, opts)` already passes `agentsmd.Options` verbatim
(`options.go:240-247`), so once agentsmd has `FS`, agentkit needs no new
option, only a release that pins the agentsmd that has it, and a line in
`docs/manual.md` (`agentkit/AGENTS.md`, "The rule": every field the kit
sets has a manual call). `TestTheManualPathIsTheSamePath` covers it.

## What it must not do

- Change behaviour with `FS` nil: every existing test passes unchanged.
- Follow a name out of the FS. An `fs.FS` that confines (os.Root's,
  a workspace's) refuses a link out; agentsmd must not resolve one itself.
- Read a file the chain would not include (the current rule, `Chain`'s
  doc: only a file that would be included is read).

## Tests

- The fixture tree under `testdata/tree` walked through `os.DirFS` and
  through `fstest.MapFS` gives the same files, omissions and render as
  the OS walk.
- Root as a sub-directory name stops the walk there; a path outside it is
  an error, as now.
- An FS whose `Open` refuses a link out reports it as an error, not as a
  file.

## What dax does with it

dax reads AGENTS.md through its session's workspace: `agentsmd.Options{FS:
ws.FS(), Root: ".", Extra: [~/.dax/AGENTS.md]}` with the path the
workspace's root, dropping its own pre-walk that screens links on the
host (`dax/agent/trust.go`, `agentsFiles`) for the workspace's
confinement. dax-skills builds its project source over the workspace's FS
(no sibling change). The project config (`.dax/config.json`) is dax's
own file and moves to the workspace in the same change.

## Open questions

- Should `Extra` take `(fs.FS, name)` pairs, so a user file can come from
  a third place?
- `File.Path` for an FS walk: the FS name, or a `Location` prefix the
  product supplies (as agentskill's `Source.Location`), so the prompt can
  say `/workspace/AGENTS.md`?
