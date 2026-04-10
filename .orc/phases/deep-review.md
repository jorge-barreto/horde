You are orchestrating a thorough final review for **horde** using an adaptive panel of domain-specific expert subagents.

Your job is to catch issues that the per-bead quick reviews missed — correctness problems, edge cases, test gaps, convention violations, and scope creep across the full changeset for this work item.

## Step 0: Determine Context

```bash
rm -f "$ARTIFACTS_DIR/deep-review-pass.txt"
WORK_ITEM=$(cat "$ARTIFACTS_DIR/current-ticket.txt" 2>/dev/null || echo "$TICKET")
BASE=$(cat "$ARTIFACTS_DIR/base-commit.txt" 2>/dev/null || echo "main")
echo "Reviewing: $WORK_ITEM (base: $BASE)"
```

Use `$BASE..HEAD` for all git diffs below (not `main..HEAD`). The base commit was recorded when work started on this item.

## Step 1: Assess Change Scope

```bash
CHANGED_FILES=$(git diff --name-only $BASE..HEAD | wc -l)
CHANGED_LINES=$(git diff $BASE..HEAD --numstat | awk '{s+=$1+$2} END {print s+0}')
echo "Files: $CHANGED_FILES, Lines: $CHANGED_LINES"
git diff --name-only $BASE..HEAD
```

Categorize changed files by package:
```bash
PROVIDER_FILES=$(git diff --name-only $BASE..HEAD | grep -c 'internal/provider/' || echo 0)
STORE_FILES=$(git diff --name-only $BASE..HEAD | grep -c 'internal/store/' || echo 0)
CONFIG_FILES=$(git diff --name-only $BASE..HEAD | grep -c 'internal/config/' || echo 0)
RUNID_FILES=$(git diff --name-only $BASE..HEAD | grep -c 'internal/runid/' || echo 0)
CMD_FILES=$(git diff --name-only $BASE..HEAD | grep -c 'cmd/horde/' || echo 0)
CDK_FILES=$(git diff --name-only $BASE..HEAD | grep -c 'cdk/' || echo 0)
DOCKER_FILES=$(git diff --name-only $BASE..HEAD | grep -c 'docker/' || echo 0)
echo "Provider: $PROVIDER_FILES, Store: $STORE_FILES, Config: $CONFIG_FILES, RunID: $RUNID_FILES, Cmd: $CMD_FILES, CDK: $CDK_FILES, Docker: $DOCKER_FILES"
```

Read `$ARTIFACTS_DIR/plan.md` for the overall plan context.

## Step 2: Run Verification

```bash
cd $PROJECT_ROOT && make build && go vet ./...
cd $PROJECT_ROOT && make test
```

If tests fail, note the failures — they become automatic BLOCKING findings.

## Step 3: Determine Tier and Select Experts

### Tier Rules

**Tier 1 — Small change (< 100 lines, < 5 files):**
Launch 2-3 experts: only the package-relevant domain experts + Test Coverage.

**Tier 2 — Medium change (100-500 lines, 5-15 files):**
Launch 3-5 experts: all affected package experts + Test Coverage.

**Tier 3 — Large change (> 500 lines or > 15 files):**
Launch all 7 experts: full panel.

### Expert Selection Rules

- Provider changes (PROVIDER_FILES > 0) → **E1: Provider Interface & Implementations**
- Store changes (STORE_FILES > 0) → **E2: Store & Data Persistence**
- Config changes (CONFIG_FILES > 0) → **E3: Config & SSM Discovery**
- Cmd changes (CMD_FILES > 0) → **E4: CLI & Command Wiring**
- CDK or Docker changes (CDK_FILES > 0 or DOCKER_FILES > 0) → **E5: Infrastructure (CDK & Docker)**
- Any Go files changed → **E6: Test Coverage** (always included)
- Tier 3 → launch ALL experts regardless of package detection

**Never launch domain experts (E1-E5) for packages with zero changed files** (unless Tier 3).

## Step 4: Launch Selected Experts

Launch the selected expert subagents **in parallel** using the Agent tool with `model: "opus"`. Each expert receives the changed files list and writes findings to its designated output file.

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

> You are reviewing Go code changes in **horde**'s provider interface and implementations.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
> Read `$PROJECT_ROOT/SPEC.md` for the Provider interface specification.
>
> Changed files: {provider files from Step 1, plus any files they import}
>
> For each changed file, read the FULL file, then run `git diff $BASE..HEAD -- <file>`.
>
> Your domain-specific checklist:
> - **Interface compliance:** Do all provider implementations (local, docker, ecs) satisfy the Provider interface (Launch, Status, Logs, Kill)?
> - **LaunchResult completeness:** Does Launch() populate all required fields in LaunchResult (InstanceID, plus ClusterARN/LogGroup/ArtifactsBucket for ECS)?
> - **Context propagation:** Is `ctx context.Context` passed through and respected? Are long-running operations cancellable?
> - **Resource cleanup:** Does the local provider clean up temp directories on failure? Does docker provider remove containers?
> - **ECS Lambda invocation:** Is the launcher Lambda invoked correctly? Are the payload fields complete (repo, ticket, branch, workflow, runID)?
> - **Error wrapping:** Are all errors wrapped with `%w` and context?
> - **Logs streaming:** Does Logs() return a proper io.ReadCloser? Is follow mode handled correctly?
> - **Kill safety:** Does Kill() handle already-stopped instances gracefully?
>
> Write findings to `$ARTIFACTS_DIR/reviews/provider.md`.
> Only flag issues in code introduced by this diff.

