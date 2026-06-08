# Token-level Telemetry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Thread per-run token counts (input/output/cache-creation/cache-read + turns) from orc through horde's store and `--json` contract, so an autonomous orchestrator can read token usage per run and derive burn-rate client-side.

**Architecture:** orc already writes token totals to `costs.json` (live, per-phase) and will later add them to `run-result.json` (final). horde reads `costs.json` first, `run-result.json` token fields as a forward fallback. Token data lands in a new `*store.TokenUsage` field on `Run`, persisted to SQLite (new columns via `ensureColumns`) and DynamoDB (new `N` attributes), and surfaced as a nested `tokens` object in `StatusV1`/`ListRunV1`/`ResultsV1` plus a summed `tokens` in `ListSummary`. Liveness is lazy/pull-based and matches the existing `fetchLiveCost` pattern — genuinely live on Docker, finalize-time on ECS.

**Tech Stack:** Go 1.24, `modernc.org/sqlite`, `aws-sdk-go-v2/dynamodb`, `urfave/cli/v3`. TypeScript (CDK status Lambda). YAML/Python (bootstrap CloudFormation Lambda).

**Spec:** `docs/superpowers/specs/2026-06-08-token-telemetry-design.md`

**Conventions (from CLAUDE.md):**
- Commit messages short and direct. NO "Co-Authored-By" / AI attribution lines.
- Stage specific files — never `git add .` / `git add -A`.
- `gofmt` and `go vet` clean. Errors returned, wrapped with context.
- Run `make unit-test` for Go store/provider/cmd changes; `cd cdk && npm run build && npm test` for the TS Lambda.

---

## File Structure

**Modify:**
- `internal/store/store.go` — add `TokenUsage` struct; `Run.Tokens *TokenUsage`; `RunUpdate.Tokens *TokenUsage`.
- `internal/store/sqlite.go` — 5 new columns in DDL + `ensureColumns`; thread through `CreateRun`, `scanRun` (+ all 5 SELECT column lists), `UpdateRun`.
- `internal/store/dynamo_schema.go` — 5 new `Attr*` consts.
- `internal/store/dynamo.go` — write in `CreateRun`/`UpdateRun`, read in `parseRun`.
- `internal/store/conformance_test.go` — round-trip assertions for `Tokens` (set + nil + update).
- `internal/provider/docker.go` — extend `RunResult` with token fields; add `ReadTokenUsage` + a shared `tokenUsageFromAudit` helper; set `run.Tokens` in all 4 `Finalize` branches.
- `internal/provider/docker_test.go` — `ReadTokenUsage` unit tests.
- `cmd/horde/jsonv1.go` — `TokensV1` type; add to `StatusV1`/`ListRunV1`/`ResultsV1`; sum in `ListSummary`; populate in converters.
- `cmd/horde/jsonv1_test.go` (or wherever jsonv1 is tested) — converter assertions.
- `cmd/horde/main.go` — extend `liveCosts` + `fetchLiveCost` (rename to `fetchLiveTelemetry`) to also return tokens; update 2 call sites (status ~582, list ~936); extend `fullRunResult`/`fullResultsToV1` wiring.
- `cdk/src/status-lambda/index.ts` — fetch `costs.json`, write token attributes to DynamoDB.
- `cdk/src/status-lambda/*.test.ts` — token extraction test.
- `internal/bootstrap/templates/stack.yaml.tmpl` — mirror the TS Lambda token logic in Python.
- `ORC_CONTRACT_EXPECTATIONS.md` — document `costs.json` token schema as a named contract.
- `internal/docs/` — update the relevant `horde docs json` topic to document the `tokens` object.

**Create:** none (all changes extend existing files).

---

## Task 1: Add `TokenUsage` to the store data model

**Files:**
- Modify: `internal/store/store.go:82-118`

- [ ] **Step 1: Add the `TokenUsage` type and the two struct fields**

In `internal/store/store.go`, immediately above `type Run struct {` (line 82), add:

```go
// TokenUsage is the per-run token telemetry orc reports, as run-level totals
// summed across phases. Zero counts are meaningful (a run can legitimately use
// 0 cache-read tokens), so presence is tracked by the enclosing *TokenUsage
// being nil — mirroring the TotalCostUSD *float64 "nil = not yet known"
// convention. Turns is the agent-invocation count summed across phases.
type TokenUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	Turns               int
}
```

In `type Run struct`, add after `TotalCostUSD *float64` (line 105):

```go
	TotalCostUSD *float64
	// Tokens is nil until orc reports usage (pre-finalize, or an orc old
	// enough that neither costs.json nor run-result.json carried token totals).
	Tokens *TokenUsage
```

In `type RunUpdate struct`, add after `TotalCostUSD *float64` (line 116):

```go
	TotalCostUSD *float64
	Tokens       *TokenUsage
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./internal/store/...`
Expected: builds clean (no other code references `Tokens` yet).

- [ ] **Step 3: Commit**

```bash
git add internal/store/store.go
git commit -m "store: add TokenUsage to Run and RunUpdate"
```

---

## Task 2: SQLite persistence for token counts

**Files:**
- Modify: `internal/store/sqlite.go` (DDL ~46-63, `ensureColumns` ~110-112, `CreateRun` ~158-180, `scanRun` ~206-281, and the 5 SELECT column lists at ~189, ~351, ~390, ~430, ~468, plus `UpdateRun` ~311-314)
- Test: `internal/store/conformance_test.go`

> **Note on the SELECT lists:** the column list
> `id, repo, ticket, branch, workflow, provider, instance_id, metadata, labels, status, exit_code, launched_by, started_at, completed_at, timeout_at, total_cost_usd`
> is duplicated verbatim in 5 read sites (`GetRun`, `ListByRepo`, `ListRuns`, `FindActiveByTicket`, `ListActive`) and `scanRun` scans them positionally. ALL 5 lists and `scanRun` must append the 5 new columns in the SAME order, or the positional scan breaks. Add the new columns at the END of each list, after `total_cost_usd`.

- [ ] **Step 1: Write the failing conformance assertion**

In `internal/store/conformance_test.go`, find the `CreateGetRun/AllFields` subtest (~line 322). After the existing `run.TotalCostUSD = ptr(1.23)` line (~329), add:

```go
		run.Tokens = &TokenUsage{
			InputTokens:         100,
			OutputTokens:        200,
			CacheCreationTokens: 300,
			CacheReadTokens:     400,
			Turns:               5,
		}
```

After the existing `TotalCostUSD` assertion block (~378-380), add:

```go
		if got.Tokens == nil {
			t.Fatalf("Tokens: got nil, want %+v", run.Tokens)
		}
		if *got.Tokens != *run.Tokens {
			t.Errorf("Tokens: got %+v, want %+v", *got.Tokens, *run.Tokens)
		}
```

In the `CreateGetRun/NilOptionalFields` subtest (~392), after the `TotalCostUSD` nil assertion (~410-411), add:

