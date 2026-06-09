package integration

import (
	"testing"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

// TestECSQueueDrainCycle is the headline #36 e2e: a run parked with
// `launch --enqueue` is drained — claimed and launched — and runs to terminal
// success. It asserts the deterministic truths of the queue+drain backbone:
//
//   - `--enqueue` returns the JSON parking contract (status=queued + priority +
//     run_id), exit 0.
//   - The parked run reaches terminal success — proving a queued run IS drained
//     and executed (whether by the enqueue-time opportunistic drain when a slot
//     is free, by the run.terminal drain Lambda, or by the lazy CLI drain). The
//     drain is mechanical; the test is deliberately drainer-agnostic.
//
// It does NOT assert the run "stays queued": the `--enqueue` path runs an
// opportunistic drain (cmd/horde/main.go) that legitimately claims the run
// immediately when capacity is free, so the queued state is not observable.
// Drain-Lambda-specific behavior is covered by the cdk drain-lambda unit tests
// + TestECSSpendCapConfigured (which proves the run.terminal→drain rule + env).
//
// NOT parallel: it launches Fargate tasks and shouldn't pile onto the parallel
// peak against maxConcurrent.
func TestECSQueueDrainCycle(t *testing.T) {
	h := newECSHarness(t)
	d := ecsd(t, h)

	qTicket := uniqueTicket("queue-drain")
	lv := h.EnqueueJSON(qTicket, "quick-success", "high", 10*time.Minute)

	// Parking contract (always true regardless of an immediate drain — the JSON
	// is built from the launch's local intent, not a store re-read).
	if lv.Status != "queued" {
		t.Errorf("enqueue status = %q, want %q", lv.Status, "queued")
	}
	if lv.Priority != "high" {
		t.Errorf("enqueue priority = %q, want %q", lv.Priority, "high")
	}
	if lv.RunID == nil || *lv.RunID == "" {
		t.Fatalf("enqueue returned no run_id: %+v", lv)
	}
	qID := *lv.RunID

	// The parked run is drained and runs to success. A separate completing run
	// fires run.terminal at the drain Lambda as a backstop in case capacity was
	// full at enqueue time (the opportunistic drain then no-ops).
	tTicket := uniqueTicket("queue-trigger")
	tID := h.Launch(tTicket, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(tID)
	if got := waitForECSTerminal(t, h, tID, 6*time.Minute); got != "success" {
		t.Fatalf("trigger run reached %q, want success", got)
	}

	if got := waitForECSTerminal(t, h, qID, 8*time.Minute); got != "success" {
		t.Fatalf("drained run reached %q, want success", got)
	}
	if ec := d.StoreExitCode(qID); ec == nil || *ec != 0 {
		t.Errorf("drained run exit_code = %v, want 0", ec)
	}
}

// TestECSQueuePrioritize verifies `horde queue prioritize` re-levels a queued
// run. The run is SEEDED directly into DynamoDB (not via `launch --enqueue`) so
// it is reliably parked — bypassing the opportunistic drain. Parallel-safe:
// seeded at `lowest` (drained last) and cancelled in cleanup. If a concurrent
// drain claims it anyway (mechanical drain is repo-wide), the test SKIPS.
func TestECSQueuePrioritize(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)
	d := ecsd(t, h)

	qID := d.SeedQueuedRun(uniqueTicket("queue-prio"), "quick-success", store.PriorityLowest)
	t.Cleanup(func() {
		if _, err := h.QueueCancel(qID); err != nil {
			t.Logf("cleanup QueueCancel(%s): %v", qID, err)
		}
	})

	if got := d.StoreStatus(qID); got != "queued" {
		t.Skipf("seeded run %s is %q, not queued (raced a drain) — prioritize not applicable", qID, got)
	}

	if out, err := h.QueuePrioritize(qID, "highest"); err != nil {
		if got := d.StoreStatus(qID); got != "queued" {
			t.Skipf("run %s drained mid-test (now %q); prioritize not applicable", qID, got)
		}
		t.Fatalf("queue prioritize failed: %v\nout: %s", err, out)
	}
	if got := d.StorePriority(qID); got != "highest" {
		t.Errorf("priority after prioritize = %q, want highest", got)
	}
}

// TestECSQueueCancel verifies `horde queue cancel` transitions a queued run to
// cancelled without ever launching a task. The run is SEEDED directly so it is
// reliably parked. Parallel-safe; SKIPS if a concurrent drain claims it first.
func TestECSQueueCancel(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)
	d := ecsd(t, h)

	qID := d.SeedQueuedRun(uniqueTicket("queue-cancel"), "quick-success", store.PriorityLowest)

	if got := d.StoreStatus(qID); got != "queued" {
		t.Skipf("seeded run %s is %q, not queued (raced a drain) — cancel not applicable", qID, got)
	}

	if out, err := h.QueueCancel(qID); err != nil {
		if got := d.StoreStatus(qID); got != "queued" {
			t.Skipf("run %s drained mid-test (now %q); cancel not applicable", qID, got)
		}
		t.Fatalf("queue cancel failed: %v\nout: %s", err, out)
	}

	// Strongly-consistent poll for the cancelled transition.
	waitForECSStatus(t, h, qID, "cancelled", 1*time.Minute)
	if iid := d.InstanceID(qID); iid != "" {
		t.Errorf("cancelled run has instance_id %q; it must never have launched", iid)
	}
	if _, ok := queueContains(h.QueueListJSON(), qID); ok {
		t.Errorf("cancelled run %s still appears in queue list", qID)
	}
}