---

### E2: Store & Data Persistence

**Output:** `$ARTIFACTS_DIR/reviews/store.md`

> You are reviewing Go code changes in **horde**'s store interface and SQLite implementation.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
> Read `$PROJECT_ROOT/SPEC.md` for the data model specification.
>
> Changed files: {store files from Step 1}
>
> For each changed file, read the FULL file, then run `git diff $BASE..HEAD -- <file>`.
>
> Your domain-specific checklist:
> - **SQL injection:** Are ALL queries parameterized? No string interpolation in SQL.
> - **Schema correctness:** Does the SQLite schema match SPEC.md's data model (id, repo, ticket, workflow, provider, instance_id, status, exit_code, cost_usd, started_at, completed_at, cluster_arn, log_group, artifacts_bucket, artifacts_uri)?
> - **Status transitions:** Are status values limited to: pending, running, success, failed, killed? Are invalid transitions prevented?
> - **Nullable handling:** Are nullable columns (exit_code, cost_usd, completed_at, cluster_arn, log_group, artifacts_bucket, artifacts_uri) handled with sql.NullXxx types or pointer types?
> - **Connection lifecycle:** Is the database connection opened and closed correctly? Is there a Close() method?
> - **Interface compliance:** Does the SQLite implementation satisfy the Store interface?
> - **Error wrapping:** Are all errors wrapped with `%w`?
> - **RFC3339 dates:** Are timestamps stored as RFC3339 strings?
>
> Write findings to `$ARTIFACTS_DIR/reviews/store.md`.
> Only flag issues in code introduced by this diff.

---

### E3: Config & SSM Discovery

**Output:** `$ARTIFACTS_DIR/reviews/config.md`

> You are reviewing Go code changes in **horde**'s configuration loading and SSM discovery.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
> Read `$PROJECT_ROOT/SPEC.md` for the configuration specification.
>
> Changed files: {config files from Step 1}
>
> For each changed file, read the FULL file, then run `git diff $BASE..HEAD -- <file>`.
>
> Your domain-specific checklist:
> - **YAML parsing:** Does the config struct match the YAML schema from SPEC.md? Are unknown fields rejected or ignored consistently?
> - **SSM discovery:** Is the SSM parameter path configurable (default `/horde/launcher`)? Is the AWS credential chain used correctly?
> - **Profile passthrough:** Does `--profile` flow through to the AWS SDK credential chain?
> - **Project dir discovery:** Does the config loader walk up from cwd to find `.horde/config.yaml`? Does it handle missing config gracefully (config is optional)?
> - **Provider validation:** Is the provider field validated against known providers (local, docker, aws-ecs)?
> - **Default values:** Are defaults applied correctly (provider, max-parallel, branch)?
> - **Error wrapping:** Are all errors wrapped with `%w`?
>
> Write findings to `$ARTIFACTS_DIR/reviews/config.md`.
> Only flag issues in code introduced by this diff.

---

### E4: CLI & Command Wiring

**Output:** `$ARTIFACTS_DIR/reviews/cli.md`

> You are reviewing Go code changes in **horde**'s CLI layer.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
> Read `$PROJECT_ROOT/SPEC.md` for CLI command specifications.
>
> Changed files: {cmd files from Step 1}
>
> For each changed file, read the FULL file, then run `git diff $BASE..HEAD -- <file>`.
>
> Your domain-specific checklist:
> - **urfave/cli/v3 patterns:** Are commands structured correctly with Action, Flags, Usage?
> - **Flag parsing:** Are global flags (--profile, --config) accessible from subcommands?
> - **Provider initialization:** Is the correct provider instantiated based on config?
> - **Error presentation:** Are errors presented to users clearly (not raw Go errors)?
> - **Output formatting:** Is output consistent across commands (status, logs, list)?
> - **Run ID handling:** Are run IDs validated before use?
> - **Store lifecycle:** Is the store opened before use and closed on exit?
> - **Signal handling:** Does the CLI handle SIGINT/SIGTERM gracefully?
>
> Write findings to `$ARTIFACTS_DIR/reviews/cli.md`.
> Only flag issues in code introduced by this diff.

---

