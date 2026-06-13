# CLI_VERSION File — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the `horde` CLI release file-driven (edit `CLI_VERSION`, merge to main, auto-release) — symmetric with the CDK's `cdk/package.json` → `tag-on-bump.yml` flow.

**Architecture:** Add a root `CLI_VERSION` file as the version source of truth. `make build` reads it. A new `tag-on-cli-bump.yml` workflow watches it on main and auto-pushes `v<version>`, which fires the existing (unchanged) `release-cli.yml` → GoReleaser. The tag still exists for GoReleaser but is machine-generated, never hand-pushed.

**Tech Stack:** GNU Make, GitHub Actions (YAML), Go (`cmd/horde/release_wiring_test.go` contract guard, `gopkg.in/yaml.v3`), GoReleaser (unchanged).

**Working directory:** `/home/jb/work/horde/.claude/worktrees/cli-version-file` (worktree on branch `worktree-cli-version-file`, off `main` @ `653e2ae`).

---

## File Structure

- **Create** `CLI_VERSION` — one-line plaintext semver. Source of truth.
- **Create** `.github/workflows/tag-on-cli-bump.yml` — watcher: `CLI_VERSION` change on main → push `v<version>`.
- **Modify** `Makefile` (lines 2–14) — derive `VERSION` from `CLI_VERSION` instead of `git describe`.
- **Modify** `cmd/horde/release_wiring_test.go` — rewrite `TestMakefileDescribesCLIPrefix`; extend `workflowTriggers` to also decode `branches`/`paths`; add `TestTagOnCLIBumpWatchesVersionFile`.
- **Modify** `CLAUDE.md` — update the CLI-bump line in the release section.

---

## Task 1: Add the `CLI_VERSION` file

**Files:**
- Create: `CLI_VERSION`

- [ ] **Step 1: Create the file with the current released CLI version**

Create `CLI_VERSION` containing exactly one line (no leading `v`, trailing newline):

```
0.8.0
```

- [ ] **Step 2: Verify it reads cleanly**

Run: `cat CLI_VERSION`
Expected: `0.8.0`

Run: `test "$(cat CLI_VERSION)" = "0.8.0" && echo OK`
Expected: `OK`

- [ ] **Step 3: Commit**

```bash
git add CLI_VERSION
git commit -m "Add CLI_VERSION file (source of truth for CLI version)"
```

---

## Task 2: Make `make build` read `CLI_VERSION`

**Files:**
- Modify: `Makefile` (version block, lines 2–14)

- [ ] **Step 1: Replace the version-derivation block**

Replace this exact block in `Makefile` (currently lines 2–14):

```makefile
# Version metadata embedded via -ldflags. `version` falls back to the short
# git describe (tag + offset + SHA) so dev builds are self-identifying;
# release builds get the clean tag from goreleaser. `commit` and `buildDate`
# are always from git / UTC now.
# Match only CLI release tags (v*) so a CDK tag (cdk-v*) can't masquerade as the
# CLI version — the 'v*' glob anchors at the start, so it never matches cdk-v*
# (which starts with 'c'). Falls back to a bare commit SHA (via --always) before
# the first v* tag, and to "dev" only outside a git repo.
VERSION    := $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)
```

with:

```makefile
# Version metadata embedded via -ldflags. `version` is read from the CLI_VERSION
# file (the source of truth; the same value tag-on-cli-bump.yml turns into a
# v<version> release tag) and stamped as v<CLI_VERSION>, with a -g<sha>[-dirty]
# suffix so dev builds stay self-identifying. Release binaries are built by
# GoReleaser, which reads the clean v<version> from the pushed tag — not this
# Makefile — so the released binary is stamped v<version> with no suffix.
# `commit` and `buildDate` are always from git / UTC now.
CLI_VERSION_BASE := $(shell cat CLI_VERSION 2>/dev/null || echo 0.0.0)
GIT_SHA          := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
GIT_DIRTY        := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo '-dirty')
VERSION          := v$(CLI_VERSION_BASE)-g$(GIT_SHA)$(GIT_DIRTY)
COMMIT           := $(GIT_SHA)
BUILD_DATE       := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS          := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)
```

