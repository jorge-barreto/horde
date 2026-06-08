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
// worker container and exits 0 once it reaches the sidecar. A green run proves
// two things at once:
//
//  1. The worker reaches the sidecar on localhost — i.e. they share the task's
//     network namespace, the user-facing point of the feature.
//  2. Run status reflects the WORKER's exit code, not the sidecar's. The
//     Postgres sidecar is non-essential and is killed (non-zero, ~137/143) when
//     the essential worker exits 0. If the status Lambda read the sidecar
//     instead of finding the worker by name, the run would land "failed".
//     Asserting success + exit_code 0 confirms the find-by-name fix holds.
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