```go
		if got.Tokens != nil {
			t.Errorf("Tokens: expected nil, got %+v", got.Tokens)
		}
```

- [ ] **Step 2: Run the conformance test to verify it fails**

Run: `go test ./internal/store/ -run 'Conformance/CreateGetRun' -v`
Expected: FAIL — `got.Tokens` is nil (not yet persisted) in the AllFields case.

- [ ] **Step 3: Add columns to the DDL and `ensureColumns`**

In `internal/store/sqlite.go`, change the `ddl` const (replace the `total_cost_usd REAL` final line, ~62) to:

```go
		timeout_at     TEXT NOT NULL,
		total_cost_usd REAL,
		input_tokens          INTEGER,
		output_tokens         INTEGER,
		cache_creation_tokens INTEGER,
		cache_read_tokens     INTEGER,
		turns                 INTEGER
	);`
```

In `ensureColumns`, extend the `additive` slice (~110-112) to:

```go
	additive := []struct{ name, ddl string }{
		{"labels", "TEXT"},
		{"input_tokens", "INTEGER"},
		{"output_tokens", "INTEGER"},
		{"cache_creation_tokens", "INTEGER"},
		{"cache_read_tokens", "INTEGER"},
		{"turns", "INTEGER"},
	}
```

- [ ] **Step 4: Thread tokens through `CreateRun`**

In `CreateRun`, change the INSERT column list and placeholders (~159-163) to add the 5 columns and 5 `?`:

```go
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (
			id, repo, ticket, branch, workflow, provider,
			instance_id, metadata, labels, status, exit_code, launched_by,
			started_at, completed_at, timeout_at, total_cost_usd,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, turns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
```

Before the `ExecContext` call, after the `completedAt` block (~156), add nullable token locals:

```go
	var inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens, turns *int
	if run.Tokens != nil {
		inputTokens = &run.Tokens.InputTokens
		outputTokens = &run.Tokens.OutputTokens
		cacheCreationTokens = &run.Tokens.CacheCreationTokens
		cacheReadTokens = &run.Tokens.CacheReadTokens
		turns = &run.Tokens.Turns
	}
```

Append these to the argument list after `run.TotalCostUSD,` (~179):

```go
		run.TotalCostUSD,
		inputTokens,
		outputTokens,
		cacheCreationTokens,
		cacheReadTokens,
		turns,
```

- [ ] **Step 5: Add the 5 columns to all SELECT lists and `scanRun`**

In EACH of the 5 SELECT statements (`GetRun` ~189-191, `ListByRepo` ~351-353, `ListRuns` ~390-392, `FindActiveByTicket` ~430-432, `ListActive` ~468-470), change the trailing `timeout_at, total_cost_usd` to:

```go
			started_at, completed_at, timeout_at, total_cost_usd,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, turns
```

In `scanRun`, after `var totalCostUSD sql.NullFloat64` (~215), add:

```go
	var inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens, turns sql.NullInt64
```

In the `scanner.Scan(...)` call, after `&totalCostUSD,` (~233), add:

```go
		&totalCostUSD,
		&inputTokens,
		&outputTokens,
		&cacheCreationTokens,
		&cacheReadTokens,
		&turns,
```

After the `if totalCostUSD.Valid { ... }` block (~264-266), add:

```go
	if inputTokens.Valid || outputTokens.Valid || cacheCreationTokens.Valid || cacheReadTokens.Valid || turns.Valid {
		run.Tokens = &TokenUsage{
			InputTokens:         int(inputTokens.Int64),
			OutputTokens:        int(outputTokens.Int64),
			CacheCreationTokens: int(cacheCreationTokens.Int64),
			CacheReadTokens:     int(cacheReadTokens.Int64),
			Turns:               int(turns.Int64),
		}
	}
```

Also update the `scanRun` doc comment (~204-205): change "all 16 columns" to "all 21 columns".

- [ ] **Step 6: Thread tokens through `UpdateRun`**

In `UpdateRun`, after the `if update.TotalCostUSD != nil { ... }` block (~311-314), add:

```go
	if update.Tokens != nil {
		setClauses = append(setClauses,
			"input_tokens = ?",
			"output_tokens = ?",
			"cache_creation_tokens = ?",
			"cache_read_tokens = ?",
			"turns = ?",
		)
		args = append(args,
			update.Tokens.InputTokens,
			update.Tokens.OutputTokens,
			update.Tokens.CacheCreationTokens,
			update.Tokens.CacheReadTokens,
			update.Tokens.Turns,
		)
	}
```

- [ ] **Step 7: Run the conformance test to verify it passes (SQLite side)**

Run: `go test ./internal/store/ -run 'Conformance/CreateGetRun' -v`
Expected: PASS for the SQLite conformance instance. (The DynamoDB instance still fails — fixed in Task 3.)

If both stores run under one `Conformance` test and the Dynamo half now fails, that's expected; proceed to Task 3 before relying on a green `./internal/store/`.

- [ ] **Step 8: Commit**

```bash
git add internal/store/sqlite.go internal/store/conformance_test.go
git commit -m "store/sqlite: persist token counts"
```

---

## Task 3: DynamoDB persistence for token counts

**Files:**
- Modify: `internal/store/dynamo_schema.go:9-26`, `internal/store/dynamo.go` (`CreateRun` ~72-74, `parseRun` ~209-219, `UpdateRun` ~306-309)

> The 5 token attributes are NOT used in any GSI key schema, so they are NOT added to `AttributeDefinitions` (DynamoDB rejects definitions for non-key attributes). Only add the `Attr*` name constants.

- [ ] **Step 1: Add attribute-name constants**

In `internal/store/dynamo_schema.go`, after `AttrTotalCostUSD = "total_cost_usd"` (~25), add:

```go
	AttrTotalCostUSD       = "total_cost_usd"
	AttrInputTokens        = "input_tokens"
	AttrOutputTokens       = "output_tokens"
	AttrCacheCreationTokens = "cache_creation_tokens"
	AttrCacheReadTokens    = "cache_read_tokens"
	AttrTurns              = "turns"
```

- [ ] **Step 2: Write tokens in `CreateRun`**

In `internal/store/dynamo.go`, after the `if run.TotalCostUSD != nil { ... }` block (~72-74), add:

```go
	if run.Tokens != nil {
		item[AttrInputTokens] = &types.AttributeValueMemberN{Value: strconv.Itoa(run.Tokens.InputTokens)}
		item[AttrOutputTokens] = &types.AttributeValueMemberN{Value: strconv.Itoa(run.Tokens.OutputTokens)}
		item[AttrCacheCreationTokens] = &types.AttributeValueMemberN{Value: strconv.Itoa(run.Tokens.CacheCreationTokens)}
		item[AttrCacheReadTokens] = &types.AttributeValueMemberN{Value: strconv.Itoa(run.Tokens.CacheReadTokens)}
		item[AttrTurns] = &types.AttributeValueMemberN{Value: strconv.Itoa(run.Tokens.Turns)}
	}
```

- [ ] **Step 3: Read tokens in `parseRun`**

In `parseRun`, after the `if av, ok := item[AttrTotalCostUSD]; ok { ... }` block (~209-219), add:

```go
	// Token counts: all five attributes are written together (or not at all)
	// by CreateRun/UpdateRun, but parse each defensively. Presence of ANY of
	// them yields a non-nil Tokens; a missing individual attribute reads as 0.
	parseTokenAttr := func(name string) (int, bool, error) {
		av, ok := item[name]
		if !ok {
			return 0, false, nil
		}
		nv, ok := av.(*types.AttributeValueMemberN)
		if !ok {
			return 0, false, fmt.Errorf("parsing run %q: invalid %q attribute", id, name)
		}
		v, err := strconv.Atoi(nv.Value)
		if err != nil {
			return 0, false, fmt.Errorf("parsing run %q: parsing %s: %w", id, name, err)
		}
		return v, true, nil
	}
	var tokens TokenUsage
	var anyToken bool
	for _, ta := range []struct {
		name string
		dst  *int
	}{
		{AttrInputTokens, &tokens.InputTokens},
		{AttrOutputTokens, &tokens.OutputTokens},
		{AttrCacheCreationTokens, &tokens.CacheCreationTokens},
		{AttrCacheReadTokens, &tokens.CacheReadTokens},
		{AttrTurns, &tokens.Turns},
	} {
		v, present, err := parseTokenAttr(ta.name)
		if err != nil {
			return nil, err
		}
		if present {
			anyToken = true
			*ta.dst = v
		}
	}
	if anyToken {
		run.Tokens = &tokens
	}
```

- [ ] **Step 4: Write tokens in `UpdateRun`**

In `UpdateRun`, after the `if update.TotalCostUSD != nil { ... }` block (~306-309), add:

```go
	if update.Tokens != nil {
		setClauses = append(setClauses,
			"input_tokens = :it",
			"output_tokens = :ot",
			"cache_creation_tokens = :cct",
			"cache_read_tokens = :crt",
			"turns = :tn",
		)
		exprAttrValues[":it"] = &types.AttributeValueMemberN{Value: strconv.Itoa(update.Tokens.InputTokens)}
		exprAttrValues[":ot"] = &types.AttributeValueMemberN{Value: strconv.Itoa(update.Tokens.OutputTokens)}
		exprAttrValues[":cct"] = &types.AttributeValueMemberN{Value: strconv.Itoa(update.Tokens.CacheCreationTokens)}
		exprAttrValues[":crt"] = &types.AttributeValueMemberN{Value: strconv.Itoa(update.Tokens.CacheReadTokens)}
		exprAttrValues[":tn"] = &types.AttributeValueMemberN{Value: strconv.Itoa(update.Tokens.Turns)}
	}
```

- [ ] **Step 5: Add an `UpdateRun` tokens conformance assertion**

In `internal/store/conformance_test.go`, find the `UpdateRun/MultipleFields` subtest (~558). It already sets `TotalCostUSD: ptr(9.99)` in the update. Add to that same `RunUpdate` literal:

```go
			TotalCostUSD: ptr(9.99),
			Tokens: &TokenUsage{
				InputTokens:         11,
				OutputTokens:        22,
				CacheCreationTokens: 33,
				CacheReadTokens:     44,
				Turns:               2,
			},
```

After that subtest's existing post-update `GetRun` + assertions, add (adjust the `got` variable name to match the subtest's local):

```go
		if got.Tokens == nil {
			t.Fatalf("Tokens after update: got nil, want non-nil")
		}
		if got.Tokens.InputTokens != 11 || got.Tokens.OutputTokens != 22 ||
			got.Tokens.CacheCreationTokens != 33 || got.Tokens.CacheReadTokens != 44 ||
			got.Tokens.Turns != 2 {
			t.Errorf("Tokens after update: got %+v", *got.Tokens)
		}
```

- [ ] **Step 6: Run the full store conformance suite (both stores green)**

Run: `go test ./internal/store/ -v`
Expected: PASS for both SQLite and DynamoDB conformance instances, including the new `Tokens` assertions.

- [ ] **Step 7: Commit**

```bash
git add internal/store/dynamo_schema.go internal/store/dynamo.go internal/store/conformance_test.go
git commit -m "store/dynamo: persist token counts"
```

---

## Task 4: `ReadTokenUsage` — parse tokens from the audit dir (costs.json first)

**Files:**
- Modify: `internal/provider/docker.go` (extend `RunResult` ~459-462; add helpers after `ReadRunResult` ~479)
- Test: `internal/provider/docker_test.go`

> orc's `costs.json` run-total field names are `total_input_tokens`, `total_output_tokens`, `total_cache_creation_input_tokens`, `total_cache_read_input_tokens`. `turns` exists ONLY per-phase in `costs.json` (each entry in the `phases` array has a `turns` int) — there is no run-total `turns`, so we sum it. The forward `run-result.json` token field names (once orc#NN lands) are assumed to match the `total_*` costs.json names.

- [ ] **Step 1: Write the failing test**

In `internal/provider/docker_test.go`, add:

```go
func TestReadTokenUsage_CostsJSON(t *testing.T) {
	home := t.TempDir()
	run := &store.Run{ID: "run123", Workflow: "implement-ticket", Ticket: "PROJ-1"}

	auditDir := filepath.Join(LocalResultsDir(home, run.ID), "audit", run.Workflow, run.Ticket)
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatal(err)
	}
	costs := `{
		"phases": [
			{"name": "plan", "turns": 1},
			{"name": "implement", "turns": 4}
		],
		"total_input_tokens": 54791,
		"total_output_tokens": 87915,
		"total_cache_creation_input_tokens": 529692,
		"total_cache_read_input_tokens": 8934181
	}`
	if err := os.WriteFile(filepath.Join(auditDir, "costs.json"), []byte(costs), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ReadTokenUsage(home, run)
	if got == nil {
		t.Fatal("ReadTokenUsage: got nil, want non-nil")
	}
	want := store.TokenUsage{
		InputTokens:         54791,
		OutputTokens:        87915,
		CacheCreationTokens: 529692,
		CacheReadTokens:     8934181,
		Turns:               5,
	}
	if *got != want {
		t.Errorf("ReadTokenUsage: got %+v, want %+v", *got, want)
	}
}

func TestReadTokenUsage_Missing(t *testing.T) {
	home := t.TempDir()
	run := &store.Run{ID: "run123", Workflow: "implement-ticket", Ticket: "PROJ-1"}
	if got := ReadTokenUsage(home, run); got != nil {
		t.Errorf("ReadTokenUsage with no files: got %+v, want nil", got)
	}
}

func TestReadTokenUsage_RunResultFallback(t *testing.T) {
	home := t.TempDir()
	run := &store.Run{ID: "run123", Workflow: "implement-ticket", Ticket: "PROJ-1"}

	auditDir := filepath.Join(LocalResultsDir(home, run.ID), "audit", run.Workflow, run.Ticket)
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No costs.json; run-result.json carries the forward token fields.
	rr := `{
		"exit_code": 0,
		"total_input_tokens": 10,
		"total_output_tokens": 20,
		"total_cache_creation_input_tokens": 30,
		"total_cache_read_input_tokens": 40,
		"turns": 3
	}`
	if err := os.WriteFile(filepath.Join(auditDir, "run-result.json"), []byte(rr), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ReadTokenUsage(home, run)
	if got == nil {
		t.Fatal("ReadTokenUsage: got nil, want non-nil from run-result.json fallback")
	}
	want := store.TokenUsage{InputTokens: 10, OutputTokens: 20, CacheCreationTokens: 30, CacheReadTokens: 40, Turns: 3}
	if *got != want {
		t.Errorf("ReadTokenUsage fallback: got %+v, want %+v", *got, want)
	}
}
```

Ensure the test file imports `os`, `path/filepath`, and `github.com/jorge-barreto/horde/internal/store` (add any missing).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/provider/ -run TestReadTokenUsage -v`
Expected: FAIL — `ReadTokenUsage` undefined.

- [ ] **Step 3: Extend `RunResult` and add the parse helpers**

In `internal/provider/docker.go`, extend the `RunResult` struct (~459-462) with the forward token fields:

```go
type RunResult struct {
	TotalCostUSD *float64 `json:"total_cost_usd"`
	ExitCode     *int     `json:"exit_code"`
	// Forward token fields: orc does not write these to run-result.json yet
	// (tracked in jorge-barreto/orc#NN). costs.json is the source today; these
	// become the run-summary fallback once orc promotes them. Field names match
	// costs.json's run-total names.
	TotalInputTokens         *int `json:"total_input_tokens"`
	TotalOutputTokens        *int `json:"total_output_tokens"`
	TotalCacheCreationTokens *int `json:"total_cache_creation_input_tokens"`
	TotalCacheReadTokens     *int `json:"total_cache_read_input_tokens"`
	Turns                    *int `json:"turns"`
}
```

After `ReadRunResult` (~479), add:

```go
// costsFile is the subset of orc's costs.json that horde reads for token
// telemetry. orc writes run-total token counts plus a per-phase array (each
// phase carries a turns count). costs.json is the live, per-phase-flushed
// source — preferred over run-result.json because it exists during a run and
// already carries tokens on every orc version.
type costsFile struct {
	TotalInputTokens         int `json:"total_input_tokens"`
	TotalOutputTokens        int `json:"total_output_tokens"`
	TotalCacheCreationTokens int `json:"total_cache_creation_input_tokens"`
	TotalCacheReadTokens     int `json:"total_cache_read_input_tokens"`
	Phases                   []struct {
		Turns int `json:"turns"`
	} `json:"phases"`
}

// tokenUsageFromCostsJSON parses raw costs.json bytes into a TokenUsage,
// summing per-phase turns into a run total (orc has no run-total turns).
// Returns nil on malformed JSON.
func tokenUsageFromCostsJSON(data []byte) *store.TokenUsage {
	var cf costsFile
	if json.Unmarshal(data, &cf) != nil {
		return nil
	}
	turns := 0
	for _, p := range cf.Phases {
		turns += p.Turns
	}
	return &store.TokenUsage{
		InputTokens:         cf.TotalInputTokens,
		OutputTokens:        cf.TotalOutputTokens,
		CacheCreationTokens: cf.TotalCacheCreationTokens,
		CacheReadTokens:     cf.TotalCacheReadTokens,
		Turns:               turns,
	}
}

// tokenUsageFromRunResult extracts the forward token fields from an already
// parsed run-result.json. Returns nil when none are present (older orc).
func tokenUsageFromRunResult(rr RunResult) *store.TokenUsage {
	if rr.TotalInputTokens == nil && rr.TotalOutputTokens == nil &&
		rr.TotalCacheCreationTokens == nil && rr.TotalCacheReadTokens == nil && rr.Turns == nil {
		return nil
	}
	deref := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	return &store.TokenUsage{
		InputTokens:         deref(rr.TotalInputTokens),
		OutputTokens:        deref(rr.TotalOutputTokens),
		CacheCreationTokens: deref(rr.TotalCacheCreationTokens),
		CacheReadTokens:     deref(rr.TotalCacheReadTokens),
		Turns:               deref(rr.Turns),
	}
}

// ReadTokenUsage reads per-run token totals from the run's local audit dir.
// costs.json is preferred (orc writes token totals there today, flushed
// per-phase and atomically); the run-result.json token fields are the forward
// fallback once orc promotes them (jorge-barreto/orc#NN). Returns nil when
// neither source carries usage — best-effort, exactly like ReadRunResult.
func ReadTokenUsage(homeDir string, run *store.Run) *store.TokenUsage {
	auditBase := LocalResultsDir(homeDir, run.ID)

	costsPath := filepath.Join(auditBase, AuditRelPath(run.Workflow, run.Ticket, "costs.json"))
	if data, err := os.ReadFile(costsPath); err == nil {
		if tu := tokenUsageFromCostsJSON(data); tu != nil {
			return tu
		}
	}

	resultPath := filepath.Join(auditBase, AuditRelPath(run.Workflow, run.Ticket, "run-result.json"))
	if data, err := os.ReadFile(resultPath); err == nil {
		var rr RunResult
		if json.Unmarshal(data, &rr) == nil {
			return tokenUsageFromRunResult(rr)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/provider/ -run TestReadTokenUsage -v`
Expected: PASS (all three cases).

- [ ] **Step 5: Commit**

```bash
git add internal/provider/docker.go internal/provider/docker_test.go
git commit -m "provider: ReadTokenUsage parses tokens from costs.json with run-result fallback"
```

---

## Task 5: Wire tokens into Docker `Finalize`

**Files:**
- Modify: `internal/provider/docker.go` `Finalize` (~539-722) — 4 branches set `run.Tokens`.

> Each `Finalize` branch already sets `run.TotalCostUSD`. Add `run.Tokens = ReadTokenUsage(homeDir, run)` right after, in each branch. The `StateUnknown` branch reads from the workspace `.orc` dir (not `LocalResultsDir`) when the container vanished — but `Finalize` copies that audit tree into `LocalResultsDir` just above (~681-685) before reading, so `ReadTokenUsage(homeDir, run)` (which reads `LocalResultsDir`) sees the copied costs.json. Place the call after the copy.

- [ ] **Step 1: Extend the existing cost-finalize test to assert tokens**

Extend `TestDockerProvider_Finalize_RunningWithMarkerAndCost` in `internal/provider/docker_test.go` (~line 1473) — it already writes a `.horde-exit-code` marker (code 0) and a `run-result.json` with cost, and runs the running-completed branch. Read its setup to learn where it writes `run-result.json` (which audit dir under `LocalResultsDir`), then:

1. Write a `costs.json` into that SAME audit dir, using the `costs` JSON literal from Task 4 Step 1.
2. After the existing post-`Finalize` cost assertion, add:

```go
	if run.Tokens == nil {
		t.Fatal("Finalize: run.Tokens is nil, want populated from costs.json")
	}
	if run.Tokens.InputTokens != 54791 || run.Tokens.OutputTokens != 87915 ||
		run.Tokens.CacheCreationTokens != 529692 || run.Tokens.CacheReadTokens != 8934181 ||
		run.Tokens.Turns != 5 {
		t.Errorf("Finalize: run.Tokens = %+v", *run.Tokens)
	}
```

> The test writes `run-result.json` under `LocalResultsDir(home, runID)/audit/<workflow>/<ticket>/` (the same path `AuditRelPath` builds). Write `costs.json` beside it. Match the test's existing `home`/`run` variable names.

- [ ] **Step 2: Set `run.Tokens` in the running-completed branch**

After `run.TotalCostUSD = cost` (~594), add:

```go
			run.TotalCostUSD = cost
			run.Tokens = ReadTokenUsage(homeDir, run)
			return nil
```

(Replace the existing `run.TotalCostUSD = cost` + `return nil` pair.)

- [ ] **Step 3: Set `run.Tokens` in the running-timeout branch**

After `run.TotalCostUSD = cost` (~621), add `run.Tokens = ReadTokenUsage(homeDir, run)` before `return nil`:

```go
			run.TotalCostUSD = cost
			run.Tokens = ReadTokenUsage(homeDir, run)
			return nil
```

- [ ] **Step 4: Set `run.Tokens` in the stopped branch**

After `run.TotalCostUSD = cost` (~669), add:

```go
		run.TotalCostUSD = cost
		run.Tokens = ReadTokenUsage(homeDir, run)
```

- [ ] **Step 5: Set `run.Tokens` in the unknown branch**

After the final `run.TotalCostUSD = cost` (~721), add:

```go
		run.TotalCostUSD = cost
		run.Tokens = ReadTokenUsage(homeDir, run)
```

- [ ] **Step 6: Run provider tests**

Run: `go test ./internal/provider/ -run 'Finalize|ReadTokenUsage' -v`
Expected: PASS. (Remove the skipped stub if you extended a real test.)

- [ ] **Step 7: Commit**

```bash
git add internal/provider/docker.go internal/provider/docker_test.go
git commit -m "provider: populate run.Tokens in Docker Finalize"
```

---

## Task 6: Live token telemetry on the status/list commands (Docker)

**Files:**
- Modify: `cmd/horde/main.go` (`liveCosts` ~1247-1249, `fetchLiveCost` ~1354-1368, call sites ~580-584 and ~934-938)

> This mirrors the existing `fetchLiveCost` lazy-live pattern: when a Docker run is still running and the store has no tokens yet, read `costs.json` from the live container. Rename `fetchLiveCost` → `fetchLiveTelemetry` and return both cost and tokens so we read the container file once.

- [ ] **Step 1: Extend the `liveCosts` type**

In `cmd/horde/main.go`, replace the `liveCosts` type (~1247-1249) with:

```go
type liveCosts struct {
	TotalCostUSD             float64 `json:"total_cost_usd"`
	TotalInputTokens         int     `json:"total_input_tokens"`
	TotalOutputTokens        int     `json:"total_output_tokens"`
	TotalCacheCreationTokens int     `json:"total_cache_creation_input_tokens"`
	TotalCacheReadTokens     int     `json:"total_cache_read_input_tokens"`
	Phases                   []struct {
		Turns int `json:"turns"`
	} `json:"phases"`
}
```

- [ ] **Step 2: Replace `fetchLiveCost` with `fetchLiveTelemetry`**

Replace `fetchLiveCost` (~1354-1368) with:

```go
// fetchLiveTelemetry reads current cost and token totals from a running
// container's costs.json. Both are best-effort (nil when unavailable). This is
// the Docker lazy-live path: the store has no values for a running run until
// Finalize, so status/list read the live file on demand.
func fetchLiveTelemetry(ctx context.Context, prov *provider.DockerProvider, run *store.Run) (*float64, *store.TokenUsage) {
	if run.InstanceID == "" {
		return nil, nil
	}
	costsPath := "/workspace/.orc/" + provider.AuditRelPath(run.Workflow, run.Ticket, "costs.json")
	data, err := prov.ReadContainerFile(ctx, run.InstanceID, costsPath)
	if err != nil {
		return nil, nil
	}
	var lc liveCosts
	if json.Unmarshal(data, &lc) != nil {
		return nil, nil
	}
	var cost *float64
	if lc.TotalCostUSD != 0 {
		cost = &lc.TotalCostUSD
	}
	turns := 0
	for _, p := range lc.Phases {
		turns += p.Turns
	}
	var tokens *store.TokenUsage
	if lc.TotalInputTokens != 0 || lc.TotalOutputTokens != 0 ||
		lc.TotalCacheCreationTokens != 0 || lc.TotalCacheReadTokens != 0 || turns != 0 {
		tokens = &store.TokenUsage{
			InputTokens:         lc.TotalInputTokens,
			OutputTokens:        lc.TotalOutputTokens,
			CacheCreationTokens: lc.TotalCacheCreationTokens,
			CacheReadTokens:     lc.TotalCacheReadTokens,
			Turns:               turns,
		}
	}
	return cost, tokens
}
```

- [ ] **Step 3: Update the `status` command call site**

In the status `Action` (~580-584), replace:

```go
			if run.TotalCostUSD == nil && (run.Status == store.StatusRunning || run.Status == store.StatusPending) {
				if dp, ok := prov.(*provider.DockerProvider); ok {
					run.TotalCostUSD = fetchLiveCost(ctx, dp, run)
				}
			}
```

with:

```go
			if run.TotalCostUSD == nil && (run.Status == store.StatusRunning || run.Status == store.StatusPending) {
				if dp, ok := prov.(*provider.DockerProvider); ok {
					cost, tokens := fetchLiveTelemetry(ctx, dp, run)
					run.TotalCostUSD = cost
					if run.Tokens == nil {
						run.Tokens = tokens
					}
				}
			}
```

- [ ] **Step 4: Update the `list` command call site**

Find the second `fetchLiveCost` call (~934-938) and apply the identical replacement (cost + tokens, guarding `run.Tokens == nil`). The surrounding condition is the same `TotalCostUSD == nil && running/pending` shape — match it.

- [ ] **Step 5: Build and vet**

Run: `go build ./... && go vet ./cmd/horde/...`
Expected: clean. No remaining references to `fetchLiveCost` (grep to confirm: `grep -rn fetchLiveCost cmd/` returns nothing).

- [ ] **Step 6: Commit**

```bash
git add cmd/horde/main.go
git commit -m "cmd: live token telemetry on status/list for running Docker runs"
```

---

## Task 7: Surface tokens in the `--json` contract

**Files:**
- Modify: `cmd/horde/jsonv1.go` (`StatusV1` ~40-54, `ListSummary` ~64-67, `ListRunV1` ~74-88, `ResultsV1` ~90-102, converters `statusToV1` ~122-142, `listToV1` ~144-177, `fullResultsToV1` ~179-210, `partialResultsToV1` ~236-246)
- Modify: `cmd/horde/main.go` (`fullRunResult` ~1370-1378 — add token fields so `results` reads them)
- Test: `cmd/horde/jsonv1_test.go`

- [ ] **Step 1: Write the failing converter test**

In `cmd/horde/jsonv1_test.go`, add:

```go
func TestStatusToV1_Tokens(t *testing.T) {
	run := &store.Run{
		ID: "r1", Ticket: "T-1", Status: store.StatusSuccess,
		StartedAt: time.Now(),
		Tokens: &store.TokenUsage{
			InputTokens: 1, OutputTokens: 2, CacheCreationTokens: 3, CacheReadTokens: 4, Turns: 5,
		},
	}
	v := statusToV1(run)
	if v.Tokens == nil {
		t.Fatal("StatusV1.Tokens: got nil, want non-nil")
	}
	if v.Tokens.Input != 1 || v.Tokens.Output != 2 || v.Tokens.CacheCreation != 3 ||
		v.Tokens.CacheRead != 4 || v.Tokens.Turns != 5 {
		t.Errorf("StatusV1.Tokens: got %+v", *v.Tokens)
	}
}

func TestStatusToV1_NoTokens(t *testing.T) {
	run := &store.Run{ID: "r1", Ticket: "T-1", Status: store.StatusRunning, StartedAt: time.Now()}
	if v := statusToV1(run); v.Tokens != nil {
		t.Errorf("StatusV1.Tokens: want nil, got %+v", v.Tokens)
	}
}

func TestListToV1_SummarySumsTokens(t *testing.T) {
	now := time.Now()
	runs := []*store.Run{
		{ID: "r1", StartedAt: now, Tokens: &store.TokenUsage{InputTokens: 1, OutputTokens: 2, Turns: 1}},
		{ID: "r2", StartedAt: now, Tokens: &store.TokenUsage{InputTokens: 10, OutputTokens: 20, Turns: 3}},
		{ID: "r3", StartedAt: now}, // no tokens — must not break the sum
	}
	v := listToV1(runs)
	if v.Summary.Tokens == nil {
		t.Fatal("ListSummary.Tokens: got nil, want non-nil")
	}
	if v.Summary.Tokens.Input != 11 || v.Summary.Tokens.Output != 22 || v.Summary.Tokens.Turns != 4 {
		t.Errorf("ListSummary.Tokens: got %+v", *v.Summary.Tokens)
	}
}
```

Add `time` and `store` imports if missing.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/horde/ -run 'StatusToV1_Tokens|ListToV1_Summary' -v`
Expected: FAIL — `v.Tokens` / `Summary.Tokens` undefined.

- [ ] **Step 3: Add the `TokensV1` type and a converter**

In `cmd/horde/jsonv1.go`, after the `ErrorV1` helpers (~38) and before `StatusV1`, add:

```go
// TokensV1 is the per-run token telemetry surfaced under --json. It is a
// pointer on its parent structs and omitempty, so it is absent entirely when
// horde has no token data for the run (pre-finalize on ECS, or an orc old
// enough not to emit tokens). Field names are the stable contract.
type TokensV1 struct {
	Input         int `json:"input"`
	Output        int `json:"output"`
	CacheCreation int `json:"cache_creation"`
	CacheRead     int `json:"cache_read"`
	Turns         int `json:"turns"`
}

// tokensToV1 maps a store.TokenUsage to the JSON contract shape, or nil.
func tokensToV1(t *store.TokenUsage) *TokensV1 {
	if t == nil {
		return nil
	}
	return &TokensV1{
		Input:         t.InputTokens,
		Output:        t.OutputTokens,
		CacheCreation: t.CacheCreationTokens,
		CacheRead:     t.CacheReadTokens,
		Turns:         t.Turns,
	}
}
```

- [ ] **Step 4: Add `Tokens` to the contract structs**

In `StatusV1` (after `TotalCostUSD` ~49):

```go
	TotalCostUSD *float64          `json:"total_cost_usd,omitempty"`
	Tokens       *TokensV1         `json:"tokens,omitempty"`
```

In `ListRunV1` (after `TotalCostUSD` ~83): add the identical `Tokens *TokensV1 \`json:"tokens,omitempty"\`` line.

In `ListSummary` (after `TotalCostUSD` ~66):

```go
	Count        int       `json:"count"`
	TotalCostUSD float64   `json:"total_cost_usd"`
	Tokens       *TokensV1 `json:"tokens,omitempty"`
```

In `ResultsV1` (after `TotalCostUSD` ~97): add `Tokens *TokensV1 \`json:"tokens,omitempty"\``.

- [ ] **Step 5: Populate `Tokens` in the converters**

In `statusToV1` (~124-137), add to the `StatusV1{...}` literal after `TotalCostUSD: run.TotalCostUSD,`:

```go
		TotalCostUSD: run.TotalCostUSD,
		Tokens:       tokensToV1(run.Tokens),
```

In `listToV1` (~151-165), add to the `ListRunV1{...}` literal after `TotalCostUSD: s.TotalCostUSD,`:

```go
			TotalCostUSD: s.TotalCostUSD,
			Tokens:       s.Tokens,
```

Still in `listToV1`, add a token accumulator. After `var totalCost float64` (~146):

```go
	var totalCost float64
	var tokenSum store.TokenUsage
	var anyTokens bool
```

Inside the loop, after the `if run.TotalCostUSD != nil { totalCost += ... }` block (~166-168):

```go
		if run.Tokens != nil {
			anyTokens = true
			tokenSum.InputTokens += run.Tokens.InputTokens
			tokenSum.OutputTokens += run.Tokens.OutputTokens
			tokenSum.CacheCreationTokens += run.Tokens.CacheCreationTokens
			tokenSum.CacheReadTokens += run.Tokens.CacheReadTokens
			tokenSum.Turns += run.Tokens.Turns
		}
```

Change the returned `ListSummary` (~172-175) to:

```go
		Summary: ListSummary{
			Count:        len(runs),
			TotalCostUSD: totalCost,
			Tokens:       summaryTokens(anyTokens, tokenSum),
		},
```

Add a helper after `listToV1`:

```go
func summaryTokens(any bool, sum store.TokenUsage) *TokensV1 {
	if !any {
		return nil
	}
	return tokensToV1(&sum)
}
```

In `partialResultsToV1` (~236-246), add after `TotalCostUSD: run.TotalCostUSD,`:

```go
		TotalCostUSD: run.TotalCostUSD,
		Tokens:       tokensToV1(run.Tokens),
```

- [ ] **Step 6: Add token fields to `fullRunResult` and populate `fullResultsToV1`**

In `cmd/horde/main.go`, extend `fullRunResult` (~1370-1378) with the forward token fields:

```go
type fullRunResult struct {
	ExitCode      int           `json:"exit_code"`
	Status        string        `json:"status"`
	Ticket        string        `json:"ticket"`
	Workflow      string        `json:"workflow"`
	TotalCostUSD  *float64      `json:"total_cost_usd"`
	TotalDuration string        `json:"total_duration"`
	Phases        []phaseResult `json:"phases"`

	TotalInputTokens         *int `json:"total_input_tokens"`
	TotalOutputTokens        *int `json:"total_output_tokens"`
	TotalCacheCreationTokens *int `json:"total_cache_creation_input_tokens"`
	TotalCacheReadTokens     *int `json:"total_cache_read_input_tokens"`
	Turns                    *int `json:"turns"`
}
```

In `jsonv1.go` `fullResultsToV1` (~179-190): the run-result.json may not carry tokens yet, so prefer the run's stored `Tokens` (set at finalize from costs.json), falling back to the run-result.json fields. Add after `TotalCostUSD: result.TotalCostUSD,` in the literal:

```go
		TotalCostUSD: result.TotalCostUSD,
		Tokens:       resultsTokens(run, result),
```

Add a helper in `jsonv1.go`:

```go
// resultsTokens prefers the run's stored token usage (populated at finalize
// from costs.json) and falls back to run-result.json's forward token fields.
func resultsTokens(run *store.Run, result *fullRunResult) *TokensV1 {
	if run.Tokens != nil {
		return tokensToV1(run.Tokens)
	}
	if result.TotalInputTokens == nil && result.TotalOutputTokens == nil &&
		result.TotalCacheCreationTokens == nil && result.TotalCacheReadTokens == nil && result.Turns == nil {
		return nil
	}
	deref := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	return &TokensV1{
		Input:         deref(result.TotalInputTokens),
		Output:        deref(result.TotalOutputTokens),
		CacheCreation: deref(result.TotalCacheCreationTokens),
		CacheRead:     deref(result.TotalCacheReadTokens),
		Turns:         deref(result.Turns),
	}
}
```

- [ ] **Step 7: Run the jsonv1 tests**

Run: `go test ./cmd/horde/ -run 'StatusToV1|ListToV1|ResultsToV1|Tokens' -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add cmd/horde/jsonv1.go cmd/horde/main.go
git add cmd/horde/*_test.go
git commit -m "cmd: surface token telemetry in --json (status/list/results + summary)"
```

---

## Task 8: ECS status Lambda (TypeScript) — fetch costs.json, write token attributes

**Files:**
- Modify: `cdk/src/status-lambda/index.ts` (alongside `fetchTotalCost` ~89-115, and the DynamoDB UpdateItem build)
- Test: the status-lambda test file (find via `ls cdk/src/status-lambda/`)

> The Lambda already fetches `run-result.json` from S3 (`horde-runs/<runId>/...`) for cost via `fetchTotalCost`. Add a parallel `fetchTokenUsage` that lists the same prefix, finds `.../costs.json`, parses the `total_*` fields and sums per-phase `turns`, and writes the 5 numeric attributes (`input_tokens`, `output_tokens`, `cache_creation_tokens`, `cache_read_tokens`, `turns`) into the same UpdateItem. Attribute names MUST match the Go `Attr*` consts from Task 3.

- [ ] **Step 1: Add a `fetchTokenUsage` function**

In `cdk/src/status-lambda/index.ts`, after `fetchTotalCost` (~115), add (adapt to the file's existing S3 client + types):

```typescript
interface TokenUsage {
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  turns: number;
}

async function fetchTokenUsage(runId: string): Promise<TokenUsage | null> {
  const prefix = `horde-runs/${runId}/`;
  try {
    const listing = await s3.send(
      new ListObjectsV2Command({ Bucket: ARTIFACTS_BUCKET, Prefix: prefix }),
    );
    const match = (listing.Contents ?? []).find((o) => o.Key?.endsWith("/costs.json"));
    if (!match?.Key) return null;
    const obj = await s3.send(new GetObjectCommand({ Bucket: ARTIFACTS_BUCKET, Key: match.Key }));
    const body = await obj.Body?.transformToString();
    if (!body) return null;
    const data = JSON.parse(body) as {
      total_input_tokens?: number;
      total_output_tokens?: number;
      total_cache_creation_input_tokens?: number;
      total_cache_read_input_tokens?: number;
      phases?: Array<{ turns?: number }>;
    };
    const turns = (data.phases ?? []).reduce((acc, p) => acc + (p.turns ?? 0), 0);
    return {
      input_tokens: data.total_input_tokens ?? 0,
      output_tokens: data.total_output_tokens ?? 0,
      cache_creation_tokens: data.total_cache_creation_input_tokens ?? 0,
      cache_read_tokens: data.total_cache_read_input_tokens ?? 0,
      turns,
    };
  } catch (err) {
    console.log("status-lambda: fetchTokenUsage non-fatal error", { runId, err: String(err) });
    return null;
  }
}
```

- [ ] **Step 2: Call it and add the attributes to the UpdateItem**

Where the handler builds the DynamoDB update (near the `fetchTotalCost` call), add a `fetchTokenUsage(runId)` call and, when non-null, add 5 `N` attributes to the update expression. Mirror exactly how `total_cost_usd` is conditionally added in this file:

```typescript
const tokens = await fetchTokenUsage(runId);
// ... in the same place total_cost_usd is appended to the SET expression:
if (tokens) {
  // example shape — match this file's existing expression-building style:
  setExpr.push("input_tokens = :it", "output_tokens = :ot",
    "cache_creation_tokens = :cct", "cache_read_tokens = :crt", "turns = :tn");
  exprValues[":it"] = { N: String(tokens.input_tokens) };
  exprValues[":ot"] = { N: String(tokens.output_tokens) };
  exprValues[":cct"] = { N: String(tokens.cache_creation_tokens) };
  exprValues[":crt"] = { N: String(tokens.cache_read_tokens) };
  exprValues[":tn"] = { N: String(tokens.turns) };
}
```

> Read the actual variable names used in this file (`setExpr`/`exprValues` are illustrative) and match them. Do NOT invent a new update path — extend the existing one so cost and tokens are written in the same UpdateItem.

- [ ] **Step 3: Add a unit test for `fetchTokenUsage` parsing**

In the status-lambda test file, add a test that feeds a mock S3 `costs.json` body (the Task 4 `costs` literal) and asserts the parsed `TokenUsage` (input 54791, output 87915, cache_creation 529692, cache_read 8934181, turns 5). Mirror the existing `fetchTotalCost` test's S3 mocking.

- [ ] **Step 4: Build and test the CDK package**

Run: `cd cdk && npm run build && npm test`
Expected: TypeScript compiles; Lambda tests pass.

- [ ] **Step 5: Commit**

```bash
git add cdk/src/status-lambda/index.ts cdk/src/status-lambda/*.test.ts
git commit -m "cdk/status-lambda: write token telemetry from costs.json"
```

---

## Task 9: Bootstrap Lambda (Python) — mirror the token logic

**Files:**
- Modify: `internal/bootstrap/templates/stack.yaml.tmpl` (the inline Python status Lambda)

> Per CLAUDE.md, the TS and Python status Lambdas are kept in lockstep. Mirror Task 8's logic in the Python Lambda: list the `horde-runs/<run-id>/` prefix, find `costs.json`, parse `total_*` fields, sum per-phase `turns`, and add the 5 numeric attributes to the same DynamoDB update the cost path uses.

- [ ] **Step 1: Locate the Python cost path**

Run: `grep -n "total_cost_usd\|costs.json\|run-result.json\|def \|update_item\|UpdateExpression" internal/bootstrap/templates/stack.yaml.tmpl`
Read the function that fetches `run-result.json` for cost and the code that builds the DynamoDB update.

- [ ] **Step 2: Add a `fetch_token_usage` and write attributes**

Mirroring the existing cost fetch, add a function that reads `costs.json` from the same S3 prefix and returns a dict (or None). Sum `turns` across `phases`. Then, where `total_cost_usd` is conditionally added to the update expression, add the 5 token attributes (`input_tokens`, `output_tokens`, `cache_creation_tokens`, `cache_read_tokens`, `turns`) as `N` values. Match the file's existing boto3/expression style exactly.

> Use the same attribute names as the Go consts and the TS Lambda. This is a cross-language contract — a mismatch silently drops tokens for ECS runs.

- [ ] **Step 3: Validate the rendered template**

Run: `make bootstrap-validate-test`
Expected: CloudFormation `ValidateTemplate` passes (no resources created, no charges).

- [ ] **Step 4: Run bootstrap Go tests (template rendering)**

Run: `go test ./internal/bootstrap/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/bootstrap/templates/stack.yaml.tmpl
git commit -m "bootstrap: mirror token telemetry in Python status Lambda"
```

---

## Task 10: Document the contract and the `tokens` JSON shape

**Files:**
- Modify: `ORC_CONTRACT_EXPECTATIONS.md` (the `run-result.json` / `costs.json` section ~37-63)
- Modify: `internal/docs/` (the `horde docs json` topic)

- [ ] **Step 1: Document `costs.json` token schema in the orc contract**

In `ORC_CONTRACT_EXPECTATIONS.md`, in the Filesystem Contract section after the `run-result.json` block (~63), add a `costs.json` subsection:

````markdown
### costs.json

Written to the audit directory and updated **incrementally** (flushed after each
phase completes, and on interrupt), alongside `run-result.json` and
`timing.json`. horde reads this file for per-run token telemetry — preferred
over `run-result.json` because it carries token totals on every orc version and
is updated live during a run.

Path: `<audit-dir>/costs.json`

Token fields horde reads:
```json
{
  "phases": [
    { "name": "plan", "turns": 1 }
  ],
  "total_input_tokens": 54791,
  "total_output_tokens": 87915,
  "total_cache_creation_input_tokens": 529692,
  "total_cache_read_input_tokens": 8934181
}
```

horde sums per-phase `turns` into a run total (orc has no run-total `turns`).
The same `total_*` token fields are expected to appear in `run-result.json` in a
future orc release (tracked upstream); horde reads `costs.json` first and falls
back to `run-result.json`'s token fields.
````

- [ ] **Step 2: Document the `tokens` object in `horde docs json`**

Run: `grep -rln "total_cost_usd\|duration_seconds" internal/docs/`
In the file that documents the `--json` status/list/results shapes, add the `tokens` object to each documented shape:

```
"tokens": {            // omitted when horde has no token data for the run
  "input": 54791,
  "output": 87915,
  "cache_creation": 529692,
  "cache_read": 8934181,
  "turns": 12
}
```

Note in the prose that `list`'s `summary.tokens` is the cohort sum (enabling client-side burn-rate over `started_at`/`completed_at`), and that ECS reports tokens at finalize while Docker reports them live.

- [ ] **Step 3: Run docs tests (if any) and build**

Run: `go test ./internal/docs/... && go build ./...`
Expected: PASS / clean.

- [ ] **Step 4: Commit**

```bash
git add ORC_CONTRACT_EXPECTATIONS.md internal/docs/
git commit -m "docs: document costs.json token contract and the tokens JSON object"
```

---

## Task 11: Full verification pass

- [ ] **Step 1: gofmt + vet**

Run: `gofmt -l . && go vet ./...`
Expected: `gofmt -l` prints nothing; `go vet` clean.

- [ ] **Step 2: Unit tests (what CI runs)**

Run: `make unit-test`
Expected: PASS.

- [ ] **Step 3: CDK tests**

Run: `cd cdk && npm ci && npm run build && npm test`
Expected: PASS.

- [ ] **Step 4: Integration test (real orc, real Docker)**

Run: `make integration-test`
Expected: PASS. Confirm a launched run surfaces a non-empty `tokens` object in `horde status --json` and `horde results --json`. If the integration harness asserts on JSON shape, add/extend an assertion that `tokens.input > 0` for a completed run.

- [ ] **Step 5: Manual smoke (optional, if Docker available)**

```bash
make build
# launch a quick run, then:
./horde status <run-id> --json | jq .tokens
./horde list --json | jq '.summary.tokens, .runs[].tokens'
./horde results <run-id> --json | jq .tokens
```
Expected: `tokens` object present with non-zero counts for a completed run; present-and-live or absent for a running run.

- [ ] **Step 6: Final commit (if any formatting/assertion tweaks)**

```bash
git add -p   # stage reviewed hunks only
git commit -m "test: assert token telemetry end-to-end"
```

---

## Follow-up issues to file (NOT part of this plan's code)

After the branch is green, file these (the orchestrating session handles this, not the implementer):

1. **orc** — promote token totals (`total_input_tokens`, `total_output_tokens`, `total_cache_creation_input_tokens`, `total_cache_read_input_tokens`, run-total `turns`) into `run-result.json`. Then replace the `orc#NN` placeholders in `internal/provider/docker.go` with the real issue number.
2. **horde** — per-phase token/cost breakdown surfaced via `horde results` (`PhaseV1` tokens).
3. **horde** — live in-flight token telemetry for ECS: periodic `costs.json` → S3 sync from the worker entrypoint, so running ECS runs report tokens before finalize (matching Docker's lazy-live behavior).
