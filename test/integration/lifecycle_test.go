package integration

import (
	"strings"
	"testing"
	"time"
)

// TestLogsAfterKill verifies that container logs are saved during kill
// and can be retrieved afterwards via `horde logs`.
func TestLogsAfterKill(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-logs-kill", "slow", 5*time.Minute)

	// Give the container time to produce output (the slow workflow echos "slow-start")
	time.Sleep(5 * time.Second)

	if err := h.Kill(runID); err != nil {
		t.Fatalf("kill failed: %v", err)
	}

	if !h.SavedLogsExist(runID) {
		t.Fatal("container.log should exist after kill")
	}

	out := h.Logs(runID)
	if out == "" {
		t.Error("logs should not be empty after kill")
	}
}

// TestResultsAfterSuccess verifies that `horde results` displays run results
// for a successfully completed workflow. Orc writes run-result.json which
// handleLazyCheck copies to the results directory.
func TestResultsAfterSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-results-ok", "quick-success", 5*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)

	// The results command internally calls handleLazyCheck which detects
	// the stopped container, copies artifacts, and updates status.
	out := h.Results(runID)

	if !strings.Contains(out, "TEST-results-ok") {
		t.Errorf("results should contain ticket name, got:\n%s", out)
	}
}

// TestResultsWhileRunning verifies that `horde results` reports a run as
// still in progress when the container is actively running.
func TestResultsWhileRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-results-run", "slow", 5*time.Minute)

	out := h.Results(runID)

	if !strings.Contains(out, "still in progress") {
		t.Errorf("expected 'still in progress' for running run, got:\n%s", out)
	}
}

// TestCleanRemovesContainer verifies that `horde clean <run-id>` removes
// the Docker container for a completed run while preserving the workspace.
func TestCleanRemovesContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-clean", "quick-success", 5*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)
	h.Status(runID) // trigger handleLazyCheck → marks success

	cid := h.ContainerID(runID)
	if cid == "" {
		t.Fatal("no container ID for run")
	}
	if !h.ContainerExists(cid) {
		t.Fatal("container should exist before clean")
	}

	out, err := h.Clean(runID, false)
	if err != nil {
		t.Fatalf("clean failed: %v\noutput: %s", err, out)
	}

	if h.ContainerExists(cid) {
		t.Error("container should be removed after clean")
	}

	if !h.WorkspaceExists(runID) {
		t.Error("workspace should persist after clean without --purge")
	}
}

// TestCleanWithPurgeRemovesWorkspace verifies that --purge also removes
// the workspace directory in addition to the container.
func TestCleanWithPurgeRemovesWorkspace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-clean-purge", "quick-success", 5*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)
	h.Status(runID)

	out, err := h.Clean(runID, true)
	if err != nil {
		t.Fatalf("clean --purge failed: %v\noutput: %s", err, out)
	}

	cid := h.ContainerID(runID)
	if cid != "" && h.ContainerExists(cid) {
		t.Error("container should be removed after clean --purge")
	}

	if h.WorkspaceExists(runID) {
		t.Error("workspace should be removed after clean --purge")
	}
}

// TestCleanRefusesActiveRun verifies that clean refuses to remove a
// container for a run that is still active (running).
func TestCleanRefusesActiveRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-clean-active", "slow", 5*time.Minute)

	_, err := h.Clean(runID, false)
	if err == nil {
		t.Fatal("expected error cleaning an active run, got nil")
	}
}

// TestWorkspacePersistsAfterSuccess verifies that the workspace directory
// persists on the host after a run completes successfully.
func TestWorkspacePersistsAfterSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-ws-persist", "quick-success", 5*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)
	h.Status(runID) // trigger lazy detection

	if !h.WorkspaceExists(runID) {
		t.Error("workspace should persist after successful completion")
	}

	// Status output should include workspace path
	out := h.Status(runID)
	if !strings.Contains(out, "Workspace:") {
		t.Errorf("status output should show workspace path, got:\n%s", out)
	}
}

// TestContainerVanishedBeforeDetection verifies the behavior when a container
// is removed externally (docker rm) before horde detects completion. Since
// horde can't read the exit code from a missing container, the run is
// marked as "failed" even if orc completed successfully.
func TestContainerVanishedBeforeDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-vanished", "quick-success", 5*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)

	// Remove the container before horde detects completion
	cid := h.ContainerID(runID)
	if cid == "" {
		t.Fatal("no container ID for run")
	}
	h.RemoveContainerExternally(cid)

	// Status triggers handleLazyCheck which sees "unknown" state
	out := h.Status(runID)

	if !strings.Contains(out, "failed") {
		t.Errorf("expected 'failed' when container vanishes before detection, got:\n%s", out)
	}

	storeStatus := h.StoreStatus(runID)
	if storeStatus != "failed" {
		t.Errorf("store status: got %q, want %q", storeStatus, "failed")
	}

	// Workspace should survive even when the container vanishes
	if !h.WorkspaceExists(runID) {
		t.Error("workspace should persist even when container vanishes")
	}
}
