# Spot auto-resume — design

**Date:** 2026-06-12
**Status:** Approved (pending spec review)
**Scope:** Move ECS runs to Fargate Spot by default and make them automatically resume after a spot interruption.

## Problem

Horde ECS runs launch on on-demand Fargate (`LaunchType: FARGATE`, hardcoded). We want to run on Fargate Spot for the cost saving. Spot capacity can be reclaimed at any time, so runs must survive interruption and resume automatically — without a human running `horde retry`.

## Key insight: most of the machinery already exists

On a spot interruption *today*:

1. ECS sends SIGTERM to the task.
2. `docker/entrypoint.sh` backgrounds orc and re-waits in a TERM trap, so orc's interrupt-save completes; then it syncs the full `/workspace` (committed + uncommitted + `.git`), `~/.claude` (agent session), and `.orc/artifacts`+`audit` to S3.
3. The task stops with ECS `stopCode: "TerminationNotice"`.
4. The status Lambda maps orc's exit (5 on signal) → `killed` and records `stop_code`/`stop_reason` in the run's `metadata` map.
5. `horde retry` already re-launches with the **same run ID**; the entrypoint already restores everything from S3 and orc resumes.

**The only missing piece is the automatic trigger.** Nothing watches for `stop_code == "TerminationNotice"` and re-launches. This spec adds that trigger plus per-launch capacity control.

## Design

Three bounded changes:

### 1. Run on Spot (capacity providers)

