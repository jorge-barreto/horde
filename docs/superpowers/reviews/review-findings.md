# Deep Review: Token Telemetry (#24 / PR #49)

**Tier:** 3 (by raw line count; ~half is design docs + regenerated CDK snapshots, so code footprint is medium)
**Base:** `c043ebe` (origin/main at branch point)
**Experts launched:** 5 — E1 Provider, E2 Store, E4 CLI, E5 Infrastructure, E6 Test Coverage (E3 Config skipped: zero changed files)
**Verification:** `make build` OK, `make vet` OK, `make unit-test` OK

## Headline

No production-code defects found. Every expert confirmed the implementation is correct in its domain: column-order coupling is aligned across all 6 SQLite occurrences + scanRun, nil/zero semantics are right in both stores, the lambda attribute-name contract matches the Go consts in all three readers, Finalize wiring is correctly ordered in all 4 branches, and the `--json` contract is intact. Findings are one test-coverage gap (blocking on principle — it guards a real future regression) plus quality warnings.

## Blocking Issues

1. **[internal/store/conformance_test.go — list-path block] No list-path test round-trips non-nil `Tokens`** (E6). `Run.Tokens` is only verified through `GetRun`. The 4 list paths (`ListByRepo`/`ListRuns`/`FindActiveByTicket`/`ListActive`) each carry their own literal SELECT column list feeding the order-coupled `scanRun`; a future reorder/typo of the trailing token columns in one SELECT would corrupt `horde list` token output while every existing test passes (`CorrectColumns` sorts before comparing, so it pins the column *set*, not order). **Fix:** set distinct non-zero `Tokens` on a run in a list-path conformance case and assert field-by-field, covering all four SELECTs for both stores.

## Warnings (high-value, addressed)

2. **costs.json nil-vs-zero divergence** (E1, confirmed-adjacent by E4). `tokenUsageFromCostsJSON` returns non-nil all-zero for `{}`/`null`/`{"phases":null}`; `fetchLiveTelemetry` returns nil for the same all-zero input. Two readers of the same 5-field contract disagree on the nil boundary → a run can show `tokens: null` while running, then `tokens: {0,...}` at finalize. **Fix:** give `tokenUsageFromCostsJSON` the same "any field non-zero" presence gate so an all-zero/empty costs.json yields nil, and `ReadTokenUsage` then correctly falls through to the run-result.json forward fields (also closes the orc#4 masking concern E1 raised).

3. **README ECS contradiction** (E4). The `status --json` example shows a populated `tokens` object on a *running ECS* run, contradicting "ECS reports at finalize." **Fix:** make the example a Docker run (Docker is the live path) or note it's a completed run.

4. **TS/Python lambda `num()` divergence** (E5). Identical on well-formed integer input, but they write different values on malformed costs.json (strings/floats/bools) — undermines lockstep. **Fix:** align the coercion rule (TS coerces strings too, matching Python's `int()`), tolerating the documented integer contract while agreeing on edge input.

5. **Python lambda token path untested** (E5, E6). `TestRender_StatusLambdaPython`'s substring allowlist wasn't extended; a Python attribute-name typo or `num` regression passes all CI. **Fix:** extend the allowlist to assert the 5 store attribute names + `fetch_token_usage`/`costs.json`.

## Warnings/Notes (accepted, not changed)

6. `resultsTokens` and `fetchLiveTelemetry` lack direct unit tests (E6 warnings). Addressed by adding a `resultsTokens` table test; `fetchLiveTelemetry`'s parse/sum/gate logic is covered transitively once #2's gate is shared, and the live-container read is exercised by integration. (See fixes.)
7. Legacy-DB migration test not extended for token columns (E2). Addressed.
8. Minor: `summaryTokens(any ...)` shadows builtin `any`; new jsonv1 tests not `t.Parallel()`; getting-started doc prose omits tokens from results summary. Low-value polish — `any` shadow and doc prose fixed; parallel nit left.

## Acceptance Criteria Check (from the design spec)

- [x] Four token counts + turns threaded orc → store → `--json` — verified by conformance + jsonv1 + integration tests.
- [x] costs.json-first, run-result.json fallback seam — verified (E1); fallback correctness improved by fix #2.
- [x] SQLite (ensureColumns) + DynamoDB persistence, backward-compatible — verified (E2), backward-compat tested.
- [x] Lazy-live Docker, finalize-time ECS, lambdas in lockstep — verified (E5); lockstep tightened by fix #4.
- [x] Nested `tokens` JSON + summed `summary.tokens` — verified (E4).
- [x] Client-side burn-rate enablement (no `horde burn`) — verified.
- [x] All four doc surfaces — verified; README inconsistency #3 fixed.

## Verdict

**PASS** after addressing the blocking test gap (#1) and high-value warnings (#2–#5). No production-logic defects were found; the fixes harden coverage and resolve cross-reader/contract inconsistencies.
