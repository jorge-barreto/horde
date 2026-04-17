# Status Detection Integration Test Harness

## Context

Runs `4t7qe3q38a75` and `sk8cgp9yog3l` both completed successfully (created PRs) but show as "killed" in `horde list`. Investigation revealed three bugs in horde's status detection chain:

1. **Timeout trumps completion** (`prov.Finalize` line 1080): The timeout check runs before the `.horde-exit-code` marker file check (line 1120). If orc completes but the container stays alive (via `sleep infinity`) past the timeout, the next lazy check marks it "killed" without reading the marker. This is the primary bug — both runs used the default 60m timeout while orc worked for 8+ and 14+ hours.

2. **`killCmd` ignores exit code** (line 577): `killCmd` unconditionally sets status to "killed" regardless of the exit code in `run-result.json`. Even exit code 0 results in "killed".

3. **"stopped" case uses docker exit code** (line 1198): When a container is stopped externally, docker reports exit code 143 (SIGTERM on `sleep infinity`). `mapExitCode(143)` returns "failed". The code never consults `run-result.json` or `.horde-exit-code` for the actual orc exit code.

This spec defines an integration test harness that reproduces all three bugs with real Docker, real orc, and real horde — no mocks or fakes.

## Test Docker Image

A test variant of `horde-worker:latest` with a test repo baked in.

### `test/integration/Dockerfile.test`

Build context is the repo root. Paths are relative to it.

```dockerfile
FROM horde-worker-base:latest
COPY test/fixtures/test-repo/ /test-repo/
COPY test/integration/test-entrypoint.sh /test-entrypoint.sh
RUN chmod +x /test-entrypoint.sh
ENTRYPOINT ["/test-entrypoint.sh"]
```

### `test/integration/test-entrypoint.sh`

```bash
#!/bin/bash
set -uo pipefail
# Seed workspace from baked-in test repo if fresh (no .git yet)
if [ ! -d /workspace/.git ]; then
    cp -a /test-repo/. /workspace/
fi
# Hand off to the real entrypoint (skips clone when .git exists)
exec /entrypoint.sh "$@"
```

This exercises the real entrypoint's restart code path (same as `horde retry`). Everything after the copy is identical to production: real entrypoint, real orc, real `sleep infinity`.

During `TestMain`, the test image is tagged as `horde-worker:latest` so horde uses it transparently.

## Test Repo Fixtures

### `test/fixtures/test-repo/`

Source files for a minimal git repo containing `.orc/workflows/` with four script-only workflows. `TestMain` copies these to a temp directory, runs `git init` + `git add` + `git commit` to create the `.git/` directory, then uses that as the Docker build context for the test repo. The fixture directory itself does NOT contain `.git/` — it's generated at test time. These are real orc workflows — they just don't call the Claude API.

| Workflow | Script | Exit Code | Duration | Purpose |
|----------|--------|-----------|----------|---------|
| `quick-success` | Writes `run-result.json` (exit 0, cost 1.23), exits 0 | 0 | ~2s | Golden path + bug reproduction |
| `quick-fail` | Writes `run-result.json` (exit 1), exits 1 | 1 | ~2s | Failure detection |
| `slow` | Sleeps 30s, then exits 0 | 0 | ~30s | Legitimate timeout |
| `signal-five` | Exits 5 | 5 | ~2s | Signal interrupt mapping |

Each workflow writes `run-result.json` to the standard orc audit path before exiting, matching the contract in `ORC_CONTRACT_EXPECTATIONS.md`.

## Test Harness Infrastructure

### `test/integration/harness_test.go`

```go
func TestMain(m *testing.M) {
    // 1. Build horde binary: go build -o <tmpdir>/horde ./cmd/horde/
    // 2. Build base image: docker build -t horde-worker-base:latest ./docker/
    // 3. Prepare test-repo fixture:
    //    a. Copy test/fixtures/test-repo/ to a temp dir
    //    b. git init + git add + git commit in the temp dir
    //    c. Copy the initialized repo (with .git/) back to a build staging area
    // 4. Build test image: docker build -t horde-worker:latest -f test/integration/Dockerfile.test .
    //    (the Dockerfile FROM's horde-worker-base, adds test repo, overrides entrypoint)
    // 5. Run tests
    // 6. Cleanup: remove test containers, temp dirs
}
```

### Harness struct

```go
type harness struct {
    t        *testing.T
    hordeBin string     // path to built horde binary
    homeDir  string     // unique temp HOME per test
    workDir  string     // project dir with .env + git remote
}
```

**`newHarness(t)`**: Creates a unique temp `$HOME`, initializes a project directory with a git remote and `.env`. The `.env` needs `GIT_TOKEN=dummy` (entrypoint exports it as `GH_TOKEN`) and `ANTHROPIC_API_KEY=dummy` (orc may validate its presence even for script-only workflows). Neither token is exercised. Sets `$HOME` and `$PATH` for the test subprocess environment.

