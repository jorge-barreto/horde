package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestECSHydrateSuccess verifies that `horde hydrate` materializes a
// terminal ECS run's audit tree from S3 onto the local filesystem.
func TestECSHydrateSuccess(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("hydrate-ok")
	runID := h.Launch(ticket, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(runID)

	waitForECSTerminal(t, h, runID, 5*time.Minute)

	into := t.TempDir()
	out, err := h.runHorde("--provider", "aws-ecs", "hydrate", "--into", into, runID)
	if err != nil {
		t.Fatalf("hydrate failed: %v\nstdout: %s", err, out)
	}

	runResultPath := filepath.Join(
		into, ".orc", "audit", "quick-success",
		fmt.Sprintf("%s-%s", ticket, runID), "run-result.json",
	)
	data, err := os.ReadFile(runResultPath)
	if err != nil {
		t.Fatalf("reading hydrated run-result.json at %s: %v\nstdout: %s", runResultPath, err, out)
	}
	var rr map[string]interface{}
	if err := json.Unmarshal(data, &rr); err != nil {
		t.Errorf("invalid JSON in run-result.json: %v\nraw: %s", err, string(data))
	}
}

// TestECSHydrateMultipleRuns verifies that `horde hydrate run1 run2` writes
// audit trees for both runs to the same destination.
func TestECSHydrateMultipleRuns(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticketA := uniqueTicket("hydrate-multi-a")
	runA := h.Launch(ticketA, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(runA)

	ticketB := uniqueTicket("hydrate-multi-b")
	runB := h.Launch(ticketB, "quick-success", 5*time.Minute)
	h.TrackRunForCleanup(runB)

	waitForECSTerminal(t, h, runA, 5*time.Minute)
	waitForECSTerminal(t, h, runB, 5*time.Minute)

	into := t.TempDir()
	out, err := h.runHorde("--provider", "aws-ecs", "hydrate", "--into", into, runA, runB)
	if err != nil {
		t.Fatalf("hydrate failed: %v\nstdout: %s", err, out)
	}

	for _, c := range []struct {
		ticket string
		runID  string
	}{
		{ticketA, runA},
		{ticketB, runB},
	} {
		path := filepath.Join(
			into, ".orc", "audit", "quick-success",
			fmt.Sprintf("%s-%s", c.ticket, c.runID), "run-result.json",
		)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("hydrated run-result missing at %s: %v\nstdout: %s", path, err, out)
		}
	}
}
