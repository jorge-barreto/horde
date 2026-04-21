You are orchestrating a holistic review of all changes made during wave **$TICKET** using an adaptive panel of domain-specific expert subagents.

All children of this wave have been implemented individually, each with its own per-item review. Your job is to catch cross-cutting issues that per-item reviews missed — integration gaps, consistency problems, and systemic quality concerns across the full wave's changeset.

## Step 1: Understand the Wave

1. Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
2. Read the wave epic for context:
   ```bash
   bd show $TICKET
   ```
3. Determine the wave base commit and assess the full changeset:
   ```bash
   WAVE_BASE=$(cat "$ARTIFACTS_DIR/wave-base-commit.txt" 2>/dev/null || echo "main")
   echo "Wave base: $WAVE_BASE"
   git log --oneline $WAVE_BASE..HEAD
   git diff --stat $WAVE_BASE..HEAD
   CHANGED_FILES=$(git diff --name-only $WAVE_BASE..HEAD | wc -l)
   CHANGED_LINES=$(git diff $WAVE_BASE..HEAD --numstat | awk '{s+=$1+$2} END {print s+0}')
   echo "Files: $CHANGED_FILES, Lines: $CHANGED_LINES"
   git diff --name-only $WAVE_BASE..HEAD
   ```

   Use `$WAVE_BASE..HEAD` for all git diffs below (not `main..HEAD`). The wave base was recorded when the first item in this wave was claimed.

Categorize changed files by package:
```bash
PROVIDER_FILES=$(git diff --name-only $WAVE_BASE..HEAD | grep -c 'internal/provider/' || echo 0)
STORE_FILES=$(git diff --name-only $WAVE_BASE..HEAD | grep -c 'internal/store/' || echo 0)
CONFIG_FILES=$(git diff --name-only $WAVE_BASE..HEAD | grep -c 'internal/config/' || echo 0)
RUNID_FILES=$(git diff --name-only $WAVE_BASE..HEAD | grep -c 'internal/runid/' || echo 0)
CMD_FILES=$(git diff --name-only $WAVE_BASE..HEAD | grep -c 'cmd/horde/' || echo 0)
CDK_FILES=$(git diff --name-only $WAVE_BASE..HEAD | grep -c 'cdk/' || echo 0)
DOCKER_FILES=$(git diff --name-only $WAVE_BASE..HEAD | grep -c 'docker/' || echo 0)
echo "Provider: $PROVIDER_FILES, Store: $STORE_FILES, Config: $CONFIG_FILES, RunID: $RUNID_FILES, Cmd: $CMD_FILES, CDK: $CDK_FILES, Docker: $DOCKER_FILES"
```

## Step 2: Run Verification

```bash
cd $PROJECT_ROOT && make build && make vet
cd $PROJECT_ROOT && make unit-test
```

If tests fail, note the failures — they become automatic BLOCKING findings.

## Step 3: Determine Tier and Select Experts

### Tier Rules

**Tier 1 — Small wave (< 200 lines, < 10 files):**
Launch 3-4 experts: package-relevant domain experts + Test Coverage + Integration.

**Tier 2 — Medium wave (200-1000 lines, 10-30 files):**
Launch 5-6 experts: all affected package experts + Test Coverage + Integration.

**Tier 3 — Large wave (> 1000 lines or > 30 files):**
Launch all 7 experts: full panel.

### Expert Selection Rules

- Provider changes (PROVIDER_FILES > 0) → **E1: Provider Interface & Implementations**
- Store changes (STORE_FILES > 0) → **E2: Store & Data Persistence**
- Config changes (CONFIG_FILES > 0) → **E3: Config & SSM Discovery**
- Cmd changes (CMD_FILES > 0) → **E4: CLI & Command Wiring**
- CDK or Docker changes (CDK_FILES > 0 or DOCKER_FILES > 0) → **E5: Infrastructure (CDK & Docker)**
- Any Go files changed → **E6: Test Coverage** (always included)
- **E7: Cross-Item Integration** → ALWAYS included in wave review (this is what distinguishes wave-review from deep-review)
- Tier 3 → launch ALL experts regardless of package detection

**Never launch domain experts (E1-E5) for packages with zero changed files** (unless Tier 3).

## Step 4: Launch Selected Experts