- [ ] **Step 2: Verify `make build` stamps the CLI_VERSION-derived string**

Run: `make build && ./horde version`
Expected: a line starting with `horde version v0.8.0-g` (clean tree may or may not show `-dirty` depending on uncommitted plan files; the `v0.8.0-g<sha>` prefix is the assertion).

Run: `make build 2>&1 | grep -o "main.version=v0.8.0-g[a-f0-9]*"`
Expected: prints `main.version=v0.8.0-g<sha>` (non-empty).

- [ ] **Step 3: Verify the dirty marker works**

Run: `touch /tmp/throwaway_dirty_check && echo "x" >> README.md && make build 2>&1 | grep -o 'main.version=v0.8.0-g[a-f0-9]*-dirty' ; git checkout README.md`
Expected: prints `main.version=v0.8.0-g<sha>-dirty` (proves `-dirty` appends), then reverts README.md.

- [ ] **Step 4: Commit**

```bash
git add Makefile
git commit -m "make build: derive version from CLI_VERSION file"
```

---

## Task 3: Update the release-wiring contract test

**Files:**
- Modify: `cmd/horde/release_wiring_test.go`

This task has two parts: (a) replace `TestMakefileDescribesCLIPrefix` whose `--match 'v*'` premise is gone, and (b) extend `workflowTriggers` + add a guard for the new workflow. Because the new workflow triggers on `branches`/`paths` (not `tags`), the struct must decode those too.

- [ ] **Step 1: Run the existing test to confirm it currently passes (baseline) — then see it break after the Makefile change**

Run: `cd /home/jb/work/horde/.claude/worktrees/cli-version-file && go test ./cmd/horde/ -run 'TestMakefileDescribesCLIPrefix|TestPublishWorkflowUsesCDKPrefix|TestReleaseCLIWorkflowUsesVPrefix' -v`
Expected: `TestMakefileDescribesCLIPrefix` **FAILS** now (Task 2 removed `--match 'v*'` from the Makefile), the other two PASS. This failure is expected and is what we fix next.

- [ ] **Step 2: Extend the `workflowTriggers` struct to decode branches + paths**

Replace this exact struct in `cmd/horde/release_wiring_test.go`:

```go
type workflowTriggers struct {
	On struct {
		Push struct {
			Tags []string `yaml:"tags"`
		} `yaml:"push"`
	} `yaml:"on"`
}
```

with:

```go
type workflowTriggers struct {
	On struct {
		Push struct {
			Tags     []string `yaml:"tags"`
			Branches []string `yaml:"branches"`
			Paths    []string `yaml:"paths"`
		} `yaml:"push"`
	} `yaml:"on"`
}
```

- [ ] **Step 3: Replace `readWorkflowTags` premise is fine; add a sibling reader for the full push trigger**

Immediately AFTER the existing `readWorkflowTags` function, add:

```go
// readWorkflowPush decodes a workflow's full push trigger (tags, branches,
// paths) so tests can assert path-filtered branch triggers, not just tag globs.
func readWorkflowPush(t *testing.T, path string) (tags, branches, paths []string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var w workflowTriggers
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatalf("unmarshalling %s: %v", path, err)
	}
	return w.On.Push.Tags, w.On.Push.Branches, w.On.Push.Paths
}
```

- [ ] **Step 4: Replace `TestMakefileDescribesCLIPrefix` with a CLI_VERSION-based assertion**

Replace this exact function:

```go
func TestMakefileDescribesCLIPrefix(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("reading Makefile: %v", err)
	}
	if !strings.Contains(string(b), "--match 'v*'") {
		t.Error("Makefile VERSION must describe against v* tags only")
	}
}
```

with:

