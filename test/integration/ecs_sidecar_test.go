package integration

import (
	"os"
	"testing"
	"time"
)

// TestECS_Sidecar exercises sidecar containers (#7) end-to-end on real Fargate.
//
// The CDK e2e stack (cdk/e2e/app.ts) adds a Postgres sidecar to the worker task
// definition. The `postgres-probe` workflow connects to localhost:5432 from the
// worker container and exits 0 once it reaches the sidecar.
//
// What this proves: the worker REACHES the sidecar on localhost — they share the
// task's network namespace, the user-facing point of the feature — and the run
// completes cleanly with sidecars present.
//
// What it does NOT prove on its own: the find-worker-by-name fix. ECS does not
// guarantee container order in the stop event/DescribeTasks response, so this
// test cannot force the sidecar ahead of the worker; on a run where the worker
// happens to sort first, even the old containers[0] logic would pass. The
// name-filter is proven deterministically by unit tests instead:
// cdk/src/status-lambda/index.test.ts and
// internal/provider/ecs_test.go::TestECSProvider_Status_SidecarExitIgnored,
// which pin a non-zero sidecar at index 0 and assert the worker's exit wins.
//
// The sidecar only exists on the CDK-deployed stack, so this skips unless the
// e2e backend is cdk (HORDE_E2E_ECS_BACKEND=cdk, as set by `make e2e-test`).
//
// The worker clones github.com/jorge-barreto/horde and runs the workflow from
// the checked-out branch. Once this lands on main, the default checkout (main)
// has postgres-probe.yaml and no branch is needed. To run the test BEFORE merge,
// set HORDE_E2E_SIDECAR_BRANCH to the feature branch (pushed to GitHub) so the
// worker checks it out and finds the workflow.
func TestECS_Sidecar(t *testing.T) {
	if os.Getenv("HORDE_E2E_ECS_BACKEND") != "cdk" {
		t.Skip("sidecar e2e requires the CDK stack (HORDE_E2E_ECS_BACKEND=cdk)")
	}
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("sidecar-postgres")
	branch := os.Getenv("HORDE_E2E_SIDECAR_BRANCH") // empty -> worker uses default branch
	runID := h.LaunchBranch(ticket, "postgres-probe", branch, 8*time.Minute)
	h.TrackRunForCleanup(runID)

	waitForECSTerminal(t, h, runID, 8*time.Minute)

	if got := h.driver.StoreStatus(runID); got != "success" {
		t.Fatalf("StoreStatus = %q, want %q — worker should reach the Postgres "+
			"sidecar on localhost and exit 0 (a 'failed' here may mean the status "+
			"Lambda read the sidecar's exit code instead of the worker's)", got, "success")
	}
	if ec := h.driver.StoreExitCode(runID); ec == nil || *ec != 0 {
		t.Fatalf("StoreExitCode = %v, want 0 (the worker's exit code, not the "+
			"non-essential sidecar's)", ec)
	}
}
