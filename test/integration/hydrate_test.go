package integration

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// findFileUnder walks root looking for a file with the given basename.
// Returns its absolute path or "" if not found. Used because the hydrated
// tree nests the orc-produced audit layout (workflow/ticket/...) inside
// the leaf dir, and we want to assert on presence, not exact nesting.
func findFileUnder(root, name string) string {
	var found string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && d.Name() == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// TestHydrate_DockerSuccess launches a short run, waits for it to complete,
// calls Status to trigger artifact collection, then hydrates the results
// into a temp dir and verifies the expected tree.
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
	// run-result.json is written by orc on every completion (success or fail).
	// It lives somewhere under the hydrated audit leaf (orc writes it under
	// its own workflow/ticket subtree, which hydrate preserves inside the leaf).
	if got := findFileUnder(auditDir, "run-result.json"); got == "" {
		t.Errorf("run-result.json not found anywhere under hydrated audit dir %s", auditDir)
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
