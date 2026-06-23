package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newLocalExecHarness is newHarness with a committed working tree so
// `horde exec --local` can seed the workspace from it. The workDir is
// initialised with the orc workflow files from the test-repo fixture committed,
// giving the container the .orc/workflows/* tree it needs without a git remote.
//
// newHarness places a nested git repo at worker/test-repo/ (for the Docker
// test image). git ls-files emits it as the directory entry "worker/" (a
// gitlink boundary). SeedWorkspaceFromWorkingTree now skips non-regular
// entries, so the nested repo is tolerated gracefully — this test also serves
// as an end-to-end cover for that nested-repo case.
func newLocalExecHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)

	repoRoot := h.repoRoot
	// Copy only the .orc/ subtree from the fixture — that is all the worker
	// needs for `orc validate`.
	orcSrc := filepath.Join(repoRoot, "test", "fixtures", "test-repo", ".orc")
	orcDst := filepath.Join(h.workDir, ".orc")
	if err := copyDir(orcSrc, orcDst); err != nil {
		t.Fatalf("copying .orc fixture into workDir: %v", err)
	}

	// Stage the .orc tree and commit. The worker/ directory created by
	// newHarness contains a nested git repo at worker/test-repo/.git; the
	// seed skips its gitlink entry automatically, so no .gitignore crutch is
	// needed.
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = h.workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "add", ".orc")
	run("git", "commit", "-m", "add orc workflows for exec test")

	return h
}

// runHordeExec is a convenience helper that runs `horde exec` and returns
// stdout, stderr, and any process error separately.
func (h *harness) runHordeExec(args ...string) (stdout, stderr string, err error) {
	h.t.Helper()
	fullArgs := append(h.providerArgs(), append([]string{"exec"}, args...)...)
	return h.runHordeFull(fullArgs...)
}

// TestExecLocalValidate exercises `horde exec --local -- validate -w
// quick-success` end-to-end using the Docker provider. It covers both new
// holes introduced by the horde exec feature:
//  1. --local workspace seeding: SeedWorkspaceFromWorkingTree copies the
//     committed working tree into the per-run workspace before launch.
//  2. ORC_SUBCMD passthrough: the entrypoint receives ORC_SUBCMD=validate,
//     skips the run-only TICKET/REPO_URL guards, runs `orc validate -w
//     quick-success` against the seeded workspace, and exits 0.
//
// Asserts: a run ID is returned, the run is recorded in the store, the
// container completes successfully (exit 0, status "success"), and the
// workspace directory was created and seeded on the host.
func TestExecLocalValidate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newLocalExecHarness(t)

	stdout, stderr, err := h.runHordeExec("--local", "--", "validate", "-w", "quick-success")
	if err != nil {
		t.Fatalf("horde exec --local -- validate failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	runID := lines[len(lines)-1]
	if runID == "" {
		t.Fatalf("horde exec returned empty run ID; stdout: %s", stdout)
	}

	// Register cleanup.
	h.TrackRunForCleanup(runID)
	t.Cleanup(func() {
		cid := h.driver.InstanceID(runID)
		if cid != "" {
			exec.Command("docker", "rm", "-f", cid).Run()
		}
	})
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		cid := h.driver.InstanceID(runID)
		if cid == "" {
			return
		}
		logs, logErr := h.driver.FetchContainerLogs(cid)
		if logErr != nil {
			t.Logf("container-logs fetch failed for %s: %v", runID, logErr)
			return
		}
		t.Logf("--- container logs for run %s (instance %s) ---\n%s\n--- end logs ---", runID, cid, logs)
	})

	// Wait for the container to finish (orc validate is fast).
	h.WaitForOrc(runID, 2*time.Minute)

	// Trigger lazy finalization and verify the run completed successfully.
	statusOut := h.Status(runID)
	if !strings.Contains(statusOut, "success") {
		t.Errorf("expected status 'success' for exec --local validate, got:\n%s", statusOut)
	}

	storeStatus := h.StoreStatus(runID)
	if storeStatus != "success" {
		t.Errorf("store status: got %q, want %q", storeStatus, "success")
	}

	// The workspace must exist on the host and contain the seeded .orc tree.
	if !h.WorkspaceExists(runID) {
		t.Error("workspace directory should exist after exec --local")
	}
	orcWorkflowsDir := filepath.Join(h.WorkspaceDir(runID), ".orc", "workflows")
	if _, err := os.Stat(orcWorkflowsDir); err != nil {
		t.Errorf("seeded .orc/workflows should exist in workspace: %v", err)
	}
}
