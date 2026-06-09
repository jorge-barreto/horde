package integration

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// e2eFixtureBranch is the git branch the ECS worker checks out for tests whose
// .orc workflow fixtures (env-echo, token-probe) are not yet on origin's
// default branch. Override with HORDE_E2E_FIXTURE_BRANCH; defaults to the
// feature branch these fixtures were committed to. Drop to "" once they land
// on main.
func e2eFixtureBranch() string {
	if b := os.Getenv("HORDE_E2E_FIXTURE_BRANCH"); b != "" {
		return b
	}
	return "worktree-run-lifecycle-event-backbone"
}

// TestECSPerLaunchEnv verifies `horde launch --env KEY=VALUE` (#22) injects the
// variable into the worker container's environment on ECS. The env-echo
// workflow echoes ENV_PROBE=$HORDE_E2E_ENV_PROBE; the test asserts the unique
// marker reaches the worker's CloudWatch logs.
//
// "Fail for the right reason": if --env were not forwarded to the
// ContainerOverride.Environment, the workflow echoes ENV_PROBE=<unset> and the
// marker is absent → assertion fails.
func TestECSPerLaunchEnv(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	marker := "marker-" + nonce
	ticket := uniqueTicket("env")

	runID := h.launchWith(ticket, "env-echo", e2eFixtureBranch(),
		[]string{"--env", "HORDE_E2E_ENV_PROBE=" + marker}, 5*time.Minute)
	h.TrackRunForCleanup(runID)

	status := waitForECSTerminal(t, h, runID, 6*time.Minute)
	if status != "success" {
		t.Fatalf("env-echo run reached %q, want success", status)
	}

	d := ecsd(t, h)
	instanceID := d.InstanceID(runID)
	logs, err := d.FetchContainerLogs(instanceID)
	if err != nil {
		t.Fatalf("FetchContainerLogs(%s): %v", instanceID, err)
	}
	want := "ENV_PROBE=" + marker
	if !strings.Contains(logs, want) {
		t.Errorf("worker logs missing %q (env var not injected?)\nlogs:\n%s", want, logs)
	}
}
