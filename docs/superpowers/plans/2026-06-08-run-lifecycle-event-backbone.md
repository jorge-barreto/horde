# Run-Lifecycle Event Backbone + Queue Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a server-side launch queue (DynamoDB-backed `queued` runs, drained mechanically by a run-lifecycle event) with a 5-level priority lever and a realized spend-rate cap, plus the first-class EventBridge event bus that drives it.

**Architecture:** A queued launch is a `Run` row with `status="queued"`; `horde launch --enqueue` writes it. A new EventBridge bus carries `run.started`/`run.terminal`/`run.cost-threshold-exceeded`. A new drain Lambda subscribes to `run.terminal`, and the Go CLI lazily drains on `launch`/`list` as a backstop. Both drain via one atomic `ClaimNextQueued` store operation (conditional `queued→pending`). The spend cap sums realized `total_cost_usd` over a trailing window to gate the drain. CDK-only.

**Tech Stack:** Go 1.24 (`urfave/cli/v3`, `modernc.org/sqlite`, `aws-sdk-go-v2` DynamoDB + EventBridge), TypeScript CDK (`aws-cdk-lib`, esbuild-bundled Lambda, `@aws-sdk/client-dynamodb` + `client-eventbridge`).

**Reference spec:** `docs/superpowers/specs/2026-06-08-run-lifecycle-event-backbone-design.md`

---

## File Structure

**Go — store layer:**
- Modify `internal/store/store.go` — add `StatusQueued`/`StatusCancelled` consts; add `Priority` type + `EnqueuedAt`/`Priority` fields to `Run`; add `Priority` to `RunUpdate`; add `ClaimNextQueued` + change `FindActiveByTicket` signature on the `Store` interface; update `matchesFilter`/`IsTerminal`.
- Modify `internal/store/sqlite.go` — schema columns (`enqueued_at`, `priority`) via `ensureColumns`; (de)serialize new fields; implement `ClaimNextQueued` (txn); add workflow arg to `FindActiveByTicket`.
- Modify `internal/store/dynamo.go` + `dynamo_schema.go` — new attributes; implement `ClaimNextQueued` (conditional `UpdateItem`); add workflow arg to `FindActiveByTicket`.
- Modify `internal/store/conformance_test.go` — new conformance cases (status round-trip, drain order, claim race, workflow-scoped duplicates).

**Go — CLI:**
- Modify `cmd/horde/main.go` — `--enqueue`/`--priority` flags on launch; enqueue branch; ECS-only guard; workflow-scoped duplicate call; lazy CLI drain helper invoked from launch/list.
- Create `cmd/horde/queue.go` — `horde queue list|prioritize|cancel` command group.
- Create `cmd/horde/queue_test.go` — queue command tests.
- Create `cmd/horde/drain.go` — shared lazy-drain helper (`drainOnce`) used by launch/list.
- Create `cmd/horde/drain_test.go` — drain helper tests against a fake store.
- Modify `cmd/horde/jsonv1.go` — `queued` launch status (`launchQueuedV1`); extend `LaunchV1`.
- Modify `cmd/horde/jsonv1_test.go` — JSON contract tests.

**Go — event emission:**
- Create `internal/event/event.go` — event types + `Emitter` interface + EventBridge impl + no-op impl.
- Create `internal/event/event_test.go` — marshalling + no-op tests.
- Modify `cmd/horde/main.go` — emit `run.started` on direct launch.

**Go — spend cap:**
- Modify `internal/config/ssm.go` — parse `maxSpendPerWindow`/`spendWindow` from SSM JSON.
- Modify `internal/config/ssm_test.go` — parse tests.
- Modify `cmd/horde/drain.go` — realized-window spend check.

**TS — CDK:**
- Create `cdk/src/drain-lambda/index.ts` — drain Lambda (subscribes `run.terminal`).
- Create `cdk/src/drain-lambda/index.test.ts` — drain Lambda unit tests.
- Modify `cdk/src/status-lambda/index.ts` — emit `run.terminal` after the terminal write.
- Modify `cdk/src/status-lambda/index.test.ts` — emit assertions.
- Modify `cdk/src/horde-worker.ts` — create bus, drain Lambda, rule, IAM; pass bus name to status Lambda + CLI config.
- Modify `cdk/src/horde-worker-props.ts` — `maxSpendPerWindow`/`spendWindow` props.
- Create `cdk/test/horde-worker-events.test.ts` — synth tests for bus/rule/IAM/props.

**Docs:**
- Modify `internal/docs/content.go` — new `queue`/`events` topics; update `json`/`cdk`/`config`/list-status docs.
- Modify `CLAUDE.md` — new Key Design Decision block.

---

## Phase 1 — Store layer

### Task 1: Status vocabulary — `queued` and `cancelled`

**Files:**
- Modify: `internal/store/store.go:14-30` (status consts), `:74-80` (`IsTerminal`)
- Test: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
func TestStatusQueuedNotTerminal(t *testing.T) {
	if StatusQueued.IsTerminal() {
		t.Error("queued must be non-terminal (it is waiting, not done)")
	}
}