- Both `FARGATE` and `FARGATE_SPOT` are **always registered** on the cluster (a capacity-provider association is free — you only pay for tasks that run, at the landing provider's rate). No `useSpot`/SSM infra knob.
- `RunTask` swaps the hardcoded `LaunchType: FARGATE` for a `CapacityProviderStrategy`. **`LaunchType` and `CapacityProviderStrategy` are mutually exclusive in the ECS API** — set exactly one:
  - `spot` → `CapacityProviderStrategy: [{CapacityProvider: "FARGATE_SPOT", Weight: 1}]`, no `LaunchType`.
  - `on-demand` → `CapacityProviderStrategy: [{CapacityProvider: "FARGATE", Weight: 1}]`, no `LaunchType`.
- Applied at **both** `RunTask` call sites:
  - Go: `internal/provider/ecs.go::Launch` (currently `ecs.go:217`).
  - TS drain: `cdk/src/drain-lambda/index.ts::runTaskInput` (currently `index.ts:167`) — reads `run.Capacity` from the queued item.

### 2. Per-launch capacity choice

- New CLI flag `horde launch --capacity spot|on-demand`, **default `spot`**. Modeled on `--priority` (enum-valued string flag in `cmd/horde/main.go`). NOT `Required` — validate the value inside the Action (per the `--json` rule: a `Required` flag prints help to stdout and bypasses the JSON error envelope).
- Stored on a new `Run.Capacity` field. Set once at launch, **carried across resume and retry** (a spot run resumes on spot; an on-demand run stays on-demand). `horde retry` must preserve the stored `Capacity` (it already reuses the stored run, like `Labels`/`Priority` — do not reset it).
- The launch flag is the single source of truth for placement. The infra just guarantees both providers exist.

### 3. Auto-resume via re-queue

When the status Lambda processes a STOPPED event with `stopCode == "TerminationNotice"`:

- **If** the run is under its resume budget (`ResumeCount < MAX_RESUMES`) **and** not already terminal:
  - Re-queue it: `status = queued`, `priority = highest` (jumps ahead of fresh backlog), `ResumeCount++`, clear `StartedAt`/`TimeoutAt` (queue-wait is free, set at drain time).
  - Do **not** emit `run.terminal` — this is a continuation, not a terminal outcome.
  - The existing drain Lambda (already subscribed to `run.terminal`; also lazy CLI drain) picks up the queued backlog under the normal concurrency + realized-spend gates and re-launches with the same run ID and the run's stored `Capacity`.
- **Else** (at cap, or already terminal): land terminal exactly as today (`killed`), so a human can `horde retry`.

**Resume respects the gates (re-queue, not bypass).** A spot-interrupted run is re-admitted through the queue so `MaxConcurrent` and the realized-spend cap apply. This also tames reclaim storms: N runs interrupted at once re-queue and drain back within the cap, instead of all firing `RunTask` simultaneously. No work is lost — everything needed to resume is already in S3.

**Infinite-loop guard.** `ResumeCount` (new `Run` field, default 0) bounds resumes. `MAX_RESUMES` is a Lambda env var (default 5), plumbed via CDK so it's tunable per deploy without a code change (mirrors `MAX_CONCURRENT`/spend caps). `horde kill` produces `stopCode: "UserInitiated"`, never `TerminationNotice`, so a user-killed run is never auto-resumed — the `stop_code` discriminator is airtight against the kill case; `ResumeCount` bounds the pathological repeated-reclaim case.

**Kill-race guard.** If `horde kill` and a spot reclaim land together, `horde kill` writes `killed` synchronously, then the status Lambda fires with `TerminationNotice`. The re-queue is a **conditional write** — `queued` only if the run is not already terminal (`attribute_not_exists(status) OR status NOT IN (success,failed,killed,timed_out,rate_limited,cancelled)`), the same guard pattern the status Lambda already uses for its terminal write. On `ConditionalCheckFailedException` the spot event is a no-op. The synchronous kill wins; no new mechanism. (Note: the existing terminal-write guard in `index.ts` omits `cancelled` from its five-status list; the resume guard adds it for correctness — a running task can't be `cancelled`, so the addition is harmless. The plan should reconcile the two lists deliberately.)

## Data flow on interruption

```
spot reclaim → SIGTERM → orc interrupt-save → entrypoint syncs S3
   → ECS STOPPED (stopCode=TerminationNotice)
   → status Lambda:
        if TerminationNotice AND ResumeCount < MAX_RESUMES AND not-already-terminal:
            conditional write: status=queued, priority=highest,
                               ResumeCount++, clear StartedAt/TimeoutAt
            (no run.terminal emitted)
        else:
            land terminal (killed) as today
   → drain Lambda (gated on concurrency + spend) → RunTask same run ID,
        capacityProviderStrategy from run.Capacity
   → entrypoint restores workspace+session from S3 → orc resumes
```

## Data model

Two additive `Run` fields (SQLite via the idempotent `ensureColumns` path; nullable Dynamo attrs absent on old items — exactly like `Priority`/`EnqueuedAt`):

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Capacity` | string (`"spot"` \| `"on-demand"`) | `"spot"` | Provider this run launches on. Set at launch, carried across resume/retry. |
| `ResumeCount` | int | 0 | Times this run has been spot-resumed. Incremented on re-queue; at `MAX_RESUMES`, stop resuming. |

Both round-trip through the **shared store conformance tests** (SQLite + DynamoDB). `priority` is a Dynamo reserved word and already aliased `#prio`; `Capacity`/`ResumeCount` are not reserved.

## Contract surfaces

- **`--json` (jsonv1.go):** `capacity` and `resume_count` become additive fields on `status`/`list`/`results` run objects. `horde launch --capacity … --json` echoes the chosen capacity. **No new top-level status enum value** — a re-queued run reports the existing `queued`; an exhausted run reports the existing `killed`. Old `--json` consumers ignore unknown fields.
- **Three lockstep status readers** (TS status Lambda / Python bootstrap Lambda / Go `ecs.go::Status`): the resume *decision* lives only in the **TS status Lambda**. Per the established #36 precedent (queue/event backbone is CDK-only; CFN is being retired), **auto-resume is CDK-only**. The Python/CFN status Lambda is intentionally NOT updated — a spot-interrupted run on a CFN-deployed cluster lands `killed` and a human runs `horde retry` (pre-existing behavior, not a regression).
- **Both-providers registration goes in BOTH** CDK (`HordeWorker`) and bootstrap (`stack.yaml.tmpl`) — it has no queue dependency and is cheap, so even a CFN cluster can run spot (just without auto-resume).

## Scope split (CDK vs CFN)

| Change | CDK | CFN/bootstrap |
|---|---|---|
| `--capacity` flag + `Run.Capacity`/`ResumeCount` (Go/CLI) | ✅ (provider-agnostic) | ✅ (same binary) |
| Both capacity providers registered on cluster | ✅ | ✅ |
| `RunTask` capacityProviderStrategy (Go `ecs.go`) | ✅ | ✅ (same binary) |
| `RunTask` capacityProviderStrategy (drain Lambda) | ✅ | n/a (no drain in CFN) |
| Auto-resume re-queue decision (status Lambda) | ✅ | ❌ intentionally not ported |
| `MAX_RESUMES` env var | ✅ | n/a |

## Documentation requirements (MANDATORY — every behavior change updates all four in the same change)

A phase is NOT done until its slice of all four layers is updated:

1. **README.md** — ECS runs default to Fargate Spot with automatic resume; `--capacity on-demand` opts a run out of reclaim.
2. **CLAUDE.md** — TWO updates, both required:
   - a new **Key Design Decision** bullet (spot-by-default, `--capacity` + `Run.Capacity`, auto-resume via re-queue, `ResumeCount`/`MAX_RESUMES` cap, both-providers-registered, CDK-only resume, kill-race conditional guard);
   - the **`## Architecture` / `## Commands`** structural notes that reference the touched files/fields (provider RunTask, the new Run fields, the lockstep-readers note, the `--capacity` command).
3. **`horde docs`** (`internal/docs/content.go`) — extend `retry` (manual recovery after auto-resume gives up) and `queue`/`events` (re-queue is part of the drain story); add a short `spot` topic: interruption → auto-resume → cap → `--capacity`.
4. **`horde --help`** (urfave `Usage:` strings in `cmd/horde/main.go`) — `--capacity` flag usage on `launch`; any field/flag descriptions touched.

## Testing strategy

- **Go unit** (`ecs_test.go`): `RunTask` input carries the correct `CapacityProviderStrategy` for spot vs on-demand and **omits `LaunchType`** (catches the mutual-exclusivity bug). Store conformance: `Capacity`/`ResumeCount` round-trip in SQLite + Dynamo.
- **CLI unit** (`main_test.go`): `--capacity` parsing, default `spot`, invalid value rejected inside the Action (not `Required`), echoed in `--json`.
- **TS Lambda unit** (`status-lambda/index.test.ts`): `TerminationNotice` under cap → re-queue write (status `queued`, `ResumeCount++`, priority highest, `StartedAt` cleared); at cap → lands `killed`; `UserInitiated` → never re-queues; kill-race conditional → CCFE → no-op. Drain test (`drain-lambda`): `runTaskInput` honors `run.Capacity`.
- **CDK test** (`cdk-test`): cluster associates both capacity providers; status Lambda receives `MAX_RESUMES` env.
- **E2E** (`TestECS_*`, developer-local, NOT CI): a real spot reclaim is hard to force. Note a manual check (simulate via `StopTask` with a spoofed `TerminationNotice`-shaped event where feasible) and state the coverage limitation honestly rather than claim full automated coverage.

## Out of scope / deferred

- Live in-flight spend enforcement during a reclaim storm (already deferred, #52) — `MaxConcurrent` remains the real blast-radius backstop.
- Weighted spot/on-demand mix or on-demand fallback strategy — `--capacity` is a single provider choice; a weighted strategy can extend the same field later.
- Porting auto-resume to the Python/CFN Lambda — intentionally not done (CFN retirement).