Launch the selected expert subagents **in parallel** using the Agent tool with `model: "opus"`. Each expert writes findings to its designated output file.

Create the reviews directory first:
```bash
mkdir -p "$ARTIFACTS_DIR/reviews"
```

### Finding Format (all experts)

```
# {Expert Name} Review

## BLOCKING
- [file:line] Description. Why it must be fixed.

## WARNING
- [file:line] Description. Not blocking but should be considered.

## NOTE
- [file:line] Informational observation.
```

If no findings in a category, write `None.`

---

### E1: Provider Interface & Implementations

**Output:** `$ARTIFACTS_DIR/reviews/provider.md`

> You are reviewing Go code changes in the **horde** project's provider interface and implementations as part of a wave-level review.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
> Read `$PROJECT_ROOT/SPEC.md` for the Provider interface specification.
>
> Changed files: {provider files from Step 1, plus any files they import}
>
> For each changed file, read the FULL file (not just the diff), then run `git diff $WAVE_BASE..HEAD -- <file>`.
>
> Your domain-specific checklist:
> - **Interface compliance:** Do all provider implementations (local, docker, ecs) satisfy the Provider interface (Launch, Status, Logs, Kill)?
> - **LaunchResult completeness:** Does Launch() populate all required fields in LaunchResult?
> - **Context propagation:** Is `ctx context.Context` passed through and respected?
> - **Resource cleanup:** Does the local provider clean up temp directories on failure? Does docker provider remove containers?
> - **ECS Lambda invocation:** Is the launcher Lambda invoked correctly?
> - **Error wrapping:** Are all errors wrapped with `%w` and context?
> - **Logs streaming:** Does Logs() return a proper io.ReadCloser? Is follow mode handled?
> - **Kill safety:** Does Kill() handle already-stopped instances gracefully?
>
> Write findings to `$ARTIFACTS_DIR/reviews/provider.md`.
> Only flag issues in code introduced by this wave's diff.

---

### E2: Store & Data Persistence

**Output:** `$ARTIFACTS_DIR/reviews/store.md`

> You are reviewing Go code changes in **horde**'s store interface and SQLite implementation as part of a wave-level review.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
> Read `$PROJECT_ROOT/SPEC.md` for the data model specification.
>
> Changed files: {store files from Step 1}
>
> For each changed file, read the FULL file (not just the diff), then run `git diff $WAVE_BASE..HEAD -- <file>`.
>
> Your domain-specific checklist:
> - **SQL injection:** Are ALL queries parameterized? No string interpolation in SQL.
> - **Schema correctness:** Does the SQLite schema match SPEC.md's data model?
> - **Status transitions:** Are status values limited to: pending, running, success, failed, killed?
> - **Nullable handling:** Are nullable columns handled with sql.NullXxx types or pointer types?
> - **Connection lifecycle:** Is the database connection opened and closed correctly?
> - **Interface compliance:** Does the SQLite implementation satisfy the Store interface?
> - **Error wrapping:** Are all errors wrapped with `%w`?
>
> Write findings to `$ARTIFACTS_DIR/reviews/store.md`.
> Only flag issues in code introduced by this wave's diff.

---

### E3: Config & SSM Discovery

**Output:** `$ARTIFACTS_DIR/reviews/config.md`

> You are reviewing Go code changes in **horde**'s configuration loading and SSM discovery as part of a wave-level review.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
> Read `$PROJECT_ROOT/SPEC.md` for the configuration specification.
>
> Changed files: {config files from Step 1}
>
> For each changed file, read the FULL file (not just the diff), then run `git diff $WAVE_BASE..HEAD -- <file>`.
>
> Your domain-specific checklist:
> - **YAML parsing:** Does the config struct match the YAML schema from SPEC.md?
> - **SSM discovery:** Is the SSM parameter path configurable (default `/horde/launcher`)?
> - **Profile passthrough:** Does `--profile` flow through to the AWS SDK credential chain?
> - **Project dir discovery:** Does the config loader walk up from cwd to find `.horde/config.yaml`?
> - **Provider validation:** Is the provider field validated against known providers?
> - **Default values:** Are defaults applied correctly?
> - **Error wrapping:** Are all errors wrapped with `%w`?
>
> Write findings to `$ARTIFACTS_DIR/reviews/config.md`.
> Only flag issues in code introduced by this wave's diff.

