# Spot Auto-Resume Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run ECS worker tasks on Fargate Spot by default, with a per-launch `--capacity` override, and automatically resume a run when Spot reclaims its task — capped to prevent loops.

**Architecture:** Spot/on-demand is a per-run choice stored on `Run.Capacity`, applied as a `CapacityProviderStrategy` on `RunTask` (both the Go provider and the TS drain Lambda); both capacity providers are always registered on the cluster (free). On a Spot interruption (ECS `stopCode == "TerminationNotice"`), the TS status Lambda re-queues the run (`status=queued`, `priority=highest`, `ResumeCount++`) instead of landing it terminal, under a `ResumeCount < MAX_RESUMES` budget; the existing drain Lambda then re-launches it under the normal concurrency + spend gates. Everything needed to resume already syncs to S3. Auto-resume is CDK-only (matches the #36 queue precedent); the CFN/Python path lands Spot interruptions as `killed` for manual `horde retry`.

**Tech Stack:** Go 1.24 (`internal/store`, `internal/provider`, `cmd/horde`), TypeScript CDK + esbuild-bundled Lambdas (`cdk/src`), CloudFormation template (`internal/bootstrap`), SQLite + DynamoDB stores.

**Reference spec:** `docs/superpowers/specs/2026-06-12-spot-auto-resume-design.md`

---

## The 4 doc layers — MANDATORY GATE

This requirement is repeated per-phase below and is non-negotiable. **No phase is complete until its slice of all four user-facing doc layers is updated in the same commit set:**

1. **README.md** — Spot-by-default + auto-resume + `--capacity` opt-out.
2. **CLAUDE.md** — BOTH (a) a new "Key Design Decision" bullet AND (b) the `## Architecture`/`## Commands` structural notes that reference touched files/fields.
3. **`horde docs`** (`internal/docs/content.go`) — new `spot` topic + touch `retry`/`queue`.
4. **`horde --help`** (`cmd/horde/main.go` urfave `Usage:` strings) — `--capacity` flag usage.

The dedicated doc tasks are Task 9 (docs content) and Task 10 (README + CLAUDE.md). They are NOT optional cleanup — they are part of "done."

---

## File Structure

**Modified — Go store (data model):**
- `internal/store/store.go` — add `Capacity`/`ResumeCount` to `Run` + `RunUpdate`; add `Capacity` type + `ParseCapacity`.
- `internal/store/sqlite.go` — two columns (DDL, `ensureColumns`, `runColumns`, INSERT, scan, `UpdateRun`).
- `internal/store/dynamo_schema.go` — `AttrCapacity`/`AttrResumeCount` consts.
- `internal/store/dynamo.go` — marshal/unmarshal/UpdateRun for the two attrs.
- `internal/store/conformance_test.go` — round-trip assertions (shared, runs against both stores).

**Modified — Go provider + CLI:**
- `internal/provider/provider.go` — `Capacity` field on `LaunchOpts`.
- `internal/provider/ecs.go` — `CapacityProviderStrategy` in `RunTask` from `opts.Capacity`.
- `internal/provider/ecs_test.go` — assert strategy per capacity, assert no `LaunchType`.
- `cmd/horde/main.go` — `--capacity` flag, validate, store on `Run.Capacity`, thread into the three `LaunchOpts{}` sites (direct launch, retry, and the `--enqueue` queued-run row); JSON echo.
- `cmd/horde/drain.go` — thread `run.Capacity` into the lazy-drain `LaunchOpts{}`.
- `cmd/horde/jsonv1.go` — `capacity`/`resume_count` in JSON run objects.

**Modified — CDK (TS) + CFN:**
- `cdk/src/horde-worker.ts` — register both capacity providers on the cluster; `MAX_RESUMES` env on the status Lambda.
- `cdk/src/status-lambda/index.ts` — re-queue branch on `TerminationNotice` under budget.
- `cdk/src/status-lambda/index.test.ts` — re-queue tests.
- `cdk/src/drain-lambda/index.ts` — `capacity` on `QueuedRun`, `capacityProviderStrategy` in `runTaskInput`.
- `internal/bootstrap/templates/stack.yaml.tmpl` — add `FARGATE_SPOT` to cluster `CapacityProviders` (NO resume logic).

**Modified — docs:**
- `internal/docs/content.go`, `README.md`, `CLAUDE.md`.

---

## Task 1: `Capacity` type + Run/RunUpdate fields (store.go)

**Files:**
- Modify: `internal/store/store.go`

- [ ] **Step 1: Add the `Capacity` type + `ParseCapacity` after the `Priority`/`ParsePriority` block (after line 82)**

```go
// Capacity selects which Fargate capacity provider a run launches on. It is a
// per-run choice (horde launch --capacity), set once and carried across
// resume/retry. spot is the default (cost); on-demand opts a run out of Spot
// reclaim. ECS maps these to a CapacityProviderStrategy in the provider; both
// providers are always registered on the cluster.
type Capacity string

const (
	CapacitySpot     Capacity = "spot"
	CapacityOnDemand Capacity = "on-demand"
)

// ParseCapacity validates a user-supplied capacity string. Empty defaults to
// spot. Unknown values are an error (surfaced to the CLI user).
func ParseCapacity(s string) (Capacity, error) {
	switch Capacity(s) {
	case "":
		return CapacitySpot, nil
	case CapacitySpot, CapacityOnDemand:
		return Capacity(s), nil
	default:
		return "", fmt.Errorf("invalid capacity %q: want spot or on-demand", s)
	}
}
```

- [ ] **Step 2: Add the two fields to the `Run` struct (after the `Priority Priority` field, currently line ~177, before `Tokens`)**

```go
	// Priority is the drain-order lever; empty for directly-launched runs.
	Priority Priority
	// Capacity selects the Fargate capacity provider (spot|on-demand). Empty
	// means spot (the default). Set at launch, carried across resume/retry.
	Capacity Capacity
	// ResumeCount counts automatic Spot resumes of this run. Incremented each
	// time the status Lambda re-queues a Spot-interrupted run; at MAX_RESUMES
	// the run is left terminal instead of resumed (loop guard).
	ResumeCount int
	// Tokens is nil until orc reports usage (pre-finalize, or an orc old
	// enough that neither costs.json nor run-result.json carried token totals).
	Tokens *TokenUsage
```

- [ ] **Step 3: Add the two fields to `RunUpdate` (after `Priority *Priority`, line ~194)**

```go
	Priority     *Priority // nil = don't update
	Capacity     *Capacity // nil = don't update
	ResumeCount  *int      // nil = don't update
```

- [ ] **Step 4: Verify it compiles**

Run: `go build ./internal/store/`
Expected: success (no callers yet; fields are additive).

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go
git commit -m "store: add Capacity type and Run.Capacity/ResumeCount fields"
```

---

## Task 2: Persist Capacity/ResumeCount in SQLite

**Files:**
- Modify: `internal/store/sqlite.go`
- Test: `internal/store/conformance_test.go` (Task 4 adds assertions; this task makes them possible)

- [ ] **Step 1: Add columns to the `runColumns` constant (line ~26-30). Append after `priority`:**

```go
const runColumns = `id, repo, ticket, branch, workflow, provider,
	instance_id, metadata, labels, status, exit_code, launched_by,
	started_at, completed_at, timeout_at, total_cost_usd,
	input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, turns,
	enqueued_at, priority, capacity, resume_count`
```

- [ ] **Step 2: Add columns to the CREATE TABLE DDL (line ~78, after the `priority` line — change the trailing comma)**

```go
	enqueued_at    TEXT,
	priority       TEXT NOT NULL DEFAULT '',
	capacity       TEXT NOT NULL DEFAULT '',
	resume_count   INTEGER NOT NULL DEFAULT 0
);`
```

