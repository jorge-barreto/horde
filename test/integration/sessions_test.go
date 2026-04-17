package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRetryPreservesSessionState verifies that files written into
// /root/.claude/ during a run survive into the retried container. This is
// the invariant orc --resume relies on: Claude CLI writes session JSON
// under ~/.claude/projects/ inside the container, and retry launches a
// fresh container — so the directory must be bind-mounted to a per-run
// host path.
func TestRetryPreservesSessionState(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)

	// session-marker workflow: phase 1 writes /root/.claude/projects/horde-test/marker,
	// phase 2 fails (so the run is eligible for retry).
	runID := h.Launch("TEST-sessions", "session-marker", 2*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)
	h.Status(runID) // trigger lazy detection
	if got := h.StoreStatus(runID); got != "failed" {
		t.Fatalf("precondition: expected failed status, got %q", got)
	}

	// The marker must exist on the host at the sessions dir.
	markerPath := filepath.Join(h.SessionsDir(runID), "projects", "horde-test", "marker")
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("reading marker from host sessions dir %s: %v", markerPath, err)
	}
	if !strings.Contains(string(data), "hello-from-phase-1") {
		t.Errorf("marker contents: got %q, want it to contain %q", data, "hello-from-phase-1")
	}

	// Retry — a fresh container launches against the preserved workspace
	// AND the preserved sessions dir. The marker from the first run must
	// still be readable from inside the new container.
	if _, err := h.Retry(runID); err != nil {
		t.Fatalf("horde retry failed: %v", err)
	}
	h.WaitForOrc(runID, 2*time.Minute)

	// The marker should still be on the host — the bind mount is the same
	// host path, so the retried container sees the same file.
	data2, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("reading marker after retry: %v", err)
	}
	if !strings.Contains(string(data2), "hello-from-phase-1") {
		t.Errorf("marker disappeared across retry: got %q", data2)
	}
}

// TestCleanPurgeRemovesSessions verifies that 'horde clean --purge' removes
// the sessions directory alongside the workspace. Without this, sessions
// dirs would accumulate forever on disk.
func TestCleanPurgeRemovesSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)

	runID := h.Launch("TEST-purge-sessions", "session-marker", 2*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)
	h.Status(runID) // trigger lazy detection → terminal status

	if !h.SessionsExists(runID) {
		t.Fatalf("precondition: sessions dir should exist after run")
	}

	if _, err := h.Clean(runID, true); err != nil {
		t.Fatalf("horde clean --purge failed: %v", err)
	}

	if h.WorkspaceExists(runID) {
		t.Error("workspace should be removed by --purge")
	}
	if h.SessionsExists(runID) {
		t.Error("sessions dir should be removed by --purge")
	}
}