func TestStatusCancelledTerminal(t *testing.T) {
	if !StatusCancelled.IsTerminal() {
		t.Error("cancelled must be terminal (it will never run)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestStatusQueued|TestStatusCancelled' -v`
Expected: FAIL — `undefined: StatusQueued` / `StatusCancelled`.

- [ ] **Step 3: Add the status constants**

In `internal/store/store.go`, after the `StatusRateLimited` const (line ~29), add:

```go
	// StatusQueued is a launch parked in the server-side backlog
	// (`horde launch --enqueue`), waiting for a concurrency slot and budget
	// headroom. It is NON-terminal and NOT "active" — it consumes no slot
	// (CountActive counts only pending+running). It drains to pending.
	StatusQueued Status = "queued"
	// StatusCancelled is a queued run cancelled before it ever ran
	// (`horde queue cancel`). Terminal. Distinct from StatusKilled, which
	// means a RUNNING task was stopped and may have committed work / spent
	// money; a cancelled run definitionally did neither.
	StatusCancelled Status = "cancelled"
```

- [ ] **Step 4: Classify `cancelled` as terminal in `IsTerminal`**

In `internal/store/store.go`, change the `IsTerminal` switch (line ~76) to include `StatusCancelled`:

```go
	case StatusSuccess, StatusFailed, StatusKilled, StatusTimedOut, StatusRateLimited, StatusCancelled:
		return true
```

(`StatusQueued` is intentionally omitted — it is non-terminal.)

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/store/ -run 'TestStatusQueued|TestStatusCancelled' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "store: add queued (non-terminal) and cancelled (terminal) statuses"
```

### Task 2: `Priority` type and `Run`/`RunUpdate` fields

**Files:**
- Modify: `internal/store/store.go:95-135` (Run, RunUpdate)
- Test: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
func TestPriorityOrdinalOrdering(t *testing.T) {
	want := []Priority{PriorityLowest, PriorityLow, PriorityMed, PriorityHigh, PriorityHighest}
	for i := 1; i < len(want); i++ {
		if want[i-1].Ordinal() >= want[i].Ordinal() {
			t.Errorf("%s ordinal %d should be < %s ordinal %d",
				want[i-1], want[i-1].Ordinal(), want[i], want[i].Ordinal())
		}
	}
}

func TestParsePriority(t *testing.T) {
	for _, s := range []string{"lowest", "low", "med", "high", "highest"} {
		if _, err := ParsePriority(s); err != nil {
			t.Errorf("ParsePriority(%q): unexpected error %v", s, err)
		}
	}
	if _, err := ParsePriority("urgent"); err == nil {
		t.Error("ParsePriority(\"urgent\"): want error, got nil")
	}
}

func TestParsePriorityEmptyDefaultsMed(t *testing.T) {
	p, err := ParsePriority("")
	if err != nil {
		t.Fatalf("ParsePriority(\"\"): %v", err)
	}
	if p != PriorityMed {
		t.Errorf("empty priority = %s, want med", p)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestPriority|TestParsePriority' -v`
Expected: FAIL — `undefined: Priority` etc.

- [ ] **Step 3: Add the `Priority` type**

In `internal/store/store.go`, after the status block (line ~30), add:

```go
// Priority is the queue drain-order lever set at enqueue and adjustable via
// `horde queue prioritize`. The drain picks highest priority first, then
// oldest enqueued_at within a level. It is purely mechanical: horde never
// decides priority from ticket meaning — the operator (or an agent above
// horde) curates the backlog.
type Priority string

const (
	PriorityLowest  Priority = "lowest"
	PriorityLow     Priority = "low"
	PriorityMed     Priority = "med"
	PriorityHigh    Priority = "high"
	PriorityHighest Priority = "highest"
)

var priorityOrdinals = map[Priority]int{
	PriorityLowest: 0, PriorityLow: 1, PriorityMed: 2, PriorityHigh: 3, PriorityHighest: 4,
}

// Ordinal is the sortable rank; higher drains first. Unknown priorities sort
// as med so a malformed stored value never starves the queue.
func (p Priority) Ordinal() int {
	if o, ok := priorityOrdinals[p]; ok {
		return o
	}
	return priorityOrdinals[PriorityMed]
}

// ParsePriority validates a user-supplied priority string. Empty defaults to
// med. Unknown values are an error (surfaced to the CLI user).
func ParsePriority(s string) (Priority, error) {
	if s == "" {
		return PriorityMed, nil
	}
	p := Priority(s)
	if _, ok := priorityOrdinals[p]; !ok {
		return "", fmt.Errorf("invalid priority %q: want one of lowest, low, med, high, highest", s)
	}
	return p, nil
}
```

Add `"fmt"` to the imports in `internal/store/store.go`.

- [ ] **Step 4: Add fields to `Run` and `RunUpdate`**

In the `Run` struct (after `TimeoutAt time.Time`, line ~117) add:

```go
	// EnqueuedAt is set when a run enters the backlog via --enqueue; it is the
	// zero value for directly-launched runs. It is the drain-order tiebreaker
	// (oldest first within a priority level). Distinct from StartedAt, which is
	// set when the run actually begins running (at drain time for queued runs).
	EnqueuedAt time.Time
	// Priority is the drain-order lever; empty for directly-launched runs.
	Priority Priority
```

In `RunUpdate` (after `TimeoutAt *time.Time`, line ~134) add:

```go
	Priority *Priority // nil = don't update
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/store/ -run 'TestPriority|TestParsePriority' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "store: add Priority type and EnqueuedAt/Priority run fields"
```

### Task 3: SQLite — persist `enqueued_at`/`priority`

**Files:**
- Modify: `internal/store/sqlite.go` (schema `:46-63`, `ensureColumns` `:83-122`, `CreateRun` `:141`, scan/update, `UpdateRun` `:325`)
- Test: covered by conformance (Task 7); add a direct migration test here.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/sqlite_test.go`:

```go
func TestSQLiteEnqueuedFieldsRoundTrip(t *testing.T) {
	s := newTestSQLiteStore(t) // existing helper in this package
	ctx := context.Background()
	enq := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	run := &Run{
		ID: "q1", Repo: "r", Ticket: "T-1", Provider: "aws-ecs",
		Status: StatusQueued, Priority: PriorityHigh, EnqueuedAt: enq,
		LaunchedBy: "me",
	}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, "q1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != PriorityHigh {
		t.Errorf("priority = %q, want high", got.Priority)
	}
	if !got.EnqueuedAt.Equal(enq) {
		t.Errorf("enqueued_at = %v, want %v", got.EnqueuedAt, enq)
	}
}
```

> If `newTestSQLiteStore` is named differently, use the existing helper in `sqlite_test.go` (grep `func newTest`); match it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestSQLiteEnqueuedFieldsRoundTrip -v`
Expected: FAIL — columns/fields not persisted (priority empty, enqueued_at zero).

- [ ] **Step 3: Add columns to the schema + migration**

In `internal/store/sqlite.go` `CREATE TABLE` (line ~46-63), add two columns after `total_cost_usd REAL`:

```sql
    enqueued_at    TEXT,
    priority       TEXT NOT NULL DEFAULT ''
```

In `ensureColumns` (line ~83-122), add additive migrations following the existing `labels`/token pattern (one `ALTER TABLE runs ADD COLUMN ...` guarded by the existing column-existence check):

```go
	{"enqueued_at", "ALTER TABLE runs ADD COLUMN enqueued_at TEXT"},
	{"priority", "ALTER TABLE runs ADD COLUMN priority TEXT NOT NULL DEFAULT ''"},
```

> Match the exact shape of the existing `ensureColumns` entries — grep the function for how it lists columns (slice of structs vs. inline calls) and follow it.

- [ ] **Step 4: Write enqueued_at/priority in `CreateRun` and read them in the row scan**

In `CreateRun` (line ~141) add the two columns to the INSERT column list and value args. Encode `EnqueuedAt` with the same RFC3339-or-empty helper the file uses for `CompletedAt` (grep for how `completed_at` is conditionally formatted — a zero time stores `NULL`/empty). Encode `Priority` as `string(run.Priority)`.

In the row-scanning helper (grep `rows.Scan` / `scanRun`), add `enqueued_at` and `priority` to the SELECT column list and scan targets, decoding `enqueued_at` with the same nullable-time parse used for `completed_at`, and `priority` into `Priority(...)`.

- [ ] **Step 5: Handle `Priority` in `UpdateRun`**

In `UpdateRun` (line ~325), add a clause mirroring the other pointer fields:

```go
	if update.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, string(*update.Priority))
	}
```

> Match the existing `sets`/`args` accumulation style in this function exactly.

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestSQLiteEnqueuedFieldsRoundTrip -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/store/sqlite.go internal/store/sqlite_test.go
git commit -m "store/sqlite: persist enqueued_at and priority"
```

### Task 4: DynamoDB — persist `enqueued_at`/`priority`

**Files:**
- Modify: `internal/store/dynamo_schema.go:9-26` (attr names), `internal/store/dynamo.go` (`CreateRun` `:47`, item (de)serialize, `UpdateRun` `:326`)
- Test: covered by conformance (Task 7).

- [ ] **Step 1: Add attribute name constants**

In `internal/store/dynamo_schema.go` (line ~9-26), add to the attribute-name block, following the existing naming style:

```go
	attrEnqueuedAt = "enqueued_at"
	attrPriority   = "priority"
```

> Grep the file for the existing const naming (e.g. `attrTotalCost`) and match it.

- [ ] **Step 2: Serialize in `CreateRun` / item builder**

In `internal/store/dynamo.go`, find the function that builds the item map (grep `marshalRun` or the inline map in `CreateRun` line ~47). Add:
- `attrPriority` → `&types.AttributeValueMemberS{Value: string(run.Priority)}` (always written, even empty — matches how `branch` is written).
- `attrEnqueuedAt` → only when `!run.EnqueuedAt.IsZero()`, as `&types.AttributeValueMemberS{Value: run.EnqueuedAt.UTC().Format(time.RFC3339)}` (mirror the `completed_at` conditional-write pattern).

- [ ] **Step 3: Deserialize in the item→Run reader**

In the function that reads an item back into a `*Run` (grep `unmarshalRun` or where `attrCompletedAt` is parsed), add parsing for `attrPriority` (into `Priority(...)`) and `attrEnqueuedAt` (nullable RFC3339, mirroring `completed_at`).

- [ ] **Step 4: Handle `Priority` in `UpdateRun`**

In `UpdateRun` (line ~326), add a SET clause for `attrPriority` when `update.Priority != nil`, mirroring the existing pointer-field update expression building in this function.

- [ ] **Step 5: Run the existing dynamo tests to verify nothing regressed**

Run: `go test ./internal/store/ -run Dynamo -v`
Expected: PASS (full coverage of the new fields comes in Task 7).

- [ ] **Step 6: Commit**

```bash
git add internal/store/dynamo.go internal/store/dynamo_schema.go
git commit -m "store/dynamo: persist enqueued_at and priority"
```

### Task 5: `FindActiveByTicket` becomes `(ticket, workflow)`-scoped

**Files:**
- Modify: `internal/store/store.go:165` (interface), `internal/store/sqlite.go:488`, `internal/store/dynamo.go:565`, and all callers (`cmd/horde/main.go` launch).
- Test: conformance (Task 7) + a focused store test here.

> **Spec note:** today's `FindActiveByTicket(ctx, repo, ticket)` is ticket-only. The spec corrects duplicate detection to `(ticket, workflow)` scope (per #20: a ticket means different things across workflows). This is a deliberate, called-out behavior change, not a silent one.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/sqlite_test.go`:

```go
func TestFindActiveByTicketWorkflowScoped(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()
	for _, r := range []*Run{
		{ID: "a", Repo: "r", Ticket: "T-1", Workflow: "plan", Provider: "aws-ecs", Status: StatusRunning, LaunchedBy: "me"},
		{ID: "b", Repo: "r", Ticket: "T-1", Workflow: "impl", Provider: "aws-ecs", Status: StatusRunning, LaunchedBy: "me"},
	} {
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.FindActiveByTicket(ctx, "r", "T-1", "plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("want only run a (workflow plan), got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestFindActiveByTicketWorkflowScoped -v`
Expected: FAIL — too many args to `FindActiveByTicket` (compile error).

- [ ] **Step 3: Change the interface signature**

In `internal/store/store.go:165`:

```go
	// FindActiveByTicket returns active (pending/running) runs for the given
	// repo + ticket + workflow. Scope includes workflow because the same
	// ticket under a different workflow is a legitimately different run
	// (#20). A queued run also counts (see Task 6's update).
	FindActiveByTicket(ctx context.Context, repo, ticket, workflow string) ([]*Run, error)
```

- [ ] **Step 4: Update SQLite impl**

In `internal/store/sqlite.go:488`, add the `workflow` param and an `AND workflow = ?` clause (with the arg) to the existing query.

- [ ] **Step 5: Update DynamoDB impl**

In `internal/store/dynamo.go:565`, add the `workflow` param and an equality on workflow to the existing query's FilterExpression (mirror how `ticket` is matched on the by-ticket GSI; add `workflow` as a filter attribute).

- [ ] **Step 6: Update the caller in launch**

In `cmd/horde/main.go` (line ~315), change:

```go
			active, err := st.FindActiveByTicket(ctx, repo, ticket)
```
to:
```go
			active, err := st.FindActiveByTicket(ctx, repo, ticket, workflow)
```

- [ ] **Step 7: Run tests + build**

Run: `go build ./... && go test ./internal/store/ -run TestFindActiveByTicketWorkflowScoped -v`
Expected: build OK, test PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/store/store.go internal/store/sqlite.go internal/store/dynamo.go cmd/horde/main.go
git commit -m "store: scope FindActiveByTicket to (ticket, workflow) (#20)"
```

### Task 6: `ClaimNextQueued` — the atomic drain claim

**Files:**
- Modify: `internal/store/store.go:155-173` (interface), `internal/store/sqlite.go`, `internal/store/dynamo.go`
- Also: include `StatusQueued` in `FindActiveByTicket`'s active set (so a queued run blocks duplicates).
- Test: conformance (Task 7) + focused tests.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/sqlite_test.go`:

```go
func TestClaimNextQueuedOrderAndAtomicity(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()
	mk := func(id string, p Priority, enq time.Time) *Run {
		return &Run{ID: id, Repo: "r", Ticket: id, Workflow: "w", Provider: "aws-ecs",
			Status: StatusQueued, Priority: p, EnqueuedAt: enq, LaunchedBy: "me"}
	}
	t0 := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)
	// low-but-older, high-newer, high-older → expect high-older first.
	if err := s.CreateRun(ctx, mk("low-old", PriorityLow, t0)); err != nil { t.Fatal(err) }
	if err := s.CreateRun(ctx, mk("high-new", PriorityHigh, t0.Add(2*time.Minute))); err != nil { t.Fatal(err) }
	if err := s.CreateRun(ctx, mk("high-old", PriorityHigh, t0.Add(1*time.Minute))); err != nil { t.Fatal(err) }

	claimed, err := s.ClaimNextQueued(ctx, "r")
	if err != nil { t.Fatal(err) }
	if claimed == nil || claimed.ID != "high-old" {
		t.Fatalf("want high-old claimed first, got %+v", claimed)
	}
	if claimed.Status != StatusPending {
		t.Errorf("claimed run status = %q, want pending", claimed.Status)
	}
	// Second claim cannot re-take the same run.
	again, err := s.ClaimNextQueued(ctx, "r")
	if err != nil { t.Fatal(err) }
	if again != nil && again.ID == "high-old" {
		t.Error("high-old claimed twice — atomicity broken")
	}
}

func TestClaimNextQueuedEmpty(t *testing.T) {
	s := newTestSQLiteStore(t)
	got, err := s.ClaimNextQueued(context.Background(), "r")
	if err != nil { t.Fatal(err) }
	if got != nil {
		t.Errorf("want nil on empty queue, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestClaimNextQueued -v`
Expected: FAIL — `undefined: ClaimNextQueued`.

- [ ] **Step 3: Add to the `Store` interface**

In `internal/store/store.go`, add to the interface (after `ListActive`):

```go
	// ClaimNextQueued atomically claims the highest-priority, oldest-enqueued
	// queued run for the repo, transitioning it queued→pending, and returns it
	// (with Status already set to pending). Returns (nil, nil) when no queued
	// run is eligible. The transition is atomic: under concurrent callers
	// exactly one wins a given run; losers skip it. This is the double-launch
	// guard for both the drain Lambda and the lazy CLI drain.
	ClaimNextQueued(ctx context.Context, repo string) (*Run, error)
```

- [ ] **Step 4: Implement in SQLite (transaction)**

In `internal/store/sqlite.go`, add:

```go
func (s *SQLiteStore) ClaimNextQueued(ctx context.Context, repo string) (*Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	// Order by priority ordinal desc, then enqueued_at asc. SQLite has no map,
	// so encode the ordinal via a CASE on the stored priority string.
	row := tx.QueryRowContext(ctx, `
		SELECT id FROM runs
		WHERE repo = ? AND status = ?
		ORDER BY
			CASE priority
				WHEN 'highest' THEN 4 WHEN 'high' THEN 3 WHEN 'med' THEN 2
				WHEN 'low' THEN 1 WHEN 'lowest' THEN 0 ELSE 2 END DESC,
			enqueued_at ASC
		LIMIT 1`, repo, string(StatusQueued))
	var id string
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("selecting next queued: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE runs SET status = ? WHERE id = ? AND status = ?`,
		string(StatusPending), id, string(StatusQueued))
	if err != nil {
		return nil, fmt.Errorf("claiming queued run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Lost the race; caller retries.
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return s.GetRun(ctx, id)
}
```

Ensure `database/sql` and `errors` are imported (grep the file header).

- [ ] **Step 5: Implement in DynamoDB (conditional UpdateItem)**

In `internal/store/dynamo.go`, add:

```go
func (s *DynamoStore) ClaimNextQueued(ctx context.Context, repo string) (*Run, error) {
	// Query the by-repo GSI for queued runs; order in-memory (backlog is small,
	// bounded by what was enqueued). Try to claim candidates in order until one
	// conditional update wins.
	candidates, err := s.ListRuns(ctx, RunFilter{Repo: repo, Statuses: []Status{StatusQueued}})
	if err != nil {
		return nil, fmt.Errorf("listing queued: %w", err)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		oi, oj := candidates[i].Priority.Ordinal(), candidates[j].Priority.Ordinal()
		if oi != oj {
			return oi > oj // higher priority first
		}
		return candidates[i].EnqueuedAt.Before(candidates[j].EnqueuedAt) // oldest first
	})
	for _, c := range candidates {
		_, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:           aws.String(s.table),
			Key:                 map[string]types.AttributeValue{attrID: &types.AttributeValueMemberS{Value: c.ID}},
			UpdateExpression:    aws.String("SET #s = :pending"),
			ConditionExpression: aws.String("#s = :queued"),
			ExpressionAttributeNames:  map[string]string{"#s": attrStatus},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pending": &types.AttributeValueMemberS{Value: string(StatusPending)},
				":queued":  &types.AttributeValueMemberS{Value: string(StatusQueued)},
			},
		})
		if err != nil {
			var ccf *types.ConditionalCheckFailedException
			if errors.As(err, &ccf) {
				continue // lost the race for this candidate; try the next
			}
			return nil, fmt.Errorf("claiming queued run %s: %w", c.ID, err)
		}
		return s.GetRun(ctx, c.ID)
	}
	return nil, nil
}
```

> Match the existing import aliases (`aws`, `dynamodb`, `types`, `attrID`, `attrStatus`, `s.ddb`, `s.table`) — grep the file to confirm the exact field/const names and fix if they differ.

- [ ] **Step 6: Include `StatusQueued` as a duplicate-blocker in `FindActiveByTicket`**

Update both `FindActiveByTicket` impls so their active-status set is `{pending, running, queued}` (a queued run for the same `(ticket, workflow)` must block a duplicate enqueue). In SQLite add `queued` to the status IN-list; in DynamoDB add it to the status filter set.

- [ ] **Step 7: Run tests**

Run: `go build ./... && go test ./internal/store/ -run TestClaimNextQueued -v`
Expected: build OK, tests PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/store/store.go internal/store/sqlite.go internal/store/dynamo.go
git commit -m "store: ClaimNextQueued atomic drain claim; queued blocks duplicates"
```

