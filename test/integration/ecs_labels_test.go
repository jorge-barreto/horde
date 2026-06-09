package integration

import (
	"fmt"
	"testing"
	"time"
)

// TestECSLabelsAndFilters verifies run labels (#17) and `horde list` filtering
// (#19) on ECS: a run launched with --label carries the labels in the store +
// --json output, and the list filters (--label / --status / --workflow /
// --ticket) include the run under matching predicates and exclude it under
// non-matching ones.
//
// "Fail for the right reason": a non-matching --label query returning the run
// means the filter wasn't applied; a matching query missing it means the label
// wasn't stored.
func TestECSLabelsAndFilters(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)
	d := ecsd(t, h)

	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	suite := "evt-" + nonce
	ticket := uniqueTicket("labels")

	runID := h.launchWith(ticket, "quick-success", "",
		[]string{"--label", "suite=" + suite, "--label", "kind=label-test"}, 5*time.Minute)
	h.TrackRunForCleanup(runID)

	if got := waitForECSTerminal(t, h, runID, 6*time.Minute); got != "success" {
		t.Fatalf("labelled run reached %q, want success", got)
	}

	// --- Labels stored + surfaced in JSON ---
	lbls := d.StoreLabels(runID)
	if lbls["suite"] != suite {
		t.Errorf("stored label suite = %q, want %q", lbls["suite"], suite)
	}
	if lbls["kind"] != "label-test" {
		t.Errorf("stored label kind = %q, want %q", lbls["kind"], "label-test")
	}
	s := h.StatusJSON(runID)
	if s.Labels["suite"] != suite {
		t.Errorf("--json status label suite = %q, want %q", s.Labels["suite"], suite)
	}

	// --- Positive filters include the run (retry for GSI eventual consistency) ---
	assertListContains(t, h, runID, true, "--label", "suite="+suite)
	assertListContains(t, h, runID, true, "--workflow", "quick-success")
	assertListContains(t, h, runID, true, "--ticket", ticket)
	assertListContains(t, h, runID, true, "--status", "success")

	// --- Negative filters exclude the run ---
	// These read the same (now-consistent) GSI state, so a single check after
	// the positive retries have warmed the GSI is safe.
	if listContains(h.ListJSON("--label", "suite=does-not-exist-"+nonce), runID) {
		t.Errorf("run %s appeared under a non-matching --label filter", runID)
	}
	if listContains(h.ListJSON("--workflow", "slow"), runID) {
		t.Errorf("run %s appeared under --workflow slow (it ran quick-success)", runID)
	}
	if listContains(h.ListJSON("--status", "failed"), runID) {
		t.Errorf("run %s appeared under --status failed (it succeeded)", runID)
	}
}

// assertListContains runs `list --json <filter>` repeatedly (up to 30s) until
// the run's presence matches want. The retry absorbs the by-repo GSI's
// eventual consistency — a just-finalized run can lag the index a few seconds.
func assertListContains(t *testing.T, h *harness, runID string, want bool, filter ...string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := listContains(h.ListJSON(filter...), runID)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("list %v: contains(%s) = %v, want %v (after 30s)", filter, runID, got, want)
			return
		}
		time.Sleep(3 * time.Second)
	}
}
