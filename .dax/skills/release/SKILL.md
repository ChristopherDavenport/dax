---
name: release
description: How a change to dax is recorded and shipped. Covers the CHANGELOG's Unreleased section, commits and pull requests, sibling bumps, and make release. Load it before writing a changelog entry, committing, opening a pull request or releasing.
---

# Changelog, commits, pull requests and releases

## The changelog

- `CHANGELOG.md` keeps an `## Unreleased` section at the top, in Keep a
  Changelog form. A user-visible change adds its line there in the same
  commit or pull request that makes it.
- Each line starts with its kind: `Added:`, `Changed:`, `Fixed:`,
  `Security:` or `Dependencies:`, and names the issue it closes (`(#190)`).
- A change to the public API (the root package `dax`, `agent`,
  `extension`, `policy`, `tool`, `toolrender` and the `ext/*` packages)
  gets a `Changed:` line naming what changed. Before v1.0.0 such a change
  may land in a minor version.
- A sibling bump is made by hand, with an Unreleased line naming the
  new version and what it brings (see Siblings in `AGENTS.md`).

## Commits and pull requests

- `make check` must pass before any commit (fmt, tidy-check, vet,
  staticcheck, govulncheck, race tests, the release guard's tests). In
  a workspace with a go.work, run
  `GOWORK=off GOFLAGS=-mod=readonly make check`.
- CI (`.github/workflows/ci.yml`) runs on pushes to main and on pull
  requests.

## Releasing

- `make release VERSION=vX.Y.Z` does the following in order: it dates the
  Unreleased section, runs `make tidy` and `make check`, commits
  `Release vX.Y.Z`, runs the guard, writes an annotated tag whose message
  is the section, and pushes the branch and the tag atomically. It refuses
  a dirty tree or a changelog with no Unreleased section. `TRAILER`, when
  set, is appended to the commit message.
- `scripts/release-guard.sh` (`make release-guard TAG=...`) refuses a
  tag that exists or does not sort above the published ones. If it
  refuses, nothing is public yet: undo with `git reset --hard HEAD~1`.
- The release workflow (`.github/workflows/release.yml`) publishes the
  tag's message as the GitHub release.