**`Launch(ticket, workflow, timeout) string`**: Runs `horde launch --provider docker --workflow <w> --timeout <t> <ticket>`. Returns the run ID parsed from stdout.

**`Status(runID) string`**: Runs `horde status <runID>`. Returns stdout.

**`Kill(runID) error`**: Runs `horde kill <runID>`.

**`List() string`**: Runs `horde list --all`. Returns stdout.

**`WaitForOrc(runID, timeout)`**: Polls the host-side workspace (`~/.horde/workspaces/<runID>/.horde-exit-code`) until the marker file appears or timeout. This detects orc completion WITHOUT going through horde's status detection — critical for testing what horde reports independently.

**`ContainerID(runID) string`**: Opens the SQLite store directly and reads the instance ID for direct Docker operations in tests that need them (e.g., `docker stop` for Bug #3).

**`t.Cleanup`**: Stops and removes any containers created during the test. Removes temp directories.

## Test Scenarios

### `test/integration/status_test.go`

Seven test functions. Tests 2, 3, 4 reproduce the three bugs (expected to fail before fix, pass after). Tests 1, 5, 6, 7 verify golden paths.

### Test 1: NormalSuccess

Verifies the happy path — orc completes within timeout, status detected correctly.

```
Setup:    Launch quick-success, timeout 5m
Action:   WaitForOrc (marker file appears), then horde status
Assert:   status = "success", exit code = 0, cost = 1.23
```

### Test 2: TimeoutMasksSuccess (Bug #1)

Reproduces the primary bug: orc finishes but timeout check fires first.

```
Setup:    Launch quick-success, timeout 10s
Wait:     WaitForOrc (orc completes in ~2s)
Wait:     Sleep until 15s after launch (past the 10s timeout)
Action:   horde status
Assert:   status SHOULD be "success" (currently reports "killed")
Verify:   Store has exit code 0, cost present
```

### Test 3: KillAfterSuccess (Bug #2)

Reproduces the kill-overrides-success bug.

```
Setup:    Launch quick-success, timeout 5m
Wait:     WaitForOrc (orc completes in ~2s)
Action:   horde kill
Assert:   EITHER status = "success" OR error "already completed"
          (currently unconditionally marks "killed")
Verify:   If status updated, exit code should reflect orc's actual result
```

### Test 4: ExternalStop (Bug #3)

Reproduces the docker-exit-code-vs-orc-exit-code bug.

```
Setup:    Launch quick-success, timeout 5m
Wait:     WaitForOrc (orc completes in ~2s)
Action:   docker stop <container> (external stop, not via horde)
Action:   horde status
Assert:   status SHOULD be "success" (currently "failed" — docker exit 143)
Verify:   Exit code in store should be 0 (orc's code), not 143 (docker's)
```

### Test 5: LegitimateTimeout

Verifies that a genuinely timed-out run IS correctly marked "killed".

```
Setup:    Launch slow (sleeps 30s), timeout 10s
Wait:     Sleep 15s (past timeout, orc still running)
Action:   horde status
Assert:   status = "killed" (correct — orc hadn't finished)
```

### Test 6: NormalFailure

Verifies failure detection works correctly.

```
Setup:    Launch quick-fail, timeout 5m
Wait:     WaitForOrc (orc completes in ~2s)
Action:   horde status
Assert:   status = "failed", exit code = 1
```

### Test 7: SignalInterrupt

Verifies exit code 5 maps to "killed" (distinct from timeout-killed).

```
Setup:    Launch signal-five, timeout 5m
Wait:     WaitForOrc (orc completes in ~2s)
Action:   horde status
Assert:   status = "killed", exit code = 5
```

## File Layout

```
test/
  integration/
    harness_test.go          # TestMain, harness struct, helpers
    status_test.go           # 7 test scenarios
    Dockerfile.test          # test image definition
    test-entrypoint.sh       # workspace seeder → exec real entrypoint
  fixtures/
    test-repo/               # git repo with .orc/ workflows
      .orc/
        workflows/
          quick-success.yaml
          quick-fail.yaml
          slow.yaml
          signal-five.yaml
```

## Running the Tests

```bash
# Run integration tests (requires Docker)
go test -v -count=1 -timeout 5m ./test/integration/

# Run with race detector
go test -v -race -count=1 -timeout 5m ./test/integration/
```

Tests require Docker to be running. Each test creates and cleans up its own containers. Total suite runtime: ~2-3 minutes.

## Verification Plan

1. **Red phase**: Run the test suite WITHOUT fixing the bugs. Tests 2, 3, 4 should fail. Tests 1, 5, 6, 7 should pass. This confirms the harness correctly detects the bugs.
2. **Fix phase**: Implement the fixes to `prov.Finalize` and `killCmd`.
3. **Green phase**: Run the test suite again. All 7 tests should pass.
4. **Regression check**: Run `go test ./...` to verify existing unit tests still pass.