### Task 7: Conformance — both stores agree

**Files:**
- Modify: `internal/store/conformance_test.go` (add cases inside `RunStoreConformance`, line ~318)

- [ ] **Step 1: Add conformance cases**

Inside `RunStoreConformance`, add `t.Run` blocks (mirror the existing style — `s := newStore(t)`):

```go
	t.Run("Queue/ClaimOrder", func(t *testing.T) {
		s := newStore(t)
		t0 := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)
		mk := func(id string, p Priority, off time.Duration) *Run {
			return &Run{ID: id, Repo: "qr", Ticket: id, Workflow: "w", Provider: "aws-ecs",
				Status: StatusQueued, Priority: p, EnqueuedAt: t0.Add(off), LaunchedBy: "me"}
		}
		for _, r := range []*Run{mk("a", PriorityLow, 0), mk("b", PriorityHigh, time.Minute), mk("c", PriorityHigh, 0)} {
			if err := s.CreateRun(ctx, r); err != nil { t.Fatal(err) }
		}
		got, err := s.ClaimNextQueued(ctx, "qr")
		if err != nil { t.Fatal(err) }
		if got == nil || got.ID != "c" {
			t.Fatalf("want c (high, oldest), got %+v", got)
		}
		if got.Status != StatusPending {
			t.Errorf("claimed status = %q, want pending", got.Status)
		}
	})

	t.Run("Queue/ClaimEmpty", func(t *testing.T) {
		s := newStore(t)
		got, err := s.ClaimNextQueued(ctx, "none")
		if err != nil { t.Fatal(err) }
		if got != nil { t.Errorf("want nil, got %+v", got) }
	})

	t.Run("Queue/QueuedBlocksDuplicate", func(t *testing.T) {
		s := newStore(t)
		if err := s.CreateRun(ctx, &Run{ID: "qd", Repo: "qr2", Ticket: "T-9", Workflow: "w",
			Provider: "aws-ecs", Status: StatusQueued, Priority: PriorityMed, LaunchedBy: "me"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.FindActiveByTicket(ctx, "qr2", "T-9", "w")
		if err != nil { t.Fatal(err) }
		if len(got) != 1 {
			t.Fatalf("queued run should block duplicate; got %d active", len(got))
		}
	})

	t.Run("Queue/StatusRoundTrip", func(t *testing.T) {
		s := newStore(t)
		for _, st := range []Status{StatusQueued, StatusCancelled} {
			id := "rt-" + string(st)
			if err := s.CreateRun(ctx, &Run{ID: id, Repo: "rtr", Ticket: id, Provider: "aws-ecs",
				Status: st, LaunchedBy: "me"}); err != nil {
				t.Fatalf("CreateRun(%s): %v", st, err)
			}
			got, err := s.GetRun(ctx, id)
			if err != nil { t.Fatal(err) }
			if got.Status != st {
				t.Errorf("status = %q, want %q", got.Status, st)
			}
		}
	})
```

- [ ] **Step 2: Run conformance against both stores**

Run: `go test ./internal/store/ -run 'Conformance|SQLite|Dynamo' -v`
Expected: PASS for both the SQLite and DynamoDB conformance invocations (grep `RunStoreConformance(` to confirm both call sites run).

- [ ] **Step 3: Run the full store package**

Run: `go test ./internal/store/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/store/conformance_test.go
git commit -m "store: conformance for queue claim order, duplicate-block, status round-trip"
```

---

## Phase 2 — Event emission (Go side)

### Task 8: `internal/event` package — types + emitter

**Files:**
- Create: `internal/event/event.go`, `internal/event/event_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/event/event_test.go`:

```go
package event

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jorge-barreto/horde/internal/store"
)

func TestDetailMarshalShape(t *testing.T) {
	cost := 1.25
	d := DetailFromRun(&store.Run{
		ID: "r1", Repo: "repo", Ticket: "T-1", Workflow: "w", Branch: "b",
		Status: store.StatusSuccess, TotalCostUSD: &cost,
		Labels: map[string]string{"epic": "E1"},
	})
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "run_id", "repo", "ticket", "workflow", "status", "total_cost_usd", "labels"} {
		if _, ok := got[k]; !ok {
			t.Errorf("detail missing key %q", k)
		}
	}
	if got["version"] != float64(1) {
		t.Errorf("version = %v, want 1", got["version"])
	}
}

func TestNopEmitterNeverErrors(t *testing.T) {
	e := NopEmitter{}
	if err := e.Emit(context.Background(), TypeRunStarted, Detail{}); err != nil {
		t.Errorf("nop emit: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/event/ -v`
Expected: FAIL — package/types undefined.

- [ ] **Step 3: Write the package**

Create `internal/event/event.go`:

```go
// Package event defines the horde run-lifecycle event contract and the
// Emitter that publishes it to the EventBridge bus. Events are the
// notification-of-truth: the store write is authoritative and happens first;
// a failed emit is logged, never fatal.
package event

import (
	"context"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

// DetailType values (EventBridge DetailType). Stable public contract.
const (
	TypeRunStarted            = "run.started"
	TypeRunTerminal           = "run.terminal"
	TypeRunCostThresholdExceeded = "run.cost-threshold-exceeded"
)

// Source is the EventBridge event Source for all horde events.
const Source = "horde"

// DetailVersion is the schema version embedded in every Detail. Bump on any
// breaking change to the field set; consumers key off it.
const DetailVersion = 1

// Detail is the JSON payload of a horde run-lifecycle event. Field names are
// the stable contract — horde vocabulary, never raw ECS shape.
type Detail struct {
	Version      int               `json:"version"`
	RunID        string            `json:"run_id"`
	Repo         string            `json:"repo"`
	Ticket       string            `json:"ticket"`
	Workflow     string            `json:"workflow"`
	Branch       string            `json:"branch"`
	Status       string            `json:"status"`
	ExitCode     *int              `json:"exit_code"`
	TotalCostUSD *float64          `json:"total_cost_usd"`
	Labels       map[string]string `json:"labels"`
	StartedAt    *time.Time        `json:"started_at"`
	CompletedAt  *time.Time        `json:"completed_at"`
	StopCode     string            `json:"stop_code,omitempty"`
	StopReason   string            `json:"stop_reason,omitempty"`
}

// DetailFromRun builds a Detail from a store.Run.
func DetailFromRun(r *store.Run) Detail {
	d := Detail{
		Version: DetailVersion, RunID: r.ID, Repo: r.Repo, Ticket: r.Ticket,
		Workflow: r.Workflow, Branch: r.Branch, Status: string(r.Status),
		ExitCode: r.ExitCode, TotalCostUSD: r.TotalCostUSD, Labels: r.Labels,
		StopCode: r.Metadata["stop_code"], StopReason: r.Metadata["stop_reason"],
	}
	if !r.StartedAt.IsZero() {
		t := r.StartedAt.UTC()
		d.StartedAt = &t
	}
	d.CompletedAt = r.CompletedAt
	return d
}

// Emitter publishes run-lifecycle events. Implementations must treat emission
// as best-effort from the caller's perspective: the caller logs an error but
// does not fail its operation.
type Emitter interface {
	Emit(ctx context.Context, detailType string, detail Detail) error
}

// NopEmitter is used when no bus is configured (e.g. docker, or ECS deployments
// predating the bus). It discards events.
type NopEmitter struct{}

func (NopEmitter) Emit(context.Context, string, Detail) error { return nil }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/event/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/event/event.go internal/event/event_test.go
git commit -m "event: run-lifecycle event contract + Emitter (nop impl)"
```

### Task 9: EventBridge emitter implementation

**Files:**
- Create: `internal/event/eventbridge.go`, `internal/event/eventbridge_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/event/eventbridge_test.go`:

```go
package event

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
)

type fakeEB struct {
	called  bool
	source  string
	detail  string
	busName string
}

func (f *fakeEB) PutEvents(ctx context.Context, in *eventbridge.PutEventsInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error) {
	f.called = true
	e := in.Entries[0]
	f.source = *e.Source
	f.detail = *e.Detail
	f.busName = *e.EventBusName
	return &eventbridge.PutEventsOutput{FailedEntryCount: 0}, nil
}

func TestEventBridgeEmitPutsEntry(t *testing.T) {
	f := &fakeEB{}
	e := &EventBridgeEmitter{client: f, busName: "horde-proj"}
	err := e.Emit(context.Background(), TypeRunTerminal, Detail{RunID: "r1", Status: "success"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.called || f.source != Source || f.busName != "horde-proj" {
		t.Fatalf("bad PutEvents call: %+v", f)
	}
	if f.detail == "" {
		t.Error("detail JSON empty")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/event/ -run TestEventBridgeEmit -v`
Expected: FAIL — `EventBridgeEmitter` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/event/eventbridge.go`:

```go
package event

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

// ebClient is the subset of the EventBridge API the emitter uses (for testing).
type ebClient interface {
	PutEvents(ctx context.Context, in *eventbridge.PutEventsInput, optFns ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error)
}

// EventBridgeEmitter publishes events to a named custom EventBridge bus.
type EventBridgeEmitter struct {
	client  ebClient
	busName string
}

// NewEventBridgeEmitter constructs an emitter for the given bus.
func NewEventBridgeEmitter(cfg aws.Config, busName string) *EventBridgeEmitter {
	return &EventBridgeEmitter{client: eventbridge.NewFromConfig(cfg), busName: busName}
}

func (e *EventBridgeEmitter) Emit(ctx context.Context, detailType string, detail Detail) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshalling event detail: %w", err)
	}
	out, err := e.client.PutEvents(ctx, &eventbridge.PutEventsInput{
		Entries: []ebtypes.PutEventsRequestEntry{{
			EventBusName: aws.String(e.busName),
			Source:       aws.String(Source),
			DetailType:   aws.String(detailType),
			Detail:       aws.String(string(b)),
		}},
	})
	if err != nil {
		return fmt.Errorf("putting event: %w", err)
	}
	if out.FailedEntryCount > 0 {
		return fmt.Errorf("eventbridge rejected %d of 1 entries", out.FailedEntryCount)
	}
	return nil
}
```

- [ ] **Step 4: Add the dependency**

Run: `go get github.com/aws/aws-sdk-go-v2/service/eventbridge`
Then: `go mod tidy`

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/event/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/event/eventbridge.go internal/event/eventbridge_test.go go.mod go.sum
git commit -m "event: EventBridge emitter implementation"
```

### Task 10: Wire bus name into config + emit `run.started` on direct launch

**Files:**
- Modify: `internal/config/ssm.go` (parse `eventBusName`), `internal/config/ssm_test.go`
- Modify: `cmd/horde/main.go` (build emitter; emit `run.started` after `queued→running` on direct launch)

- [ ] **Step 1: Write the failing config test**

Add to `internal/config/ssm_test.go` a case asserting a `event_bus_name` (or the existing JSON key convention — grep the file for how `max_concurrent` is named) field parses into the config struct. Follow the existing SSM-parse test exactly.

```go
func TestSSMParsesEventBusName(t *testing.T) {
	cfg, err := parseHordeConfig([]byte(`{"repo":"r","max_concurrent":5,"event_bus_name":"horde-proj"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EventBusName != "horde-proj" {
		t.Errorf("EventBusName = %q, want horde-proj", cfg.EventBusName)
	}
}
```

> Use the actual parse function/struct names from `ssm.go` (grep `func parse` / the config struct). Match field-tag style.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestSSMParsesEventBusName -v`
Expected: FAIL — `EventBusName` undefined.

- [ ] **Step 3: Add the config field**

In `internal/config/ssm.go`, add `EventBusName string` with the JSON tag matching the convention (e.g. `json:"event_bus_name"`), as an **optional** field (absent ⇒ empty ⇒ NopEmitter). Add `MaxSpendPerWindow float64` and `SpendWindow string` here too (used in Phase 4) with optional tags.

- [ ] **Step 4: Run config test**

Run: `go test ./internal/config/ -run TestSSMParsesEventBusName -v`
Expected: PASS.

- [ ] **Step 5: Build the emitter in the CLI**

In `cmd/horde/main.go`, add a helper near `initProviderAndStore`:

```go
// newEmitter returns an EventBridge emitter when the deployment configures a
// bus (ECS), else a no-op (docker, or pre-bus deployments).
func newEmitter(awsCfg aws.Config, hordeCfg *config.HordeConfig) event.Emitter {
	if hordeCfg != nil && hordeCfg.EventBusName != "" {
		return event.NewEventBridgeEmitter(awsCfg, hordeCfg.EventBusName)
	}
	return event.NopEmitter{}
}
```

> Match the actual `HordeConfig` type name and `awsCfg` type from `initProviderAndStore`'s return. Add imports for `internal/event` and the aws config type.

- [ ] **Step 6: Emit `run.started` after a direct launch reaches running**

In `cmd/horde/main.go`, right after the `queued→running` `UpdateRun` succeeds (line ~409-416), add:

```go
			emitter := newEmitter(awsCfg, hordeCfg)
			run.Status = store.StatusRunning
			run.InstanceID = result.InstanceID
			if err := emitter.Emit(ctx, event.TypeRunStarted, event.DetailFromRun(run)); err != nil {
				fmt.Fprintf(os.Stderr, "warning: emitting run.started: %v\n", err)
			}
```

(Best-effort: a failed emit warns, never fails the launch.)

- [ ] **Step 7: Build + run**