- [ ] **Step 3: Add to the `additive` slice in `ensureColumns` (line ~133-134, after the `priority` entry)**

```go
		{"enqueued_at", "TEXT"},
		{"priority", "TEXT NOT NULL DEFAULT ''"},
		{"capacity", "TEXT NOT NULL DEFAULT ''"},
		{"resume_count", "INTEGER NOT NULL DEFAULT 0"},
```

- [ ] **Step 4: Add to the INSERT in `CreateRun` (line ~196-227). Add the two columns, two `?` placeholders, and two values at the end:**

Change the column list line to end with `enqueued_at, priority, capacity, resume_count`, add two `?` to the VALUES tuple (now 25 placeholders), and append after `string(run.Priority),`:

```go
		enqueuedAt,
		string(run.Priority),
		string(run.Capacity),
		run.ResumeCount,
	)
```

- [ ] **Step 5: Add scan vars + scan args + assignment in `scanRun` (line ~251-294)**

Add var decls after `var priority string`:

```go
	var priority string
	var capacity string
	var resumeCount sql.NullInt64
```

Add to the `.Scan(...)` arg list after `&priority,`:

```go
		&priority,
		&capacity,
		&resumeCount,
```

Add after `run.Priority = Priority(priority)`:

```go
	run.Priority = Priority(priority)
	run.Capacity = Capacity(capacity)
	if resumeCount.Valid {
		run.ResumeCount = int(resumeCount.Int64)
	}
```

- [ ] **Step 6: Add SET clauses in `UpdateRun` (line ~409-412, after the Priority block)**

```go
	if update.Priority != nil {
		setClauses = append(setClauses, "priority = ?")
		args = append(args, string(*update.Priority))
	}
	if update.Capacity != nil {
		setClauses = append(setClauses, "capacity = ?")
		args = append(args, string(*update.Capacity))
	}
	if update.ResumeCount != nil {
		setClauses = append(setClauses, "resume_count = ?")
		args = append(args, *update.ResumeCount)
	}
```

- [ ] **Step 7: Verify it compiles and existing tests still pass**

Run: `go build ./internal/store/ && go test ./internal/store/ -run TestSQLite -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/store/sqlite.go
git commit -m "store/sqlite: persist Capacity and ResumeCount"
```

---

## Task 3: Persist Capacity/ResumeCount in DynamoDB

**Files:**
- Modify: `internal/store/dynamo_schema.go`
- Modify: `internal/store/dynamo.go`

- [ ] **Step 1: Add attr consts in `dynamo_schema.go` (after `AttrPriority`, line ~34)**

```go
	AttrEnqueuedAt = "enqueued_at"
	AttrPriority   = "priority"
	AttrCapacity   = "capacity"
	AttrResumeCount = "resume_count"
```

- [ ] **Step 2: Marshal in `CreateRun` (dynamo.go line ~58, in the always-written item map alongside `AttrPriority`)**

```go
		AttrPriority:   &types.AttributeValueMemberS{Value: string(run.Priority)},
		AttrCapacity:   &types.AttributeValueMemberS{Value: string(run.Capacity)},
		AttrResumeCount: &types.AttributeValueMemberN{Value: strconv.Itoa(run.ResumeCount)},
```

- [ ] **Step 3: Unmarshal in `parseRun` (dynamo.go, after the `AttrPriority` block, line ~240)**

```go
	if av, ok := item[AttrCapacity]; ok {
		sv, ok := av.(*types.AttributeValueMemberS)
		if !ok {
			return nil, fmt.Errorf("parsing run %q: invalid %q attribute", id, AttrCapacity)
		}
		run.Capacity = Capacity(sv.Value)
	}

	if av, ok := item[AttrResumeCount]; ok {
		nv, ok := av.(*types.AttributeValueMemberN)
		if !ok {
			return nil, fmt.Errorf("parsing run %q: invalid %q attribute", id, AttrResumeCount)
		}
		n, err := strconv.Atoi(nv.Value)
		if err != nil {
			return nil, fmt.Errorf("parsing run %q: parsing resume_count: %w", id, err)
		}
		run.ResumeCount = n
	}
```

- [ ] **Step 4: UpdateRun SET clauses (dynamo.go, after the `#prio` block, line ~409). `capacity`/`resume_count` are NOT reserved words, so no alias needed:**

```go
	if update.Capacity != nil {
		setClauses = append(setClauses, "capacity = :cap")
		exprAttrValues[":cap"] = &types.AttributeValueMemberS{Value: string(*update.Capacity)}
	}
	if update.ResumeCount != nil {
		setClauses = append(setClauses, "resume_count = :rc")
		exprAttrValues[":rc"] = &types.AttributeValueMemberN{Value: strconv.Itoa(*update.ResumeCount)}
	}
```

- [ ] **Step 5: Verify it compiles**

Run: `go build ./internal/store/`
Expected: success.

- [ ] **Step 6: Commit**

```bash
git add internal/store/dynamo_schema.go internal/store/dynamo.go
git commit -m "store/dynamo: persist Capacity and ResumeCount"
```

