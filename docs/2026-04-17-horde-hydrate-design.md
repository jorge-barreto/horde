# horde hydrate — Design

## Problem

`orc improve` and `orc doctor` operate on a local `.orc/audit/` and `.orc/artifacts/` tree — they learn from past runs or diagnose orc configuration by reading those folders in the current working directory. When runs execute inside horde, that data lives somewhere else:

- **Docker provider:** copied into `~/.horde/results/<run-id>/{audit,artifacts}/` when the container stops.
- **ECS provider:** uploaded to `s3://<artifacts-bucket>/horde-runs/<run-id>/{audit,artifacts}/` by the worker entrypoint.

Neither location is `.orc/` in the user's project tree, so running `orc improve` against horde runs requires manually assembling that tree. Users want to analyze a single run on demand, and — on a recurring basis (e.g. a weekly cron) — analyze a batch of runs together.

## Solution

A new `horde hydrate` command that materializes `.orc/audit/` and `.orc/artifacts/` from one or more completed runs into a user-specified local directory. The user then `cd`s into that directory and invokes orc normally. horde stays out of orc's way — it does not shell out to `orc improve`/`orc doctor`, and it does not learn any orc subcommand names beyond `orc run` (which is already documented in `ORC_CONTRACT_EXPECTATIONS.md`).

## Command

```
horde hydrate <run-id> [<run-id>...] --into <dir>
```

- One or more run-ids, positional.
- `--into <dir>` required. Created if it doesn't exist.
- No filter flags (`--ticket`, `--since`, etc.). Compose with `horde list --json | jq | xargs` when broader selection is needed.

## Output Layout

Run data is nested under a `<ticket>-<run-id>` segment to eliminate collisions across runs of the same ticket:

```
<dir>/.orc/audit/<ticket>-<run-id>/…
<dir>/.orc/artifacts/<ticket>-<run-id>/…
```

For runs that used a named workflow, the layout follows orc's existing named-workflow scheme:

```
<dir>/.orc/audit/<workflow>/<ticket>-<run-id>/…
<dir>/.orc/artifacts/<workflow>/<ticket>-<run-id>/…
```

Single-run and multi-run hydration use the same scheme — no special cases. orc reads the tree exactly as it would read a locally-produced `.orc/` folder; the `<ticket>-<run-id>` segment simply appears where `<ticket>` would have been in a local run.

## Semantics

### Idempotency

Per run-id, if the target subdir already exists, skip that run-id silently. This makes re-running `horde hydrate` cheap and cron-friendly. To force a refresh, the user deletes the subdir. No `--force` flag in v1.

### Partial-failure handling

Each run-id is processed independently. A failure on one run-id does not abort the others. After processing all run-ids:

| Outcome | Exit code |
|---|---|
| All succeeded (or were skipped as already-present) | 0 |
| Some succeeded, some failed | non-zero (1) |
| All failed | non-zero (1) |

Failures are logged to stderr with the run-id and the reason. A summary line at the end reports counts: `hydrated: N, skipped: M, failed: K`.

Cases that count as "failed" and are skipped without aborting:

- Run-id not found in the store.
- Run is not terminal (pending or running). Hydrating mid-run artifacts is not supported in v1.
- Source artifacts missing at the expected location (returned as `*provider.FileNotFoundError`).
- Transport errors (S3 access denied, filesystem errors, etc.).

Usage errors (missing `--into`, zero run-ids) are handled by urfave/cli and exit before any work begins.

## Provider Interface

Hydration is a new `Provider` method, parallel to the existing `ReadFile`:

```go
// HydrateRun copies the run's audit and artifacts trees to the given destination
// directories. destAuditDir and destArtifactsDir are the exact paths the
// provider should write into (the caller has already assembled the
// <ticket>-<run-id> segment and any workflow prefix).
// Returns *FileNotFoundError if the source tree is unavailable for this run.
HydrateRun(ctx context.Context, run *store.Run, destAuditDir, destArtifactsDir string) error
```

The command in `cmd/horde/hydrate.go` is responsible for:

1. Looking up each run-id in the store.
2. Rejecting non-terminal runs with a clear message.
3. Computing `destAuditDir` and `destArtifactsDir` from `run.Ticket`, `run.ID`, and `run.Workflow`.
4. Skipping if either destination dir already exists.
5. Calling `Provider.HydrateRun`.
6. Aggregating outcomes and setting the exit code.

Provider implementations only move bytes — they do not assemble paths from run fields.

### Docker provider

Copies from the local results store:

```
~/.horde/results/<run-id>/audit/     → destAuditDir
~/.horde/results/<run-id>/artifacts/ → destArtifactsDir
```

This tree is populated by the existing lazy-check and kill flows (`docker.go` already calls `CopyFromContainer` into `opts.ResultsDir` on stop/kill). If the results dir is absent (run predates the feature, or the completion hook failed), return `*FileNotFoundError`.

Uses standard `io/fs` directory-copy helpers — no Docker API calls needed, since the data is already on the local filesystem.

### ECS provider

Downloads from S3 using the existing `s3.Client` in the ECS provider:

```
s3://<bucket>/horde-runs/<run-id>/audit/     → destAuditDir
s3://<bucket>/horde-runs/<run-id>/artifacts/ → destArtifactsDir
```

Bucket name is already available to the provider via SSM config. Uses `ListObjectsV2` paginator + parallel `GetObject` calls, writing each object to its corresponding local path. Empty prefix → `*FileNotFoundError`.

## User Journeys

### Single-run inspection

```
horde hydrate abc123def456 --into /tmp/inspect
cd /tmp/inspect
orc improve
```

### Weekly batch analysis (cron)

```
horde list --since 7d --status success --json \
  | jq -r '.[].id' \
  | xargs horde hydrate --into /tmp/weekly-$(date +%Y-%W)
cd /tmp/weekly-$(date +%Y-%W)
orc improve
```

The cron script gets a non-zero exit from `horde hydrate` if any run failed to hydrate, which lets it surface the partial failure without losing the runs that did succeed.

## Code Layout

- `cmd/horde/hydrate.go` — new command, registered in `main.go`.
- `internal/provider/provider.go` — add `HydrateRun` to the `Provider` interface.
- `internal/provider/docker.go` — Docker implementation (local fs copy).
- `internal/provider/ecs.go` — ECS implementation (S3 download).
- `internal/provider/provider_conformance_test.go` (or equivalent) — add shared conformance tests for `HydrateRun` across both providers (success, missing source, non-terminal run rejected at caller level).
- `internal/docs/` — new embedded topic, surfaced via `horde docs hydrate`.
- `SPEC.md` — document `horde hydrate` alongside `horde results`.
- `ORC_CONTRACT_EXPECTATIONS.md` — no change. The orc boundary is unchanged; horde only reads the files orc has already written.

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Command shape | Dedicated `horde hydrate`, no `horde improve`/`horde doctor` wrappers | Keeps the orc-subcommand surface at the contract boundary (only `orc run`). Wrappers are additive if ever needed. |
| Destination | User-supplied `--into <dir>` | Single-run inspection, cron batches, and project-tree merging are all just "pick a dir." No magic. |
| Layout | `<ticket>-<run-id>` segment | Single rule covers single-run and multi-run with no collisions. |
| Query surface | Run-ids only | `horde list --json | xargs` already covers every filter we could add; no duplicated filter logic. |
| Idempotency | Skip if target subdir exists | Cron-friendly cheap re-runs; `rm -rf` to refresh is obvious and low-cost. |
| Partial failure | Best-effort, non-zero exit if any failed | One bad run-id shouldn't throw away 4 good ones, but scripts must detect partial failure. |
| In-progress runs | Skip with warning, counted as failure | Hydrating partial artifacts risks misleading `orc improve` input; wait for the run to finish. |
| Provider method | New `HydrateRun`, not reuse of `ReadFile` | `ReadFile` is single-file with a size cap; hydration is whole-tree. Different contract, different primitive. |
| Path assembly | Command assembles destination paths; providers receive fully-formed dirs | Keeps `<ticket>-<run-id>` and workflow-prefix logic in one place. Providers only move bytes. |

## Out of Scope (v1)

- Filter flags on `hydrate` (`--ticket`, `--since`, `--status`). Compose with `horde list`.
- `horde improve` / `horde doctor` wrappers.
- Hydrating in-progress runs.
- `--force` / refresh flag.
- Non-local destinations (S3-to-S3, remote filesystems).
- Manifest or index file summarizing what was hydrated. Directory listing is sufficient.

## Epic / Dependencies

- Independent of the ECS bootstrap and CDK tracks (`jqd`, `5fh`) — hydrate just reads from the locations those tracks already populate.
- Depends only on the existing Docker results store layout and the existing S3 upload path in the ECS entrypoint. Both are already in place.
- Fits naturally into `11- CLI v0.2 features (i2c)` or `14- Observability and missing-path tests` as a small standalone task.
