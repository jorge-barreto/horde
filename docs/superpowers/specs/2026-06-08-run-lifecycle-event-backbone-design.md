# Run-lifecycle event backbone → queue + throughput cap

**Issue:** [#36](https://github.com/jorge-barreto/horde/issues/36)
**Date:** 2026-06-08
**Status:** Design — approved, pending spec review

## Summary

Generalize the latent run-lifecycle signal that horde's ECS status-sync Lambda
already computes into a **first-class emitted event** on a dedicated EventBridge
bus, and build the **server-side queue + spend-rate cap** as its first internal
consumer. Notifications and external-system consumers are deferred to a follow-on
spec — but the event bus they need is built and exercised now, so they become a
small additive bolt-on rather than a backbone redesign.

This is **cloud-native (ECS) only**. The docker provider stays lazy/pull-based
with no daemon and no watcher — there is no event source there and we deliberately
do not grow one.

This work is **CDK-only**. All new infrastructure lands in the TypeScript CDK
construct (`cdk/src/`). The CloudFormation/bootstrap path
(`internal/bootstrap/templates/stack.yaml.tmpl`) is **not** touched — it is being
retired in a separate effort, so the queue/event features land CDK-first and the
CFN path intentionally never catches up.

## Background — what exists today

On ECS the run-lifecycle signal already exists in all but name:

- An EventBridge rule (`ECS Task State Change`, filtered to the horde cluster +
  `lastStatus == STOPPED`) fires the **status-sync Lambda**
  (`cdk/src/status-lambda/index.ts`).
- The Lambda finds the `horde-worker` container **by name** (sidecars make
  `containers[0]` unreliable), maps its exit code to a terminal run status
  (`0→success`, `2→timed_out`, `4→rate_limited`, `5→killed`, else `failed`),
  best-effort fetches `total_cost_usd` from the run's S3 `run-result.json`, and
  writes the run terminal into DynamoDB under a conditional guard that skips
  already-terminal runs.

That Lambda is a **terminal sink** — it emits nothing downstream. "Run X reached
terminal state Y" is computed and then discarded. This design turns it into an
emitted event.

What does **not** exist today: any queue, any `--enqueue` verb, any `run.started`
event, any throughput/spend cap, and any notification fan-out. The concurrency
gate is purely client-side: `horde launch` calls `store.CountActive()` and returns
the `capped` status when `>= MaxConcurrent`.

Prerequisites are landed: **#17** (run metadata) and **#35** (recoverable runs /
terminal capture) are both merged and closed.

## Goals

1. A first-class, versioned run-lifecycle event (`run.started`, `run.terminal`,
   and the derived `run.cost-threshold-exceeded`) on a dedicated EventBridge bus,
   carrying horde's vocabulary (not raw ECS shape).
2. A durable, queryable **server-side queue** of launches (`horde launch
   --enqueue`), drained automatically as slots free, with a 5-level priority lever.
3. A **realized spend-rate cap** ($/window) that gates the drain alongside the
   existing concurrency limit.
4. Notifications + external consumers as a **documented, exercised extension
   point** — the bus exists and has a real subscriber (the drain + the operator's
   external agent), but notification *targets* (webhook/SNS/signing/routing) are a
   later spec.

## Non-goals

- **No docker daemon / watcher.** The whole backbone is ECS-only. `--enqueue` on
  the docker provider is a hard error.
