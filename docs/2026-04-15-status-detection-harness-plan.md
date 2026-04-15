# Status Detection Integration Test Harness — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a real integration test harness that exercises horde's status detection chain with real Docker, real orc, and real horde — reproducing three bugs where successful runs report as "killed" or "failed".

**Architecture:** Each test gets a unique temp `$HOME` with its own SQLite store. Tests create a project directory with `worker/Dockerfile` (horde's existing project-image mechanism), which bakes a test git repo into the Docker image. The test entrypoint seeds `/workspace` from the baked-in repo, then hands off to the real entrypoint. `horde launch` builds the image and starts the container; status/kill/list commands exercise the status detection chain.

**Tech Stack:** Go integration tests, real Docker, real orc (script-only workflows), SQLite store, horde CLI binary.

**Spec:** `docs/2026-04-15-status-detection-test-harness-design.md`

**Key files:**
- `cmd/horde/main.go:1064-1260` — `handleLazyCheck` (the buggy function)
- `cmd/horde/main.go:502-594` — `killCmd` (bug #2)
- `cmd/horde/main.go:745-753` — `mapExitCode`
- `internal/provider/docker.go:114-152` — `DockerProvider.Status`
- `internal/provider/dockerimage.go:19-48` — `EnsureImage` (builds base + project image)
- `docker/entrypoint.sh:8-11` — restart detection (skips clone when .git exists)
- `internal/store/store.go` — `Run`, `Status` types
- `internal/config/envfile.go:42` — `.env` requires `CLAUDE_CODE_OAUTH_TOKEN` + `GIT_TOKEN`

---

### Task 1: Create test fixture directory and orc workflows

**Files:**
- Create: `test/fixtures/test-repo/.orc/workflows/quick-success.yaml`
- Create: `test/fixtures/test-repo/.orc/workflows/quick-fail.yaml`
- Create: `test/fixtures/test-repo/.orc/workflows/slow.yaml`
- Create: `test/fixtures/test-repo/.orc/workflows/signal-five.yaml`
- Create: `test/fixtures/test-repo/README.md`

**Note:** The `.orc/workflows/*.yaml` files must follow orc's actual workflow schema. The script contents below are correct; the YAML wrapper format must match whatever orc expects for script-only phases. Verify by running `orc run -w quick-success TEST-1 --auto --no-color` locally in a directory with these workflows.

- [ ] **Step 1: Create the fixture directory structure**

```bash
mkdir -p test/fixtures/test-repo/.orc/workflows
```

- [ ] **Step 2: Create README.md (minimal repo needs at least one file)**

```markdown
# horde test repo
Minimal repo for integration testing horde's status detection.
```

Write to `test/fixtures/test-repo/README.md`.

- [ ] **Step 3: Create quick-success workflow**

This workflow should: run a script phase that exits 0 in ~2 seconds. Orc will automatically write `run-result.json` with cost data to the audit path.

Write to `test/fixtures/test-repo/.orc/workflows/quick-success.yaml`. The script content:

```bash
echo "quick-success: done"
exit 0
```

- [ ] **Step 4: Create quick-fail workflow**

Script that exits 1 in ~2 seconds.

Write to `test/fixtures/test-repo/.orc/workflows/quick-fail.yaml`. The script content:

```bash
echo "quick-fail: intentional failure"
exit 1
```

- [ ] **Step 5: Create slow workflow**

Script that sleeps 30 seconds then exits 0. Used for legitimate timeout tests.

Write to `test/fixtures/test-repo/.orc/workflows/slow.yaml`. The script content:

```bash
echo "slow: starting 30s sleep"
sleep 30
echo "slow: done"
exit 0
```

- [ ] **Step 6: Create signal-five workflow**

Script that exits with code 5 (signal interrupt).

Write to `test/fixtures/test-repo/.orc/workflows/signal-five.yaml`. The script content:

```bash
echo "signal-five: exiting with code 5"
exit 5
```

- [ ] **Step 7: Verify workflows locally (manual)**

If orc is installed locally, run:

```bash
cd test/fixtures/test-repo
git init && git add -A && git commit -m "test repo"
orc run -w quick-success TEST-1 --auto --no-color
echo $?  # should be 0
```

If orc is not installed locally, skip — the integration tests will validate this end-to-end.

- [ ] **Step 8: Commit**

```bash
git add test/fixtures/test-repo/
git commit -m "add orc test workflow fixtures for integration tests"
```

---

### Task 2: Create test Docker infrastructure

**Files:**
- Create: `test/integration/test-entrypoint.sh`
- Create: `test/integration/worker-dockerfile.tmpl`

The test uses horde's existing `worker/Dockerfile` project-image mechanism. At test time, `newHarness` copies `test-entrypoint.sh` and the initialized test repo into a temp project's `worker/` directory and writes a Dockerfile there. `horde launch` builds this as the project image automatically via `EnsureImage`.

- [ ] **Step 1: Create test-entrypoint.sh**

Write to `test/integration/test-entrypoint.sh`:

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

- [ ] **Step 2: Create Dockerfile template**

Write to `test/integration/worker-dockerfile.tmpl`. This is a plain text file (not Go template syntax) — the harness copies it as-is into the test project's `worker/Dockerfile`:

```dockerfile
FROM horde-worker-base:latest
COPY test-repo/ /test-repo/
COPY test-entrypoint.sh /test-entrypoint.sh
RUN chmod +x /test-entrypoint.sh
ENTRYPOINT ["/test-entrypoint.sh"]
```

- [ ] **Step 3: Commit**

```bash
git add test/integration/test-entrypoint.sh test/integration/worker-dockerfile.tmpl
git commit -m "add test Docker infrastructure for integration tests"
```

---

### Task 3: Create harness TestMain and binary build

**Files:**
- Create: `test/integration/harness_test.go`

- [ ] **Step 1: Create harness_test.go with TestMain and package-level vars**

Write to `test/integration/harness_test.go`:

```go
package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

var hordeBin string // set by TestMain

func TestMain(m *testing.M) {
	// Build horde binary
	tmp, err := os.MkdirTemp("", "horde-integration-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	hordeBin = filepath.Join(tmp, "horde")
	if runtime.GOOS == "windows" {
		hordeBin += ".exe"
	}

	// Resolve repo root (test/integration/ -> repo root)
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving repo root: %v\n", err)
		os.Exit(1)
	}

	cmd := exec.Command("go", "build", "-o", hordeBin, "./cmd/horde/")
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "building horde binary: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}
```

- [ ] **Step 2: Run to verify compilation**

```bash
cd /home/jb/work/horde && go vet ./test/integration/
```

Expected: no errors (no tests to run yet, just compiles).

- [ ] **Step 3: Commit**

```bash
git add test/integration/harness_test.go
git commit -m "add integration test TestMain with horde binary build"
```

---

### Task 4: Create harness struct and newHarness

**Files:**
- Modify: `test/integration/harness_test.go`

- [ ] **Step 1: Add the harness struct and newHarness function**

Append to `test/integration/harness_test.go`:

```go
type harness struct {
	t        *testing.T
	homeDir  string // unique temp HOME for this test
	workDir  string // project directory (cwd for horde commands)
	repoRoot string // horde repo root
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}

	homeDir := t.TempDir()

	// Create project directory with git remote
	workDir := filepath.Join(homeDir, "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("creating project dir: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "remote", "add", "origin", "https://github.com/test/repo.git")
	run("git", "config", "user.name", "integration-test")
	run("git", "config", "user.email", "test@test.com")

	// Write .env with required keys (values are dummy — script workflows don't use them)
	envContent := "CLAUDE_CODE_OAUTH_TOKEN=test-token\nGIT_TOKEN=test-token\n"
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte(envContent), 0o644); err != nil {
		t.Fatalf("writing .env: %v", err)
	}

	// Prepare worker/ directory with project Dockerfile
	workerDir := filepath.Join(workDir, "worker")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatalf("creating worker dir: %v", err)
	}

	// Initialize a git repo from the test fixtures and copy to worker/test-repo/
	fixtureDir := filepath.Join(repoRoot, "test", "fixtures", "test-repo")
	testRepoDir := filepath.Join(workerDir, "test-repo")
	if err := copyDir(fixtureDir, testRepoDir); err != nil {
		t.Fatalf("copying test fixtures: %v", err)
	}
	runIn := func(dir string, args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v in %s failed: %v\n%s", args, dir, err, out)
		}
	}
	runIn(testRepoDir, "git", "init")
	runIn(testRepoDir, "git", "config", "user.name", "test")
	runIn(testRepoDir, "git", "config", "user.email", "test@test.com")
	runIn(testRepoDir, "git", "add", "-A")
	runIn(testRepoDir, "git", "commit", "-m", "initial")

	// Copy test-entrypoint.sh and Dockerfile into worker/
	integrationDir := filepath.Join(repoRoot, "test", "integration")
	copyFile(t, filepath.Join(integrationDir, "test-entrypoint.sh"), filepath.Join(workerDir, "test-entrypoint.sh"))
	copyFile(t, filepath.Join(integrationDir, "worker-dockerfile.tmpl"), filepath.Join(workerDir, "Dockerfile"))

	return &harness{
		t:        t,
		homeDir:  homeDir,
		workDir:  workDir,
		repoRoot: repoRoot,
	}
}

// copyDir recursively copies src to dst.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

// copyFile copies a single file from src to dst.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", dst, err)
	}
}
```

- [ ] **Step 2: Verify compilation**

```bash
cd /home/jb/work/horde && go vet ./test/integration/
```

- [ ] **Step 3: Commit**

```bash
git add test/integration/harness_test.go
git commit -m "add harness struct and newHarness with project setup"
```

---

### Task 5: Add harness command helpers (Launch, Status, Kill, List, WaitForOrc, ContainerID)

**Files:**
- Modify: `test/integration/harness_test.go`

- [ ] **Step 1: Add the command runner and all helper methods**

Add the following imports to the existing import block at the top of `test/integration/harness_test.go`: `"context"`, `"database/sql"`, `"errors"`, `"strings"`, `"time"`, and `_ "github.com/mattn/go-sqlite3"`.

Then append to the file:

```go
// env returns the environment for horde subprocess calls.
func (h *harness) env() []string {
	env := os.Environ()
	// Override HOME so horde uses our temp store/workspace
	filtered := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, "HOME=") {
			continue
		}
		filtered = append(filtered, e)
	}
	return append(filtered, "HOME="+h.homeDir)
}

// runHorde executes the horde binary with args, returning stdout and any error.
func (h *harness) runHorde(args ...string) (string, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, hordeBin, args...)
	cmd.Dir = h.workDir
	cmd.Env = h.env()
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Launch runs horde launch and returns the run ID.
func (h *harness) Launch(ticket, workflow string, timeout time.Duration) string {
	h.t.Helper()
	args := []string{"--provider", "docker", "launch", "--timeout", timeout.String()}
	if workflow != "" {
		args = append(args, "--workflow", workflow)
	}
	args = append(args, ticket)
	out, err := h.runHorde(args...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde launch failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	// Last line of stdout is the run ID
	lines := strings.Split(out, "\n")
	runID := lines[len(lines)-1]
	if runID == "" {
		h.t.Fatalf("horde launch returned empty run ID; stdout: %s", out)
	}

	// Register cleanup to stop and remove the container
	h.t.Cleanup(func() {
		cid := h.ContainerID(runID)
		if cid != "" {
			exec.Command("docker", "rm", "-f", cid).Run()
		}
	})

	return runID
}

// Status runs horde status and returns stdout.
func (h *harness) Status(runID string) string {
	h.t.Helper()
	out, err := h.runHorde("--provider", "docker", "status", runID)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde status failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	return out
}

// Kill runs horde kill. Returns error (does not fatal) since some tests expect failure.
func (h *harness) Kill(runID string) error {
	h.t.Helper()
	_, err := h.runHorde("--provider", "docker", "kill", runID)
	return err
}

// List runs horde list --all and returns stdout.
func (h *harness) List() string {
	h.t.Helper()
	out, err := h.runHorde("--provider", "docker", "list", "--all")
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde list failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	return out
}

// WaitForOrc polls the host-side workspace for .horde-exit-code until it appears
// or the timeout expires. This detects orc completion WITHOUT going through horde's
// status detection — critical for testing what horde reports independently.
func (h *harness) WaitForOrc(runID string, timeout time.Duration) {
	h.t.Helper()
	markerPath := filepath.Join(h.homeDir, ".horde", "workspaces", runID, ".horde-exit-code")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(markerPath); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.t.Fatalf("WaitForOrc: marker file not found after %v at %s", timeout, markerPath)
}

// ContainerID reads the instance_id from the SQLite store for the given run.
func (h *harness) ContainerID(runID string) string {
	h.t.Helper()
	dbPath := filepath.Join(h.homeDir, ".horde", "horde.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return ""
	}
	defer db.Close()
	var instanceID string
	err = db.QueryRow("SELECT instance_id FROM runs WHERE id = ?", runID).Scan(&instanceID)
	if err != nil {
		return ""
	}
	return instanceID
}

// StoreStatus reads the status from the SQLite store for the given run.
func (h *harness) StoreStatus(runID string) string {
	h.t.Helper()
	dbPath := filepath.Join(h.homeDir, ".horde", "horde.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		h.t.Fatalf("opening store: %v", err)
	}
	defer db.Close()
	var status string
	err = db.QueryRow("SELECT status FROM runs WHERE id = ?", runID).Scan(&status)
	if err != nil {
		h.t.Fatalf("reading status from store: %v", err)
	}
	return status
}

// StoreExitCode reads the exit_code from the SQLite store. Returns nil if NULL.
func (h *harness) StoreExitCode(runID string) *int {
	h.t.Helper()
	dbPath := filepath.Join(h.homeDir, ".horde", "horde.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		h.t.Fatalf("opening store: %v", err)
	}
	defer db.Close()
	var code sql.NullInt64
	err = db.QueryRow("SELECT exit_code FROM runs WHERE id = ?", runID).Scan(&code)
	if err != nil {
		h.t.Fatalf("reading exit_code from store: %v", err)
	}
	if !code.Valid {
		return nil
	}
	v := int(code.Int64)
	return &v
}
```

- [ ] **Step 2: Verify compilation**

```bash
cd /home/jb/work/horde && go vet ./test/integration/
```

- [ ] **Step 3: Commit**

```bash
git add test/integration/harness_test.go
git commit -m "add harness command helpers: Launch, Status, Kill, WaitForOrc, etc."
```

---

### Task 6: Write Test 1 (NormalSuccess) and run it

**Files:**
- Create: `test/integration/status_test.go`

- [ ] **Step 1: Create status_test.go with Test 1**

Write to `test/integration/status_test.go`:

```go
package integration

import (
	"strings"
	"testing"
	"time"
)

// TestNormalSuccess verifies the golden path: orc completes within timeout,
// status detection works correctly.
func TestNormalSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-SUCCESS-1", "quick-success", 5*time.Minute)

	h.WaitForOrc(runID, 60*time.Second)

	out := h.Status(runID)
	if !strings.Contains(out, "success") {
		t.Errorf("status output missing 'success': %s", out)
	}

	// Verify store
	status := h.StoreStatus(runID)
	if status != "success" {
		t.Errorf("store status = %q, want %q", status, "success")
	}
	exitCode := h.StoreExitCode(runID)
	if exitCode == nil || *exitCode != 0 {
		t.Errorf("store exit_code = %v, want 0", exitCode)
	}
}
```

- [ ] **Step 2: Run the test**

```bash
cd /home/jb/work/horde && go test -v -count=1 -timeout 5m -run TestNormalSuccess ./test/integration/
```

Expected: PASS. This confirms the harness works end-to-end. The first run will be slow (~1-2 min) because it builds the Docker image. If it fails, debug the Docker image build, orc workflow format, or entrypoint seeding.

- [ ] **Step 3: Commit**

```bash
git add test/integration/status_test.go
git commit -m "add TestNormalSuccess integration test — golden path"
```

---

### Task 7: Write Tests 2-4 (bug reproductions) and verify they fail

**Files:**
- Modify: `test/integration/status_test.go`

- [ ] **Step 1: Add TestTimeoutMasksSuccess (Bug #1)**

Append to `test/integration/status_test.go`:

```go
// TestTimeoutMasksSuccess reproduces Bug #1: orc completes but timeout check
// fires first, marking the run "killed" even though .horde-exit-code is 0.
func TestTimeoutMasksSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-TIMEOUT-1", "quick-success", 10*time.Second)

	// Wait for orc to complete (~2s)
	h.WaitForOrc(runID, 30*time.Second)

	// Wait past the 10s timeout
	time.Sleep(15 * time.Second)

	// Now check status — the bug: timeout check fires before marker file check
	out := h.Status(runID)
	if !strings.Contains(out, "success") {
		t.Errorf("BUG #1: status should be 'success' but got: %s", out)
	}

	status := h.StoreStatus(runID)
	if status != "success" {
		t.Errorf("BUG #1: store status = %q, want %q", status, "success")
	}
	exitCode := h.StoreExitCode(runID)
	if exitCode == nil || *exitCode != 0 {
		t.Errorf("BUG #1: store exit_code = %v, want 0", exitCode)
	}
}
```

- [ ] **Step 2: Add TestKillAfterSuccess (Bug #2)**

Append to `test/integration/status_test.go`:

```go
// TestKillAfterSuccess reproduces Bug #2: horde kill unconditionally marks
// "killed" even when orc completed successfully.
func TestKillAfterSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-KILL-1", "quick-success", 5*time.Minute)

	// Wait for orc to complete
	h.WaitForOrc(runID, 30*time.Second)

	// Trigger lazy detection of completion first
	h.Status(runID)

	// Now kill — should error because the run is already complete
	err := h.Kill(runID)
	if err == nil {
		// If kill succeeded without error, check what status was set to.
		// Bug #2: killCmd sets "killed" even when exit code is 0.
		status := h.StoreStatus(runID)
		if status == "killed" {
			t.Errorf("BUG #2: kill after success set status to 'killed'; should remain 'success' or error")
		}
	}
	// If kill returned an error like "already success", that's the correct behavior.
}
```

- [ ] **Step 3: Add TestExternalStop (Bug #3)**

Append to `test/integration/status_test.go`:

```go
// TestExternalStop reproduces Bug #3: when a container is stopped externally
// (docker stop), horde uses docker's exit code (143) instead of orc's (0).
func TestExternalStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-STOP-1", "quick-success", 5*time.Minute)

	// Wait for orc to complete
	h.WaitForOrc(runID, 30*time.Second)

	// Stop the container externally (not via horde kill)
	cid := h.ContainerID(runID)
	if cid == "" {
		t.Fatal("container ID not found in store")
	}
	stopCmd := exec.Command("docker", "stop", cid)
	if out, err := stopCmd.CombinedOutput(); err != nil {
		t.Fatalf("docker stop failed: %v\n%s", err, out)
	}

	// Now check status — the bug: uses docker exit code 143, not orc exit code 0
	out := h.Status(runID)
	if !strings.Contains(out, "success") {
		t.Errorf("BUG #3: status should be 'success' but got: %s", out)
	}

	status := h.StoreStatus(runID)
	if status != "success" {
		t.Errorf("BUG #3: store status = %q, want %q", status, "success")
	}
	exitCode := h.StoreExitCode(runID)
	if exitCode == nil || *exitCode != 0 {
		t.Errorf("BUG #3: store exit_code = %v, want 0", exitCode)
	}
}
```

- [ ] **Step 4: Verify compilation**

```bash
cd /home/jb/work/horde && go vet ./test/integration/
```

- [ ] **Step 5: Run bug tests — expect failures (red phase)**

```bash
cd /home/jb/work/horde && go test -v -count=1 -timeout 5m -run "TestTimeoutMasksSuccess|TestKillAfterSuccess|TestExternalStop" ./test/integration/
```

Expected: Tests 2 and 4 (TimeoutMasksSuccess, ExternalStop) FAIL. Test 3 (KillAfterSuccess) may pass if `handleLazyCheck` already updates status to "success" before `killCmd` checks `run.Status != running`. This confirms the harness correctly detects the bugs.

- [ ] **Step 6: Commit**

```bash
git add test/integration/status_test.go
git commit -m "add bug reproduction tests: TimeoutMasksSuccess, KillAfterSuccess, ExternalStop"
```

---

### Task 8: Write Tests 5-7 (remaining golden paths) and run full suite

**Files:**
- Modify: `test/integration/status_test.go`

- [ ] **Step 1: Add TestLegitimateTimeout**

Append to `test/integration/status_test.go`:

```go
// TestLegitimateTimeout verifies that a genuinely timed-out run IS correctly
// marked "killed". The slow workflow sleeps 30s; timeout is 10s.
func TestLegitimateTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-TIMEOUT-2", "slow", 10*time.Second)

	// Wait past the timeout (orc is still running at this point)
	time.Sleep(15 * time.Second)

	out := h.Status(runID)
	if !strings.Contains(out, "killed") {
		t.Errorf("status should be 'killed' for timed-out run: %s", out)
	}

	status := h.StoreStatus(runID)
	if status != "killed" {
		t.Errorf("store status = %q, want %q", status, "killed")
	}
}
```

- [ ] **Step 2: Add TestNormalFailure**

Append to `test/integration/status_test.go`:

```go
// TestNormalFailure verifies failure detection works correctly.
func TestNormalFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-FAIL-1", "quick-fail", 5*time.Minute)

	h.WaitForOrc(runID, 60*time.Second)

	out := h.Status(runID)
	if !strings.Contains(out, "failed") {
		t.Errorf("status should be 'failed': %s", out)
	}

	status := h.StoreStatus(runID)
	if status != "failed" {
		t.Errorf("store status = %q, want %q", status, "failed")
	}
	exitCode := h.StoreExitCode(runID)
	if exitCode == nil || *exitCode != 1 {
		t.Errorf("store exit_code = %v, want 1", exitCode)
	}
}
```

- [ ] **Step 3: Add TestSignalInterrupt**

Append to `test/integration/status_test.go`:

```go
// TestSignalInterrupt verifies exit code 5 maps to "killed".
func TestSignalInterrupt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-SIG-1", "signal-five", 5*time.Minute)

	h.WaitForOrc(runID, 60*time.Second)

	out := h.Status(runID)
	if !strings.Contains(out, "killed") {
		t.Errorf("status should be 'killed': %s", out)
	}

	status := h.StoreStatus(runID)
	if status != "killed" {
		t.Errorf("store status = %q, want %q", status, "killed")
	}
	exitCode := h.StoreExitCode(runID)
	if exitCode == nil || *exitCode != 5 {
		t.Errorf("store exit_code = %v, want 5", exitCode)
	}
}
```

- [ ] **Step 4: Verify compilation**

```bash
cd /home/jb/work/horde && go vet ./test/integration/
```

- [ ] **Step 5: Run the golden path tests**

```bash
cd /home/jb/work/horde && go test -v -count=1 -timeout 5m -run "TestLegitimateTimeout|TestNormalFailure|TestSignalInterrupt" ./test/integration/
```

Expected: All three PASS.

- [ ] **Step 6: Run the full suite to see the final red/green picture**

```bash
cd /home/jb/work/horde && go test -v -count=1 -timeout 10m ./test/integration/
```

Expected results:
- PASS: TestNormalSuccess, TestLegitimateTimeout, TestNormalFailure, TestSignalInterrupt
- FAIL: TestTimeoutMasksSuccess (Bug #1), TestExternalStop (Bug #3)
- TestKillAfterSuccess: may pass or fail depending on timing

- [ ] **Step 7: Run existing unit tests to verify no regression**

```bash
cd /home/jb/work/horde && go test ./cmd/horde/ ./internal/...
```

Expected: All existing tests pass (harness doesn't modify any production code).

- [ ] **Step 8: Commit**

```bash
git add test/integration/status_test.go
git commit -m "add remaining golden path tests: LegitimateTimeout, NormalFailure, SignalInterrupt"
```
