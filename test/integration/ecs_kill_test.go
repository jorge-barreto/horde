package integration

import (
	"testing"
	"time"
)

// TestECSKillRunningTask verifies that `horde kill` against a running ECS
// task: stops the Fargate task, transitions the DynamoDB row to "killed",
// and that the task ARN is no longer reported as running by ECS shortly
// after StopTask propagates.
func TestECSKillRunningTask(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("kill-running")
	runID := h.Launch(ticket, "slow", 10*time.Minute)
	h.TrackRunForCleanup(runID)

	waitForECSStatus(t, h, runID, "running", 5*time.Minute)

	taskARN := h.driver.InstanceID(runID)
	if taskARN == "" {
		t.Fatal("no ECS task ARN recorded for run")
	}

	if err := h.Kill(runID); err != nil {
		t.Fatalf("kill failed: %v", err)
	}

	// After kill the run should reach the "killed" terminal state. Either
	// the kill command updates DynamoDB synchronously or the status Lambda
	// observes the StoppedTask event and updates it.
	terminal := waitForECSTerminal(t, h, runID, 5*time.Minute)
	if terminal != "killed" {
		t.Errorf("StoreStatus after kill = %q, want %q", terminal, "killed")
	}

	// ECS StopTask propagates over a few seconds. Poll for the task to
	// leave RUNNING up to ~60s after the kill command returned.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if !h.driver.InstanceRunning(taskARN) {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Errorf("ECS task %s still reported as running 60s after kill", taskARN)
}

// TestECSKillAlreadyStopped verifies the behavior of `horde kill` when the
// run is already in a terminal state. Mirrors the Docker
// TestKillAlreadyFailed/TestKillAlreadyKilled behavior: we expect kill to
// return an error rather than overwrite a completed run.
func TestECSKillAlreadyStopped(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("kill-stopped")
	runID := h.Launch(ticket, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(runID)

	terminal := waitForECSTerminal(t, h, runID, 5*time.Minute)
	if terminal != "success" {
		t.Fatalf("precondition: expected success, got %q", terminal)
	}

	err := h.Kill(runID)
	if err == nil {
		t.Fatal("expected error killing an already-stopped run, got nil")
	}

	// Status should still be "success" — kill must not overwrite a
	// successfully completed run.
	if got := h.driver.StoreStatus(runID); got != "success" {
		t.Errorf("StoreStatus after kill-on-stopped = %q, want %q", got, "success")
	}
}
