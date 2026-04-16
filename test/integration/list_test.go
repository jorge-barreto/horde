package integration

import (
	"strings"
	"testing"
	"time"
)

// TestListShowsActiveRuns verifies that `list` (without --all) shows only
// active runs. A completed run should be filtered out after handleLazyCheck
// detects its container has stopped.
func TestListShowsActiveRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	doneID := h.Launch("TEST-list-done", "quick-success", 5*time.Minute)
	_ = h.Launch("TEST-list-active", "slow", 5*time.Minute)

	h.WaitForOrc(doneID, 2*time.Minute)

	out := h.ListActive()

	if strings.Contains(out, "TEST-list-done") {
		t.Errorf("active-only list should not contain completed ticket, got:\n%s", out)
	}
	if !strings.Contains(out, "TEST-list-active") {
		t.Errorf("active-only list should contain running ticket, got:\n%s", out)
	}
}

// TestListAllShowsCompleted verifies that `list --all` includes both
// active and terminal runs.
func TestListAllShowsCompleted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	doneID := h.Launch("TEST-listall-done", "quick-success", 5*time.Minute)
	_ = h.Launch("TEST-listall-active", "slow", 5*time.Minute)

	h.WaitForOrc(doneID, 2*time.Minute)

	out := h.List()

	if !strings.Contains(out, "TEST-listall-done") {
		t.Errorf("list --all should contain completed ticket, got:\n%s", out)
	}
	if !strings.Contains(out, "TEST-listall-active") {
		t.Errorf("list --all should contain running ticket, got:\n%s", out)
	}
}

// TestListEmpty verifies that list reports no runs when none exist.
func TestListEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	out := h.ListActive()

	if !strings.Contains(out, "No active runs") {
		t.Errorf("expected 'No active runs' message, got:\n%s", out)
	}
}
