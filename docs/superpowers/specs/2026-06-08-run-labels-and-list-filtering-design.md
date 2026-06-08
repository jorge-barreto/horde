# Run labels + `horde list` filtering — design

Date: 2026-06-08
Issues: [#17] (attach arbitrary metadata to runs and query by it), [#19] (filtering on `horde list`).
Branch: `run-labels` (worktree `.worktrees/run-labels`, based on `origin/main`).

## Why

A factory orchestrator dispatches `horde launch` repeatedly across an epic and needs to ask
questions of run history that don't fit on a run today: *which dispatches were the
orchestrator vs. a human, which belonged to tick #12, which used prompt variant v3, what's
total spend on factory dispatches this month* (#17). It also needs to query run history
efficiently — "in-flight runs for this epic", "failed runs in the last 24h" — without pulling
every row and filtering in `jq` (#19).

Today a run carries only fields about the *work* (`id`, `ticket`, `workflow`, `branch`,
`status`, `duration`, `cost`). There is nowhere to put information about the *dispatch*, and
`horde list` is unfiltered (returns every run for the repo). This is the #17 prerequisite the
roadmap places under the #36 keystone (cost grouping half).

## Scope

In scope, one coherent slice:

1. **Set labels at launch** — `horde launch --label key=value` (repeatable), stamped on the run.
2. **Persist labels** — a new, dedicated `Labels` field on `Run`, separate from the existing
   provider-internal `Metadata`.
3. **Filter `horde list`** — by labels (AND-style exact match) **and** by first-class fields
   (`--status`, `--workflow`, `--ticket`, `--since`, `--until`) (#19 basic filters).
4. **Show labels** — in `horde status` and in the `--json` shapes of `status`/`list`.
5. **Cost rollup by cohort** — `horde list --json` carries a `summary { count, total_cost_usd }`
   aggregate over the filtered result set, so a label cohort's spend is readable without
   client-side summing.
6. **Documentation** — update SPEC.md, README.md, CLAUDE.md, and `horde docs` (the existing
   `quickstart` topic plus a new `labels` topic; see §6 for why a new topic).

Explicitly **out of scope** (deferred to their own slices):

- Mutating labels after launch (labels are set once, at launch; `RunUpdate` does not carry them).
- A standalone `horde stats` command (SPEC v0.4 roadmap line 779) — the cohort rollup rides on
  `list --json` instead.
- Prefix/wildcard/range label matching — #17 explicitly says exact-match is enough.
- Anything from #36 (event backbone, queue, `--enqueue`, throughput cap, notifications). This
  slice only builds the label/cost-grouping substrate #36 later consumes.

## Design

### 1. Data model: a dedicated `Labels` field

The existing `Run.Metadata map[string]string` holds **provider-internal** data (ECS
`cluster_arn`, `log_group`, `log_stream_prefix`, `artifacts_bucket`), written by
`provider.Launch()` and returned via `LaunchResult.Metadata`. `UpdateRun` overwrites the whole
map. User labels must not share that map: they'd collide with reserved keys and the
overwrite-whole-map update semantics would clobber one or the other.

So labels get their own field:

```go
type Run struct {
    // ...existing fields...
    Metadata map[string]string // provider-internal — UNCHANGED
    Labels   map[string]string // NEW: user-supplied tags from --label, set once at launch
}
```

Labels are **set at `CreateRun` only**. They are not on `RunUpdate` (no mutation path), and
`provider.Launch` never touches them — keeping the provider boundary clean (a provider knows
nothing about user labels).

**SQLite** (`internal/store/sqlite.go`):
- New `labels TEXT` column, JSON-encoded exactly like `metadata` is today (NULL when no labels).
- **Migration**: the schema today is a bare `CREATE TABLE IF NOT EXISTS`. On store open, after the
  create, run an idempotent ALTER-if-missing:
  - `PRAGMA table_info(runs)` → if no `labels` column, `ALTER TABLE runs ADD COLUMN labels TEXT`.
  - This keeps existing `~/.horde/horde.db` files working seamlessly and establishes a tiny
    pattern for future additive columns. (docker/SQLite is local-testing-only, but a confusing
    breakage is still worth avoiding for one cheap pragma check.)

**DynamoDB** (`internal/store/dynamo.go`, `dynamo_schema.go`):
- New `labels` attribute, a Map of String (`AttributeValueMemberM`), written at `CreateRun`,
  parsed in `parseRun`. Mirrors the existing `metadata` marshaling exactly. Omitted when empty.
- New attribute-name constant `AttrLabels = "labels"`.
- No new GSI (see §3).

### 2. Setting labels at launch

`horde launch --label key=value`, repeatable, AND-combined into a map:

```
horde launch --workflow implement-ticket --label epic=KS-100 --label variant=v3 KS-243
```

- Implemented as a `cli.StringSliceFlag` named `label` on the `launch` command.
- Parsed **inside the Action** (not via flag constraints). Per the `--json` contract
  (`cmd/horde/jsonv1.go`, CLAUDE.md): do **not** mark the flag `Required` and do not rely on
  urfave validation, which prints help to stdout and bypasses the `ErrorV1` envelope. A malformed
  pair emits `ErrorV1` under `--json` and `error: ...` on stderr otherwise.
- Each entry is split on the **first** `=` (values may contain `=`; keys may not).
- **Validation** (strict keys, free-form values):
  - Key: matches `^[A-Za-z0-9_.-]+$`, length 1–64. (k8s/Docker-style; safe for future query,
    metrics-dimension, and column contexts.)
  - Value: any non-empty UTF-8, length 1–256. (Flexible for descriptive tags.)
  - Duplicate key in one launch is an error (ambiguous).
- The parsed map is set on `Run.Labels` before `CreateRun`. This is the only write path.

`horde retry` re-launches with the same params. It reads the original run's `Labels` and stamps
them on the retried run, consistent with how retry preserves `--workflow`/`--branch`.

### 3. Filtering `horde list`

New flags on `list`, all AND-combined, scoped to the current repo (as `list` already is):

| Flag | Filters on | Source |
|---|---|---|
| `--label key=value` (repeatable) | `Run.Labels[key] == value` for every given pair | #17 |
| `--status <s>` (repeatable) | `Run.Status` ∈ given set | #19 |
| `--workflow <name>` | `Run.Workflow == name` | #19 |
| `--ticket <id>` | `Run.Ticket == id` | #19 |
| `--since <when>` | `Run.StartedAt >= t` | #19 |
| `--until <when>` | `Run.StartedAt <= t` | #19 |

- `--label` on `list` reuses the same `key=value` parser + validation as `launch`.
- `--since`/`--until` accept either an RFC3339 date (`2026-04-01`, `2026-04-01T12:00:00Z`) or a
  relative duration meaning "ago" (`1h`, `24h`, `7d`*). Parsed in the Action; invalid values emit
  `ErrorV1`. (*Go's `time.ParseDuration` has no `d`; we add a small helper that accepts `d` as
  24h, else falls back to `time.ParseDuration`, else tries RFC3339.)
- **Interaction with `--all`**: today `--all` toggles active-only vs. all-statuses. When
  `--status` is supplied it takes over the status dimension and implies the breadth of `--all`
  (terminal runs are eligible). When neither `--status` nor `--all` is given, the existing
  active-only default holds. Documented in `horde docs list` and the command description.

**Store change** — one filtered query method replaces ad-hoc predicates:

```go
type RunFilter struct {
    Repo     string             // always set (list is repo-scoped)
    Statuses []Status           // empty = no status filter
    Workflow string             // "" = no filter
    Ticket   string             // "" = no filter
    Labels   map[string]string  // nil/empty = no filter; AND exact-match
    Since    *time.Time         // nil = no lower bound
    Until    *time.Time         // nil = no upper bound
}

// Store interface
ListRuns(ctx context.Context, f RunFilter) ([]*Run, error)
```

`ListRuns` **subsumes `ListByRepo`** — `ListByRepo(repo, activeOnly)` becomes
`ListRuns(RunFilter{Repo: repo, Statuses: activeStatuses-or-nil})`. We collapse to one method and
update all call sites (`list`, and any internal callers). The shared conformance suite covers
both stores against the new method.

- **DynamoDB (the path that matters — often many thousands of rows/repo)**: queries the existing
  **`by-repo` GSI** (partition `repo`, sort key `started_at`, `ScanIndexForward=false`) with a
  real, server-side **`FilterExpression`** so the query is fast and the wire payload small. The
  `started_at` range (`Since`/`Until`) goes in the `KeyConditionExpression` (sort-key range);
  statuses, workflow, ticket, and each label pair (`labels.#k = :v`) go in the `FilterExpression`,
  all AND-combined. No new GSI: this reuses the repo-scoping index and is the explicit goal — push
  filtering to DynamoDB rather than ship rows to the CLI. The in-memory `functionalDynamo` test
  fake is generalized to actually evaluate these filter expressions so the conformance suite
  exercises the real query path.
- **SQLite (local-testing-only — perf irrelevant)**: kept deliberately simple. Fetches the
  repo-scoped rows (existing by-repo query, with the `started_at` range applied in SQL) and applies
  the remaining status/workflow/ticket/label predicates via a shared `matchesFilter(run, filter)`
  Go helper. Not optimized — docker/SQLite is the local pipeline-test path, not production, so the
  added query machinery lives where it's worth it (DynamoDB).

### 4. Showing labels in output

- `ListRunV1` and `StatusV1` gain `Labels map[string]string` with `json:"labels,omitempty"` —
  omitted when empty, so existing `--json` consumers and golden files are unchanged for
  label-less runs. Stays exactly one JSON object — honors the `--json` contract.
- Human output:
  - `horde status` prints a `Labels: k=v, k2=v2` line (sorted by key) when present.
  - `horde list`'s table stays as-is (labels are too verbose for a column); they surface via
    `--json` and in `status`.

### 5. Cost rollup over the filtered cohort

`ListV1` gains a `summary` block, always present (stable shape), aggregating the runs in `runs`:

```jsonc
{
  "runs": [ /* ListRunV1... */ ],
  "summary": {
    "count": 12,
    "total_cost_usd": 4.82
  }
}
```

- `count` = number of runs in `runs`.
- `total_cost_usd` = sum of non-null `TotalCostUSD` across those runs (a run with unknown cost
  contributes 0 and does not make the sum null).
- The cohort is "whatever the filter matched" — `horde list --label epic=KS-100 --all --json`
  yields that epic's total spend with no client-side summing.
- Human output: a trailing line under the table, e.g. `12 runs, $4.82 total`.

### 6. Documentation surfaces

All four updated in the same PR:

- **SPEC.md**:
  - Move `horde launch --label key=value` / `horde list --label key=value` out of the v0.4
    roadmap (line ~778) into the implemented CLI commands + flags sections.
  - Add the first-class `list` filters (`--status/--workflow/--ticket/--since/--until`).
  - Add `labels` to the SQLite schema table and the DynamoDB attribute table (distinct from
    `metadata`). Note the by-repo + FilterExpression query strategy.
  - Document the `summary` block on `list --json`.
- **README.md**: add a `--label` launch example and the filtered `horde list` examples next to the
  existing `horde list` block.
- **CLAUDE.md**: add a Key Design Decision line — user labels live in `Run.Labels` (own
  column/attribute), strictly separate from provider-internal `Run.Metadata`; set once at launch;
  the `ListRuns`/`RunFilter` query split (DynamoDB FilterExpression vs SQLite Go predicate).
- **`horde docs`** (`internal/docs/content.go`): add label-setting + filtering + the `summary`
  aggregate to the `quickstart` topic (which already covers core CLI usage), and add a dedicated
  **`labels`** topic covering the full label/filter/cohort-cost story. A new topic is appended to
  the `topics` slice; `docs_test.go` asserts the first topic stays `quickstart`, so append, don't
  prepend.

## Verification

- `make unit-test`, `make vet`, `make integration-test`, and cdk-test (`npm test`, 81 tests) all
  pass. Store conformance covers labels + every `RunFilter` dimension against both SQLite and
  DynamoDB (the in-memory fake was generalized to evaluate the real FilterExpression).
- A **live DynamoDB e2e** (`internal/store/dynamo_labels_e2e_test.go`, gated behind
  `HORDE_E2E_LABELS=1` + `HORDE_E2E_PROFILE`/`HORDE_E2E_RUNS_TABLE`, never CI) exercises the full
  query path against a real runs table: round-trip, single/multi-label AND, status set,
  workflow+ticket, started_at range (key condition), combined filter, and ordering. Verified green
  against the prepdesk stack (`horde-runs-aetherialproductions-prepdesk`); it writes under a unique
  synthetic repo and deletes every row it creates.
- Real-CLI smoke (docker/SQLite): `horde launch --label`, `horde list --label/--status/--workflow`
  (single + combined/AND), the `summary` block in `--json`, and the human `N runs, $X total` line
  all confirmed.

## Integration note

This branch was developed against `main` at the time and rebased onto an advanced `main` that had
meanwhile merged #27 (launch `--json` contract), #25 (richer `list --json`), and #22 (`--env`).
Resolutions: `launch` carries both `--env` and `--label`; `ListRunV1` is the #25 rich struct **plus**
a `Labels` field; `ListV1` keeps the #25 derive-from-`statusToV1` shape **plus** the new `summary`
block. `Run.Labels` set-at-launch is independent of the launch `--json` output shape (labels are an
input, surfaced on read via `status`/`list`).

## Testing

- **Store conformance suite** (`internal/store/conformance_test.go`, runs against *both* SQLite and
  DynamoDB): label round-trip (nil / empty / populated), the SQLite ALTER-if-missing migration
  (open an old-schema DB, confirm it gains the column and round-trips), and `ListRuns` across every
  `RunFilter` dimension and combination (status set, workflow, ticket, since/until range, single +
  multiple labels AND, and mixed first-class + label filters).
- **Launch flag tests** (`cmd/horde`): `--label` parsing, split-on-first-`=`, key/value validation
  (valid + each rejection case), duplicate-key error, and that labels reach `CreateRun`. Both human
  and `--json` error envelopes.
- **List filter tests** (`cmd/horde`): each flag, `--all`/`--status` interaction, `--since`/`--until`
  parsing (date + duration + invalid), and the `summary` aggregate (counts, cost summation with
  null costs).
- **`--json` golden updates**: `labels` omitted when empty; present and sorted when set; `summary`
  present and correct.
- **retry**: labels carried over from the original run.
- `make unit-test`, `make vet`, and `cd cdk && npm test` (cdk-test) all clean. (No CDK/Lambda change
  is required — labels are written by the CLI at `CreateRun`; the status Lambda only updates
  terminal fields and does not touch `labels`. The DynamoDB table needs no schema migration because
  attributes are schemaless; only the by-repo GSI is used, which already exists.)

## Risks / notes

- **DynamoDB FilterExpression cost**: reads scan all repo rows server-side before filtering. At the
  stated scale this is fine; if a repo's history grows into the 10k+ range and label queries become
  hot, the follow-up is a dedicated label GSI (deferred, see Scope). Flagged so it's a conscious
  tradeoff, not an accident.
- **`--all` semantics**: layering `--status` onto the existing `--all` boolean is the one place the
  CLI surface gets subtle; covered by explicit tests and docs.
- **No provider/Lambda churn**: this is store + CLI + docs only. The keystone (#36) is what later
  consumes the `labels`/cost-grouping substrate this slice establishes.

[#17]: https://github.com/jorge-barreto/horde/issues/17
[#19]: https://github.com/jorge-barreto/horde/issues/19