---

## Task 4: Store conformance tests for Capacity/ResumeCount

**Files:**
- Modify: `internal/store/conformance_test.go`

The conformance tests run against BOTH SQLite and DynamoDB (in-memory/local), so one test covers both backends.

- [ ] **Step 1: Add a round-trip + update test. Place it alongside the other `t.Run("CreateGetRun/...")`/`UpdateRun/...` subtests (search the file for an existing `t.Run("CreateGetRun` to find the block).**

```go
	t.Run("CreateGetRun/CapacityResumeCount", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		now := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
		r := &Run{
			ID: "cap-1", Repo: "capr", Ticket: "T-1", Provider: "aws-ecs",
			Status: StatusRunning, LaunchedBy: "me",
			StartedAt: now, TimeoutAt: now.Add(time.Hour),
			Capacity: CapacityOnDemand, ResumeCount: 2,
		}
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRun(ctx, "cap-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Capacity != CapacityOnDemand {
			t.Errorf("Capacity = %q, want on-demand", got.Capacity)
		}
		if got.ResumeCount != 2 {
			t.Errorf("ResumeCount = %d, want 2", got.ResumeCount)
		}
	})

	t.Run("UpdateRun/CapacityResumeCount", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		now := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
		// Default capacity (empty) on create; bump resume_count via update.
		if err := s.CreateRun(ctx, &Run{
			ID: "cap-2", Repo: "capr2", Ticket: "T-2", Provider: "aws-ecs",
			Status: StatusQueued, LaunchedBy: "me",
			StartedAt: now, TimeoutAt: now.Add(time.Hour), Capacity: CapacitySpot,
		}); err != nil {
			t.Fatal(err)
		}
		newCount := 1
		newCap := CapacityOnDemand
		if err := s.UpdateRun(ctx, "cap-2", &RunUpdate{ResumeCount: &newCount, Capacity: &newCap}); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRun(ctx, "cap-2")
		if err != nil {
			t.Fatal(err)
		}
		if got.ResumeCount != 1 {
			t.Errorf("ResumeCount = %d, want 1", got.ResumeCount)
		}
		if got.Capacity != CapacityOnDemand {
			t.Errorf("Capacity = %q, want on-demand", got.Capacity)
		}
	})
```

- [ ] **Step 2: Run the conformance tests (both backends)**

Run: `go test ./internal/store/ -run 'CapacityResumeCount' -count=1 -v`
Expected: PASS for both the SQLite and DynamoDB conformance instantiations.

- [ ] **Step 3: Run the full store suite to confirm nothing regressed**

Run: `go test ./internal/store/ -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/store/conformance_test.go
git commit -m "store: conformance tests for Capacity and ResumeCount"
```

---

## Task 5: Apply CapacityProviderStrategy in the Go provider

**Files:**
- Modify: `internal/provider/provider.go`
- Modify: `internal/provider/ecs.go:214-238`
- Test: `internal/provider/ecs_test.go`

- [ ] **Step 1: Add `Capacity` to `LaunchOpts` (provider.go, after the `ExtraEnv` field, before the closing brace ~line 65)**

```go
	ExtraEnv map[string]string

	// Capacity selects the Fargate capacity provider for this launch
	// ("spot" | "on-demand"). Empty defaults to spot. ECS-only; the docker
	// provider ignores it. Carried across resume/retry so a run stays on the
	// same kind of capacity.
	Capacity string
}
```

- [ ] **Step 2: Write the failing provider test FIRST. Add to `ecs_test.go`:**

