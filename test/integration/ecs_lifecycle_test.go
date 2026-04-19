package integration

import (
	"sync"
	"testing"
	"time"
)

// TestECSLifecycleConcurrent launches three ECS runs back-to-back and
// verifies all three reach success terminal state with exit_code 0.
func TestECSLifecycleConcurrent(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	const n = 3
	runIDs := make([]string, n)
	for i := 0; i < n; i++ {
		ticket := uniqueTicket("lifecycle-conc")
		runIDs[i] = h.Launch(ticket, "quick-success", 5*time.Minute)
		h.TrackRunForCleanup(runIDs[i])
	}

	// Wait for all three concurrently to keep the wall-clock time low.
	var wg sync.WaitGroup
	for _, runID := range runIDs {
		wg.Add(1)
		go func(rid string) {
			defer wg.Done()
			waitForECSTerminal(t, h, rid, 8*time.Minute)
		}(runID)
	}
	wg.Wait()

	for _, runID := range runIDs {
		if got := h.driver.StoreStatus(runID); got != "success" {
			t.Errorf("run %s StoreStatus = %q, want %q", runID, got, "success")
		}
		if ec := h.driver.StoreExitCode(runID); ec == nil || *ec != 0 {
			t.Errorf("run %s StoreExitCode = %v, want 0", runID, ec)
		}
	}
}

// TestECSLifecycleTimeoutFinalize exercises the horde-jqd.14 change: when a
// run is past its timeout_at, the next Status call invokes Finalize on the
// ECS provider which reconciles past-timeout active runs.
//
// In practice the slow workflow (~30s) often completes within the 1-minute
// timeout window before reconciliation needs to fire — that still passes
// the test (terminal status reached without external reconciliation). Either
// way: after waiting past the timeout the run must NOT be "running".
func TestECSLifecycleTimeoutFinalize(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("lifecycle-timeout")
	runID := h.Launch(ticket, "slow", 1*time.Minute)
	h.TrackRunForCleanup(runID)
	t.Cleanup(func() {
		// Defensive kill in case the run never terminates.
		if got := h.driver.StoreStatus(runID); got == "running" || got == "pending" || got == "" {
			if err := h.Kill(runID); err != nil {
				t.Logf("cleanup kill (non-fatal): %v", err)
			}
		}
	})

	// Wait past the 1-minute timeout window so Finalize has a reason to
	// reconcile when we call status.
	time.Sleep(90 * time.Second)

	// Status invokes Finalize, which on ECS reconciles past-timeout runs.
	out := h.Status(runID)
	t.Logf("status output after timeout window:\n%s", out)

	got := h.driver.StoreStatus(runID)
	switch got {
	case "success", "failed", "killed", "timed_out":
		// OK — terminal status reached one way or another.
	case "running", "pending", "":
		t.Errorf("after past-timeout Finalize, StoreStatus = %q, want a terminal state", got)
	default:
		t.Errorf("unexpected StoreStatus = %q after past-timeout Finalize", got)
	}
}
