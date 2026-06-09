package integration

import (
	"testing"
	"time"
)

// TestECSStatusJSONFullContract verifies the full StatusV1 --json contract
// (#25/#27/#40) is populated for a terminal ECS run — not just status + id
// (already covered by TestECSStatusJSON) but every field a consumer relies on.
//
// "Fail for the right reason": any field that fails to populate on a terminal
// run (e.g. launched_by empty, completed_at missing, duration_seconds zero)
// fails the contract.
func TestECSStatusJSONFullContract(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("jsoncontract")
	runID := h.Launch(ticket, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(runID)
	if got := waitForECSTerminal(t, h, runID, 6*time.Minute); got != "success" {
		t.Fatalf("run reached %q, want success", got)
	}

	s := h.StatusJSON(runID)
	if s.ID != runID {
		t.Errorf("id = %q, want %q", s.ID, runID)
	}
	if s.Ticket != ticket {
		t.Errorf("ticket = %q, want %q", s.Ticket, ticket)
	}
	if s.Workflow != "quick-success" {
		t.Errorf("workflow = %q, want %q", s.Workflow, "quick-success")
	}
	if s.Status != "success" {
		t.Errorf("status = %q, want success", s.Status)
	}
	if s.ExitCode == nil || *s.ExitCode != 0 {
		t.Errorf("exit_code = %v, want 0", s.ExitCode)
	}
	if s.DurationSecs <= 0 {
		t.Errorf("duration_seconds = %v, want > 0", s.DurationSecs)
	}
	if s.LaunchedBy == "" {
		t.Error("launched_by is empty")
	}
	if s.StartedAt == "" {
		t.Error("started_at is empty")
	} else if _, err := time.Parse(time.RFC3339, s.StartedAt); err != nil {
		t.Errorf("started_at %q is not RFC3339: %v", s.StartedAt, err)
	}
	if s.CompletedAt == "" {
		t.Error("completed_at is empty on a terminal run")
	} else if _, err := time.Parse(time.RFC3339, s.CompletedAt); err != nil {
		t.Errorf("completed_at %q is not RFC3339: %v", s.CompletedAt, err)
	}
}

// TestECSRepoOverridePartitioning verifies the canonical-bucket-key contract
// (#33/#29): an explicit --repo override does NOT re-partition run history.
// SSM cfg.Repo is the authority for ECS; the override must not change the
// partition the row is written/queried under, so `horde list` (which queries
// by cfg.Repo) still finds the run.
//
// HORDE_SSM_PATH override is covered by construction: the entire CDK-backend
// suite runs with HORDE_SSM_PATH pointed at this stack (see
// newECSHarnessForRepoWithSSM), so every passing ECS test already exercises it.
//
// "Fail for the right reason": if --repo overrode the write partition key, the
// row would land under a different repo bucket and `list` (using SSM cfg.Repo)
// would not find it.
func TestECSRepoOverridePartitioning(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("repo-override")
	// --repo is a root persistent flag — it must precede the subcommand.
	// providerArgs() also yields root flags; order: [--provider X] [--repo Y] launch ...
	args := append(h.providerArgs(), "--repo", "github.com/jorge-barreto/horde",
		"launch", "--workflow", "quick-success", "--timeout", "5m", ticket)
	out, err := h.runHorde(args...)
	if err != nil {
		t.Fatalf("horde launch --repo failed: %v\nout: %s", err, out)
	}
	runID := lastNonEmptyLine(out)
	if runID == "" {
		t.Fatalf("launch --repo returned empty run ID; out: %s", out)
	}
	h.TrackRunForCleanup(runID)

	if got := waitForECSTerminal(t, h, runID, 6*time.Minute); got != "success" {
		t.Fatalf("--repo run reached %q, want success", got)
	}

	// The run must be discoverable via list, which queries by SSM cfg.Repo.
	// If --repo had re-partitioned the write, this would not find it.
	assertListContains(t, h, runID, true, "--ticket", ticket)
}
