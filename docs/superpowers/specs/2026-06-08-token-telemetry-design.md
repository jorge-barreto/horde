# Token-level telemetry on runs — Design

**Issue:** [jorge-barreto/horde#24](https://github.com/jorge-barreto/horde/issues/24)
**Date:** 2026-06-08
**Status:** Approved (pending spec review)

## Problem

horde records `total_cost_usd`, `duration_seconds`, and `status` per run. An
autonomous orchestrator making dispatch decisions needs more — specifically:

1. **Per-run token counts**: input, output, cache-read, cache-write.
2. **Token burn rate** across concurrent runs (tokens-per-minute over a recent
   window).

Cost and duration alone don't let the orchestrator reason about token pressure
or rate-limit proximity. The token data already exists upstream in orc; it just
isn't threaded through horde's store or `--json` contract.

## Investigation findings (ground truth)

These facts drove the design and are recorded so the plan isn't re-derived:

- **orc already captures all four token counts plus `turns`.** It parses the
  Claude CLI stream (`internal/dispatch/stream.go`), aggregates per-phase and
  run-total, and **writes them to `costs.json`** in the audit dir today. Fields:
  `total_input_tokens`, `total_output_tokens`,
  `total_cache_creation_input_tokens`, `total_cache_read_input_tokens`, and
  per-phase `turns` (orc tracks `turns` per phase only, not as a run total).
- **orc does NOT write token counts to `run-result.json`** — that file stops at
  `total_cost_usd` + `total_duration_seconds` + per-phase cost/duration/status.
- **Both files are written atomically** (write-temp-then-rename), so a consumer
  reading mid-run never sees half-written JSON.
- **`costs.json` is flushed incrementally** — after every phase completes
  (orc `runner.go` flush sites), and on every exit path including
  SIGTERM/interrupt (via orc's `failAndHint`). So it always reflects the
  last-completed phase.
- **`run-result.json` is written on every exit path too** (not just clean
  completion), and at end-of-run its totals equal `costs.json`'s totals.
- **horde's status detection is already lazy/pull-based**: `Finalize()` runs on
  `status`/`list`/`results` calls, never on a background timer. `TotalCostUSD`
  is written once at finalize; a running run shows `null` until terminal.

## Scope

**In scope:**

- Four run-level token counts + `turns`, threaded orc → store → `--json`.
- Lazy-live token totals for Docker running runs (read `costs.json` for
  non-terminal runs, not just at finalize).
- Finalize-time token totals for ECS (status Lambda reads `costs.json` from S3).
- Client-side burn-rate enablement: horde stores per-run tokens + the
  timestamps it already has; the orchestrator derives tokens-per-minute from
  `horde list --json`.

**Explicitly out of scope (deferred to follow-up issues):**

- **ECS in-flight liveness** — periodic `costs.json` → S3 sync from the worker.
  → new horde issue.
- **Per-phase token/cost breakdown** in `horde results`.
  → new horde issue.
- **orc promoting token totals into `run-result.json`** (the forward seam).
  → new orc issue.
- `phases_completed` / `phases_total` / `failed_phase` surfacing — a separate
  "surface orc progress fields" effort, not part of this issue.
- A dedicated `horde burn` / metrics command — burn rate is derived
  client-side, not a horde feature.

## Boundary decision: the contract seam

Per the horde/orc boundary rule, horde must never depend on orc workflow
internals — only on the documented contract files. Token counts flow through
the same channel cost already uses: the audit-dir JSON files.

**Read order: `costs.json` first, `run-result.json` token fields as fallback.**

- `costs.json` has the token totals **today**, so this works against the
  current orc immediately — no coordinated release required.
- `run-result.json` token fields are the **forward** path: once orc promotes
  the totals into the run summary (tracked in a new orc issue), horde reads them
  there too. Either source yields identical end-of-run totals.
- This mirrors the existing precedent in `cmd/horde/jsonv1.go`
  (`parseOrcDuration` + orc#3): a horde-side bridge today, with a tracking orc
  issue for the clean long-term shape.

`ORC_CONTRACT_EXPECTATIONS.md` is updated to document `costs.json`'s token
schema, promoting it from an incidental sibling file to a named contract horde
reads.

## Liveness model: lazy-live (pull-based)

We keep horde's existing pull-based, single-writer-from-the-host model. "Live"
means *fresh when the caller asks*, not a background writer. The only change vs.
cost-today is that finalize/status reads `costs.json` for **running** runs too,
exposing token totals as-of-the-last-completed-phase.

- **Docker**: the audit dir is a local host file (`~/.horde/results/<run-id>/`).
  Reading `costs.json` for a running run is a free local read → genuinely live.
- **ECS**: the audit dir is on Fargate ephemeral FS, synced to S3 only on
  finalize/SIGTERM. So ECS gets **finalize-time** tokens now; in-flight ECS
  liveness (periodic S3 sync) is deferred to a follow-up issue.

Push-based telemetry (worker writes the store on a timer) was rejected: it
breaks the single-writer model, requires container→store credentials and IAM,
introduces concurrent-writer correctness and corruption risks against
`Finalize()`'s terminal-status guards — and buys no extra data, because orc only
flushes per-phase, which lazy-live already exposes.

## Data model

### `internal/store/store.go`

A cohesive embedded struct rather than five loose fields:

```go
// TokenUsage is the per-run token telemetry orc reports, as run-level totals
// across phases. Zero counts are meaningful, so presence is tracked by the
// enclosing *TokenUsage being nil (mirrors TotalCostUSD *float64 semantics).
type TokenUsage struct {
    InputTokens         int
    OutputTokens        int
    CacheCreationTokens int
    CacheReadTokens     int
    Turns               int
}
```

- `Run` gains `Tokens *TokenUsage` (nil = not yet known: pre-finalize, or an
  orc old enough that no source file carried tokens).
- `RunUpdate` gains `Tokens *TokenUsage`, set on finalize.

### Persistence

**SQLite** (`internal/store/sqlite.go`): five new `INTEGER` columns added via the
existing idempotent `ensureColumns()` path (the same mechanism that added
`labels`) — `input_tokens`, `output_tokens`, `cache_creation_tokens`,
`cache_read_tokens`, `turns`. No destructive migration. Old rows read back NULL;
`scanRun` reads each as `sql.NullInt64` and leaves `Tokens == nil` when all are
NULL.

**DynamoDB** (`internal/store/dynamo.go`, `dynamo_schema.go`): five new `N`
attributes with matching `Attr*` consts, written in `CreateRun`/`UpdateRun`,
read in the item parser. Absent attributes on old items → `Tokens == nil`.

Storing on the run (vs. re-reading files per call) matches how `TotalCostUSD`
works and is what makes client-side burn-rate cheap: `horde list --json` already
returns everything the orchestrator needs.

## Read path & finalize wiring

### New parse helper — `internal/provider/docker.go`

```go
// ReadTokenUsage reads per-run token totals from the run's audit dir.
// costs.json is preferred (orc writes token totals there today); the
// run-result.json token fields are the forward fallback once orc promotes
// them (tracked in jorge-barreto/orc#<NN>). Returns nil when neither source
// carries usage — best-effort, exactly like ReadRunResult.
func ReadTokenUsage(homeDir string, run *store.Run) *store.TokenUsage
```

- Maps `costs.json`'s `total_*` token fields; sums per-phase `turns` into a run
  total (orc has no run-total `turns`).
- Falls back to `run-result.json` token fields if `costs.json` is
  absent/malformed/lacks tokens.
- Missing → nil. Never returns an error (a crashed run may have written
  neither file).

### Finalize / Status

- **Docker** (`docker.go::Finalize`): alongside the existing `ReadRunResult`
  call, call `ReadTokenUsage` and set `update.Tokens`. Do this for **running**
  runs as well as terminal ones (lazy-live) — the read is cheap and local.
- **ECS** (`ecs.go::Status` lazy-reconcile + the **status Lambda**): the Lambda
  already fetches `run-result.json` from S3 for cost. Extend it to also fetch
  `costs.json` (same `horde-runs/<run-id>/` prefix) and write the new token
  attributes at finalize.
- **Both status Lambdas must stay in lockstep** — the TypeScript
  (`cdk/src/status-lambda/index.ts`) and Python
  (`internal/bootstrap/templates/stack.yaml.tmpl`) implementations are a
  documented cross-language contract.

### S3 sync on SIGTERM

The ECS worker already syncs the audit dir (`.orc/audit/`) to S3 on interrupt,
and `costs.json` lives there reflecting the last completed phase — so
recoverable/killed ECS runs carry whatever token totals orc had flushed. Verify
the existing sync glob covers `costs.json` (it syncs the dir, so it does); no
entrypoint change in this issue.

## `--json` contract

A nested, `omitempty` `tokens` object (keeps the flat field set uncluttered,
mirrors the `*TokenUsage` model). Omitted entirely when unknown (nil).

```jsonc
"tokens": {
  "input": 54791,
  "output": 87915,
  "cache_creation": 529692,
  "cache_read": 8934181,
  "turns": 12
}
```

- **`StatusV1`** and **`ListRunV1`** gain the nested `tokens` object.
- **`ListSummary`** gains a summed `tokens` object (same cohort-aggregate
  treatment `total_cost_usd` already gets), so
  `horde list --json --label cohort=x` returns a cohort token total directly.
  Per-run rows + `started_at`/`completed_at` give the orchestrator everything to
  compute burn-rate over any window.
- **`ResultsV1`** gains the run-total `tokens` object. Per-phase token breakdown
  in `PhaseV1` is **deferred** (follow-up issue).

The conversion functions (`statusToV1`, `listToV1`, `fullResultsToV1`,
`partialResultsToV1`) populate the object from `Run.Tokens`.

## Testing

- **Store conformance** (shared SQLite + DynamoDB suite): round-trip a run with
  `Tokens` set; round-trip with `Tokens == nil`; `UpdateRun` setting tokens on a
  run created without them; old-row/old-item read (NULL/absent → nil).
- **`ReadTokenUsage`**: `costs.json` present → parsed totals + summed turns;
  `costs.json` absent but `run-result.json` has tokens → fallback; both absent →
  nil; malformed JSON → nil (no error).
- **jsonv1**: `tokens` object present/omitted; `ListSummary.tokens` summed
  across a result set; nil-tokens run omits the object.
- **Docker provider finalize**: a running run with a flushed `costs.json`
  surfaces live token totals via `Finalize`/`Status`.
- **ECS / Lambda**: unit-test the `costs.json` extraction in both the TS and
  Python Lambda; assert the new DynamoDB attributes are written. (cdk-test job
  covers the TS Lambda.)
- **Integration** (`make integration-test`, real orc): launch a run, assert the
  `tokens` object appears in `horde status --json` / `results --json` with
  non-zero counts.

## Follow-up issues to file

> The `orc#<NN>` reference in the read-path helper docstring above is a
> placeholder — replace it with the real number once issue 1 below is filed.

1. **orc** — promote token totals (`total_input_tokens` etc.) into
   `run-result.json`, so horde's forward fallback has a clean run-summary source.
2. **horde** — per-phase token/cost breakdown surfaced via `horde results`
   (`PhaseV1` tokens).
3. **horde** — live in-flight token telemetry for ECS: periodic
   `costs.json` → S3 sync from the worker entrypoint so running ECS runs report
   tokens before finalize, matching Docker's lazy-live behavior.