- **No client-side `horde launch --wait`.** A client blocking on cluster state is
  a polling antipattern (per the #27 close). The queue is server-side and drained
  by the event, not by a waiting client.
- **No cohort/epic-shaped fixed budget.** The cap is a throughput/spend-*rate*, not
  a fixed total per arbitrary cohort (the #21 cohort shape was rejected as
  premature shape-locking).
- **No CloudFormation/bootstrap parity.** CDK-only; the CFN path is being retired.
- **No live/predictive spend enforcement in v1.** Cost is only known at run end,
  so the cap is realized-only (see "Known limitations"). A deferred issue tracks
  live enforcement once live telemetry exists.

## Design philosophy — smartness lives *above* horde

The drain is **mechanical and never makes a priority judgment**. horde does not
understand tickets, epics, or "what's really urgent right now." When the factory
trips — a bad commit breaks staging CI, and a hotfix ticket must jump the line —
that judgment belongs to a layer *above* horde: a human, or a long-running agent
the operator points at the project and tells "this is the queue."

horde's job is to provide a **durable, queryable, steerable substrate** for that
brain to operate on:

- The backlog is queryable (`horde queue list` / `horde list --status queued`).
- Every queued run carries an overridable **priority** the brain can bump.
- `horde launch --force` bypasses the queue entirely for a true emergency.

The drain picks mechanically (highest priority, then oldest); the brain curates
the backlog. This mirrors the boundary horde draws everywhere else: orc owns
per-run cost caps, humans own merges, horde just launches.

## Architecture

```
  horde launch ─────────────────┐
   (CLI, Go)                     │ PutEvents(run.started)
                                 v
                          ┌─────────────┐
   status Lambda ────────>│  horde bus  │  (EventBridge, per deployment)
   (TS) PutEvents         │ (custom)    │
     run.terminal /       └──────┬──────┘
     run.cost-threshold          │ rules
     -exceeded         ┌─────────┼──────────────┬─────────────┐
                       v         v              v             v
                  drain Lambda  (later)       (later)    operator's
                   (TS, new)    webhook       external     agent
                                target        consumer   (subscribes now)

   Queue substrate: DynamoDB runs table. A queued launch is a Run row with
   status="queued". Drained by: (A) drain Lambda on run.terminal [primary],
   (B) lazy CLI drain on `horde launch`/`list` [backstop].
```

### Component 1 — The event bus (the backbone)

**Infra (CDK construct only):** one custom EventBridge bus per horde deployment,
e.g. `horde-<project-slug>`, created by `HordeWorker`.

> Naming note: the `HordeWorker` construct is a misnomer (it provisions the whole
> dispatch apparatus and launches *many* workers, it is not "a worker"). A rename
> to `HordeFleet` is tracked as a **separate** breaking-change ticket and is out of
> scope here; this design keeps referring to `HordeWorker`.

**Producers:**

- **`run.started`** — emitted by **whatever transitions the run to `running`**,
  via `events:PutEvents`:
  - a **direct** `horde launch` (no `--enqueue`) → the **Go CLI** emits it (it
    holds AWS creds and the run record).
  - a **drained** queued run → the **drain Lambda** (or the lazy CLI drainer)
    emits it, because the originating `launch --enqueue` CLI process exited at
    enqueue time and is long gone by the time the run actually starts.

  In both cases the emit happens at the same lifecycle moment (run → `running`);
  the emitter differs by path. This keeps `run.started` symmetric with
  `run.terminal` without a dedicated emitter Lambda.
- **`run.terminal`** — emitted by the **status Lambda** *after* its existing
  authoritative DynamoDB terminal write. Emission is **best-effort**: a failed
  `PutEvents` logs but never blocks or reverses the store write. The store remains
  the source of truth; the event is a notification *of* truth, not truth itself.
- **`run.cost-threshold-exceeded`** — emitted by the **drain** (Lambda and/or CLI
  path) when a drain is *held* specifically because the spend cap is exceeded
  (distinct from being held by the concurrency limit).

**Event contract (stable, versioned):**

| Field          | EventBridge `DetailType` |
|----------------|--------------------------|
| `run.started`  | a run began executing    |
| `run.terminal` | a run reached a terminal state |
| `run.cost-threshold-exceeded` | a drain was held by the spend cap |

- `Source`: `horde`
- `Detail` (JSON), horde vocabulary — never raw ECS shape:
  `version`, `run_id`, `repo`, `ticket`, `workflow`, `branch`, `status`,
  `exit_code`, `total_cost_usd`, `labels`, `started_at`, `completed_at`,
  `stop_code`, `stop_reason`. `status` uses horde's terminal vocabulary
  (`success`/`failed`/`killed`/`timed_out`/`rate_limited`/`cancelled`).
- `version` makes the shape a forward-compatible public contract. Subscribers get
  horde's run identity and mapped status directly — they never re-implement the
  by-name container exit-code logic.

**Why a custom bus (vs. reusing the raw ECS event or an in-process call):** the
operator has a real, imminent external subscriber (a long-running agent reacting
to terminal events), so the fan-out seam is *exercised* in v1, not just designed.
The raw `ECS Task State Change` event is ECS-shaped (no run identity, no
`run.started`, forces every subscriber to re-derive status) and is therefore
rejected. An in-process-only call would defer the bus but leaves the seam
unexercised; with a real external consumer now, building the bus is the honest
choice.

### Component 2 — The queue (DynamoDB)

A queued launch **is a `Run` row** with `status = "queued"`. No separate queue
schema, no second source of truth. It carries everything a launched run carries
(`repo`, `ticket`, `workflow`, `branch`, `labels`, per-launch env, declared
timeout) so the drainer can launch exactly what was submitted.

**Two new `Run` fields** (additive; SQLite `ensureColumns` migration + new
DynamoDB attributes; both nullable so existing rows are unaffected):

- `enqueued_at` (timestamp) — set at submit; the drain-order tiebreaker.
- `priority` (enum: `lowest | low | med | high | highest`, default `med`) — the
  steerable lever. Stored with an ordinal (`lowest=0 … highest=4`) for sortable
  drain order.

**Status model.** `queued` is **non-terminal and not "active."** `CountActive()`
counts `{pending, running}` only — a `queued` run is *waiting* for a slot, it does
not consume one. The lifecycle gains a third resting state:

```
queued ──drain──> pending ──> running ──> {success|failed|killed|timed_out|rate_limited}
   │
   └── cancel ──> cancelled   (terminal; never ran, never cost anything)
```

`cancelled` is a new terminal status (free-text — no migration), distinct from
`killed` (which means a *running* task was stopped and may have committed work or
spent money). `IsTerminal()` returns true for `cancelled`, false for `queued`.

**Timeout semantics.** Queue-wait is free. `started_at` and `timeout_at` are set
when the run **drains** to `pending` — a run that waited 3h in the queue is not
"timed out" before it ever started. `enqueued_at` is the only timestamp set at
submit.

**Drain-time re-resolution.** When a run drains, the drainer re-resolves current
config (secrets, mounts) so it launches against today's setup, not a snapshot
captured at enqueue time.

**Duplicate detection** is scoped by **`(ticket, workflow)`** — not ticket alone.
The same ticket under a *different* workflow is a legitimately different run (per
#20: the `ticket` field means different things across workflows). A `queued` run
counts as a duplicate-blocker for the same `(ticket, workflow)`; `--force`
overrides. (Implementation note: today's `FindActiveByTicket` / launch duplicate
check must be verified — if it is currently ticket-only, this design corrects it
to `(ticket, workflow)`-scoped and that correction is called out explicitly, not
made silently.)

**ECS-only guard.** `--enqueue` on the docker provider is a hard error:
`"--enqueue requires the aws-ecs provider; docker has no event source to drain
the queue"`.

### Component 3 — The drain

Two drainers share one correctness mechanism.

**A. Event drainer (primary) — a new TS drain Lambda** subscribed to
`run.terminal` on the bus via an EventBridge rule. On each terminal event:

1. Capacity: `CountActive(repo) < MaxConcurrent`? If not → stop.
2. Budget: realized spend in the trailing window `< cap`? If not → emit
   `run.cost-threshold-exceeded`, stop (leave runs `queued`).
3. Claim the next queued run: pick **highest `priority`, then oldest
   `enqueued_at`**, via an atomic conditional update `status: queued → pending`
   (condition `status == queued`).
4. On a winning claim, this drainer owns the run: re-resolve config, set
   `started_at`/`timeout_at = now`, call `Provider.Launch`, update to `running`.
   If the conditional update *fails* (a concurrent drainer claimed it first), retry
   from step 1 for the next candidate.
5. **Drain one run per event, then stop.** A terminal event frees exactly one slot;
   draining one per event is the natural rate and avoids one invocation launching a
   stampede. Extra free slots are caught by subsequent events and the lazy CLI path.

**B. Lazy CLI drainer (backstop) — Go, in `horde launch`/`list`.** Mirrors how
`Finalize()` already runs on those commands. After the existing work, if capacity
+ budget allow and queued runs exist, claim-and-launch using the *same* store
operation and the *same* mechanical order. This self-heals a missed/dropped event
the next time any human or agent touches the project — which is exactly an
in-the-loop moment, never a background daemon.

**The claim is the correctness crux.** The conditional update (`queued → pending`,
condition `status == queued`) is the single guard against double-launch, whether
two terminal events race or an event races the CLI path. Exactly one writer wins;
losers see the condition fail and move on. This is the same conditional-write
pattern the status Lambda already uses for its terminal guard.

**New store method:** `ClaimNextQueued(ctx, repo) (*Run, error)` — returns the
claimed run (now `pending`) or nil if none eligible. Implemented on **both** stores
and covered by the shared conformance tests, keeping the `Store` interface uniform
even though the drain only ever *runs* on ECS/DynamoDB. SQLite implements it in a
transaction (`SELECT ... ORDER BY priority DESC, enqueued_at ASC LIMIT 1` then
conditional `UPDATE ... WHERE status='queued'`); single-process local use makes
contention a non-issue there. DynamoDB queries the by-repo GSI filtered to
`queued`, orders in the Lambda (backlog is small, bounded by what was enqueued),
and claims via a conditional `UpdateItem`.

**Failure handling.** A run whose `Provider.Launch` fails *after* a successful
claim becomes `failed` (with the launch error in metadata) — it is **not**
re-queued, to avoid a poison run looping forever. It surfaces in `list` for a
human/agent to retry. EventBridge → Lambda has built-in retries + a DLQ on the
rule; a hard drain-Lambda failure lands in the DLQ, and the lazy CLI path is the
backstop. The drainer is idempotent: re-running it claims the next eligible run or
no-ops.

### Component 4 — The spend-rate cap

Gates the **drain**, not the submit (`--enqueue` always succeeds — it just parks a
row). Checked at drain time after the concurrency check.

**Config (SSM JSON, surfaced as CDK `HordeWorkerProps`, following the sidecars
opt-in pattern):**

- `maxSpendPerWindow` (number, USD) — optional.
- `spendWindow` (duration, e.g. `"24h"`) — optional.
- Both absent ⇒ no spend cap (concurrency-only — today's behavior).

**Measure (realized-only).** At drain, query the by-repo GSI for runs whose
`completed_at` falls in the **trailing rolling window** `[now − spendWindow, now]`,
sum their `total_cost_usd`. If `sum >= maxSpendPerWindow`, hold the drain (leave
the run `queued`) and emit `run.cost-threshold-exceeded`.

**How a held backlog un-holds (no daemon).** A spend-capped backlog drains again
when the **trailing window slides** (older spend ages out) or **in-flight runs
finish** (freeing slots and becoming countable). The lazy CLI drainer re-checks on
the next `launch`/`list`, so a human/agent touching the project re-evaluates the
budget. A held backlog drains on window-slide + CLI touch, not instantly — the
correct daemon-free behavior.

### Component 5 — CLI surface

**Submit:** `horde launch --enqueue <ticket> --workflow <wf> [--priority high]`

- Writes a `Run` with `status: queued`, `priority` (default `med`),
  `enqueued_at: now`. Skips the concurrency gate (it is waiting, not contending).
- Still runs `(ticket, workflow)`-scoped duplicate detection (incl. `queued`);
  `--force` overrides. Invalid `--priority` value is a real error.

**`horde queue` command group** (the queue is a first-class noun the operator and
brain-agent operate on):

- `horde queue list` — backlog in **drain order** (priority desc, enqueued_at asc);
  sugar over `list --status queued`.
- `horde queue prioritize <run-id> --priority <level>` — `RunUpdate` on `priority`
  of a `queued` run only (error if it already drained).
- `horde queue cancel <run-id>` — `queued → cancelled` (terminal); removes it from
  the backlog without ever running it.

**`--json` contract** — a new `queued` status in the launch enum (alongside
`launched | capped | duplicate | error`):

- `queued` — run parked (`run_id`, `priority`, `enqueued_at` set); **exit 0** (a
  protocol-level outcome the caller acts on, like `capped`/`duplicate`).
- Per the project `--json` rule, no `Required: true` flag tricks — validate inside
  the Action so the error envelope holds.

A programmatic caller does `launch --enqueue --json`, gets
`{"status":"queued","run_id":...,"priority":"high"}`, exit 0, and knows the run is
parked.

## Data flow

**Enqueue → drain → run:**

```
horde launch --enqueue PROJ-1 --workflow impl --priority high
  → Run{status:queued, priority:high, enqueued_at:T0}  (DynamoDB)
  → JSON {status:queued, run_id, priority:high}, exit 0

... time passes, a running run finishes ...

EventBridge: ECS Task State Change / STOPPED
  → status Lambda: write Run terminal (DynamoDB), PutEvents(run.terminal) → bus
  → bus rule → drain Lambda:
       CountActive < MaxConcurrent?  yes
       realized window spend < cap?  yes
       ClaimNextQueued(repo): highest priority, oldest → PROJ-1
         conditional update queued→pending (wins)
       re-resolve config; started_at/timeout_at = now
       Provider.Launch → running
       drain Lambda emits run.started (the launch --enqueue CLI is long gone)
```

**Spend-capped hold:**

```
drain: CountActive < MaxConcurrent?  yes
       realized window spend >= cap?  HELD
       → PutEvents(run.cost-threshold-exceeded) → bus
       → leave run queued
... window slides / in-flight runs finish / next CLI touch ...
       → re-evaluate, drain resumes
```

## Error handling — summary

| Situation | Behavior |
|-----------|----------|
| `PutEvents` (any event) fails | Log, continue. Store write already authoritative; never blocks. |
| Two drainers race for one queued run | Conditional update: one wins, losers retry next candidate. |
| `Provider.Launch` fails after claim | Run → `failed` (error in metadata). Not re-queued. Surfaced in `list`. |
| Missed/dropped `run.terminal` event | Rule DLQ + lazy CLI drain self-heals on next `launch`/`list`. |
| `--enqueue` on docker | Hard error (ECS-only guard). |
| Spend cap exceeded | Drain held; `run.cost-threshold-exceeded` emitted; un-holds on window-slide / CLI touch. |
| Reprioritize/cancel a non-queued run | Error (can't steer something already running/terminal). |

## Testing strategy

- **Store (conformance — both SQLite + DynamoDB):** `ClaimNextQueued`; the
  `(ticket, workflow)`-scoped duplicate query; `queued`/`cancelled` status
  round-trip + `IsTerminal()`; drain-order determinism (priority desc, enqueued_at
  asc); the atomic-claim race (two concurrent claims, exactly one wins — the
  DynamoDB fake already models conditional writes, so this is testable without AWS).
- **CLI (Go, `jsonv1_test.go` patterns):** `--enqueue` JSON contract (`queued`,
  exit 0); `queue list/prioritize/cancel` happy + error paths; docker `--enqueue`
  rejection.
- **Lazy CLI drainer (Go, fake store):** capacity/budget gating, mechanical order,
  claim-failure retry, launch-failure → `failed`.
- **TS drain Lambda + emission (`cdk/src`, mirroring
  `status-lambda/index.test.ts`):** event-shape contract, claim+launch, held-on-
  budget path, `run.cost-threshold-exceeded` emission.
- **CDK construct synth tests (mirroring sidecar tests):** new bus, rule, IAM
  (`events:PutEvents`, DLQ), and `HordeWorkerProps` (`maxSpendPerWindow`,
  `spendWindow`).
- **E2E (developer-local, never CI):** an enqueue→drain cycle against the real
  stack as a `TestECS_*` extension, gated like the existing E2E suite.
- **No new CI infra** — everything lands in the existing `unit-test` + `cdk-test`
  jobs.

## Documentation

- New `horde docs queue` — `--enqueue`, priority levels, drain semantics, spend
  cap, and the "smartness lives above horde" model (curate via priority/`--force`).
- New `horde docs events` — the bus, the `run.started`/`run.terminal`/
  `run.cost-threshold-exceeded` contract, and how to subscribe (the external
  agent's entry point).
- Update `horde docs json` (new `queued` launch status), `horde docs cdk` (new
  props), `horde docs config` (spend-cap config), and `--status` filter values
  (`queued`, `cancelled`).
- New **Key Design Decision** in `CLAUDE.md`: bus is the backbone; queue =
  `queued` Run rows drained mechanically by terminal-event + lazy-CLI; priority is
  the steerable lever; spend cap is realized-only / window-sliding; CDK-only;
  `cancelled` vs `killed` distinction; deferred items.

## Deferred — documented extension point (not built)

Notifications and external-system consumers attach as **new rules/targets on the
existing bus** — webhook/SNS targets, payload signing, retry/DLQ policy, per-label
routing. Because the bus exists and is exercised (the drain subscribes; the
operator's agent subscribes), this is an additive bolt-on, not a backbone
redesign. A follow-on spec covers it. The ECS-today workaround (subscribe your own
rule to the raw `ECS Task State Change` event, per the #23 close) remains available
in the interim.

## Follow-on issues to file (after approval)

1. **Live/predictive spend-rate cap** — v1 is realized-only; in-flight runs are
   uncosted until they finish, so a burst of drains can overshoot before any report
   cost (the concurrency limit is the real blast-radius backstop). Live enforcement
   needs live telemetry, which is end-of-run marked today even with the in-flight
   `telemetry-token-counts` work — so true live enforcement is a separate effort.
2. **`HordeWorker` → `HordeFleet` rename** — breaking public CDK API change; needs
   its own deprecation/alias plan and doc updates. Out of scope here.
3. **Notifications + external-consumer spec** — the deferred extension point above.

## Known limitations (v1, by design)

- **Realized-only spend cap** can under-count and overshoot (see follow-on #1).
- **Spend-capped backlog does not drain instantly** — it un-holds on window-slide
  or next CLI touch (the daemon-free trade).
- **CDK-only** — bootstrap/CFN deployments do not gain queue/event features (CFN is
  being retired).
- **Drain is mechanical** — no built-in priority intelligence; that judgment is the
  operator's / their agent's, steering via priority + `--force`.
