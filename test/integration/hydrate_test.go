package integration

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHydrate_DockerSuccess launches a short run, waits for it to complete,
// calls Status to trigger artifact collection, then hydrates the results
// into a temp dir and verifies the expected tree.
//
// With the workflow-aware hydrate, the run's audit data is copied DIRECTLY
// into the leaf dir — the source's <workflow>/<ticket>/ nesting is stripped,
// because the provider reads from that exact subtree.
func TestHydrate_DockerSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	h := newHarness(t)

	runID := h.Launch("TEST-hydrate-ok", "quick-success", 5*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)
	h.Status(runID) // trigger handleLazyCheck → copies artifacts out of container

	into := t.TempDir()
	out, err := h.runHorde("--provider", "docker", "hydrate", runID, "--into", into)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("hydrate failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	if !strings.Contains(out, "hydrated: 1") {
		t.Errorf("summary missing from output: %q", out)
	}

	// Named workflow → <into>/.orc/audit/quick-success/<ticket>-<runID>/
	leaf := "TEST-hydrate-ok-" + runID
	auditDir := filepath.Join(into, ".orc", "audit", "quick-success", leaf)
	if _, err := os.Stat(auditDir); err != nil {
		t.Fatalf("audit dir not created at %s: %v", auditDir, err)
	}
	// orc writes run-result.json into its audit subtree; hydrate should
	// place it directly under the leaf (not re-nested under workflow/ticket).
	runResultPath := filepath.Join(auditDir, "run-result.json")
	if _, err := os.Stat(runResultPath); err != nil {
		t.Errorf("run-result.json missing at expected path %s: %v", runResultPath, err)
	}
	// Sanity check: the old double-nested path should NOT exist.
	doubleNested := filepath.Join(auditDir, "quick-success", "TEST-hydrate-ok", "run-result.json")
	if _, err := os.Stat(doubleNested); err == nil {
		t.Errorf("audit tree is double-nested at %s — hydrate did not strip source workflow/ticket prefix", doubleNested)
	}

	// orc config surface (config.yaml + any user-defined folders) should be
	// copied from the run's workspace into <into>/.orc/ so orc improve /
	// orc doctor can find the project config alongside the per-run data.
	// The test fixture is a multi-workflow project (no top-level config.yaml;
	// workflows live in workflows/<name>.yaml), so we assert on the workflow
	// file that drove this run.
	workflowFile := filepath.Join(into, ".orc", "workflows", "quick-success.yaml")
	if _, err := os.Stat(workflowFile); err != nil {
		t.Errorf("workflow config not hydrated at %s: %v", workflowFile, err)
	}
}

// TestHydrate_Idempotent hydrates the same run twice into the same dir and
// verifies the second run reports skipped.
func TestHydrate_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	h := newHarness(t)

	runID := h.Launch("TEST-hydrate-idem", "quick-success", 5*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)
	h.Status(runID)

	into := t.TempDir()
	if _, err := h.runHorde("--provider", "docker", "hydrate", runID, "--into", into); err != nil {
		t.Fatalf("first hydrate: %v", err)
	}
	out, err := h.runHorde("--provider", "docker", "hydrate", runID, "--into", into)
	if err != nil {
		t.Fatalf("second hydrate: %v\nstdout: %s", err, out)
	}
	if !strings.Contains(out, "skipped: 1") {
		t.Errorf("expected 'skipped: 1' on second hydrate, got: %q", out)
	}
}

// TestHydrate_PartialFailure hydrates a valid run-id plus a bogus one.
// Expect non-zero exit code, the valid run materialized, and summary with
// "hydrated: 1" + "failed: 1".
func TestHydrate_PartialFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	h := newHarness(t)

	runID := h.Launch("TEST-hydrate-partial", "quick-success", 5*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)
	h.Status(runID)

	into := t.TempDir()
	out, err := h.runHorde("--provider", "docker", "hydrate", runID, "does-not-exist", "--into", into)

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected non-zero exit with *exec.ExitError, got err=%v, stdout=%s", err, out)
	}
	if exitErr.ExitCode() == 0 {
		t.Errorf("expected non-zero exit code, got 0")
	}

	leaf := "TEST-hydrate-partial-" + runID
	validDir := filepath.Join(into, ".orc", "audit", "quick-success", leaf)
	if _, err := os.Stat(validDir); err != nil {
		t.Errorf("valid run's audit dir should still be materialized at %s: %v", validDir, err)
	}
}

// TestHydrate_RunStillRunning expects a non-terminal run to be rejected.
// Uses the "slow" workflow so the run stays running long enough to attempt
// hydration against it.
func TestHydrate_RunStillRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	h := newHarness(t)

	// "slow" keeps the run non-terminal while we try to hydrate.
	runID := h.Launch("TEST-hydrate-running", "slow", 5*time.Minute)

	into := t.TempDir()
	_, err := h.runHorde("--provider", "docker", "hydrate", runID, "--into", into)

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected non-zero exit for non-terminal run, got err=%v", err)
	}
	if exitErr.ExitCode() == 0 {
		t.Errorf("expected non-zero exit code, got 0")
	}

	// Kill the run so harness cleanup can proceed quickly.
	_ = h.Kill(runID)
}