---

### E4: CLI & Command Wiring

**Output:** `$ARTIFACTS_DIR/reviews/cli.md`

> You are reviewing Go code changes in **horde**'s CLI layer as part of a wave-level review.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
> Read `$PROJECT_ROOT/SPEC.md` for CLI command specifications.
>
> Changed files: {cmd files from Step 1}
>
> For each changed file, read the FULL file (not just the diff), then run `git diff $WAVE_BASE..HEAD -- <file>`.
>
> Your domain-specific checklist:
> - **urfave/cli/v3 patterns:** Are commands structured correctly?
> - **Flag parsing:** Are global flags accessible from subcommands?
> - **Provider initialization:** Is the correct provider instantiated based on config?
> - **Error presentation:** Are errors presented to users clearly?
> - **Output formatting:** Is output consistent across commands?
> - **Run ID handling:** Are run IDs validated before use?
> - **Store lifecycle:** Is the store opened before use and closed on exit?
> - **Signal handling:** Does the CLI handle SIGINT/SIGTERM gracefully?
> - **Documentation surfaces:** If the change affects user-visible behavior (commands, flags, output format, error messages), are all doc surfaces updated (`cmd/horde/main.go` help text, `README.md`, `SPEC.md`)?
>
> Write findings to `$ARTIFACTS_DIR/reviews/cli.md`.
> Only flag issues in code introduced by this wave's diff.

---

### E5: Infrastructure (CDK & Docker)

**Output:** `$ARTIFACTS_DIR/reviews/infra.md`

> You are reviewing infrastructure changes in **horde**'s CDK construct and Docker worker image as part of a wave-level review.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
> Read `$PROJECT_ROOT/SPEC.md` for infrastructure specifications.
> Read `$PROJECT_ROOT/ORC_CONTRACT_EXPECTATIONS.md` for the orc interface contract.
>
> Changed files: {cdk and docker files from Step 1}
>
> For each changed file, read the FULL file (not just the diff), then run `git diff $WAVE_BASE..HEAD -- <file>`.
>
> Your domain-specific checklist:
> - **CDK construct:** Does HordeWorker create all required resources?
> - **IAM scoping:** Is the task role scoped to only S3 write + Secrets Manager read?
> - **Secrets injection:** Are secrets injected via `valueFrom`, NOT plain env vars?
> - **SSM parameter:** Is the launcher Lambda ARN published to the correct SSM path?
> - **Dockerfile:** Does the base image include all required tools?
> - **Entrypoint robustness:** Does the entrypoint capture orc's exit code correctly?
> - **Environment contract:** Does the worker environment NOT set CLAUDECODE?
>
> Write findings to `$ARTIFACTS_DIR/reviews/infra.md`.
> Only flag issues in code introduced by this wave's diff.

---

### E6: Test Coverage

**Output:** `$ARTIFACTS_DIR/reviews/test-coverage.md`

> You are reviewing Go test coverage for all changes in wave **$TICKET**.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
>
> Changed files: {all changed Go files}
>
> For each changed `.go` file, read the full file and its corresponding `_test.go` file.
>
> Your checklist:
> - Every new exported function has a test?
> - Tests follow existing patterns (table-driven tests, `t.TempDir()`)?
> - Edge cases covered: empty input, nil, zero, missing files, error paths?
> - Tests verify behavior, not just "runs without panic"?
> - Test names descriptive (`TestFunctionName_Scenario`)?
> - For provider tests: mock/fake implementations used correctly?
> - For store tests: in-memory or temp-file SQLite used?
> - For config tests: validation error messages checked (not just error presence)?
> - Are there untested error paths or concurrent code paths in the changed code? (Only flag gaps where the absence of a test could let a crash, data race, or silent wrong result ship — do not flag missing tests for rendering, formatting, or simple deterministic functions.)
>
> Write findings to `$ARTIFACTS_DIR/reviews/test-coverage.md`.
> Only flag issues in test code introduced by this wave's diff.

---

### E7: Cross-Item Integration (wave-review only)

**Output:** `$ARTIFACTS_DIR/reviews/integration.md`