```go
func TestECSProvider_Launch_CapacityStrategy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		capacity string
		want     string
	}{
		{"spot default (empty)", "", "FARGATE_SPOT"},
		{"explicit spot", "spot", "FARGATE_SPOT"},
		{"on-demand", "on-demand", "FARGATE"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeECSClient{
				runTaskOutput: &ecs.RunTaskOutput{
					Tasks: []ecstypes.Task{{TaskArn: aws.String("arn:task/x")}},
				},
			}
			p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
			_, err := p.Launch(context.Background(), LaunchOpts{
				Repo: "r", Ticket: "T", RunID: "abc123def456", Capacity: tc.capacity,
			})
			if err != nil {
				t.Fatalf("Launch() error = %v", err)
			}
			in := fake.runTaskInput
			if in.LaunchType != "" {
				t.Errorf("LaunchType = %q, want empty (mutually exclusive with strategy)", in.LaunchType)
			}
			if len(in.CapacityProviderStrategy) != 1 {
				t.Fatalf("CapacityProviderStrategy len = %d, want 1", len(in.CapacityProviderStrategy))
			}
			if got := aws.ToString(in.CapacityProviderStrategy[0].CapacityProvider); got != tc.want {
				t.Errorf("CapacityProvider = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 3: Run it — expect FAIL (still uses LaunchType)**

Run: `go test ./internal/provider/ -run TestECSProvider_Launch_CapacityStrategy -count=1`
Expected: FAIL — `LaunchType` non-empty, `CapacityProviderStrategy` len 0.

- [ ] **Step 4: Replace `LaunchType` in the `RunTaskInput` literal (ecs.go ~line 217). Remove the `LaunchType:` line and add a `CapacityProviderStrategy`:**

```go
	provider := "FARGATE_SPOT"
	if opts.Capacity == string(store.CapacityOnDemand) {
		provider = "FARGATE"
	}
	input := &ecs.RunTaskInput{
		TaskDefinition: aws.String(p.config.TaskDefinitionARN),
		Cluster:        aws.String(p.config.ClusterARN),
		CapacityProviderStrategy: []ecstypes.CapacityProviderStrategyItem{
			{CapacityProvider: aws.String(provider), Weight: 1},
		},
		Count: aws.Int32(1),
		NetworkConfiguration: &ecstypes.NetworkConfiguration{
```

(Leave the rest of the literal — `NetworkConfiguration`, `Overrides`, `Tags` — unchanged. `store` is already imported in ecs.go.)

- [ ] **Step 5: Update the legacy assertion in `TestECSProvider_Launch_Success` (ecs_test.go ~line 236). Replace the `LaunchType` check with:**

```go
	if in.LaunchType != "" {
		t.Errorf("LaunchType = %v, want empty (using CapacityProviderStrategy)", in.LaunchType)
	}
	if len(in.CapacityProviderStrategy) != 1 || aws.ToString(in.CapacityProviderStrategy[0].CapacityProvider) != "FARGATE_SPOT" {
		t.Errorf("CapacityProviderStrategy = %+v, want one FARGATE_SPOT item", in.CapacityProviderStrategy)
	}
```

- [ ] **Step 6: Run both tests — expect PASS**

Run: `go test ./internal/provider/ -run TestECSProvider_Launch -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/provider/provider.go internal/provider/ecs.go internal/provider/ecs_test.go
git commit -m "provider/ecs: launch via CapacityProviderStrategy (spot default)"
```

---

## Task 6: `--capacity` flag + thread through launch/retry/enqueue/drain

**Files:**
- Modify: `cmd/horde/main.go`
- Modify: `cmd/horde/drain.go:196`
- Test: `cmd/horde/main_test.go`

- [ ] **Step 1: Write the failing CLI test FIRST. Add to `main_test.go` (mirror an existing `--priority` parse test; search for `ParsePriority` or `"priority"` in that file for the harness style). This tests the validation path:**

```go
func TestLaunchCapacityFlagValidation(t *testing.T) {
	// invalid capacity must error before any provider call.
	app := newTestApp(t) // use the existing test app constructor in this file
	err := app.Run(context.Background(), []string{
		"horde", "launch", "T-1", "--capacity", "bogus",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid capacity") {
		t.Fatalf("want invalid-capacity error, got %v", err)
	}
}
```

(If `newTestApp` is not the exact helper name, use whatever the surrounding launch tests use — match the file's existing pattern. The assertion is: invalid `--capacity` returns an error containing "invalid capacity".)

- [ ] **Step 2: Run it — expect FAIL (flag doesn't exist yet, so urfave errors with "unknown flag" not "invalid capacity")**

Run: `go test ./cmd/horde/ -run TestLaunchCapacityFlagValidation -count=1`
Expected: FAIL.

- [ ] **Step 3: Add the `--capacity` flag to `launchCmd()` (main.go, in the `Flags` slice after the `priority` flag ~line 223)**

```go
			&cli.StringFlag{
				Name:  "capacity",
				Usage: "Fargate capacity for this run: spot|on-demand (default spot). on-demand opts out of Spot reclaim. ECS-only; ignored on docker. Carried across retry/resume.",
			},
```

- [ ] **Step 4: Parse + validate inside the launch Action (main.go, near where `priority` is parsed ~line 250). NOT `Required` — validate in the Action per the --json rule:**

```go
			priority, err := store.ParsePriority(cmd.String("priority"))
			if err != nil {
				return err
			}
			capacity, err := store.ParseCapacity(cmd.String("capacity"))
			if err != nil {
				return err
			}
```

- [ ] **Step 5: Set `Capacity` on the queued-run row (the `--enqueue` path, main.go ~line 373-383, the `qrun := &store.Run{...}`)**

```go
				qrun := &store.Run{
					// ... existing fields ...
					Priority:   priority,
					Capacity:   capacity,
				}
```

- [ ] **Step 6: Set `Capacity` on the direct-launch run row (main.go ~line 402-411, the `run := &store.Run{...}`) AND thread it into the `LaunchOpts` (main.go ~line 444)**

On the `run := &store.Run{...}` literal add `Capacity: capacity,`. On the `prov.Launch(ctx, provider.LaunchOpts{...})` literal (line ~444) add:

```go
				OrcArgs:        orcArgs,
				SecretEnvRemap: secretRemap,
				ExtraEnv:       extraEnv,
				Capacity:       string(capacity),
```

(Match the exact existing field set at that call site; the key addition is `Capacity: string(capacity)`.)

- [ ] **Step 7: Thread stored capacity into the RETRY launch (main.go ~line 610, the retry `prov.Launch` — it reuses the stored `run`). Add:**

```go
				OrcArgs:        orcArgs,
				SecretEnvRemap: secretRemap,
				Capacity:       string(run.Capacity),
			})
```

- [ ] **Step 8: Thread stored capacity into the lazy-drain launch (drain.go ~line 196, the `lc.prov.Launch(ctx, provider.LaunchOpts{...})`). Add `Capacity: string(<queued run>.Capacity),` using whatever the run variable is named at that call site.**

- [ ] **Step 9: Run the CLI test — expect PASS**

Run: `go test ./cmd/horde/ -run TestLaunchCapacityFlagValidation -count=1`
Expected: PASS.

- [ ] **Step 10: `horde --help` gate — the flag's `Usage:` string IS the `--help` doc layer. Confirm it renders:**

Run: `go run ./cmd/horde launch --help 2>&1 | grep -A1 capacity`
Expected: the `--capacity` usage line appears.

- [ ] **Step 11: Commit**

```bash
git add cmd/horde/main.go cmd/horde/drain.go cmd/horde/main_test.go
git commit -m "cli: horde launch --capacity (spot default), thread through retry/enqueue/drain"
```

---

## Task 7: Surface capacity/resume_count under `--json`

**Files:**
- Modify: `cmd/horde/jsonv1.go`
- Test: `cmd/horde/jsonv1_test.go` (search for an existing test of `statusToV1` / `tokens` to match style)

Three JSON types carry run data: `RunV1` (built by `statusToV1`, ~line 155), `ListRunV1` (built by `listToV1`, which copies field-by-field from `statusToV1`'s output `s`, ~line 184), and `ResultsV1` (built by a results converter, ~line 315). `Priority` is NOT on `RunV1` (it lives only on the queue-specific types), so anchor on the `Tokens:` line in each converter — `tokens` IS present in all three. Adding to `RunV1` + propagating through `listToV1` covers `status` and `list`; `ResultsV1` is the partial-results shape and gets the fields too.

- [ ] **Step 1: Write the failing test FIRST (jsonv1_test.go). `statusToV1` takes a `*store.Run`:**

```go
func TestStatusV1IncludesCapacity(t *testing.T) {
	r := &store.Run{ID: "j1", Capacity: store.CapacityOnDemand, ResumeCount: 3,
		StartedAt: time.Now()}
	v := statusToV1(r)
	if v.Capacity != "on-demand" {
		t.Errorf("Capacity = %q, want on-demand", v.Capacity)
	}
	if v.ResumeCount != 3 {
		t.Errorf("ResumeCount = %d, want 3", v.ResumeCount)
	}
}
```

- [ ] **Step 2: Run — expect FAIL (compile error: unknown field `Capacity` on RunV1)**

Run: `go test ./cmd/horde/ -run TestStatusV1IncludesCapacity -count=1`
Expected: FAIL.

- [ ] **Step 3: Add the two fields to the `RunV1` struct (jsonv1.go, after `Tokens *TokensV1` ~line 76)**

```go
	Tokens       *TokensV1         `json:"tokens,omitempty"`
	Capacity     string            `json:"capacity,omitempty"`
	ResumeCount  int               `json:"resume_count,omitempty"`
```

- [ ] **Step 4: Set them in `statusToV1` (after the `Tokens: tokensToV1(run.Tokens),` line ~164)**

```go
		Tokens:       tokensToV1(run.Tokens),
		Capacity:     string(run.Capacity),
		ResumeCount:  run.ResumeCount,
```

- [ ] **Step 5: Add the two fields to `ListRunV1` (after its `Tokens *TokensV1` line) and propagate in `listToV1` (after its `Tokens: s.Tokens,` line)**

Struct:
```go
	Tokens       *TokensV1         `json:"tokens,omitempty"`
	Capacity     string            `json:"capacity,omitempty"`
	ResumeCount  int               `json:"resume_count,omitempty"`
```
Converter (copying from `s`):
```go
			Tokens:       s.Tokens,
			Capacity:     s.Capacity,
			ResumeCount:  s.ResumeCount,
```

- [ ] **Step 6: Add the two fields to `ResultsV1` + its converter (after the `Tokens: tokensToV1(run.Tokens),` at ~line 322)**

Struct (find `type ResultsV1 struct`, add after its `Tokens` field):
```go
	Capacity    string `json:"capacity,omitempty"`
	ResumeCount int    `json:"resume_count,omitempty"`
```
Converter (~line 322):
```go
		Tokens:       tokensToV1(run.Tokens),
		Capacity:     string(run.Capacity),
		ResumeCount:  run.ResumeCount,
```

- [ ] **Step 7: Run — expect PASS**

Run: `go test ./cmd/horde/ -run TestStatusV1IncludesCapacity -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add cmd/horde/jsonv1.go cmd/horde/jsonv1_test.go
git commit -m "json: surface capacity and resume_count on run objects"
```

---

## Task 8: CDK + CFN — register both capacity providers; drain honors capacity; MAX_RESUMES env

**Files:**
- Modify: `cdk/src/horde-worker.ts`
- Modify: `cdk/src/drain-lambda/index.ts`
- Modify: `internal/bootstrap/templates/stack.yaml.tmpl`
- Test: `cdk/src/*.test.ts` (cluster + drain), `cdk/test` snapshot

- [ ] **Step 1: Register both providers on the CDK cluster (horde-worker.ts ~line 202). Replace the `new ecs.Cluster(...)` with capacity providers enabled:**

```typescript
    this.cluster = new ecs.Cluster(this, "Cluster", {
      vpc: this.vpc,
      clusterName: `horde-${slug}`,
      enableFargateCapacityProviders: true, // registers FARGATE + FARGATE_SPOT
    });
```

(`enableFargateCapacityProviders: true` associates both `FARGATE` and `FARGATE_SPOT` with the cluster — the registration is free; tasks pick a provider per RunTask.)

- [ ] **Step 2: Add `FARGATE_SPOT` to the bootstrap CFN cluster (stack.yaml.tmpl ~line 204). NO resume logic here — CFN is intentionally not given auto-resume.**

```yaml
      CapacityProviders:
        - FARGATE
        - FARGATE_SPOT
```

(Leave `RequiresCompatibilities: [FARGATE]` on the task definition at line ~308 UNCHANGED — that is a launch-type compatibility declaration, not a capacity provider, and Spot tasks still require FARGATE compatibility.)

- [ ] **Step 3: Add `capacity` to `QueuedRun` and its unmarshal (drain-lambda/index.ts). In the interface (~line 38):**

```typescript
interface QueuedRun {
  id: string;
  repo: string;
  ticket: string;
  branch: string;
  workflow: string;
  priority: string;
  enqueuedAt: string;
  capacity: string; // "spot" | "on-demand" | "" (empty => spot)
}
```

In `listQueued`'s push (~line 111):

```typescript
        priority: s(item.priority),
        enqueuedAt: s(item.enqueued_at),
        capacity: s(item.capacity),
```

- [ ] **Step 4: Replace `launchType` with `capacityProviderStrategy` in `runTaskInput` (drain-lambda/index.ts ~line 167). Remove `launchType: "FARGATE" as const,` and add:**

```typescript
  return {
    taskDefinition: process.env.TASK_DEF_ARN,
    cluster: process.env.CLUSTER_ARN,
    capacityProviderStrategy: [
      {
        capacityProvider: run.capacity === "on-demand" ? "FARGATE" : "FARGATE_SPOT",
        weight: 1,
      },
    ],
    count: 1,
```

- [ ] **Step 5: Add `MAX_RESUMES` env to the STATUS Lambda (horde-worker.ts ~line 505, the `environment:` map). Mirror how `MAX_CONCURRENT` is passed to the drain Lambda:**

```typescript
      environment: {
        RUNS_TABLE: this.runsTable.tableName,
        ARTIFACTS_BUCKET: this.artifactsBucket.bucketName,
        EVENT_BUS_NAME: this.eventBus.eventBusName,
        MAX_RESUMES: String(props.maxSpotResumes ?? 5),
      },
```

- [ ] **Step 6: Add the `maxSpotResumes?: number` prop to `HordeWorkerProps`. Find the props interface in horde-worker.ts (search `maxConcurrent` / `maxSpendPerWindow` for the block) and add:**

```typescript
  /**
   * Max automatic Spot resumes per run before it is left terminal (loop guard).
   * @default 5
   */
  readonly maxSpotResumes?: number;
```

- [ ] **Step 7: Build the bundles and run the CDK tests**

Run: `cd cdk && npm run build && npm test`
Expected: PASS. If a CDK snapshot test exists, update it (the cluster now has capacity-provider associations + the status Lambda has a new env var): `npm test -- -u` then re-run.

- [ ] **Step 8: Validate the rendered CFN template still parses**

Run: `cd /home/jb/work/horde && make bootstrap-validate-test`
Expected: PASS (ValidateTemplate accepts the two-provider cluster).

- [ ] **Step 9: Commit**

```bash
git add cdk/src/horde-worker.ts cdk/src/drain-lambda/index.ts internal/bootstrap/templates/stack.yaml.tmpl
git add cdk/src/*.test.ts cdk/test 2>/dev/null
git commit -m "cdk+cfn: register FARGATE_SPOT, drain honors capacity, MAX_RESUMES env"
```

---

## Task 9: Auto-resume branch in the status Lambda

**Files:**
- Modify: `cdk/src/status-lambda/index.ts`
- Test: `cdk/src/status-lambda/index.test.ts`

This is the keystone. On `stopCode == "TerminationNotice"`, if the run is under budget and not already terminal, re-queue it (priority highest, resume_count++, status=queued, clear started_at/instance_id/timeout) INSTEAD of the terminal write. Otherwise fall through to today's terminal write.

- [ ] **Step 1: Write the failing tests FIRST (index.test.ts). Mirror the existing TerminationNotice test's mocking style (`ddbMock.on(QueryCommand).resolves(...)`, `commandCalls(UpdateItemCommand)`).**

```typescript
  it("re-queues a Spot-interrupted run under the resume budget", async () => {
    process.env.MAX_RESUMES = "5";
    // findRun returns the run row incl. its current resume_count.
    ddbMock.on(QueryCommand).resolves({
      Items: [{ id: { S: "run-spot" }, repo: { S: "r" }, resume_count: { N: "1" } }],
    });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    const r = await handler(
      event({
        lastStatus: "STOPPED",
        taskArn: "arn:task/spot",
        stopCode: "TerminationNotice",
        stoppedReason: "Your Spot Task was interrupted",
        containers: [{ name: "horde-worker", exitCode: 5 }],
      }),
      ctx,
      () => {},
    );

    // The LAST update must be the re-queue, conditional on not-already-terminal,
    // setting status=queued, priority=highest, resume_count=2.
    const calls = ddbMock.commandCalls(UpdateItemCommand);
    const requeue = calls[calls.length - 1].args[0].input;
    expect(requeue.UpdateExpression).toContain("#s = :queued");
    expect(requeue.ConditionExpression).toContain("attribute_not_exists(#s)");
    expect(requeue.ExpressionAttributeValues?.[":queued"]).toEqual({ S: "queued" });
    expect(requeue.ExpressionAttributeValues?.[":rc"]).toEqual({ N: "2" });
    expect(r).toMatchObject({ requeued: "run-spot" });
  });

  it("does NOT re-queue past the resume budget (lands terminal instead)", async () => {
    process.env.MAX_RESUMES = "5";
    ddbMock.on(QueryCommand).resolves({
      Items: [{ id: { S: "run-exhausted" }, repo: { S: "r" }, resume_count: { N: "5" } }],
    });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    await handler(
      event({
        lastStatus: "STOPPED",
        taskArn: "arn:task/spot",
        stopCode: "TerminationNotice",
        containers: [{ name: "horde-worker", exitCode: 5 }],
      }),
      ctx,
      () => {},
    );

    const calls = ddbMock.commandCalls(UpdateItemCommand);
    const last = calls[calls.length - 1].args[0].input;
    // Terminal write (killed), not a re-queue.
    expect(last.ExpressionAttributeValues?.[":s"]).toEqual({ S: "killed" });
    expect(last.UpdateExpression).not.toContain(":queued");
  });

  it("does NOT re-queue a UserInitiated stop (horde kill)", async () => {
    process.env.MAX_RESUMES = "5";
    ddbMock.on(QueryCommand).resolves({
      Items: [{ id: { S: "run-killed" }, repo: { S: "r" }, resume_count: { N: "0" } }],
    });
    ddbMock.on(UpdateItemCommand).resolves({});
    s3Mock.on(ListObjectsV2Command).resolves({ Contents: [] });

    await handler(
      event({
        lastStatus: "STOPPED",
        taskArn: "arn:task/killed",
        stopCode: "UserInitiated",
        containers: [{ name: "horde-worker", exitCode: 5 }],
      }),
      ctx,
      () => {},
    );

    const calls = ddbMock.commandCalls(UpdateItemCommand);
    const last = calls[calls.length - 1].args[0].input;
    expect(last.UpdateExpression).not.toContain(":queued");
  });
```

- [ ] **Step 2: Run — expect FAIL (no re-queue path yet)**

Run: `cd cdk && npm test -- status-lambda`
Expected: FAIL on the three new tests.

- [ ] **Step 3: Make `findRun` also return `resume_count`. In index.ts `findRun` (line ~75), extend the return shape and read the attr:**

```typescript
async function findRun(
  taskArn: string,
): Promise<{ runId: string; repo: string; labels: Record<string, string>; resumeCount: number } | null> {
```

After reading `repo`/`labels`, add:

```typescript
  const rcAttr = items[0].resume_count;
  const resumeCount = rcAttr && "N" in rcAttr ? Number(rcAttr.N ?? "0") : 0;
```

and include `resumeCount` in the returned object. Update the destructure at the call site (`const { runId, repo, labels } = found;`) to `const { runId, repo, labels, resumeCount } = found;`.

- [ ] **Step 4: Add the re-queue branch BEFORE the terminal-write block (index.ts, right before the `try { await ddb.send(new UpdateItemCommand({... ConditionExpression ...}))` at line ~332). The stop-reason metadata write above it stays (diagnostic). Insert:**

```typescript
  // Spot auto-resume: a TerminationNotice means AWS reclaimed the task, not a
  // failure. If the run is under its resume budget, re-queue it (status=queued,
  // top priority, resume_count++) so the drain re-launches it with the same run
  // ID — everything needed to resume already synced to S3. Conditional on
  // not-already-terminal so a racing `horde kill` (UserInitiated, sets killed
  // synchronously) wins. UserInitiated never reaches here. At the cap we fall
  // through to the normal terminal write below. CDK-only (the Python lambda
  // has no queue to drain).
  const maxResumes = Number(process.env.MAX_RESUMES ?? "5");
  if (detail.stopCode === "TerminationNotice" && resumeCount < maxResumes) {
    try {
      await ddb.send(
        new UpdateItemCommand({
          TableName: RUNS_TABLE,
          Key: { id: { S: runId } },
          UpdateExpression:
            "SET #s = :queued, #prio = :highest, resume_count = :rc REMOVE instance_id, started_at",
          ConditionExpression:
            "attribute_not_exists(#s) OR NOT (#s IN (:success, :failed, :killed, :timed_out, :rate_limited, :cancelled))",
          ExpressionAttributeNames: { "#s": "status", "#prio": "priority" },
          ExpressionAttributeValues: {
            ":queued": { S: "queued" },
            ":highest": { S: "highest" },
            ":rc": { N: String(resumeCount + 1) },
            ":success": { S: "success" },
            ":failed": { S: "failed" },
            ":killed": { S: "killed" },
            ":timed_out": { S: "timed_out" },
            ":rate_limited": { S: "rate_limited" },
            ":cancelled": { S: "cancelled" },
          },
        }),
      );
      console.log("status-lambda: re-queued Spot-interrupted run", { runId, resumeCount: resumeCount + 1 });
      // No run.terminal emission — this is a continuation, not a terminal outcome.
      return { requeued: runId } as unknown as Result;
    } catch (err) {
      if (err instanceof ConditionalCheckFailedException) {
        console.log("status-lambda: run already terminal, not re-queuing (kill race)", { runId });
        return { skipped: "already terminal", runId };
      }
      throw err;
    }
  }
```

- [ ] **Step 5: Add `requeued` to the `Result` union (index.ts ~line 66)**

```typescript
type Result =
  | { readonly skipped: string; readonly runId?: string; readonly status?: string }
  | { readonly requeued: string }
  | {
      readonly updated: true;
      readonly runId: string;
      readonly status: string;
      readonly exitCode: number | null;
    };
```

(With this added, the `as unknown as Result` cast in Step 4 can be simplified to `return { requeued: runId };`.)

- [ ] **Step 6: Run — expect PASS (all status-lambda tests, including the pre-existing ones)**

Run: `cd cdk && npm test -- status-lambda`
Expected: PASS. The pre-existing "records the stop reason even when already terminal" test still passes because the stop-reason writes run before the new branch and the branch's own CCFE path returns `already terminal`.

- [ ] **Step 7: Full CDK build + test**

Run: `cd cdk && npm run build && npm test`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add cdk/src/status-lambda/index.ts cdk/src/status-lambda/index.test.ts
git commit -m "status-lambda: auto-resume Spot-interrupted runs via re-queue (budget-capped)"
```

---

## Task 10: `horde docs` — new `spot` topic + touch retry/queue (doc layer 3)

**Files:**
- Modify: `internal/docs/content.go`

- [ ] **Step 1: Find the topic registry. Search:**

Run: `grep -n '"retry"\|"queue"\|"events"\|topics\s*=\|map\[string\]' internal/docs/content.go | head`
Expected: the map/slice of topic name → content. Note the exact structure (key + heredoc/const body) so the new entry matches.

- [ ] **Step 2: Add a `spot` topic entry. Match the existing entries' format exactly (same map/struct shape found in Step 1). Content:**

```
horde and Fargate Spot
=======================

ECS runs launch on Fargate Spot by default for the cost saving. Spot capacity
can be reclaimed by AWS at any time; horde makes runs survive that.

Per-launch capacity
-------------------
  horde launch <ticket> --capacity spot        # default
  horde launch <ticket> --capacity on-demand   # opt out of reclaim

Capacity is stored on the run and reused on retry and auto-resume, so a run
stays on the kind of capacity it started on. Both providers are always
registered on the cluster; you only pay for the tasks that run.

Auto-resume (aws-ecs, CDK deployments)
--------------------------------------
When Spot reclaims a task, ECS stops it with stopCode "TerminationNotice".
The worker has already synced its workspace, agent session, and artifacts to
S3 (the same snapshot `horde retry` uses). The status updater then RE-QUEUES
the run at top priority instead of failing it; the queue drain re-launches it
with the same run ID under the normal concurrency and spend caps, and orc
resumes where it left off.

Loop guard
----------
Each automatic resume increments the run's resume_count. After MAX_RESUMES
(default 5, configurable via the CDK construct's maxSpotResumes) the run is
left terminal ("killed") instead of resumed; recover it manually with
`horde retry`. A `horde kill` (stopCode "UserInitiated") is never auto-resumed.

CloudFormation bootstrap deployments register Spot but do NOT auto-resume
(no queue): a reclaimed run lands "killed"; run `horde retry` to resume it.
```

- [ ] **Step 3: Cross-reference from `retry` and `queue` topics. In the `retry` topic body, add a line noting Spot auto-resume is the automatic counterpart; in the `queue` topic, note that re-queued resumes share the drain. (Add one sentence each — match surrounding prose.)**

- [ ] **Step 4: If there is a hard-coded list of topic names (for `horde docs` with no arg, or a test asserting the topic set), add `spot` to it. Search:**

Run: `grep -rn "spot\|availableTopics\|topicNames\|docs_test" internal/docs/`
Expected: find and update any topic-list test/const so `horde docs spot` is discoverable and tests pass.

- [ ] **Step 5: Verify the topic renders**

Run: `go run ./cmd/horde docs spot 2>&1 | head -5`
Expected: the Spot topic header prints.

- [ ] **Step 6: Run docs tests**

Run: `go test ./internal/docs/ -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/docs/content.go
git commit -m "docs: add 'spot' topic; cross-reference retry/queue"
```

---

## Task 11: README + CLAUDE.md (doc layers 1 & 2)

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`

- [ ] **Step 1: README — add Spot to the user-facing feature description. Find the ECS/launch section (search `README.md` for "aws-ecs" or "launch") and add a short paragraph:**

```markdown
ECS runs use Fargate Spot by default for the cost saving and resume
automatically if Spot capacity is reclaimed. Pin a run to guaranteed capacity
with `horde launch <ticket> --capacity on-demand`. See `horde docs spot`.
```

- [ ] **Step 2: CLAUDE.md (a) — add a new "Key Design Decision" bullet (in the `## Key Design Decisions` list). Verbatim:**

```markdown
- Fargate Spot by default + auto-resume (aws-ecs): ECS runs launch via a `CapacityProviderStrategy` (NOT `LaunchType` — the two are mutually exclusive in `RunTask`), defaulting to `FARGATE_SPOT`; `horde launch --capacity spot|on-demand` overrides per run (stored on `Run.Capacity`, reused on retry/resume). Both providers are always registered on the cluster (free — registration costs nothing; you pay per running task). On a Spot interruption the task stops with ECS `stopCode == "TerminationNotice"` (a `horde kill` is `UserInitiated` and never matches); the worker's S3 snapshot (workspace + `~/.claude` + artifacts, already synced on SIGTERM) is the resume source. The TS status Lambda then RE-QUEUES the run (`status=queued`, `priority=highest`, `ResumeCount++`, `REMOVE instance_id/started_at`) under a `ResumeCount < MAX_RESUMES` budget (default 5, CDK `maxSpotResumes`) instead of the terminal write — the existing drain re-launches it with the same run ID under the concurrency + spend gates. The re-queue is conditional on not-already-terminal, so a racing synchronous `horde kill` wins. Auto-resume is **CDK-only** (it needs the #36 queue/drain; the Python/CFN status Lambda is intentionally not updated — a reclaimed run there lands `killed` for manual `horde retry`). Both CDK (`enableFargateCapacityProviders`) and bootstrap CFN register `FARGATE_SPOT`; the task def keeps `RequiresCompatibilities: [FARGATE]` (a compatibility, not a capacity provider). `Run.ResumeCount`/`Run.Capacity` are additive store fields (SQLite `ensureColumns`, nullable Dynamo attrs) surfaced under `--json`.
```

- [ ] **Step 3: CLAUDE.md (b) — update the `## Architecture` notes. In the `internal/provider/` line, note the capacity-provider strategy; in the `internal/store/` line, note the new fields; in the three-lockstep-readers design decision (the sidecar/by-name reader note), add that the resume DECISION is TS-Lambda-only. Add to the `cmd/horde/` command list that `launch` carries `--capacity`. Keep edits surgical — one clause each, matching surrounding style.**

- [ ] **Step 4: Verify markdown is well-formed (no broken lists)**

Run: `grep -n "capacity\|Spot\|ResumeCount" README.md CLAUDE.md | head`
Expected: the new content appears in both files.

- [ ] **Step 5: Commit**

```bash
git add README.md CLAUDE.md
git commit -m "docs: README + CLAUDE.md for Spot-by-default and auto-resume"
```

---

## Task 12: Full verification sweep

**Files:** none (verification only)

- [ ] **Step 1: Unit tests (what CI runs)**

Run: `make unit-test`
Expected: PASS. Covers store conformance (Task 4), provider strategy (Task 5), CLI flag (Task 6), JSON (Task 7), docs (Task 10).

- [ ] **Step 2: `go vet`**

Run: `make vet`
Expected: clean.

- [ ] **Step 3: CDK build + tests + CFN validation**

Run: `cd cdk && npm run build && npm test && cd .. && make bootstrap-validate-test`
Expected: PASS — status-lambda re-queue tests, drain capacity, cluster snapshot, CFN ValidateTemplate.

- [ ] **Step 4: Doc-gate confirmation (all 4 layers present)**

Run:
```bash
go run ./cmd/horde launch --help 2>&1 | grep -q capacity && echo "help OK"
go run ./cmd/horde docs spot 2>&1 | grep -qi "Fargate Spot" && echo "docs OK"
grep -qi "spot" README.md && echo "README OK"
grep -qi "Fargate Spot by default" CLAUDE.md && echo "CLAUDE OK"
```
Expected: all four "OK" lines.

- [ ] **Step 5: Integration tests (Docker path must be unaffected — capacity is ECS-only)**

Run: `make integration-test`
Expected: PASS. The docker provider ignores `Capacity`; this confirms no regression.

- [ ] **Step 6: Note E2E limitation honestly**

E2E (`make e2e-*`, developer-local) is out of automated scope. A real Spot reclaim cannot be forced on demand. Manual verification, if run: deploy the CDK stack, launch a run, then simulate by stopping the task with a `TerminationNotice`-shaped event (or `aws ecs stop-task` and observe the status Lambda re-queue in CloudWatch logs + the run returning to `queued` then `running` with `resume_count=1`). Do NOT claim automated E2E coverage of Spot reclaim — state the simulation and its limits in the PR description.

- [ ] **Step 7: Final commit if any verification fixes were needed (otherwise skip)**

```bash
git add -p   # stage only verification-driven fixes, per the repo's "no git add ." rule
git commit -m "fix: verification sweep adjustments"
```

---

## Self-Review

**Spec coverage:**
- Run on Spot (capacity providers, both registered) → Tasks 5, 8 ✅
- Per-launch `--capacity` (default spot, stored, reused on resume/retry) → Tasks 1, 5, 6 ✅
- Auto-resume via re-queue on TerminationNotice → Task 9 ✅
- Resume respects gates (re-queue, drain picks up) → Task 9 (re-queue) + existing drain (Task 8 makes drain honor capacity) ✅
- Resume budget (ResumeCount/MAX_RESUMES) → Tasks 1, 8 (env), 9 (logic) ✅
- Kill-race conditional guard → Task 9 Step 4 ✅
- Data model (Capacity/ResumeCount additive, both stores, conformance) → Tasks 1–4 ✅
- `--json` surface → Task 7 ✅
- CDK-only scope; CFN registers Spot but no resume → Tasks 8, 9 ✅
- 4 doc layers → Tasks 6 (help), 10 (docs), 11 (README+CLAUDE) ✅
- Testing strategy (Go unit, CLI, TS Lambda, CDK, E2E note) → Tasks 4,5,6,7,9 + Task 12 ✅

**Placeholder scan:** No TBD/TODO. Task 7's converters (`statusToV1`/`listToV1`/`ResultsV1`) were read verbatim and the steps cite exact line anchors. One remaining "match the existing pattern" instruction (Task 6 Step 1 CLI test-app constructor name) is guarded by "use whatever the surrounding launch tests use" — a discovery instruction, not a placeholder, because that harness name wasn't read verbatim. Every code block is complete.

**Type consistency:** `Capacity`/`CapacitySpot`/`CapacityOnDemand`/`ParseCapacity` (store) used consistently in Tasks 1,4,5,6. `ResumeCount` (Go int), `resume_count` (SQLite col / Dynamo attr / JSON key) consistent. `MAX_RESUMES` env (TS) ↔ `maxSpotResumes` CDK prop (Task 8) ↔ read in status Lambda (Task 9) consistent. `--capacity` string flag → `string(capacity)` into `LaunchOpts.Capacity` (string) → compared to `string(store.CapacityOnDemand)` in ecs.go consistent. `requeued` Result variant added in Task 9 Step 5 matches the test assertion in Step 1.
