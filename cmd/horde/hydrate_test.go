package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jorge-barreto/horde/internal/store"
)

func TestHydrateDestPaths_DefaultWorkflow(t *testing.T) {
	t.Parallel()
	auditDir, artDir := hydrateDestPaths("/out", "", "PROJ-123", "abc123")
	wantAudit := filepath.Join("/out", ".orc", "audit", "PROJ-123-abc123")
	wantArt := filepath.Join("/out", ".orc", "artifacts", "PROJ-123-abc123")
	if auditDir != wantAudit {
		t.Errorf("audit dir: got %q want %q", auditDir, wantAudit)
	}
	if artDir != wantArt {
		t.Errorf("artifacts dir: got %q want %q", artDir, wantArt)
	}
}

func TestHydrateDestPaths_NamedWorkflow(t *testing.T) {
	t.Parallel()
	auditDir, artDir := hydrateDestPaths("/out", "review", "PROJ-123", "abc123")
	wantAudit := filepath.Join("/out", ".orc", "audit", "review", "PROJ-123-abc123")
	wantArt := filepath.Join("/out", ".orc", "artifacts", "review", "PROJ-123-abc123")
	if auditDir != wantAudit {
		t.Errorf("audit dir: got %q want %q", auditDir, wantAudit)
	}
	if artDir != wantArt {
		t.Errorf("artifacts dir: got %q want %q", artDir, wantArt)
	}
}

func TestHydrateSummary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		outcomes []hydrateOutcome
		want     string
	}{
		{
			name: "all hydrated",
			outcomes: []hydrateOutcome{
				{RunID: "a", Status: hydrateStatusHydrated},
				{RunID: "b", Status: hydrateStatusHydrated},
			},
			want: "hydrated: 2, skipped: 0, failed: 0",
		},
		{
			name: "mixed",
			outcomes: []hydrateOutcome{
				{RunID: "a", Status: hydrateStatusHydrated},
				{RunID: "b", Status: hydrateStatusSkipped},
				{RunID: "c", Status: hydrateStatusFailed, Err: errors.New("boom")},
			},
			want: "hydrated: 1, skipped: 1, failed: 1",
		},
	}
	for _, tc := range cases {
		got := hydrateSummary(tc.outcomes)
		if got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestHydrateHasFailure(t *testing.T) {
	t.Parallel()
	if hydrateHasFailure([]hydrateOutcome{{Status: hydrateStatusHydrated}, {Status: hydrateStatusSkipped}}) {
		t.Error("no failures should report false")
	}
	if !hydrateHasFailure([]hydrateOutcome{{Status: hydrateStatusHydrated}, {Status: hydrateStatusFailed}}) {
		t.Error("one failure should report true")
	}
}

func TestHydrateWriteFailures(t *testing.T) {
	t.Parallel()
	outcomes := []hydrateOutcome{
		{RunID: "a", Status: hydrateStatusHydrated},
		{RunID: "b", Status: hydrateStatusFailed, Err: errors.New("not found")},
	}
	var buf bytes.Buffer
	hydrateWriteFailures(&buf, outcomes)
	got := buf.String()
	if !bytes.Contains([]byte(got), []byte("b")) || !bytes.Contains([]byte(got), []byte("not found")) {
		t.Errorf("failure output missing run id or error: %q", got)
	}
	if bytes.Contains([]byte(got), []byte("a")) {
		t.Errorf("successful run id should not appear in failures: %q", got)
	}
}

func TestRunNotTerminalCheck(t *testing.T) {
	t.Parallel()
	if !isTerminalStatus(store.StatusSuccess) {
		t.Error("success should be terminal")
	}
	if !isTerminalStatus(store.StatusFailed) {
		t.Error("failed should be terminal")
	}
	if !isTerminalStatus(store.StatusKilled) {
		t.Error("killed should be terminal")
	}
	if isTerminalStatus(store.StatusRunning) {
		t.Error("running should not be terminal")
	}
	if isTerminalStatus(store.StatusPending) {
		t.Error("pending should not be terminal")
	}
}