```go
// The CLI version is sourced from the CLI_VERSION file (not git tags), and the
// Makefile stamps it as a v-prefixed string so the CLI version stays distinct
// from the cdk-v* prefix. Guards both halves of that contract.
func TestMakefileReadsCLIVersionFile(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("reading Makefile: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "cat CLI_VERSION") {
		t.Error("Makefile VERSION must read the CLI_VERSION file")
	}
	// Whitespace-insensitive: the VERSION assignment must stamp a v-prefixed
	// string built from CLI_VERSION_BASE (keeps the CLI version distinct from
	// the cdk-v* prefix). Matches `VERSION := v$(CLI_VERSION_BASE)...` with any
	// run of spaces around `:=`.
	if !regexp.MustCompile(`VERSION\s*:=\s*v\$\(CLI_VERSION_BASE\)`).MatchString(src) {
		t.Error("Makefile must stamp a v-prefixed version from CLI_VERSION (keeps CLI distinct from cdk-v*)")
	}
}

// The CLI_VERSION file is the source of truth tag-on-cli-bump.yml turns into a
// v<version> tag; it must contain a bare semver with no leading v.
func TestCLIVersionFileIsBareSemver(t *testing.T) {
	b, err := os.ReadFile("../../CLI_VERSION")
	if err != nil {
		t.Fatalf("reading CLI_VERSION: %v", err)
	}
	v := strings.TrimSpace(string(b))
	if strings.HasPrefix(v, "v") {
		t.Errorf("CLI_VERSION must be bare semver with no leading 'v'; got %q", v)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v) {
		t.Errorf("CLI_VERSION must be MAJOR.MINOR.PATCH; got %q", v)
	}
}

// tag-on-cli-bump.yml is the CLI analog of tag-on-bump.yml: it must watch the
// CLI_VERSION file on main so a bump auto-pushes the v<version> release tag.
func TestTagOnCLIBumpWatchesVersionFile(t *testing.T) {
	_, branches, paths := readWorkflowPush(t, "../../.github/workflows/tag-on-cli-bump.yml")
	if len(branches) != 1 || branches[0] != "main" {
		t.Errorf("tag-on-cli-bump.yml must trigger on push to [main]; got branches=%#v", branches)
	}
	foundPath := false
	for _, p := range paths {
		if p == "CLI_VERSION" {
			foundPath = true
		}
	}
	if !foundPath {
		t.Errorf("tag-on-cli-bump.yml must filter on the CLI_VERSION path; got paths=%#v", paths)
	}
}
```

- [ ] **Step 5: Add the `regexp` import**

In the import block of `cmd/horde/release_wiring_test.go`, add `"regexp"`. The block becomes:

```go
import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)
```

- [ ] **Step 6: Run the tests — TagOnCLIBump test will FAIL until Task 4 creates the workflow**

