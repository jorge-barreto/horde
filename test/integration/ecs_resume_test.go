package integration

import (
	"strings"
	"testing"
	"time"
)

// TestECSResumeRestoresSession is the headline e2e for issue #35: an ECS run
// that is interrupted mid-phase can be resumed, with the agent session
// (~/.claude) restored from S3 so orc continues where it left off.
//
// The resume-marker workflow is self-verifying: on the first run it writes a
// marker under ~/.claude and sleeps (so we can kill it); on resume the worker
// restores ~/.claude from S3, the marker is already present, and the phase
// exits 0. So a terminal "success" after retry is reachable ONLY if the
// session survived the kill + resume round-trip through S3.
func TestECSResumeRestoresSession(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("resume-session")
	runID := h.Launch(ticket, "resume-marker", 15*time.Minute)
	h.TrackRunForCleanup(runID)
	t.Cleanup(func() {
		if got := h.driver.StoreStatus(runID); got == "running" || got == "pending" || got == "" {
			if err := h.Kill(runID); err != nil {
				t.Logf("cleanup kill (non-fatal): %v", err)
			}
		}
	})

	// Wait for the worker to write the marker and start sleeping.
	waitForECSStatus(t, h, runID, "running", 5*time.Minute)
	waitForLogLine(t, h, runID, "RESUME-MARKER-WRITTEN", 5*time.Minute)

	// Kill mid-run. The entrypoint's SIGTERM handler uploads ~/.claude to S3
	// before the task is reaped.
	if err := h.Kill(runID); err != nil {
		t.Fatalf("kill failed: %v", err)
	}
	if got := waitForECSTerminal(t, h, runID, 5*time.Minute); got != "killed" {
		t.Fatalf("after kill, StoreStatus = %q, want killed", got)
	}

	// The session must have been persisted to S3.
	if n := ecsd(t, h).SessionObjectCount(runID); n == 0 {
		t.Fatalf("no objects under horde-runs/%s/sessions/ after kill — session not uploaded", runID)
	}

	// Resume. retry defaults to passing --resume to orc.
	if out, err := h.Retry(runID); err != nil {
		t.Fatalf("retry failed: %v\nstdout: %s", err, out)
	}

	// The resumed run must reach success — only possible if the restored
	// marker was found by the phase on resume.
	waitForECSStatus(t, h, runID, "running", 5*time.Minute)
	if got := waitForECSTerminal(t, h, runID, 10*time.Minute); got != "success" {
		logs, _ := h.driver.FetchContainerLogs(h.driver.InstanceID(runID))
		t.Fatalf("after resume, StoreStatus = %q, want success\nlogs:\n%s", got, logs)
	}
}

// TestECSResumeRecoversGitTree proves committed-but-unpushed work survives a
// task stop (#32) via the snapshot ref the entrypoint pushes on terminate and
// recovers on resume. The resume-gittree workflow commits a file then sleeps;
// on resume the committed file must be present for the phase to exit 0.
func TestECSResumeRecoversGitTree(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("resume-gittree")
	runID := h.Launch(ticket, "resume-gittree", 15*time.Minute)
	h.TrackRunForCleanup(runID)
	t.Cleanup(func() {
		if got := h.driver.StoreStatus(runID); got == "running" || got == "pending" || got == "" {
			if err := h.Kill(runID); err != nil {
				t.Logf("cleanup kill (non-fatal): %v", err)
			}
		}
	})

	waitForECSStatus(t, h, runID, "running", 5*time.Minute)
	waitForLogLine(t, h, runID, "RESUME-GITTREE-COMMITTED", 5*time.Minute)

	if err := h.Kill(runID); err != nil {
		t.Fatalf("kill failed: %v", err)
	}
	if got := waitForECSTerminal(t, h, runID, 5*time.Minute); got != "killed" {
		t.Fatalf("after kill, StoreStatus = %q, want killed", got)
	}

	if out, err := h.Retry(runID); err != nil {
		t.Fatalf("retry failed: %v\nstdout: %s", err, out)
	}

	waitForECSStatus(t, h, runID, "running", 5*time.Minute)
	if got := waitForECSTerminal(t, h, runID, 10*time.Minute); got != "success" {
		logs, _ := h.driver.FetchContainerLogs(h.driver.InstanceID(runID))
		t.Fatalf("after resume, StoreStatus = %q, want success (git tree not recovered)\nlogs:\n%s", got, logs)
	}
}

// TestECSStopReasonRecorded verifies the status Lambda records the ECS stop
// reason into the run's metadata map. For a user `horde kill` the StopTask
// reason is horde's own string; the spot value ("TerminationNotice") is only
// observable under a real spot interruption (out of scope here), so this
// asserts the plumbing, not the spot-specific value.
func TestECSStopReasonRecorded(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("stop-reason")
	runID := h.Launch(ticket, "slow", 10*time.Minute)
	h.TrackRunForCleanup(runID)

	waitForECSStatus(t, h, runID, "running", 5*time.Minute)
	if err := h.Kill(runID); err != nil {
		t.Fatalf("kill failed: %v", err)
	}
	if got := waitForECSTerminal(t, h, runID, 5*time.Minute); got != "killed" {
		t.Fatalf("after kill, StoreStatus = %q, want killed", got)
	}

	// The Lambda records stop_code/stop_reason in metadata. It runs off the
	// EventBridge STOPPED event, which can lag the synchronous kill update by
	// a few seconds — poll for the metadata to appear.
	deadline := time.Now().Add(3 * time.Minute)
	var code, reason string
	for time.Now().Before(deadline) {
		code = ecsd(t, h).StoreMetadata(runID, "stop_code")
		reason = ecsd(t, h).StoreMetadata(runID, "stop_reason")
		if code != "" || reason != "" {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if code == "" && reason == "" {
		t.Errorf("neither stop_code nor stop_reason recorded in metadata for killed run %s", runID)
	} else {
		t.Logf("recorded stop_code=%q stop_reason=%q", code, reason)
	}
}

// ecsd returns the harness's concrete ECS driver for ECS-only assertions
// (S3 session prefix, metadata). These tests only run under newECSHarness, so
// the assertion always holds.
func ecsd(t *testing.T, h *harness) *ecsDriver {
	t.Helper()
	d, ok := h.driver.(*ecsDriver)
	if !ok {
		t.Fatalf("expected *ecsDriver, got %T", h.driver)
	}
	return d
}

// waitForLogLine polls the run's worker logs until substr appears, up to
// timeout. Fails the test if it never does.
func waitForLogLine(t *testing.T, h *harness, runID, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if inst := h.driver.InstanceID(runID); inst != "" {
			if logs, err := h.driver.FetchContainerLogs(inst); err == nil && strings.Contains(logs, substr) {
				return
			}
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("log line %q never appeared for run %s within %s", substr, runID, timeout)
}
