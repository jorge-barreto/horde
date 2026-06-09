package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/jorge-barreto/horde/internal/event"
)

// TestECSRunLifecycleEvents verifies the #36 event backbone end-to-end: a
// direct ECS launch emits run.started, and the status Lambda emits run.terminal
// on the custom EventBridge bus. A transient SQS queue + EventBridge rule taps
// the bus; the test asserts both event Details land with the right shape.
//
// "Fail for the right reason": if the bus were not wired (or emission removed),
// DetailsFor returns no matching events and the run.terminal assertion fails.
func TestECSRunLifecycleEvents(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)
	d := ecsd(t, h)

	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	// Scope the capture rule to a unique launch label so the queue receives
	// ONLY this run's events — under the parallel sweep a broad source-only
	// rule floods the queue with every concurrent test's events and races this
	// run's (earlier) run.started out of the poll budget.
	const evtLabelKey = "evtcap"
	evtCap := newEventCapture(t, d.awsCfg, d.cfg.EventBusName, nonce, evtLabelKey, nonce)

	// Let the rule + target propagate before the run fires its events.
	time.Sleep(8 * time.Second)

	ticket := uniqueTicket("events")
	runID := h.launchWith(ticket, "quick-success", "",
		[]string{"--label", evtLabelKey + "=" + nonce}, 5*time.Minute)
	h.TrackRunForCleanup(runID)

	waitForECSTerminal(t, h, runID, 6*time.Minute)

	// Poll the capture queue for this run's events (generous budget: the
	// status Lambda fires after the ECS task stops + EventBridge→SQS delivery).
	details := evtCap.DetailsFor(runID, 4*time.Minute)
	if len(details) == 0 {
		t.Fatalf("captured no events for run %s on bus %s", runID, d.cfg.EventBusName)
	}

	var started, terminal *event.Detail
	for i := range details {
		switch details[i].Status {
		case "running", "pending":
			started = &details[i]
		case "success", "failed", "killed", "timed_out":
			terminal = &details[i]
		}
	}

	// run.terminal is the load-bearing assertion (the status Lambda emits it).
	if terminal == nil {
		t.Fatalf("no terminal event captured for run %s; got statuses %v", runID, statusesOf(details))
	}
	if terminal.Version != event.DetailVersion {
		t.Errorf("terminal event version = %d, want %d", terminal.Version, event.DetailVersion)
	}
	if terminal.RunID != runID {
		t.Errorf("terminal event run_id = %q, want %q", terminal.RunID, runID)
	}
	if terminal.Repo == "" {
		t.Error("terminal event repo is empty; the drain needs it to find the backlog")
	}
	if terminal.Status != "success" {
		t.Errorf("terminal event status = %q, want %q", terminal.Status, "success")
	}
	if terminal.ExitCode == nil || *terminal.ExitCode != 0 {
		t.Errorf("terminal event exit_code = %v, want 0", terminal.ExitCode)
	}

	// run.started is emitted by the Go CLI on a direct launch (best-effort
	// after the running transition). Assert it too — its absence would mean the
	// launch-path emission regressed.
	if started == nil {
		t.Errorf("no run.started (running/pending) event captured for run %s; got statuses %v", runID, statusesOf(details))
	} else if started.RunID != runID {
		t.Errorf("started event run_id = %q, want %q", started.RunID, runID)
	}
}

func statusesOf(details []event.Detail) []string {
	out := make([]string, 0, len(details))
	for _, d := range details {
		out = append(out, d.Status)
	}
	return out
}
