package integration

import (
	"strings"
	"testing"
	"time"
)

// TestKillRunningContainer verifies that killing a running container:
// - transitions the store status to "killed"
// - preserves the workspace on the host
// - saves container logs to the results directory
func TestKillRunningContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-kill-running", "slow", 5*time.Minute)

	if err := h.Kill(runID); err != nil {
		t.Fatalf("kill failed: %v", err)
	}

	storeStatus := h.StoreStatus(runID)
	if storeStatus != "killed" {
		t.Errorf("store status after kill: got %q, want %q", storeStatus, "killed")
	}

	if !h.WorkspaceExists(runID) {
		t.Error("workspace should be preserved after kill")
	}

	if !h.SavedLogsExist(runID) {
		t.Error("container.log should be saved after kill")
	}

	// Status output should reflect the killed state
	out := h.Status(runID)
	if !strings.Contains(out, "killed") {
		t.Errorf("expected 'killed' in status output, got:\n%s", out)
	}
}

// TestKillAlreadyKilled verifies that killing an already-killed run
// returns an error rather than silently succeeding.
func TestKillAlreadyKilled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-kill-twice", "slow", 5*time.Minute)

	if err := h.Kill(runID); err != nil {
		t.Fatalf("first kill failed: %v", err)
	}

	err := h.Kill(runID)
	if err == nil {
		t.Fatal("expected error on second kill of an already-killed run, got nil")
	}
}

// TestKillAlreadyFailed verifies that killing an already-failed run
// returns an error. prov.Finalize detects the failure first.
func TestKillAlreadyFailed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-kill-failed", "quick-fail", 5*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)

	// Kill triggers prov.Finalize which detects the stopped container
	// and marks it "failed" before kill checks the status.
	err := h.Kill(runID)
	if err == nil {
		t.Fatal("expected error killing an already-failed run, got nil")
	}
}