> You are reviewing the full wave changeset for **cross-cutting integration issues** that per-item reviews could not catch.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
>
> This wave implemented multiple work items independently. Your job is to find problems that emerge from the *combination* of changes, not from any single item.
>
> Read ALL changed files in full. Run `git log --oneline $WAVE_BASE..HEAD` to see the commit history and understand which items were implemented.
>
> Your checklist:
> - **Cross-feature consistency:** Do features implemented in different items work together correctly? Are there conflicting assumptions about shared state, interfaces, or data formats?
> - **Duplicate code across items:** Was similar logic written independently in multiple items? Should it be unified into a shared helper?
> - **Convention drift:** Do changes from different items follow the same patterns? Did one item introduce a new convention that contradicts another item's approach?
> - **API surface cohesion:** If multiple items added/modified CLI commands, flags, or config fields, are they consistent in naming, behavior, and error handling?
> - **Interface compatibility:** If one item changed an interface and another item added an implementation, do they match?
> - **Import graph:** Are there new circular dependencies or unnecessary coupling between packages?
> - **Error message consistency:** Do error messages from different items follow the same format and tone?
> - **Missing integration tests:** Are there scenarios where two features interact that no individual item's tests cover?
> - **Documentation coherence:** Do the combined doc changes tell a consistent story across all three surfaces (`cmd/horde/main.go` help text, `README.md`, `SPEC.md`)? Are there contradictions between sections updated by different items? Are there user-visible changes with no corresponding doc updates?
>
> Write findings to `$ARTIFACTS_DIR/reviews/integration.md`.
> Only flag issues in code introduced by this wave's diff.

---

## Step 5: Synthesize Results

After deduplication:

1. Read all report files from `$ARTIFACTS_DIR/reviews/`
2. Compile all BLOCKING findings not already covered by existing beads → **Bugs**
3. Compile all WARNING findings not already covered → evaluate: bugs or improvements?
4. Compile all NOTE findings not already covered → **Improvements**
5. Deduplicate across experts: if 2+ experts flag the same issue, note it as "high confidence"
6. Identify test gaps from E6 and E7 not already covered

## Step 6: Deduplicate Against Existing Beads

Before writing findings, check what beads already exist so you don't report findings that are already tracked:

```bash
bd list --parent $TICKET --all          # all children of this wave (open and closed)
bd list --type orphan           # search orphan beads too
```

For each candidate finding from the expert reports, check whether an existing bead already covers the same file, same issue, or same area — even if the title or wording differs. Drop any finding that is already tracked. This is critical to prevent duplicate bead creation downstream.


## Step 7: Write Findings

Write your findings to `$ARTIFACTS_DIR/wave-review-findings.md`:

```markdown
# Wave Review: $TICKET

**Tier:** {1|2|3}
**Experts launched:** {count} ({comma-separated list of expert names})

## Bugs (must fix — create beads in current wave)

Issues that represent incorrect behavior, crashes, data loss, race conditions, or security problems.

1. **[file:line]** Description of the bug.
   **Flagged by:** {expert name(s)}
   **Impact:** What goes wrong.
   **Suggested fix:** How to fix it.

(If none: "None found.")

## Improvements (future work — create standalone beads)

Issues that represent missed opportunities, tech debt, or enhancements — NOT bugs.

1. **[file:line]** Description of the improvement.
   **Flagged by:** {expert name(s)}
   **Rationale:** Why this would be valuable.

(If none: "None.")

## Test Gaps

Scenarios that should be tested but aren't covered by any existing test.

1. Description of the untested scenario.
   **Flagged by:** {expert name(s)}

## Summary

Overall assessment of the wave's changes: quality, cohesion, remaining risk.
```

## Rules

- **Bugs vs Improvements**: A bug is something that is *wrong* — incorrect behavior, crashes, security issues, data loss, race conditions. An improvement is something that *could be better* — performance, readability, missing features, tech debt. Be precise about this distinction because it determines whether the bead goes into the current wave (bugs) or future work (improvements).
- **Only flag issues in code changed by this wave's diff.** Pre-existing issues are out of scope.
- **Never launch domain experts (E1-E5) for packages with zero changed files** (unless Tier 3).
- **Always launch E7 (Integration)** — this is the wave-review's unique value.
- Be specific — cite file paths and line numbers.
- Every bug must include a suggested fix.
- Launch all selected experts in parallel for speed.
- Focus on cross-cutting issues. Per-file bugs should have been caught by per-item deep-reviews.
