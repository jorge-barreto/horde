package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestECSStatusRunning verifies that a launched ECS run transitions to the
// "running" status after the Fargate task starts and that `horde status`
// reports it. The slow workflow is used to keep the task alive long enough
// to observe the transition.
func TestECSStatusRunning(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("status-running")
	runID := h.Launch(ticket, "slow", 10*time.Minute)
	h.TrackRunForCleanup(runID)
	t.Cleanup(func() {
		// Stop the long-running task so cleanup doesn't leave a Fargate ticker.
		if err := h.Kill(runID); err != nil {
			t.Logf("cleanup kill (non-fatal): %v", err)
		}
	})

	// Wait for the status Lambda to mark the run "running".
	waitForECSStatus(t, h, runID, "running", 5*time.Minute)

	out := h.Status(runID)
	if !strings.Contains(out, "running") {
		t.Errorf("expected 'running' in status output, got:\n%s", out)
	}
}

// TestECSStatusSuccess verifies the success terminal status is reported by
// `horde status` after a quick-success workflow completes.
func TestECSStatusSuccess(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("status-success")
	runID := h.Launch(ticket, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(runID)

	waitForECSTerminal(t, h, runID, 5*time.Minute)

	out := h.Status(runID)
	if !strings.Contains(out, "success") {
		t.Errorf("expected 'success' in status output, got:\n%s", out)
	}
}

// TestECSStatusFailed verifies that a workflow exiting non-zero is recorded
// as "failed" in DynamoDB with a non-zero exit code, and that `horde status`
// reports "failed".
func TestECSStatusFailed(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("status-failed")
	runID := h.Launch(ticket, "quick-fail", 5*time.Minute)
	h.TrackRunForCleanup(runID)

	waitForECSTerminal(t, h, runID, 5*time.Minute)

	if got := h.driver.StoreStatus(runID); got != "failed" {
		t.Errorf("StoreStatus = %q, want %q", got, "failed")
	}
	if ec := h.driver.StoreExitCode(runID); ec == nil || *ec == 0 {
		t.Errorf("StoreExitCode = %v, want non-nil and non-zero", ec)
	}

	out := h.Status(runID)
	if !strings.Contains(out, "failed") {
		t.Errorf("expected 'failed' in status output, got:\n%s", out)
	}
}

// TestECSStatusJSON verifies that `horde --json status` returns valid JSON
// with the expected status field after a successful run.
func TestECSStatusJSON(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("status-json")
	runID := h.Launch(ticket, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(runID)

	waitForECSTerminal(t, h, runID, 5*time.Minute)

	out, err := h.runHorde("--provider", "aws-ecs", "--json", "status", runID)
	if err != nil {
		t.Fatalf("horde --json status failed: %v\nout: %s", err, out)
	}
	var s struct {
		Status string `json:"status"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("invalid JSON from --json status: %v\nraw: %s", err, out)
	}
	if s.Status != "success" {
		t.Errorf("JSON status = %q, want %q\nraw: %s", s.Status, "success", out)
	}
	if s.ID != runID {
		t.Errorf("JSON id = %q, want %q", s.ID, runID)
	}
}
