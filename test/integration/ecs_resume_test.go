package integration

import (
	"os"
	"strings"
	"testing"
	"time"
)

// resumeWorkflowBranch is the git branch the ECS worker checks out for the
// resume e2e tests. The resume-marker workflow + its prompt live on the
// feature branch, not yet on origin's default branch, so the worker must be
// told to check it out. Override with HORDE_E2E_RESUME_BRANCH (defaults to the
// feature branch); once these land on the default branch this can drop to "".
func resumeWorkflowBranch() string {
	if b := os.Getenv("HORDE_E2E_RESUME_BRANCH"); b != "" {
		return b
	}
	return "recoverable-runs-ecs"
}

// TestECSResumeRestoresSession is the headline e2e for issue #35: an ECS run
// interrupted mid-phase can be resumed, with the agent session (~/.claude)
// AND the working tree (/workspace) restored from S3 so orc continues where
// it left off.
//
// The resume-marker workflow runs an orc AGENT phase (orc can --resume an
// interrupted agent session; a killed script phase returns exit 6). The test
// kills the task mid-agent-phase — orc saves the interrupted session and the
// entrypoint syncs ~/.claude and /workspace to S3 — then `horde retry`s. On
// resume the worker restores both from S3 and orc reattaches the saved
// session and runs to completion. A terminal "success" after retry is
// reachable ONLY if the session + workspace survived the kill + resume
// round-trip through S3.
func TestECSResumeRestoresSession(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("resume-session")
	runID := h.LaunchBranch(ticket, "resume-marker", resumeWorkflowBranch(), 15*time.Minute)
	h.TrackRunForCleanup(runID)
	t.Cleanup(func() {
		if got := h.driver.StoreStatus(runID); got == "running" || got == "pending" || got == "" {
			if err := h.Kill(runID); err != nil {
				t.Logf("cleanup kill (non-fatal): %v", err)
			}
		}
	})

	// Wait for the agent phase to start. The `ready` script phase prints this
	// just before the agent phase begins; give the agent a few seconds to
	// open its session so there is something to interrupt and resume.
	waitForECSStatus(t, h, runID, "running", 5*time.Minute)
	waitForLogLine(t, h, runID, "RESUME-MARKER-READY", 5*time.Minute)
	time.Sleep(20 * time.Second)

	// Kill mid-agent-phase. The entrypoint's SIGTERM handler lets orc save the
	// interrupted session, then syncs ~/.claude and /workspace to S3.
	if err := h.Kill(runID); err != nil {
		t.Fatalf("kill failed: %v", err)
	}
	if got := waitForECSTerminal(t, h, runID, 5*time.Minute); got != "killed" {
		t.Fatalf("after kill, StoreStatus = %q, want killed", got)
	}

	// The session AND the workspace must have been persisted to S3. `horde
	// kill` sets the store status to killed synchronously, but the container's
	// SIGTERM handler is still flushing to S3 (within the 120s stopTimeout) —
	// so poll rather than check once.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		sessions := ecsd(t, h).SessionObjectCount(runID)
		workspace := ecsd(t, h).WorkspaceObjectCount(runID)
		if sessions > 0 && workspace > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("within 2m of kill: sessions=%d workspace=%d (both must be >0)", sessions, workspace)
		}
		time.Sleep(5 * time.Second)
	}

	// Resume. retry defaults to passing --resume to orc.
	if out, err := h.Retry(runID); err != nil {
		t.Fatalf("retry failed: %v\nstdout: %s", err, out)
	}

	// The resumed run must reach success — only possible if orc reattached the
	// restored session and completed the agent phase.
	waitForECSStatus(t, h, runID, "running", 5*time.Minute)
	if got := waitForECSTerminal(t, h, runID, 12*time.Minute); got != "success" {
		logs, _ := h.driver.FetchContainerLogs(h.driver.InstanceID(runID))
		t.Fatalf("after resume, StoreStatus = %q, want success\nlogs:\n%s", got, logs)
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
