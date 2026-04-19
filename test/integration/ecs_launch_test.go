package integration

import (
	"strings"
	"testing"
	"time"
)

// TestECSLaunchSuccess verifies that a run launched via the aws-ecs provider
// reaches terminal success: DynamoDB row transitions to success, ECS task
// exits cleanly, status Lambda fires and updates the store.
//
// This overlaps TestECSSmoke but exercises the harness (not inline helpers),
// paving the way for subsequent ecs_*_test.go files to share a setup pattern.
func TestECSLaunchSuccess(t *testing.T) {
	h := newECSHarness(t)

	ticket := uniqueTicket("launch-success")
	runID := h.Launch(ticket, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(runID)

	waitForECSTerminal(t, h, runID, 5*time.Minute)

	if got := h.driver.StoreStatus(runID); got != "success" {
		t.Errorf("StoreStatus = %q, want %q", got, "success")
	}
	if ec := h.driver.StoreExitCode(runID); ec == nil || *ec != 0 {
		t.Errorf("StoreExitCode = %v, want 0", ec)
	}
}

// TestECSLaunchDuplicateTicket verifies that launching the same ticket twice
// rejects the second launch with a 'duplicate' error. Mirrors the Docker
// TestLaunchDuplicateTicket behavior.
func TestECSLaunchDuplicateTicket(t *testing.T) {
	h := newECSHarness(t)

	ticket := uniqueTicket("launch-dup")
	runID := h.Launch(ticket, "slow", 10*time.Minute)
	h.TrackRunForCleanup(runID)

	_, stderr, err := h.runHordeFull("launch",
		"--workflow", "slow", "--timeout", "10m", ticket)
	if err == nil {
		t.Fatal("expected error for duplicate ticket, got nil")
	}
	if !strings.Contains(stderr, "duplicate") {
		t.Errorf("expected 'duplicate' in stderr, got:\n%s", stderr)
	}

	// Stop the long-running task so cleanup doesn't leave a ticking timer.
	if err := h.Kill(runID); err != nil {
		t.Logf("kill failed (non-fatal): %v", err)
	}
}
