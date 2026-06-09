package integration

import (
	"testing"
	"time"
)

// TestECSQueueDrainCycle is the headline #36 e2e: a run parked with
// `launch --enqueue` (status queued, no Fargate task) is drained — claimed and
// launched — when a separate run completing fires run.terminal at the drain
// Lambda. The queued run then transitions queued → pending → running → success.
//
// NOT parallel: its two Fargate tasks (trigger + drained) shouldn't pile onto
// the parallel-test peak against maxConcurrent, and the trigger reliably frees
// a slot for the drained run.
//
// "Fail for the right reason": if the run.terminal → drain rule were not wired,
// the queued run never leaves `queued` and waitForECSStatus(running) times out.
func TestECSQueueDrainCycle(t *testing.T) {
	h := newECSHarness(t)
	d := ecsd(t, h)

	// --- Enqueue (no task launched) ---
	// Use the `slow` workflow for the queued run so its `running` state lingers
	// (~30s) long enough for the 5s-interval poll to observe it — `quick-success`
	// would blow through running→success between polls and the running assertion
	// could spuriously time out.
	qTicket := uniqueTicket("queue-drain")
	lv := h.EnqueueJSON(qTicket, "slow", "high", 10*time.Minute)
	if lv.Status != "queued" {
		t.Fatalf("enqueue status = %q, want %q", lv.Status, "queued")
	}
	if lv.RunID == nil || *lv.RunID == "" {
		t.Fatalf("enqueue returned no run_id: %+v", lv)
	}
	if lv.Priority != "high" {
		t.Errorf("enqueue priority = %q, want %q", lv.Priority, "high")
	}
	qID := *lv.RunID

	// Stored as queued, with enqueued_at + priority, and NO Fargate task.
	if got := d.StoreStatus(qID); got != "queued" {
		t.Fatalf("queued run status = %q, want %q", got, "queued")
	}
	if d.StoreEnqueuedAt(qID) == "" {
		t.Error("queued run has empty enqueued_at")
	}
	if got := d.StorePriority(qID); got != "high" {
		t.Errorf("queued run priority = %q, want %q", got, "high")
	}
	if iid := d.InstanceID(qID); iid != "" {
		t.Errorf("queued run has instance_id %q; a queued run must not launch a task", iid)
	}

	// queue list shows it.
	ql := h.QueueListJSON()
	if item, ok := queueContains(ql, qID); !ok {
		t.Errorf("queue list does not contain queued run %s; got %+v", qID, ql.Queued)
	} else if item.Priority != "high" {
		t.Errorf("queue list priority for %s = %q, want %q", qID, item.Priority, "high")
	}

	// --- Trigger a drain: complete a separate direct run → run.terminal ---
	tTicket := uniqueTicket("queue-trigger")
	tID := h.Launch(tTicket, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(tID)
	if got := waitForECSTerminal(t, h, tID, 6*time.Minute); got != "success" {
		t.Fatalf("trigger run reached %q, want success", got)
	}

	// --- The queued run is now claimed + launched by the drain ---
	// First it must leave `queued` (→ pending/running). The lazy CLI drain on
	// the next `list`/`launch` is a backstop, but the trigger's run.terminal
	// should drive the drain Lambda. Either way ClaimNextQueued ensures exactly
	// one claim — we assert the transition, not which drainer won.
	waitForECSStatus(t, h, qID, "running", 6*time.Minute)
	if got := waitForECSTerminal(t, h, qID, 6*time.Minute); got != "success" {
		t.Fatalf("drained run reached %q, want success", got)
	}
	if ec := d.StoreExitCode(qID); ec == nil || *ec != 0 {
		t.Errorf("drained run exit_code = %v, want 0", ec)
	}
}

// TestECSQueuePrioritize verifies `horde queue prioritize` re-levels a queued
// run. Parallel-safe: enqueued at `lowest` so a concurrent opportunistic drain
// claims other backlog first. Mechanical drain is repo-wide and any
// launch/list/enqueue can trigger it (main.go opportunistic drain), so if this
// run is legitimately drained mid-test the test SKIPS (the drain working as
// designed) rather than failing — it is cancelled in cleanup otherwise.
func TestECSQueuePrioritize(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)
	d := ecsd(t, h)

	lv := h.EnqueueJSON(uniqueTicket("queue-prio"), "quick-success", "lowest", 5*time.Minute)
	if lv.RunID == nil {
		t.Fatalf("enqueue returned no run_id: %+v", lv)
	}
	qID := *lv.RunID
	t.Cleanup(func() {
		if _, err := h.QueueCancel(qID); err != nil {
			t.Logf("cleanup QueueCancel(%s): %v", qID, err)
		}
	})

	if got := d.StoreStatus(qID); got != "queued" {
		t.Skipf("run %s is %q, not queued (raced an opportunistic drain) — prioritize is not applicable", qID, got)
	}

	if out, err := h.QueuePrioritize(qID, "highest"); err != nil {
		// A drain between the status check and here would make prioritize fail
		// with "not queued"; treat that as a skip, not a failure.
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
// cancelled without ever launching a task. Parallel-safe: enqueued at `lowest`
// and cancelled immediately. If a concurrent opportunistic drain claims it
// first (the mechanical drain working as designed), the test SKIPS.
func TestECSQueueCancel(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)
	d := ecsd(t, h)

	lv := h.EnqueueJSON(uniqueTicket("queue-cancel"), "quick-success", "lowest", 5*time.Minute)
	if lv.RunID == nil {
		t.Fatalf("enqueue returned no run_id: %+v", lv)
	}
	qID := *lv.RunID

	if got := d.StoreStatus(qID); got != "queued" {
		t.Skipf("run %s is %q, not queued (raced an opportunistic drain) — cancel not applicable", qID, got)
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

	// It should no longer appear in the queue.
	if _, ok := queueContains(h.QueueListJSON(), qID); ok {
		t.Errorf("cancelled run %s still appears in queue list", qID)
	}
}
