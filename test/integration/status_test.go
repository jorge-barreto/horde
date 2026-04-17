package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestNormalSuccess is the golden path: a fast-completing workflow should be
// detected as "success" with exit code 0.
func TestNormalSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-success", "quick-success", 5*time.Minute)

	h.WaitForOrc(runID, 2*time.Minute)

	out := h.Status(runID)

	if !strings.Contains(out, "success") {
		t.Errorf("expected status output to contain 'success', got:\n%s", out)
	}

	storeStatus := h.StoreStatus(runID)
	if storeStatus != "success" {
		t.Errorf("store status: got %q, want %q", storeStatus, "success")
	}

	exitCode := h.StoreExitCode(runID)
	if exitCode == nil {
		t.Errorf("store exit code is nil, want 0")
	} else if *exitCode != 0 {
		t.Errorf("store exit code: got %d, want 0", *exitCode)
	}
}

// TestTimeoutMasksSuccess reproduces Bug #1: handleLazyCheck checks timeout
// before the .horde-exit-code marker file. When orc completes successfully but
// the container is past its timeout, horde reports "killed" instead of "success".
func TestTimeoutMasksSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	launchTime := time.Now()
	runID := h.Launch("TEST-timeout-mask", "quick-success", 10*time.Second)

	// Orc completes in ~2s — wait for it.
	h.WaitForOrc(runID, 2*time.Minute)

	// Sleep until we are past the 10s timeout window.
	elapsed := time.Since(launchTime)
	if remaining := 15*time.Second - elapsed; remaining > 0 {
		time.Sleep(remaining)
	}

	out := h.Status(runID)

	// The correct behavior is "success" because orc finished before timeout.
	// BUG #1: horde currently reports "killed" because handleLazyCheck checks
	// timeout before the marker file.
	if !strings.Contains(out, "success") {
		t.Errorf("BUG #1: timeout masks success — orc completed (exit 0) but horde "+
			"reported status after timeout window.\nExpected 'success' in output, got:\n%s", out)
	}

	storeStatus := h.StoreStatus(runID)
	if storeStatus != "success" {
		t.Errorf("BUG #1: store status: got %q, want %q", storeStatus, "success")
	}
}

// TestKillAfterSuccess reproduces Bug #2: killCmd should not overwrite a
// completed run. When orc finished successfully, kill should refuse.
func TestKillAfterSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-kill-after", "quick-success", 5*time.Minute)

	h.WaitForOrc(runID, 2*time.Minute)

	// Trigger lazy detection so the store records "success".
	out := h.Status(runID)
	if !strings.Contains(out, "success") {
		t.Fatalf("precondition failed: expected status 'success' before kill, got:\n%s", out)
	}

	// Kill should error because the run is already "success".
	err := h.Kill(runID)
	if err == nil {
		// BUG #2: kill succeeded without error on a completed run.
		storeStatus := h.StoreStatus(runID)
		if storeStatus == "killed" {
			t.Errorf("BUG #2: kill overwrote successful run — store status is %q, want %q",
				storeStatus, "success")
		}
	}
	// If err != nil, kill correctly refused — that is the expected path.
}

// TestExternalStop reproduces Bug #3: when a container is stopped externally
// (docker stop), the docker exit code is 143 (SIGTERM on sleep infinity).
// handleLazyCheck's stopped case uses the docker exit code, not orc's marker.
func TestExternalStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-ext-stop", "quick-success", 5*time.Minute)

	// Wait for orc to complete (writes .horde-exit-code 0).
	h.WaitForOrc(runID, 2*time.Minute)

	// Stop the container externally, bypassing horde.
	cid := h.ContainerID(runID)
	if cid == "" {
		t.Fatal("no container ID in store")
	}
	cmd := exec.Command("docker", "stop", cid)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker stop failed: %v\n%s", err, out)
	}

	out := h.Status(runID)

	// The correct behavior is "success" because orc wrote exit code 0.
	// BUG #3: horde currently reports "failed" because the stopped-container
	// branch uses docker's exit code (143) instead of the orc marker.
	if !strings.Contains(out, "success") {
		t.Errorf("BUG #3: external stop misreports status — orc completed (exit 0) but "+
			"horde used docker exit code (143).\nExpected 'success' in output, got:\n%s", out)
	}

	storeStatus := h.StoreStatus(runID)
	if storeStatus != "success" {
		t.Errorf("BUG #3: store status: got %q, want %q", storeStatus, "success")
	}
}

// TestLegitimateTimeout is a golden path test: a genuinely timed-out run
// (orc still running past timeout) should correctly be "killed".
func TestLegitimateTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-legit-timeout", "slow", 10*time.Second)

	// The "slow" workflow sleeps 30s. Wait past the 10s timeout.
	time.Sleep(15 * time.Second)

	out := h.Status(runID)

	if !strings.Contains(out, "killed") {
		t.Errorf("expected status output to contain 'killed' for timed-out run, got:\n%s", out)
	}

	storeStatus := h.StoreStatus(runID)
	if storeStatus != "killed" {
		t.Errorf("store status: got %q, want %q", storeStatus, "killed")
	}
}

// TestNormalFailure is a golden path test: a workflow that exits with code 1
// should be detected as "failed".
func TestNormalFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-failure", "quick-fail", 5*time.Minute)

	h.WaitForOrc(runID, 2*time.Minute)

	out := h.Status(runID)

	if !strings.Contains(out, "failed") {
		t.Errorf("expected status output to contain 'failed', got:\n%s", out)
	}

	storeStatus := h.StoreStatus(runID)
	if storeStatus != "failed" {
		t.Errorf("store status: got %q, want %q", storeStatus, "failed")
	}

	exitCode := h.StoreExitCode(runID)
	if exitCode == nil {
		t.Errorf("store exit code is nil, want 1")
	} else if *exitCode != 1 {
		t.Errorf("store exit code: got %d, want 1", *exitCode)
	}
}

// Note: TestSignalInterrupt was removed. Exit code 5 requires sending SIGTERM
// to the orc process, not a script that runs `exit 5`. The exit code 5 mapping
// is covered by the unit test TestMapExitCode in internal/provider/docker_test.go.
