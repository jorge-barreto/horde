package integration

import (
	"strings"
	"testing"
	"time"
)

// TestECSLogsAfterTerminal verifies that `horde logs` returns CloudWatch
// log content for a terminal ECS run. The quick-success workflow writes the
// marker "quick-success done" which we expect to appear in the logs.
func TestECSLogsAfterTerminal(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("logs-terminal")
	runID := h.Launch(ticket, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(runID)

	waitForECSTerminal(t, h, runID, 5*time.Minute)

	out := h.Logs(runID)
	if !strings.Contains(out, "quick-success done") {
		t.Errorf("expected 'quick-success done' in logs, got:\n%s", out)
	}
}

// TestECSLogsDuringRunning verifies that `horde logs` succeeds (does not
// error) while the ECS task is still running, even if the output is empty
// because the task only just started.
func TestECSLogsDuringRunning(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("logs-running")
	runID := h.Launch(ticket, "slow", 10*time.Minute)
	h.TrackRunForCleanup(runID)
	t.Cleanup(func() {
		if err := h.Kill(runID); err != nil {
			t.Logf("cleanup kill (non-fatal): %v", err)
		}
	})

	waitForECSStatus(t, h, runID, "running", 5*time.Minute)

	// Logs while running must not error. Output may legitimately be empty
	// if the task just started and hasn't flushed any lines yet.
	out := h.Logs(runID)
	t.Logf("running-state logs (length=%d):\n%s", len(out), out)
}

// TestECSLogsFollow is a placeholder for follow-mode coverage. CloudWatch
// streaming requires a different test scaffold than this harness provides.
func TestECSLogsFollow(t *testing.T) {
	// TODO(horde-lbx.7): add follow-mode CloudWatch streaming coverage.
	t.Skip("follow mode covered manually; no automated test yet")
}
