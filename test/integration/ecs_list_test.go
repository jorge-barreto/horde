package integration

import (
	"strings"
	"testing"
	"time"
)

// TestECSList verifies that `horde list --all` displays both completed runs
// and exposes their tickets and run IDs.
func TestECSList(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticketA := uniqueTicket("list-a")
	runA := h.Launch(ticketA, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(runA)

	ticketB := uniqueTicket("list-b")
	runB := h.Launch(ticketB, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(runB)

	waitForECSTerminal(t, h, runA, 5*time.Minute)
	waitForECSTerminal(t, h, runB, 5*time.Minute)

	out := h.List()

	for _, want := range []string{runA, runB, ticketA, ticketB} {
		if !strings.Contains(out, want) {
			t.Errorf("list --all missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestECSListActive verifies that `horde list` (active only) includes a
// running ECS task. The run is killed in cleanup to avoid a Fargate ticker.
func TestECSListActive(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("list-active")
	runID := h.Launch(ticket, "slow", 10*time.Minute)
	h.TrackRunForCleanup(runID)
	t.Cleanup(func() {
		if err := h.Kill(runID); err != nil {
			t.Logf("cleanup kill (non-fatal): %v", err)
		}
	})

	waitForECSStatus(t, h, runID, "running", 5*time.Minute)

	out := h.ListActive()
	if !strings.Contains(out, runID) {
		t.Errorf("list (active) missing run %q\nfull output:\n%s", runID, out)
	}
}