Run: `go build ./... && go test ./cmd/horde/ ./internal/config/ ./internal/event/`
Expected: build OK, tests PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/config/ssm.go internal/config/ssm_test.go cmd/horde/main.go
git commit -m "config+cli: event bus config; emit run.started on direct launch"
```

---

## Phase 3 — CLI submit + queue commands

### Task 11: `horde launch --enqueue` + `--priority`

**Files:**
- Modify: `cmd/horde/main.go` (launch flags + enqueue branch), `cmd/horde/jsonv1.go` (`queued` status)
- Test: `cmd/horde/jsonv1_test.go`, `cmd/horde/main_test.go`

- [ ] **Step 1: Write the failing JSON-contract test**

Add to `cmd/horde/jsonv1_test.go`:

```go
func TestLaunchQueuedV1Shape(t *testing.T) {
	v := launchQueuedV1("run123", "T-1", "impl", "main", "high")
	if v.Status != "queued" {
		t.Errorf("status = %q, want queued", v.Status)
	}
	if v.RunID == nil || *v.RunID != "run123" {
		t.Errorf("run_id = %v, want run123", v.RunID)
	}
	if v.Priority != "high" {
		t.Errorf("priority = %q, want high", v.Priority)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/horde/ -run TestLaunchQueuedV1Shape -v`
Expected: FAIL — `launchQueuedV1` undefined.

- [ ] **Step 3: Extend `LaunchV1` + add constructor**

In `cmd/horde/jsonv1.go`, add a `Priority` field to `LaunchV1`:

```go
	Priority string `json:"priority,omitempty"`
```

And add (after `launchDuplicateV1`):

```go
// launchQueuedV1 reports a launch parked in the server-side backlog
// (`--enqueue`). Exit 0 — a protocol-level outcome like capped/duplicate.
func launchQueuedV1(runID, ticket, workflow, branch, priority string) LaunchV1 {
	return LaunchV1{Status: "queued", RunID: &runID, Ticket: ticket, Workflow: workflow, Branch: branch, Priority: priority}
}
```

- [ ] **Step 4: Add `--enqueue` / `--priority` flags**

In `launchCmd()` `Flags` (line ~182), add:

```go
			&cli.BoolFlag{
				Name:  "enqueue",
				Usage: "Park the launch in the server-side queue (aws-ecs only); it runs when a slot frees. Exits 0 with status 'queued' under --json.",
			},
			&cli.StringFlag{
				Name:  "priority",
				Usage: "Queue priority for --enqueue: lowest|low|med|high|highest (default med). Higher drains first.",
			},
```

- [ ] **Step 5: Add the enqueue branch in the Action**

In `launchCmd()`'s Action, after the duplicate check (line ~342, after `active = stillActive` and its `len(active) > 0` block) and before the `now := time.Now()` run-creation, insert the enqueue path. Read the flags near the other flag reads (line ~221):

```go
			enqueue := cmd.Bool("enqueue")
			priority, err := store.ParsePriority(cmd.String("priority"))
			if err != nil {
				return err
			}
```

Then the branch (after duplicate handling):

```go
			if enqueue {
				if provName != "aws-ecs" {
					return fmt.Errorf("--enqueue requires the aws-ecs provider; docker has no event source to drain the queue")
				}
				now := time.Now()
				qrun := &store.Run{
					ID: id, Repo: repo, Ticket: ticket, Branch: branch, Workflow: workflow,
					Provider: provName, Status: store.StatusQueued, Labels: labels,
					LaunchedBy: launchedBy, Priority: priority, EnqueuedAt: now,
				}
				if err := st.CreateRun(ctx, qrun); err != nil {
					return fmt.Errorf("enqueuing run: %w", err)
				}
				if jsonOut {
					return writeJSONTo(cmd.Writer, launchQueuedV1(id, ticket, workflow, branch, string(priority)))
				}
				fmt.Printf("%s (queued, priority %s)\n", id, priority)
				return nil
			}
```

> Note: an enqueued run does NOT pass through the concurrency gate above. The gate at line ~250 runs before this branch; that's acceptable because a `queued` run does not count toward active and the gate only *rejects a direct launch*. But to avoid a confusing "capped" on `--enqueue`, move the `enqueue` flag read up and **skip the concurrency-gate block when `enqueue` is true** (wrap the gate `if activeCount >= maxConcurrent {…}` in `if !enqueue && activeCount >= maxConcurrent`). Make that change in this step.

- [ ] **Step 6: Write a launch-enqueue integration-style test**

Add to `cmd/horde/main_test.go` a test that runs the launch command with `--enqueue --json` against the docker provider and asserts the ECS-only error envelope (docker rejects enqueue). Follow the existing CLI-invocation test harness in this file (grep for how other `--json` command tests build and run the app).

```go
func TestLaunchEnqueueRejectedOnDocker(t *testing.T) {
	// ... build app with docker provider, run: launch --enqueue --json --workflow w T-1
	// assert stdout JSON {"status":"error", "reason": contains "--enqueue requires the aws-ecs provider"}
}
```

> Match the existing test harness precisely; if the harness can't easily force the docker provider, assert via the unit-level error string instead.

- [ ] **Step 7: Build + test**

Run: `go build ./... && go test ./cmd/horde/ -run 'TestLaunchQueued|TestLaunchEnqueue' -v`
Expected: build OK, PASS.

- [ ] **Step 8: Commit**

```bash
git add cmd/horde/main.go cmd/horde/jsonv1.go cmd/horde/jsonv1_test.go cmd/horde/main_test.go
git commit -m "cli: horde launch --enqueue/--priority; queued JSON status; ECS-only guard"
```

### Task 12: `horde queue` command group

**Files:**
- Create: `cmd/horde/queue.go`, `cmd/horde/queue_test.go`
- Modify: `cmd/horde/main.go` (register `queueCmd()` in the app's command list)

- [ ] **Step 1: Write the failing test**

Create `cmd/horde/queue_test.go`:

```go
package main

import "testing"

func TestQueueCmdRegistered(t *testing.T) {
	app := newApp() // existing constructor that builds the *cli.Command root; grep main.go
	var found bool
	for _, c := range app.Commands {
		if c.Name == "queue" {
			found = true
			// subcommands present
			subs := map[string]bool{}
			for _, sc := range c.Commands {
				subs[sc.Name] = true
			}
			for _, want := range []string{"list", "prioritize", "cancel"} {
				if !subs[want] {
					t.Errorf("queue missing subcommand %q", want)
				}
			}
		}
	}
	if !found {
		t.Error("queue command not registered")
	}
}
```

> Replace `newApp()` with the actual root-command constructor (grep `main.go` for `&cli.Command{` at top level or a `func newApp`/`func rootCmd`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/horde/ -run TestQueueCmdRegistered -v`
Expected: FAIL — `queue` not registered.

- [ ] **Step 3: Write the queue command group**

Create `cmd/horde/queue.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jorge-barreto/horde/internal/store"
	"github.com/urfave/cli/v3"
)

func queueCmd() *cli.Command {
	return &cli.Command{
		Name:  "queue",
		Usage: "Inspect and steer the server-side launch queue (aws-ecs)",
		Commands: []*cli.Command{
			queueListCmd(),
			queuePrioritizeCmd(),
			queueCancelCmd(),
		},
	}
}

func queueListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List queued runs in drain order (priority desc, oldest first)",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			_, st, _, _, _, hordeCfg, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			resolver := newResolver(cmd)
			repo, err := resolveCanonicalRepo(hordeCfg, resolver)
			if err != nil {
				return err
			}
			runs, err := st.ListRuns(ctx, store.RunFilter{Repo: repo, Statuses: []store.Status{store.StatusQueued}})
			if err != nil {
				return fmt.Errorf("listing queued runs: %w", err)
			}
			sortByDrainOrder(runs)
			if cmd.Bool("json") {
				return writeJSONTo(cmd.Writer, queueListV1(runs))
			}
			for _, r := range runs {
				fmt.Printf("%s  %-8s  %s  %s\n", r.ID, r.Priority, r.Ticket, r.Workflow)
			}
			return nil
		},
	}
}

func queuePrioritizeCmd() *cli.Command {
	return &cli.Command{
		Name:      "prioritize",
		Usage:     "Change the priority of a queued run",
		ArgsUsage: "<run-id> --priority <level>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "priority", Usage: "lowest|low|med|high|highest"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.Args().First()
			if id == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}
			p, err := store.ParsePriority(cmd.String("priority"))
			if err != nil {
				return err
			}
			if cmd.String("priority") == "" {
				return fmt.Errorf("--priority is required")
			}
			_, st, _, _, _, _, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			run, err := st.GetRun(ctx, id)
			if err != nil {
				return err
			}
			if run.Status != store.StatusQueued {
				return fmt.Errorf("run %s is %s, not queued — only queued runs can be reprioritized", id, run.Status)
			}
			if err := st.UpdateRun(ctx, id, &store.RunUpdate{Priority: &p}); err != nil {
				return fmt.Errorf("updating priority: %w", err)
			}
			if cmd.Bool("json") {
				return writeJSONTo(cmd.Writer, queuePrioritizeV1(id, string(p)))
			}
			fmt.Fprintf(os.Stdout, "%s priority set to %s\n", id, p)
			return nil
		},
	}
}

func queueCancelCmd() *cli.Command {
	return &cli.Command{
		Name:      "cancel",
		Usage:     "Cancel a queued run before it ever runs",
		ArgsUsage: "<run-id>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.Args().First()
			if id == "" {
				return fmt.Errorf("missing required argument: <run-id>")
			}
			_, st, _, _, _, _, cleanup, err := initProviderAndStore(ctx, cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			run, err := st.GetRun(ctx, id)
			if err != nil {
				return err
			}
			if run.Status != store.StatusQueued {
				return fmt.Errorf("run %s is %s, not queued — only queued runs can be cancelled (use 'horde kill' for a running task)", id, run.Status)
			}
			cancelled := store.StatusCancelled
			if err := st.UpdateRun(ctx, id, &store.RunUpdate{Status: &cancelled}); err != nil {
				return fmt.Errorf("cancelling run: %w", err)
			}
			if cmd.Bool("json") {
				return writeJSONTo(cmd.Writer, queueCancelV1(id))
			}
			fmt.Fprintf(os.Stdout, "%s cancelled\n", id)
			return nil
		},
	}
}
```

> **Adapt the `initProviderAndStore` return arity** to match its real signature (the launch code shows it returns `prov, st, maxConcurrent, provName, awsCfg, hordeCfg, cleanup, err`). Use blank identifiers for the values each subcommand doesn't need, matching the exact order.

- [ ] **Step 4: Add the JSON helpers**

In `cmd/horde/jsonv1.go`, add:

```go
// QueueListV1 / QueuePrioritizeV1 / QueueCancelV1 are the queue command JSON contracts.
type QueueListItemV1 struct {
	RunID      string `json:"run_id"`
	Ticket     string `json:"ticket"`
	Workflow   string `json:"workflow"`
	Priority   string `json:"priority"`
	EnqueuedAt string `json:"enqueued_at"`
}
type QueueListV1 struct {
	Status string            `json:"status"`
	Queued []QueueListItemV1 `json:"queued"`
}

func queueListV1(runs []*store.Run) QueueListV1 {
	out := QueueListV1{Status: "ok", Queued: make([]QueueListItemV1, 0, len(runs))}
	for _, r := range runs {
		out.Queued = append(out.Queued, QueueListItemV1{
			RunID: r.ID, Ticket: r.Ticket, Workflow: r.Workflow,
			Priority: string(r.Priority), EnqueuedAt: r.EnqueuedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

type QueuePrioritizeV1 struct {
	Status   string `json:"status"`
	RunID    string `json:"run_id"`
	Priority string `json:"priority"`
}

func queuePrioritizeV1(runID, priority string) QueuePrioritizeV1 {
	return QueuePrioritizeV1{Status: "reprioritized", RunID: runID, Priority: priority}
}

type QueueCancelV1 struct {
	Status string `json:"status"`
	RunID  string `json:"run_id"`
}

func queueCancelV1(runID string) QueueCancelV1 {
	return QueueCancelV1{Status: "cancelled", RunID: runID}
}
```

- [ ] **Step 5: Add the shared drain-order sort**

Create the sort in `cmd/horde/drain.go` (Task 13 will add more there; create the file now with just this):

```go
package main

import (
	"sort"

	"github.com/jorge-barreto/horde/internal/store"
)

// sortByDrainOrder orders runs the way the drain claims them: highest priority
// first, then oldest enqueued_at. Used by `horde queue list` for an accurate
// preview of what launches next.
func sortByDrainOrder(runs []*store.Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		oi, oj := runs[i].Priority.Ordinal(), runs[j].Priority.Ordinal()
		if oi != oj {
			return oi > oj
		}
		return runs[i].EnqueuedAt.Before(runs[j].EnqueuedAt)
	})
}
```

- [ ] **Step 6: Register the command**

In `cmd/horde/main.go`, add `queueCmd()` to the root command's `Commands` slice (grep for where `launchCmd()`, `retryCmd()` etc. are listed) — insert alphabetically or next to `launch`.

- [ ] **Step 7: Build + test**

Run: `go build ./... && go test ./cmd/horde/ -run TestQueueCmdRegistered -v`
Expected: build OK, PASS.

- [ ] **Step 8: Commit**

```bash
git add cmd/horde/queue.go cmd/horde/queue_test.go cmd/horde/jsonv1.go cmd/horde/drain.go cmd/horde/main.go
git commit -m "cli: horde queue list/prioritize/cancel command group"
```

### Task 13: Queue command behavior tests

**Files:**
- Modify: `cmd/horde/queue_test.go`

- [ ] **Step 1: Write the tests**

Add behavior tests using the package's existing CLI test harness (grep `queue_test.go`/`main_test.go` for the harness that injects a fake/sqlite store). Cover:

```go
// TestQueuePrioritizeRejectsNonQueued: GetRun returns a running run -> prioritize errors with "not queued".
// TestQueueCancelRejectsNonQueued: cancel a success run -> errors.
// TestQueueCancelSetsCancelled: cancel a queued run -> status becomes cancelled.
// TestQueuePrioritizeRequiresPriorityFlag: missing --priority -> error.
```

Implement each by driving the command with an in-memory/sqlite store seeded with the relevant run, then asserting the returned error or resulting store state. Match the harness style already used in this package.

- [ ] **Step 2: Run tests**

Run: `go test ./cmd/horde/ -run TestQueue -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add cmd/horde/queue_test.go
git commit -m "cli: queue command behavior tests (reject non-queued, cancel sets cancelled)"
```

---

## Phase 4 — Lazy CLI drain + spend cap

### Task 14: `drainOnce` lazy drain helper (capacity-gated)

**Files:**
- Modify: `cmd/horde/drain.go`, create `cmd/horde/drain_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/horde/drain_test.go`:

```go
package main

import (
	"context"
	"testing"

	"github.com/jorge-barreto/horde/internal/store"
)

func TestDrainOnceRespectsCapacity(t *testing.T) {
	st := newMemStore(t) // package fake/sqlite helper — match existing
	ctx := context.Background()
	// At capacity: one running + maxConcurrent=1 → no drain.
	_ = st.CreateRun(ctx, &store.Run{ID: "run", Repo: "r", Ticket: "A", Workflow: "w", Provider: "aws-ecs", Status: store.StatusRunning, LaunchedBy: "me"})
	_ = st.CreateRun(ctx, &store.Run{ID: "q", Repo: "r", Ticket: "B", Workflow: "w", Provider: "aws-ecs", Status: store.StatusQueued, Priority: store.PriorityMed, LaunchedBy: "me"})

	launched := false
	drainOnce(ctx, drainDeps{
		store:         st,
		repo:          "r",
		maxConcurrent: 1,
		launch: func(ctx context.Context, run *store.Run) error { launched = true; return nil },
		spendOK: func(ctx context.Context) (bool, error) { return true, nil },
		emit:    func(string, *store.Run) {},
	})
	if launched {
		t.Error("drained despite being at capacity")
	}
}

func TestDrainOnceLaunchesWhenSlotFree(t *testing.T) {
	st := newMemStore(t)
	ctx := context.Background()
	_ = st.CreateRun(ctx, &store.Run{ID: "q", Repo: "r", Ticket: "B", Workflow: "w", Provider: "aws-ecs", Status: store.StatusQueued, Priority: store.PriorityMed, LaunchedBy: "me"})
	var launchedID string
	drainOnce(ctx, drainDeps{
		store: st, repo: "r", maxConcurrent: 5,
		launch:  func(ctx context.Context, run *store.Run) error { launchedID = run.ID; return nil },
		spendOK: func(ctx context.Context) (bool, error) { return true, nil },
		emit:    func(string, *store.Run) {},
	})
	if launchedID != "q" {
		t.Errorf("expected to drain q, launched %q", launchedID)
	}
}
```

> Replace `newMemStore` with the existing store test helper in `cmd/horde` (grep test files; the package already constructs stores for command tests).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/horde/ -run TestDrainOnce -v`
Expected: FAIL — `drainOnce`/`drainDeps` undefined.

- [ ] **Step 3: Implement `drainOnce`**

Append to `cmd/horde/drain.go`:

```go
import (
	"context"
	"fmt"
	"os"
	// (keep existing "sort"/store imports; merge import blocks)
)

// drainDeps are the injected dependencies of drainOnce, so it is testable
// without a live provider or AWS. launch starts the run; spendOK is the
// realized spend-rate gate; emit fires lifecycle events (run.started /
// run.cost-threshold-exceeded).
type drainDeps struct {
	store         store.Store
	repo          string
	maxConcurrent int
	launch        func(ctx context.Context, run *store.Run) error
	spendOK       func(ctx context.Context) (bool, error)
	emit          func(detailType string, run *store.Run)
}

// drainOnce drains at most one queued run for the repo if capacity and budget
// allow. It is the lazy CLI backstop (invoked from launch/list) and mirrors the
// drain Lambda's logic. Best-effort: errors are logged, never returned to the
// caller's command (a failed drain must not fail an unrelated `horde list`).
func drainOnce(ctx context.Context, d drainDeps) {
	active, err := d.store.CountActive(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: drain capacity check: %v\n", err)
		return
	}
	if active >= d.maxConcurrent {
		return // no slot
	}
	ok, err := d.spendOK(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: drain spend check: %v\n", err)
		return
	}
	if !ok {
		// Held by budget: signal it, leave runs queued.
		if next, _ := peekNextQueued(ctx, d.store, d.repo); next != nil {
			d.emit("run.cost-threshold-exceeded", next)
		}
		return
	}
	run, err := d.store.ClaimNextQueued(ctx, d.repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: claiming queued run: %v\n", err)
		return
	}
	if run == nil {
		return // empty queue
	}
	if err := d.launch(ctx, run); err != nil {
		// Launch failed after claim: mark failed (NOT re-queued — avoid a poison loop).
		failed := store.StatusFailed
		now := timeNow()
		md := map[string]string{"drain_launch_error": err.Error()}
		if uerr := d.store.UpdateRun(ctx, run.ID, &store.RunUpdate{Status: &failed, CompletedAt: &now, Metadata: md}); uerr != nil {
			fmt.Fprintf(os.Stderr, "warning: marking drained run failed: %v\n", uerr)
		}
		fmt.Fprintf(os.Stderr, "warning: launching drained run %s: %v\n", run.ID, err)
		return
	}
	d.emit("run.started", run)
}

// peekNextQueued returns the next run the drain WOULD claim, without claiming.
func peekNextQueued(ctx context.Context, st store.Store, repo string) (*store.Run, error) {
	runs, err := st.ListRuns(ctx, store.RunFilter{Repo: repo, Statuses: []store.Status{store.StatusQueued}})
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	sortByDrainOrder(runs)
	return runs[0], nil
}

// timeNow is a seam for tests.
var timeNow = func() time.Time { return time.Now() }
```

Add `"time"` to the import block.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/horde/ -run TestDrainOnce -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/horde/drain.go cmd/horde/drain_test.go
git commit -m "cli: drainOnce lazy-drain helper (capacity + budget gated, launch-fail→failed)"
```

### Task 15: Realized spend-rate gate

**Files:**
- Modify: `cmd/horde/drain.go` (real `spendOK`), `cmd/horde/drain_test.go`

- [ ] **Step 1: Write the failing test**

Add to `cmd/horde/drain_test.go`:

```go
func TestRealizedSpendOK(t *testing.T) {
	st := newMemStore(t)
	ctx := context.Background()
	cost := func(c float64) *float64 { return &c }
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { tt := now.Add(d); return &tt }
	// $30 inside window, $100 outside.
	_ = st.CreateRun(ctx, &store.Run{ID: "in", Repo: "r", Ticket: "A", Provider: "aws-ecs", Status: store.StatusSuccess, LaunchedBy: "me", TotalCostUSD: cost(30), CompletedAt: at(-1 * time.Hour)})
	_ = st.CreateRun(ctx, &store.Run{ID: "out", Repo: "r", Ticket: "B", Provider: "aws-ecs", Status: store.StatusSuccess, LaunchedBy: "me", TotalCostUSD: cost(100), CompletedAt: at(-48 * time.Hour)})

	gate := realizedSpendGate(st, "r", 50, 24*time.Hour, func() time.Time { return now })
	ok, err := gate(ctx)
	if err != nil { t.Fatal(err) }
	if !ok {
		t.Error("want OK: $30 in-window < $50 cap (the $100 is 48h ago, outside)")
	}

	gate2 := realizedSpendGate(st, "r", 20, 24*time.Hour, func() time.Time { return now })
	ok2, _ := gate2(ctx)
	if ok2 {
		t.Error("want held: $30 in-window >= $20 cap")
	}
}

func TestNoSpendCapAlwaysOK(t *testing.T) {
	gate := realizedSpendGate(newMemStore(t), "r", 0, 0, time.Now)
	ok, err := gate(context.Background())
	if err != nil || !ok {
		t.Errorf("no cap configured must always pass; ok=%v err=%v", ok, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/horde/ -run 'TestRealizedSpend|TestNoSpendCap' -v`
Expected: FAIL — `realizedSpendGate` undefined.

- [ ] **Step 3: Implement the gate**

Append to `cmd/horde/drain.go`:

```go
// realizedSpendGate builds a spendOK function that sums TotalCostUSD of runs
// completed within the trailing window and reports whether the project is under
// the cap. Realized-only: in-flight runs contribute $0 until they finish (the
// concurrency limit is the blast-radius backstop; see follow-on #52). A cap of
// 0 or a zero window means "no spend cap configured" — always OK.
func realizedSpendGate(st store.Store, repo string, cap float64, window time.Duration, now func() time.Time) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		if cap <= 0 || window <= 0 {
			return true, nil
		}
		since := now().Add(-window)
		runs, err := st.ListRuns(ctx, store.RunFilter{Repo: repo})
		if err != nil {
			return false, fmt.Errorf("listing runs for spend window: %w", err)
		}
		var total float64
		for _, r := range runs {
			if r.TotalCostUSD == nil || r.CompletedAt == nil {
				continue
			}
			if r.CompletedAt.Before(since) {
				continue
			}
			total += *r.TotalCostUSD
		}
		return total < cap, nil
	}
}
```

> Note `RunFilter.Since`/`Until` filter on `StartedAt`, not `CompletedAt`, so the window filtering is done in-loop on `CompletedAt` here (correct per the spec: the window is keyed on completion).

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/horde/ -run 'TestRealizedSpend|TestNoSpendCap' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/horde/drain.go cmd/horde/drain_test.go
git commit -m "cli: realized spend-rate gate (trailing window on completed_at)"
```

### Task 16: Invoke lazy drain from `launch` and `list`

**Files:**
- Modify: `cmd/horde/main.go` (launch Action tail + list Action)

- [ ] **Step 1: Add a wiring helper**

In `cmd/horde/drain.go`, add a function that assembles `drainDeps` from live components (store, provider, emitter, config) and calls `drainOnce`. The `launch` closure must perform the real provider launch + `queued→running` update + re-resolved config (mirror the direct-launch path in `launchCmd`). Keep it small:

```go
// lazyDrain runs one opportunistic drain pass using live components. Best-effort.
func lazyDrain(ctx context.Context, lc lazyDrainComponents) {
	if lc.provName != "aws-ecs" {
		return // queue/drain is ECS-only
	}
	window, _ := time.ParseDuration(lc.spendWindow) // empty → 0 → no cap
	drainOnce(ctx, drainDeps{
		store: lc.store, repo: lc.repo, maxConcurrent: lc.maxConcurrent,
		spendOK: realizedSpendGate(lc.store, lc.repo, lc.maxSpend, window, time.Now),
		launch:  lc.launchFn,
		emit:    func(dt string, r *store.Run) {
			if err := lc.emitter.Emit(ctx, dt, event.DetailFromRun(r)); err != nil {
				fmt.Fprintf(os.Stderr, "warning: emitting %s: %v\n", dt, err)
			}
		},
	})
}

type lazyDrainComponents struct {
	store         store.Store
	repo          string
	provName      string
	maxConcurrent int
	maxSpend      float64
	spendWindow   string
	emitter       event.Emitter
	launchFn      func(ctx context.Context, run *store.Run) error
}
```

Add the `internal/event` import.

- [ ] **Step 2: Build the launch closure once and reuse it**

In `cmd/horde/main.go`, extract the provider-launch + running-update sequence (lines ~362-416) into a method/closure `launchRun(ctx, run) error` that both the direct path and the drain reuse, so re-resolution and emission are identical. The closure: re-resolve secrets/mounts, set `StartedAt`/`TimeoutAt = now`, call `prov.Launch`, `UpdateRun` to running with InstanceID/Metadata. (DRY: the drain and direct launch must not diverge.)

- [ ] **Step 3: Call `lazyDrain` at the end of `launch` and `list`**

At the end of `launchCmd`'s Action (after the success print) and at the end of `listCmd`'s Action (after results render), add a best-effort `lazyDrain(ctx, components)` call. For `list`, build the components the same way (it already initializes provider+store; add emitter + config-derived caps).

> Keep these calls strictly best-effort and AFTER the command's own output, so a drain hiccup never corrupts `--json` stdout.

- [ ] **Step 4: Build + run unit tests**

Run: `go build ./... && go test ./cmd/horde/...`
Expected: build OK, PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/horde/main.go cmd/horde/drain.go
git commit -m "cli: lazy drain on launch/list (ECS-only, best-effort backstop)"
```

---

## Phase 5 — CDK infra + drain Lambda (TS)

### Task 17: Status Lambda emits `run.terminal`

**Files:**
- Modify: `cdk/src/status-lambda/index.ts` (after the terminal write, line ~313-320), `cdk/src/status-lambda/index.test.ts`

- [ ] **Step 1: Write the failing test**

In `cdk/src/status-lambda/index.test.ts`, add a case (mirror the existing handler tests; they mock the DynamoDB client) that mocks the EventBridge client and asserts a `PutEvents` with `Source: "horde"`, `DetailType: "run.terminal"`, and a `Detail` JSON containing the mapped `status` and `run_id`. Use the existing aws-sdk-client-mock setup in this test file.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cdk && npm run build && npm test -- status-lambda`
Expected: FAIL — no PutEvents emitted.

- [ ] **Step 3: Add emission to the handler**

In `cdk/src/status-lambda/index.ts`:
- Import EventBridge: `import { EventBridgeClient, PutEventsCommand } from "@aws-sdk/client-eventbridge";`
- Add `const EVENT_BUS_NAME = process.env.EVENT_BUS_NAME ?? "";` near the other env reads (line ~33).
- Add `const eb = new EventBridgeClient({});` near the other clients (line ~44).
- After the conditional terminal `UpdateItemCommand` succeeds (line ~313-320), emit best-effort (the store write stays authoritative):

```ts
    if (EVENT_BUS_NAME) {
      try {
        await eb.send(new PutEventsCommand({
          Entries: [{
            EventBusName: EVENT_BUS_NAME,
            Source: "horde",
            DetailType: "run.terminal",
            Detail: JSON.stringify({
              version: 1,
              run_id: runId,
              repo: runRepo, // REQUIRED: the drain Lambda Queries the by-repo GSI
                             // to find this repo's queued backlog. Read repo from
                             // the run row fetched by findRunId (add it to that
                             // read if not already returned).
              status: terminalStatus,
              exit_code: exitCode ?? null,
              total_cost_usd: totalCost ?? null,
              stop_code: stopCode ?? "",
              stop_reason: stopReason ?? "",
            }),
          }],
        }));
      } catch (err) {
        console.error("emit run.terminal failed (non-fatal):", err);
      }
    }
```

> Use the actual local variable names from the handler for `runId`, `terminalStatus`, `exitCode`, `totalCost`, `stopCode`, `stopReason` — grep the handler body and match. Emit only on the branch where the terminal write actually applied (not when the run was already terminal — the conditional-check-failed path should not re-emit).

- [ ] **Step 4: Run test to verify it passes**

Run: `cd cdk && npm run build && npm test -- status-lambda`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cdk/src/status-lambda/index.ts cdk/src/status-lambda/index.test.ts
git commit -m "cdk/status-lambda: emit run.terminal to the event bus (best-effort)"
```

### Task 18: Drain Lambda

**Files:**
- Create: `cdk/src/drain-lambda/index.ts`, `cdk/src/drain-lambda/index.test.ts`

- [ ] **Step 1: Write the failing test**

Create `cdk/src/drain-lambda/index.test.ts` (mirror status-lambda test setup — aws-sdk-client-mock for DynamoDB + EventBridge):

```ts
// Cases:
// 1. Slot free + queued run exists → claims highest-priority oldest (conditional
//    UpdateItem queued→pending), then would RunTask, then emits run.started.
// 2. At capacity (active >= maxConcurrent) → no claim, no RunTask.
// 3. Conditional check fails on first candidate → tries next.
```

> Build these against the handler's exported functions. Keep RunTask mocked.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cdk && npm run build && npm test -- drain-lambda`
Expected: FAIL — module missing.

- [ ] **Step 3: Write the drain Lambda**

Create `cdk/src/drain-lambda/index.ts`. Responsibilities (reuse the status-lambda patterns for env + clients):
- Env: `RUNS_TABLE`, `EVENT_BUS_NAME`, `MAX_CONCURRENT`, `MAX_SPEND_PER_WINDOW`, `SPEND_WINDOW_SECONDS`, plus the RunTask inputs the construct injects (`CLUSTER_ARN`, `TASK_DEF_ARN`, `SUBNETS`, `SECURITY_GROUP`, `ASSIGN_PUBLIC_IP`, container name).
- On a `run.terminal` event: count active (Query by-status GSI for pending+running, sum); if `>= MAX_CONCURRENT`, return.
- Realized spend gate: if a cap is set, Query by-repo GSI, sum `total_cost_usd` where `completed_at >= now - window`; if `>= cap`, emit `run.cost-threshold-exceeded` for the next queued run and return.
- Claim: Query by-repo GSI for `status = queued`, sort highest-priority-then-oldest in-memory, conditional `UpdateItem` (`SET #s = :pending` cond `#s = :queued`) walking candidates until one wins.
- On a win: `RunTask` (mirror the Go ECS provider's RunTaskInput shape — container env REPO_URL/TICKET/BRANCH/WORKFLOW/RUN_ID, tags), then `UpdateItem` set instance_id + started_at + status=running, then emit `run.started`.
- All best-effort logging on failure; a RunTask failure marks the run `failed` (not re-queued).

> The drain finds queued runs by Querying the by-repo GSI; it gets the `repo` from the `run.terminal` event Detail (Task 17 includes `repo` in the emitted Detail). Read `event.detail.repo` and Query that repo's `status = queued` rows.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd cdk && npm run build && npm test -- drain-lambda`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cdk/src/drain-lambda/index.ts cdk/src/drain-lambda/index.test.ts cdk/src/status-lambda/index.ts
git commit -m "cdk/drain-lambda: drain queue on run.terminal (capacity+budget gated)"
```

### Task 19: Construct wiring — bus, rule, drain Lambda, IAM, props

**Files:**
- Modify: `cdk/src/horde-worker.ts`, `cdk/src/horde-worker-props.ts`
- Create: `cdk/test/horde-worker-events.test.ts`

- [ ] **Step 1: Write the failing synth test**

Create `cdk/test/horde-worker-events.test.ts` (mirror `cdk/test/horde-worker-sidecars.test.ts` — `Template.fromStack`):

```ts
// Assert the synthesized template has:
// - an AWS::Events::EventBus
// - the status Lambda env includes EVENT_BUS_NAME
// - a Lambda function for the drain (Environment with RUNS_TABLE + EVENT_BUS_NAME + MAX_CONCURRENT)
// - an AWS::Events::Rule with EventPattern source ["horde"], detail-type ["run.terminal"], targeting the drain Lambda
// - drain Lambda IAM allows dynamodb UpdateItem/Query + ecs:RunTask + iam:PassRole + events:PutEvents
// - status Lambda IAM gains events:PutEvents
// - when props.maxSpendPerWindow set, drain env MAX_SPEND_PER_WINDOW is present
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cdk && npm run build && npm test -- horde-worker-events`
Expected: FAIL — resources absent.

- [ ] **Step 3: Add props**

In `cdk/src/horde-worker-props.ts`, add (JSDoc following the sidecars style):

```ts
  /**
   * Maximum realized spend (USD) over `spendWindow` before the queue drain is
   * held. Realized-only: in-flight runs are uncosted until they finish. Omit to
   * disable the spend cap (concurrency-only gating).
   */
  readonly maxSpendPerWindow?: number;
  /**
   * Trailing window for `maxSpendPerWindow`, as a CDK Duration. Defaults to 24h
   * when maxSpendPerWindow is set. Ignored without maxSpendPerWindow.
   */
  readonly spendWindow?: Duration;
```

Add the `Duration` import from `aws-cdk-lib` if not already present.

- [ ] **Step 4: Create the bus + wire the status Lambda env**

In `cdk/src/horde-worker.ts`, where the status Lambda is created (line ~462-479):
- Create `const bus = new events.EventBus(this, "RunEventBus", { eventBusName: \`horde-${slug}\` });` (use the construct's existing slug/name source; grep how the cluster/table are named).
- Add `EVENT_BUS_NAME: bus.eventBusName` to the status Lambda's `environment`.
- Grant `events:PutEvents` on the bus to the status Lambda role (`bus.grantPutEventsTo(statusLambda)` if using a NodejsFunction, else add an inline policy mirroring the existing IAM block at line ~481-504).
- Expose the SSM config JSON that the CLI reads (grep where `max_concurrent` etc. are written into the SSM parameter) and add `event_bus_name`, `max_spend_per_window`, `spend_window` so `horde launch/list` (the lazy drain + run.started emit) pick them up.

- [ ] **Step 5: Create the drain Lambda + rule + IAM**

Still in `cdk/src/horde-worker.ts`:
- Create the drain `NodejsFunction` from `cdk/src/drain-lambda/index.ts` (mirror the status Lambda's `NodejsFunction`/esbuild bundling setup; grep `package.json` esbuild entry for how status-lambda is bundled and add a drain entry).
- Environment: `RUNS_TABLE`, `EVENT_BUS_NAME`, `MAX_CONCURRENT` (from `props.maxConcurrent`/default), `CLUSTER_ARN`, `TASK_DEF_ARN`, subnets/SG/public-IP, worker container name, and conditionally `MAX_SPEND_PER_WINDOW`/`SPEND_WINDOW_SECONDS` when `props.maxSpendPerWindow` is set.
- IAM: DynamoDB `Query`/`UpdateItem`/`GetItem` on the table + GSIs; `ecs:RunTask`; `iam:PassRole` for the task + execution roles (mirror how the launching side is granted, if present, or the task-def role grants); `events:PutEvents` on the bus.
- Rule: `new events.Rule(this, "DrainOnTerminal", { eventBus: bus, eventPattern: { source: ["horde"], detailType: ["run.terminal"] } });` then `rule.addTarget(new targets.LambdaFunction(drainLambda));` Add a DLQ (`targets.LambdaFunction(drainLambda, { deadLetterQueue: dlq })`) for missed-event resilience.

> Update the esbuild bundle config in `cdk/package.json` (and any `cdk/tsconfig`/build script) to also bundle `src/drain-lambda/index.ts`, mirroring the status-lambda bundle entry exactly.

- [ ] **Step 6: Run test to verify it passes**

Run: `cd cdk && npm run build && npm test -- horde-worker-events`
Expected: PASS.

- [ ] **Step 7: Run the full CDK test suite**

Run: `cd cdk && npm test`
Expected: PASS (existing tests unaffected; new event tests green).

- [ ] **Step 8: Commit**

```bash
git add cdk/src/horde-worker.ts cdk/src/horde-worker-props.ts cdk/test/horde-worker-events.test.ts cdk/package.json
git commit -m "cdk: event bus, drain Lambda, run.terminal rule, spend-cap props"
```

---

## Phase 6 — Docs + final verification

### Task 20: Built-in docs + CLAUDE.md

**Files:**
- Modify: `internal/docs/content.go`, `internal/docs/docs_test.go`, `CLAUDE.md`

- [ ] **Step 1: Write the failing docs test**

In `internal/docs/docs_test.go`, add a case asserting the `queue` and `events` topics resolve and are non-empty (mirror the existing topic-presence test — grep `docs_test.go` for how topics are enumerated/validated).

```go
func TestQueueAndEventsTopicsExist(t *testing.T) {
	for _, topic := range []string{"queue", "events"} {
		body, ok := lookupTopic(topic) // match the actual lookup API
		if !ok || body == "" {
			t.Errorf("docs topic %q missing or empty", topic)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/docs/ -run TestQueueAndEventsTopicsExist -v`
Expected: FAIL — topics missing.

- [ ] **Step 3: Add the topics + update existing ones**

In `internal/docs/content.go`:
- Add a `queue` topic: `--enqueue`, `--priority` levels, `horde queue list/prioritize/cancel`, drain semantics (terminal-event + lazy CLI), the realized spend cap and its window/un-hold behavior, and the "smartness lives above horde — curate via priority/`--force`" model.
- Add an `events` topic: the bus, the `run.started`/`run.terminal`/`run.cost-threshold-exceeded` contract (field list + version), and how to subscribe an external rule.
- Update the `json` topic: new `queued` launch status (exit 0) + queue command JSON shapes.
- Update the `cdk` topic: `maxSpendPerWindow`/`spendWindow` props + the event bus.
- Update the `config` topic and the `--status` filter doc to include `queued`/`cancelled`.

> Match the exact registration mechanism (grep how `retry`/`config` topics are added to the content map).

- [ ] **Step 4: Add the Key Design Decision to CLAUDE.md**

Append a bullet under `## Key Design Decisions` in `CLAUDE.md` summarizing: event bus is the backbone (`run.started`/`run.terminal`/`run.cost-threshold-exceeded`, versioned, horde-vocabulary, CDK-only); queue = `queued` Run rows drained mechanically (priority desc → oldest) by terminal-event Lambda + lazy CLI backstop; `ClaimNextQueued` conditional `queued→pending` is the double-launch guard; `cancelled` (never ran) vs `killed` (ran) distinction; realized-only spend cap on a trailing `completed_at` window; `FindActiveByTicket` now `(ticket, workflow)`-scoped and counts `queued`; launch-after-claim failure → `failed` not re-queued; deferred follow-ons #52/#53/#54.

- [ ] **Step 5: Run docs test**

Run: `go test ./internal/docs/ -run TestQueueAndEventsTopicsExist -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/docs/content.go internal/docs/docs_test.go CLAUDE.md
git commit -m "docs: queue + events topics; CLAUDE.md design decision; update json/cdk/config"
```

### Task 21: Full verification

- [ ] **Step 1: Go build + vet**

Run: `make build && make vet`
Expected: clean.

- [ ] **Step 2: Full unit suite**

Run: `make unit-test`
Expected: all PASS.

- [ ] **Step 3: Integration suite (docker)**

Run: `make integration-test`
Expected: PASS (docker path: `--enqueue` errors as ECS-only; everything else unaffected).

- [ ] **Step 4: CDK build + test**

Run: `cd cdk && npm ci && npm run build && npm test`
Expected: PASS.

- [ ] **Step 5: Manual smoke (docker, no AWS)**

Run: `./horde launch --enqueue --workflow w SMOKE-1 --json`
Expected: JSON error envelope, reason contains "--enqueue requires the aws-ecs provider", exit 1.

Run: `./horde queue list` (docker)
Expected: empty list (no queued runs in local SQLite) — no crash.

- [ ] **Step 6: Commit any final fixes, then summarize**

```bash
git add -p   # stage specific fixes only
git commit -m "fixups from full verification"
```

Report: all CI-equivalent suites green; ECS drain path validated by unit + CDK synth tests; E2E (`make e2e-*`) is developer-local and out of CI per project convention.

---

## Notes for the implementer

- **TDD throughout:** every task writes the failing test first. Do not skip Step "run to verify it fails."
- **Best-effort emission/drain:** event emits and lazy drains must NEVER fail or corrupt the host command's output (especially `--json` stdout). Always log-and-continue.
- **The claim is the only double-launch guard.** Do not add a second "is it already running?" check — trust the conditional update.
- **DRY the launch path:** the direct launch and the drain must share one `launchRun` closure so re-resolution + emission can't drift.
- **Lockstep note:** the Python Lambda in `internal/bootstrap/templates/stack.yaml.tmpl` is intentionally NOT updated (CFN path is being retired; queue/events are CDK-only). Do not touch it.
- **Signature adaptation:** several steps say "match the existing signature/helper." `initProviderAndStore` returns 8 values; grep the real arities before writing call sites.