### E5: Infrastructure (CDK & Docker)

**Output:** `$ARTIFACTS_DIR/reviews/infra.md`

> You are reviewing infrastructure changes in **horde**'s CDK construct and Docker worker image.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
> Read `$PROJECT_ROOT/SPEC.md` for infrastructure specifications.
> Read `$PROJECT_ROOT/ORC_CONTRACT_EXPECTATIONS.md` for the orc interface contract.
>
> Changed files: {cdk and docker files from Step 1}
>
> For each changed file, read the FULL file, then run `git diff $BASE..HEAD -- <file>`.
>
> Your domain-specific checklist:
> - **CDK construct:** Does HordeWorker create all required resources (cluster, task def, secrets, IAM roles, log group, S3 bucket, security group, launcher Lambda, SSM parameter)?
> - **IAM scoping:** Is the task role scoped to only S3 write + Secrets Manager read? Is the execution role scoped to ECR pull + CloudWatch Logs write?
> - **Secrets injection:** Are secrets injected via `valueFrom` (Secrets Manager ARN), NOT plain environment variables?
> - **SSM parameter:** Is the launcher Lambda ARN published to the correct SSM path (`/horde/launcher`)?
> - **Dockerfile:** Does the base image include git, jq, bash, curl, AWS CLI v2, orc, claude CLI, bd?
> - **Entrypoint robustness:** Does the entrypoint capture orc's exit code without `set -e` killing the script? Are artifact uploads best-effort (|| true)?
> - **Environment contract:** Does the worker environment NOT set CLAUDECODE? Does it set ANTHROPIC_API_KEY and GIT_TOKEN?
>
> Write findings to `$ARTIFACTS_DIR/reviews/infra.md`.
> Only flag issues in code introduced by this diff.

---

### E6: Test Coverage

**Output:** `$ARTIFACTS_DIR/reviews/test-coverage.md`

> You are reviewing Go test coverage for changes in the **horde** project.
>
> Read `$PROJECT_ROOT/CLAUDE.md` for project conventions.
>
> Changed files: {all changed Go files}
>
> For each changed `.go` file, read the full file and its corresponding `_test.go` file.
>
> Your checklist:
> - Every new exported function has a test?
> - Tests follow existing patterns in the package's `_test.go` files (table-driven tests, `t.TempDir()` for temp dirs)?
> - Edge cases covered: empty input, nil, zero, missing files, error paths?
> - Tests verify behavior, not just "runs without panic"?
> - Test names are descriptive and follow Go conventions (`TestFunctionName_Scenario`)?
> - For provider tests: are mock/fake implementations used correctly?
> - For store tests: is an in-memory or temp-file SQLite used?
> - For config tests: are validation error messages checked (not just error presence)?
> - Are there untested error paths or concurrent code paths in the changed code? (Only flag gaps where the absence of a test could let a crash, data race, or silent wrong result ship — do not flag missing tests for rendering, formatting, or simple deterministic functions.)
>
> Write findings to `$ARTIFACTS_DIR/reviews/test-coverage.md`.
> Only flag issues in test code introduced by this diff.

---

## Step 5: Synthesize Results

After all subagents complete:

1. Read all report files from `$ARTIFACTS_DIR/reviews/`
2. Compile all BLOCKING findings
3. Compile all WARNING findings
4. Deduplicate: if 2+ experts flag the same issue, note it as "high confidence"

## Step 6: Acceptance Criteria Check

Read `$ARTIFACTS_DIR/plan.md` and check each acceptance criterion:
- Can it be verified by reading the code or running a command?
- Is the criterion met?

## Step 7: Write Verdict

Write your full review to `$ARTIFACTS_DIR/review-findings.md`:

```markdown
# Deep Review: $WORK_ITEM

**Tier:** {1|2|3}
**Experts launched:** {count} ({comma-separated list of expert names})

## Blocking Issues
{numbered list with [file:line] and flagging expert, or "None."}

## Warnings
{numbered list with [file:line] and flagging expert, or "None."}

## Acceptance Criteria Check

- [x] Criterion 1 — verified by: how you verified
- [ ] Criterion 2 — NOT MET: explanation

## Verdict

**PASS** or **FAIL**
```

## Step 8: Pass/Fail Decision

**If zero blocking issues AND all acceptance criteria met:**
```bash
echo "PASS" > "$ARTIFACTS_DIR/deep-review-pass.txt"
```

**If any blocking issues exist:**
Do NOT create `deep-review-pass.txt`. The blocking issues in `review-findings.md` are sufficient — the orchestrator will loop back to plan.

## Rules

- **Only flag issues introduced by this ticket's diff.** Pre-existing issues are out of scope.
- **Never launch experts for packages with zero changed files** (unless Tier 3).
- Be specific. Cite file paths and line numbers.
- Every blocking issue needs a suggested fix.
- Launch all selected experts in parallel for speed.
