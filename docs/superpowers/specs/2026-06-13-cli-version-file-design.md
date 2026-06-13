# CLI_VERSION file — file-driven CLI releases

**Date:** 2026-06-13
**Status:** Design approved

## Goal

Make the `horde` CLI release the same way `@horde.io/cdk` does: editing a version
file and merging to `main` auto-releases. Today the CDK is fully file-driven
(`tag-on-bump.yml` watches `cdk/package.json` → pushes `cdk-v<version>` →
`publish.yml`), while the CLI requires a human to push the `v*` tag by hand. This
closes that asymmetry.

After this change, both artifacts are file-driven and symmetric:

| Artifact | Source file        | Watcher → tag                       | Release workflow            |
|----------|--------------------|-------------------------------------|-----------------------------|
| CDK      | `cdk/package.json` | `tag-on-bump.yml` → `cdk-v<v>`      | `publish.yml` → npm         |
| CLI      | `CLI_VERSION`      | `tag-on-cli-bump.yml` → `v<v>`      | `release-cli.yml` → GoReleaser |

The mental model becomes uniform: **edit the version file, merge to main, it
releases.** Tags still exist (both publish paths are tag-triggered, and
GoReleaser/npm need them) but they are machine-generated — no human pushes a
release tag for either artifact again.

## Approach (chosen: "A" — file is everything; tag exists internally)

The `v*` tag is **kept** — GoReleaser is tag-driven by design (reads
`{{ .Version }}` from the tag, anchors the GitHub Release and changelog on it).
Fully removing the tag would *add* complexity (GORELEASER_CURRENT_TAG hacks, loss
of tag-anchored releases). Instead, the tag stops being human-managed: a new
watcher workflow generates it from `CLI_VERSION`, exactly mirroring the CDK side.

## Components

### 1. `CLI_VERSION` (new, repo root)

A one-line plaintext file containing plain semver, no leading `v`:

```
0.8.0
```

This is the human-edited source of truth for the CLI version. (Initialized to
`0.8.0` to match the current released CLI version.)

### 2. `make build` / `make install` — read `CLI_VERSION`

The Makefile's `VERSION` is derived from `CLI_VERSION` instead of
`git describe --tags --match 'v*'`. Dev builds stay self-identifying by
appending a git short-SHA and `-dirty` marker when the tree is not a clean
release checkout:

```makefile
CLI_VERSION_BASE := $(shell cat CLI_VERSION 2>/dev/null || echo 0.0.0)
GIT_SHA          := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
GIT_DIRTY        := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo '-dirty' || echo '')
VERSION          := v$(CLI_VERSION_BASE)-g$(GIT_SHA)$(GIT_DIRTY)
```

- A clean dev checkout → `v0.8.0-g653e2ae`
- A dirty dev checkout → `v0.8.0-g653e2ae-dirty`
- At release time GoReleaser does NOT use `make build` — it reads the tag — so
  the binary GoReleaser ships is stamped `v0.8.0` (clean) from the tag. `make
  build` is for local/dev builds only.

Rationale for keeping a SHA suffix on every `make build`: it preserves the
existing "dev builds are self-identifying" property. The release binary's clean
`v0.8.0` stamp comes from GoReleaser via the tag, not from `make build`.

### 3. `.github/workflows/tag-on-cli-bump.yml` (new)

A near-verbatim copy of `tag-on-bump.yml`, adapted for the CLI:

- `on: push: branches: [main], paths: ["CLI_VERSION"]`
- Reads `CLI_VERSION` at HEAD and `HEAD~1`; if changed, pushes `v<version>`.
- Pushed with `RELEASE_TAG_PAT` (NOT `GITHUB_TOKEN`) so the tag push fires
  `release-cli.yml` — GITHUB_TOKEN-pushed tags are deliberately suppressed from
  triggering workflows.
- Idempotent: skips if the tag already exists on origin.
- `concurrency` group to serialize.

### 4. `release-cli.yml` / GoReleaser — UNCHANGED

Still `on: push: tags: v*`, still reads the version from the tag. The only change
is that the tag now arrives from `tag-on-cli-bump.yml` instead of a human.

### 5. `cmd/horde/release_wiring_test.go` — update the contract guard

`TestMakefileDescribesCLIPrefix` currently asserts the Makefile contains
`--match 'v*'`. That premise goes away. Replace it with an assertion that the
Makefile sources `CLI_VERSION` and still produces a `v`-prefixed version
(preserving the real contract: CLI version string is `v*`, distinct from
`cdk-v*`). Add `TestTagOnCLIBumpWatchesVersionFile` asserting
`tag-on-cli-bump.yml` watches `CLI_VERSION` (path filter) and pushes a `v`-prefix
tag — mirroring how the CDK side is guarded.

The two existing prefix-contract tests stay green and unchanged:
- `TestPublishWorkflowUsesCDKPrefix` (publish.yml → `cdk-v*`)
- `TestReleaseCLIWorkflowUsesVPrefix` (release-cli.yml → `v*`)

## Data flow

```
edit CLI_VERSION → merge to main
  → tag-on-cli-bump.yml (paths: CLI_VERSION) detects change → pushes v<version>
    → release-cli.yml (on: tags v*) fires → GoReleaser
      → cross-platform binaries + checksums + GitHub Release + Homebrew formula
```

## Docs to update

- `CLAUDE.md` release section: the "To bump the CLI: no file edit is needed…
  human pushes the v<version> tag" line becomes "edit `CLI_VERSION`; on merge to
  main, `tag-on-cli-bump.yml` auto-pushes `v<version>`". Also note `make build`
  now reads `CLI_VERSION`.

`internal/docs/content.go` (`horde docs install`) is NOT touched: it documents
installing/updating the binary (download script, Homebrew, `horde update`), not
the release-cutting mechanism. So this PR's only doc surface is the repo-internal
`CLAUDE.md` — which means **no user-facing CLI behavior changes from the docs**.
The CLI bump for this PR is justified by the Makefile + `release_wiring_test.go`
changes (build/release wiring), not by docs.

## Versioning of THIS change

Tension: by the strict rule ("bump only if a user can observe the change from an
installed artifact"), this PR changes only the Makefile, a test, CLAUDE.md, and
adds CI workflow + `CLI_VERSION` — none of which alter the shipped binary's
behavior. So strictly it need not bump.

Counter: this is release-infrastructure, and bumping `CLI_VERSION` in this PR
**dog-foods** the new path — merging it would auto-push `v<version>` and prove
the mechanism end-to-end on its first use.

Decision (per user): bump `CLI_VERSION` to `0.9.0` in this PR as the final
"Release" commit, so the merge exercises the new auto-release. No CDK bump
(no `cdk/` change). `CLI_VERSION` is initialized at `0.8.0` (current released
CLI) and then the release commit moves it to `0.9.0`.

## Out of scope (YAGNI)

- The CDK side stays exactly as-is.
- No unifying of the two version files into one.
- No changelog format changes.
- No change to GoReleaser config (`.goreleaser.yaml`).
```