Run: `go test ./cmd/horde/ -run 'TestMakefileReadsCLIVersionFile|TestCLIVersionFileIsBareSemver|TestPublishWorkflowUsesCDKPrefix|TestReleaseCLIWorkflowUsesVPrefix' -v`
Expected: all four PASS (these don't depend on the new workflow).

Run: `go test ./cmd/horde/ -run TestTagOnCLIBumpWatchesVersionFile -v`
Expected: FAIL — `reading ../../.github/workflows/tag-on-cli-bump.yml: ... no such file`. This is expected; Task 4 creates the file.

- [ ] **Step 7: Commit (test + the regexp import; the new test is red pending Task 4)**

```bash
git add cmd/horde/release_wiring_test.go
git commit -m "release_wiring_test: assert CLI version sources CLI_VERSION + guard new workflow"
```

---

## Task 4: Add the `tag-on-cli-bump.yml` workflow

**Files:**
- Create: `.github/workflows/tag-on-cli-bump.yml`

- [ ] **Step 1: Create the workflow (CLI analog of tag-on-bump.yml)**

Create `.github/workflows/tag-on-cli-bump.yml` with exactly:

```yaml
name: tag-on-cli-bump

# When a push to main changes the CLI_VERSION file, create and push a matching
# v<version> tag. That tag push is what drives release-cli.yml (GoReleaser).
# Mirror of tag-on-bump.yml (which does the same for cdk/package.json -> cdk-v*).
# Without this, a CLI version bump can merge and sit unreleased.
#
# The tag MUST be pushed with a PAT (secret RELEASE_TAG_PAT), not the default
# GITHUB_TOKEN: GitHub deliberately suppresses workflow triggers for refs pushed
# by GITHUB_TOKEN, so a token-pushed tag would never fire release-cli.yml.

on:
  push:
    branches: [main]
    paths:
      - "CLI_VERSION"

concurrency:
  group: tag-on-cli-bump
  cancel-in-progress: false

jobs:
  tag-on-cli-bump:
    name: tag-on-cli-bump
    runs-on: ubuntu-latest
    permissions:
      contents: read # tag push is authed via the PAT, not GITHUB_TOKEN
    steps:
      # fetch-depth 2 so HEAD~1 (the pre-merge parent) is available for the
      # version diff. Squash-merge to main means HEAD~1 is the correct "before".
      - uses: actions/checkout@v6
        with:
          fetch-depth: 2
          token: ${{ secrets.RELEASE_TAG_PAT }}

      - id: bump
        name: Detect version bump
        run: |
          new=$(tr -d '[:space:]' < CLI_VERSION)
          if git cat-file -e HEAD~1:CLI_VERSION 2>/dev/null; then
            old=$(git show HEAD~1:CLI_VERSION | tr -d '[:space:]')
          else
            old=""
          fi
          echo "old=$old new=$new"
          if [ "$old" = "$new" ]; then
            echo "version unchanged ($new) — nothing to tag."
            echo "bumped=false" >> "$GITHUB_OUTPUT"
          else
            echo "bumped=true" >> "$GITHUB_OUTPUT"
            echo "version=$new" >> "$GITHUB_OUTPUT"
          fi

      - name: Create and push tag
        if: steps.bump.outputs.bumped == 'true'
        env:
          VERSION: ${{ steps.bump.outputs.version }}
        run: |
          tag="v$VERSION"
          if git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null 2>&1; then
            echo "Tag $tag already exists on origin — skipping."
            exit 0
          fi
          git config user.name "github-actions[bot]"
          git config user.email "github-actions[bot]@users.noreply.github.com"
          git tag -a "$tag" -m "Release horde CLI $VERSION"
          git push origin "$tag"
          echo "Pushed $tag — release-cli.yml will build the CLI release."
```

- [ ] **Step 2: Verify the new workflow guard test now passes**

Run: `cd /home/jb/work/horde/.claude/worktrees/cli-version-file && go test ./cmd/horde/ -run TestTagOnCLIBumpWatchesVersionFile -v`
Expected: PASS.

- [ ] **Step 3: Verify the whole release_wiring_test suite is green**

Run: `go test ./cmd/horde/ -run 'Workflow|CLIVersion|CLIBump|Makefile|Prefix' -v`
Expected: all PASS (TestPublishWorkflowUsesCDKPrefix, TestReleaseCLIWorkflowUsesVPrefix, TestMakefileReadsCLIVersionFile, TestCLIVersionFileIsBareSemver, TestTagOnCLIBumpWatchesVersionFile).

- [ ] **Step 4: Lint the YAML parses (sanity)**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/tag-on-cli-bump.yml')); print('YAML OK')"`
Expected: `YAML OK`

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/tag-on-cli-bump.yml
git commit -m "ci: tag-on-cli-bump workflow auto-pushes v<version> from CLI_VERSION"
```

---

## Task 5: Update CLAUDE.md release docs

**Files:**
- Modify: `CLAUDE.md` (the release-scheme bullet that says how to bump the CLI)

- [ ] **Step 1: Find the current CLI-bump line**

Run: `grep -n "To bump the CLI" CLAUDE.md`
Expected: one match in the "Releases & tag scheme" bullet (a `- **To bump the CDK:** … **To bump the CLI:** …` line).

- [ ] **Step 2: Read the surrounding bullet for exact text**

Run: `grep -n "To bump the CDK" CLAUDE.md`
Then read that line in full (it contains both the CDK and CLI bump instructions in one bullet).

- [ ] **Step 3: Replace the CLI-bump clause**

In that bullet, replace the CLI half — the sentence currently reading approximately:

```
**To bump the CLI:** no file edit is needed for the version (GoReleaser/`make build` derive it from `git describe --match 'v*'`); the release is the human pushing the plain `v<version>` tag against the merged commit on `main`.
```

with:

```
**To bump the CLI:** edit the root `CLI_VERSION` file (one line, bare semver, no leading `v`), commit as `Release <version>`. On merge to `main`, `.github/workflows/tag-on-cli-bump.yml` auto-pushes `v<version>` → `release-cli.yml` runs GoReleaser. `make build` stamps the version from `CLI_VERSION` (suffixed `-g<sha>[-dirty]` for dev builds); the released binary's clean `v<version>` comes from GoReleaser reading the tag. No hand-pushed tag.
```

If the surrounding text still asserts the CLI version comes from `git describe`, update that phrasing too (search `git describe --match 'v*'` and reconcile — the Makefile no longer uses it).

- [ ] **Step 4: Verify no stale `git describe --match 'v*'` claim remains in CLAUDE.md**

Run: `grep -n "git describe --match 'v\*'" CLAUDE.md || echo "clean"`
Expected: `clean` (or, if a match remains, it must be in a context that's still accurate — reconcile it).

- [ ] **Step 5: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: CLAUDE.md — CLI bump is now editing CLI_VERSION"
```

---

## Task 6: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Build + vet + unit-test green**

Run: `make build && make vet && make unit-test 2>&1 | tail -12`
Expected: build succeeds, vet clean, every package `ok` (0 failures) — including `cmd/horde`.

- [ ] **Step 2: Confirm version stamp**

Run: `./horde version`
Expected: `horde version v0.8.0-g<sha>...` (CLI_VERSION-derived).

- [ ] **Step 3: Confirm the three release-wiring contract tests all pass together**

Run: `go test ./cmd/horde/ -run 'Prefix|CLIVersion|CLIBump|Makefile' -v`
Expected: all PASS.

- [ ] **Step 4: Sanity-check no Makefile target broke**

Run: `make -n build && make -n install && echo "targets parse"`
Expected: both expand without error, then `targets parse`.

---

## Task 7: Release bump (dog-food the new mechanism)

**Files:**
- Modify: `CLI_VERSION`

Per the design decision: bump in THIS PR so merging it exercises the new auto-release path on first use.

- [ ] **Step 1: Bump CLI_VERSION 0.8.0 → 0.9.0**

Replace the contents of `CLI_VERSION` with exactly:

```
0.9.0
```

- [ ] **Step 2: Verify build stamps the new version**

Run: `make build && make build 2>&1 | grep -o 'main.version=v0.9.0-g[a-f0-9]*'`
Expected: prints `main.version=v0.9.0-g<sha>`.

- [ ] **Step 3: Re-run the bare-semver guard test**

Run: `go test ./cmd/horde/ -run TestCLIVersionFileIsBareSemver -v`
Expected: PASS (0.9.0 is valid bare semver).

- [ ] **Step 4: Commit as the Release commit**

```bash
git add CLI_VERSION
git commit -m "Release 0.9.0"
```

---

## Notes for the implementer

- **Why HEAD~1 in the workflow:** main uses squash-merge, so the merged commit's parent (`HEAD~1`) is the pre-merge state — the correct "before" for the version diff. `fetch-depth: 2` makes it available.
- **Why `RELEASE_TAG_PAT` not `GITHUB_TOKEN`:** GitHub suppresses workflow triggers for tags pushed by `GITHUB_TOKEN`. The CDK side already relies on this secret existing; the CLI workflow reuses it.
- **GoReleaser is untouched:** it still triggers on `v*` and versions from the tag. Do not edit `.goreleaser.yaml` or `release-cli.yml`.
- **The released binary is clean (`v0.9.0`), not `-g<sha>`:** because GoReleaser builds it from the tag, not via `make build`. The `-g<sha>[-dirty]` suffix only appears on local/dev `make build` output.
- **CI for THIS PR:** the `unit-test` job runs `go test ./cmd/...` which includes the new contract tests; they must pass. `cdk-test` and `integration-test` are unaffected (no cdk/ or provider changes).
```
