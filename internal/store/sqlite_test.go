package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestNewSQLiteStore_CreatesDirectoryAndFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "subdir", "horde.db")

	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore(%q) error: %v", dbPath, err)
	}
	defer s.Close()

	info, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("expected %q to be a directory", filepath.Dir(dbPath))
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("expected db file to exist: %v", err)
	}
}

func TestNewSQLiteStore_IdempotentReopen(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "horde.db")

	s1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("first open error: %v", err)
	}
	s1.Close()

	s2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("second open error: %v", err)
	}
	s2.Close()
}

func TestNewSQLiteStore_CorrectColumns(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "horde.db")

	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore error: %v", err)
	}
	defer s.Close()

	rows, err := s.db.Query("PRAGMA table_info(runs)")
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dfltValue interface{}
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []string{
		"id", "repo", "ticket", "branch", "workflow", "provider",
		"instance_id", "metadata", "status", "exit_code", "launched_by",
		"started_at", "completed_at", "timeout_at", "total_cost_usd",
	}

	if len(cols) != len(want) {
		t.Errorf("column count: got %d, want %d\ngot: %v\nwant: %v", len(cols), len(want), cols, want)
		return
	}

	got := append([]string(nil), cols...)
	wantSorted := append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(wantSorted)

	for i := range got {
		if got[i] != wantSorted[i] {
			t.Errorf("column mismatch at index %d: got %q, want %q\nfull got: %v\nfull want: %v", i, got[i], wantSorted[i], got, wantSorted)
			break
		}
	}
}

func TestNewSQLiteStore_InvalidPath(t *testing.T) {
	t.Parallel()
	_, err := NewSQLiteStore("/dev/null/impossible/horde.db")
	if err == nil {
		t.Fatal("expected error for invalid path, got nil")
	}
	if !strings.Contains(err.Error(), "creating store directory") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "creating store directory")
	}
}

func TestNewSQLiteStore_PragmasSet(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "horde.db")

	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore error: %v", err)
	}
	defer s.Close()

	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("querying journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode: got %q, want %q", journalMode, "wal")
	}

	var busyTimeout int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("querying busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout: got %d, want %d", busyTimeout, 5000)
	}
}

func newTestRun() *Run {
	now := time.Now().Truncate(time.Second) // RFC3339 has second precision
	timeout := now.Add(60 * time.Minute)
	return &Run{
		ID:         "abc123xyz789",
		Repo:       "github.com/org/repo.git",
		Ticket:     "PROJ-42",
		Branch:     "main",
		Workflow:   "default",
		Provider:   "docker",
		InstanceID: "container-abc123",
		Status:     StatusPending,
		LaunchedBy: "testuser",
		StartedAt:  now,
		TimeoutAt:  timeout,
	}
}

func TestSQLiteStore_CreateGetRun_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	run := newTestRun()
	exitCode := 0
	run.ExitCode = &exitCode
	completedAt := run.StartedAt.Add(5 * time.Minute)
	run.CompletedAt = &completedAt
	cost := 1.23
	run.TotalCostUSD = &cost
	run.Metadata = map[string]string{
		"task_arn": "arn:aws:ecs:us-east-1:123456789012:task/prod/abc",
		"cluster":  "prod",
	}

	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got.ID != run.ID {
		t.Errorf("ID: got %q, want %q", got.ID, run.ID)
	}
	if got.Repo != run.Repo {
		t.Errorf("Repo: got %q, want %q", got.Repo, run.Repo)
	}
	if got.Ticket != run.Ticket {
		t.Errorf("Ticket: got %q, want %q", got.Ticket, run.Ticket)
	}
	if got.Branch != run.Branch {
		t.Errorf("Branch: got %q, want %q", got.Branch, run.Branch)
	}
	if got.Workflow != run.Workflow {
		t.Errorf("Workflow: got %q, want %q", got.Workflow, run.Workflow)
	}
	if got.Provider != run.Provider {
		t.Errorf("Provider: got %q, want %q", got.Provider, run.Provider)
	}
	if got.InstanceID != run.InstanceID {
		t.Errorf("InstanceID: got %q, want %q", got.InstanceID, run.InstanceID)
	}
	if got.Status != run.Status {
		t.Errorf("Status: got %q, want %q", got.Status, run.Status)
	}
	if got.ExitCode == nil || *got.ExitCode != *run.ExitCode {
		t.Errorf("ExitCode: got %v, want %v", got.ExitCode, run.ExitCode)
	}
	if got.LaunchedBy != run.LaunchedBy {
		t.Errorf("LaunchedBy: got %q, want %q", got.LaunchedBy, run.LaunchedBy)
	}
	if !got.StartedAt.Equal(run.StartedAt) {
		t.Errorf("StartedAt: got %v, want %v", got.StartedAt, run.StartedAt)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(*run.CompletedAt) {
		t.Errorf("CompletedAt: got %v, want %v", got.CompletedAt, run.CompletedAt)
	}
	if !got.TimeoutAt.Equal(run.TimeoutAt) {
		t.Errorf("TimeoutAt: got %v, want %v", got.TimeoutAt, run.TimeoutAt)
	}
	if got.TotalCostUSD == nil || *got.TotalCostUSD != *run.TotalCostUSD {
		t.Errorf("TotalCostUSD: got %v, want %v", got.TotalCostUSD, run.TotalCostUSD)
	}
	for k, v := range run.Metadata {
		if got.Metadata[k] != v {
			t.Errorf("Metadata[%q]: got %q, want %q", k, got.Metadata[k], v)
		}
	}
	if len(got.Metadata) != len(run.Metadata) {
		t.Errorf("Metadata length: got %d, want %d", len(got.Metadata), len(run.Metadata))
	}
}

func TestSQLiteStore_CreateGetRun_NullFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	run := newTestRun()
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got.ExitCode != nil {
		t.Errorf("ExitCode: got %v, want nil", got.ExitCode)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt: got %v, want nil", got.CompletedAt)
	}
	if got.TotalCostUSD != nil {
		t.Errorf("TotalCostUSD: got %v, want nil", got.TotalCostUSD)
	}
	if got.Metadata != nil {
		t.Errorf("Metadata: got %v, want nil", got.Metadata)
	}
}

func TestSQLiteStore_GetRun_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	_, err = s.GetRun(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("errors.Is(err, ErrRunNotFound) = false, err = %v", err)
	}
	if !strings.Contains(err.Error(), "nonexistent-id") {
		t.Errorf("error %q does not contain %q", err.Error(), "nonexistent-id")
	}
}

func TestSQLiteStore_CreateGetRun_EmptyMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	run := newTestRun()
	run.Metadata = map[string]string{}

	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got.Metadata == nil {
		t.Error("Metadata: got nil, want non-nil empty map")
	}
	if len(got.Metadata) != 0 {
		t.Errorf("Metadata length: got %d, want 0", len(got.Metadata))
	}
}

func TestSQLiteStore_CreateGetRun_UTCNormalization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	est := time.FixedZone("EST", -5*3600)
	run := newTestRun()
	// Override timestamps with EST-zoned values (truncated to second for RFC3339 precision)
	run.StartedAt = time.Now().In(est).Truncate(time.Second)
	run.TimeoutAt = run.StartedAt.Add(60 * time.Minute)
	completedAt := run.StartedAt.Add(5 * time.Minute)
	run.CompletedAt = &completedAt

	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if !got.StartedAt.Equal(run.StartedAt) {
		t.Errorf("StartedAt: got %v, want %v (same instant)", got.StartedAt, run.StartedAt)
	}
	if got.StartedAt.Location() != time.UTC {
		t.Errorf("StartedAt location: got %v, want UTC", got.StartedAt.Location())
	}

	if !got.TimeoutAt.Equal(run.TimeoutAt) {
		t.Errorf("TimeoutAt: got %v, want %v (same instant)", got.TimeoutAt, run.TimeoutAt)
	}
	if got.TimeoutAt.Location() != time.UTC {
		t.Errorf("TimeoutAt location: got %v, want UTC", got.TimeoutAt.Location())
	}

	if got.CompletedAt == nil || !got.CompletedAt.Equal(*run.CompletedAt) {
		t.Errorf("CompletedAt: got %v, want %v (same instant)", got.CompletedAt, run.CompletedAt)
	}
	if got.CompletedAt != nil && got.CompletedAt.Location() != time.UTC {
		t.Errorf("CompletedAt location: got %v, want UTC", got.CompletedAt.Location())
	}
}

func TestSQLiteStore_CreateRun_DuplicateID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	run := newTestRun()
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("first CreateRun: %v", err)
	}

	err = s.CreateRun(ctx, run)
	if err == nil {
		t.Fatal("second CreateRun: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "inserting run") {
		t.Errorf("error %q does not contain %q", err.Error(), "inserting run")
	}
}

func ptr[T any](v T) *T { return &v }

func TestSQLiteStore_UpdateRun_SingleField(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	run := newTestRun()
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := s.UpdateRun(ctx, run.ID, &RunUpdate{Status: ptr(StatusRunning)}); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got.Status != StatusRunning {
		t.Errorf("Status: got %q, want %q", got.Status, StatusRunning)
	}
	if got.InstanceID != run.InstanceID {
		t.Errorf("InstanceID: got %q, want %q", got.InstanceID, run.InstanceID)
	}
	if got.ExitCode != nil {
		t.Errorf("ExitCode: got %v, want nil", got.ExitCode)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt: got %v, want nil", got.CompletedAt)
	}
	if got.TotalCostUSD != nil {
		t.Errorf("TotalCostUSD: got %v, want nil", got.TotalCostUSD)
	}
	if got.Metadata != nil {
		t.Errorf("Metadata: got %v, want nil", got.Metadata)
	}
}

func TestSQLiteStore_UpdateRun_MultipleFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	run := newTestRun()
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	completedAt := run.StartedAt.Add(10 * time.Minute).Truncate(time.Second)
	update := &RunUpdate{
		Status:       ptr(StatusSuccess),
		InstanceID:   ptr("new-container-id"),
		ExitCode:     ptr(0),
		CompletedAt:  &completedAt,
		TotalCostUSD: ptr(2.50),
		Metadata:     map[string]string{"key": "value"},
	}

	if err := s.UpdateRun(ctx, run.ID, update); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got.Status != StatusSuccess {
		t.Errorf("Status: got %q, want %q", got.Status, StatusSuccess)
	}
	if got.InstanceID != "new-container-id" {
		t.Errorf("InstanceID: got %q, want %q", got.InstanceID, "new-container-id")
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode: got %v, want 0", got.ExitCode)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(completedAt) {
		t.Errorf("CompletedAt: got %v, want %v", got.CompletedAt, completedAt)
	}
	if got.TotalCostUSD == nil || *got.TotalCostUSD != 2.50 {
		t.Errorf("TotalCostUSD: got %v, want 2.50", got.TotalCostUSD)
	}
	if len(got.Metadata) != 1 || got.Metadata["key"] != "value" {
		t.Errorf("Metadata: got %v, want map[key:value]", got.Metadata)
	}
}

func TestSQLiteStore_UpdateRun_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	err = s.UpdateRun(ctx, "nonexistent-id", &RunUpdate{Status: ptr(StatusRunning)})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("errors.Is(err, ErrRunNotFound) = false, err = %v", err)
	}
	if !strings.Contains(err.Error(), "nonexistent-id") {
		t.Errorf("error %q does not contain %q", err.Error(), "nonexistent-id")
	}
}

func TestSQLiteStore_UpdateRun_FieldIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	run := newTestRun()
	run.InstanceID = "original-container"
	run.Metadata = map[string]string{"orig": "data"}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := s.UpdateRun(ctx, run.ID, &RunUpdate{Status: ptr(StatusRunning)}); err != nil {
		t.Fatalf("UpdateRun (status only): %v", err)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("Status: got %q, want %q", got.Status, StatusRunning)
	}
	if got.InstanceID != "original-container" {
		t.Errorf("InstanceID: got %q, want %q", got.InstanceID, "original-container")
	}
	if len(got.Metadata) != 1 || got.Metadata["orig"] != "data" {
		t.Errorf("Metadata: got %v, want map[orig:data]", got.Metadata)
	}
	if got.ExitCode != nil {
		t.Errorf("ExitCode: got %v, want nil", got.ExitCode)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt: got %v, want nil", got.CompletedAt)
	}
	if got.TotalCostUSD != nil {
		t.Errorf("TotalCostUSD: got %v, want nil", got.TotalCostUSD)
	}

	if err := s.UpdateRun(ctx, run.ID, &RunUpdate{ExitCode: ptr(1), TotalCostUSD: ptr(0.75)}); err != nil {
		t.Fatalf("UpdateRun (exit+cost): %v", err)
	}

	got, err = s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("Status: got %q, want %q (should not revert)", got.Status, StatusRunning)
	}
	if got.InstanceID != "original-container" {
		t.Errorf("InstanceID: got %q, want %q", got.InstanceID, "original-container")
	}
	if len(got.Metadata) != 1 || got.Metadata["orig"] != "data" {
		t.Errorf("Metadata: got %v, want map[orig:data]", got.Metadata)
	}
	if got.ExitCode == nil || *got.ExitCode != 1 {
		t.Errorf("ExitCode: got %v, want 1", got.ExitCode)
	}
	if got.TotalCostUSD == nil || *got.TotalCostUSD != 0.75 {
		t.Errorf("TotalCostUSD: got %v, want 0.75", got.TotalCostUSD)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt: got %v, want nil", got.CompletedAt)
	}
}

func TestSQLiteStore_UpdateRun_MetadataUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	run := newTestRun()
	run.Metadata = map[string]string{"a": "1"}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := s.UpdateRun(ctx, run.ID, &RunUpdate{Metadata: map[string]string{"b": "2"}}); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(got.Metadata) != 1 || got.Metadata["b"] != "2" {
		t.Errorf("Metadata after overwrite: got %v, want map[b:2]", got.Metadata)
	}

	if err := s.UpdateRun(ctx, run.ID, &RunUpdate{Metadata: map[string]string{}}); err != nil {
		t.Fatalf("UpdateRun (empty): %v", err)
	}

	got, err = s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Metadata == nil {
		t.Error("Metadata: got nil, want non-nil empty map")
	}
	if len(got.Metadata) != 0 {
		t.Errorf("Metadata length: got %d, want 0", len(got.Metadata))
	}
}

func TestSQLiteStore_UpdateRun_UTCNormalization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	run := newTestRun()
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	est := time.FixedZone("EST", -5*3600)
	completedAt := run.StartedAt.Add(10 * time.Minute).In(est).Truncate(time.Second)
	if err := s.UpdateRun(ctx, run.ID, &RunUpdate{CompletedAt: &completedAt}); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	if got.CompletedAt == nil || !got.CompletedAt.Equal(completedAt) {
		t.Errorf("CompletedAt: got %v, want %v (same instant)", got.CompletedAt, completedAt)
	}
	if got.CompletedAt != nil && got.CompletedAt.Location() != time.UTC {
		t.Errorf("CompletedAt location: got %v, want UTC", got.CompletedAt.Location())
	}
}

func TestSQLiteStore_UpdateRun_NoFieldsSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	run := newTestRun()
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := s.UpdateRun(ctx, run.ID, &RunUpdate{}); err != nil {
		t.Fatalf("UpdateRun (no fields): %v", err)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != run.Status {
		t.Errorf("Status: got %q, want %q (should be unchanged)", got.Status, run.Status)
	}
}

func TestSQLiteStore_UpdateRun_NoFieldsSet_NonexistentID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "horde.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	err = s.UpdateRun(ctx, "nonexistent", &RunUpdate{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("errors.Is(err, ErrRunNotFound) = false, err = %v", err)
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error %q does not contain %q", err.Error(), "nonexistent")
	}
}

func TestSQLiteStore_ListByRepo_FiltersAndOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "horde.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	now := time.Now().Truncate(time.Second)

	// run-1: target repo, earliest started_at
	run1 := newTestRun()
	run1.ID = "run-1"
	run1.StartedAt = now.Add(-2 * time.Minute)
	run1.TimeoutAt = now.Add(60 * time.Minute)

	// run-2: target repo, latest started_at
	run2 := newTestRun()
	run2.ID = "run-2"
	run2.StartedAt = now
	run2.TimeoutAt = now.Add(60 * time.Minute)

	// run-3: different repo
	run3 := newTestRun()
	run3.ID = "run-3"
	run3.Repo = "github.com/other/project.git"
	run3.StartedAt = now.Add(-1 * time.Minute)
	run3.TimeoutAt = now.Add(60 * time.Minute)

	for _, r := range []*Run{run1, run2, run3} {
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatalf("CreateRun(%s): %v", r.ID, err)
		}
	}

	results, err := s.ListByRepo(ctx, "github.com/org/repo.git", false)
	if err != nil {
		t.Fatalf("ListByRepo: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	// Descending order: run-2 (latest) first
	if results[0].ID != "run-2" {
		t.Errorf("results[0].ID = %q, want %q", results[0].ID, "run-2")
	}
	if results[1].ID != "run-1" {
		t.Errorf("results[1].ID = %q, want %q", results[1].ID, "run-1")
	}
	// Other repo not included
	for _, r := range results {
		if r.Repo != "github.com/org/repo.git" {
			t.Errorf("unexpected repo %q in results", r.Repo)
		}
	}
}

func TestSQLiteStore_ListByRepo_ActiveOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "horde.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	repo := "github.com/org/repo.git"

	run1 := newTestRun()
	run1.ID = "run-pending"
	run1.Status = StatusPending

	run2 := newTestRun()
	run2.ID = "run-running"
	run2.Status = StatusRunning

	run3 := newTestRun()
	run3.ID = "run-success"
	run3.Status = StatusSuccess

	run4 := newTestRun()
	run4.ID = "run-failed"
	run4.Status = StatusFailed

	run5 := newTestRun()
	run5.ID = "run-killed"
	run5.Status = StatusKilled

	for _, r := range []*Run{run1, run2, run3, run4, run5} {
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatalf("CreateRun(%s): %v", r.ID, err)
		}
	}

	active, err := s.ListByRepo(ctx, repo, true)
	if err != nil {
		t.Fatalf("ListByRepo(activeOnly=true): %v", err)
	}
	if len(active) != 2 {
		t.Errorf("activeOnly=true: got %d results, want 2", len(active))
	}
	for _, r := range active {
		if r.Status != StatusPending && r.Status != StatusRunning {
			t.Errorf("activeOnly=true returned run with status %q", r.Status)
		}
	}

	all, err := s.ListByRepo(ctx, repo, false)
	if err != nil {
		t.Fatalf("ListByRepo(activeOnly=false): %v", err)
	}
	if len(all) != 5 {
		t.Errorf("activeOnly=false: got %d results, want 5", len(all))
	}
}

func TestSQLiteStore_ListByRepo_EmptyResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "horde.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	results, err := s.ListByRepo(ctx, "github.com/org/repo.git", false)
	if err != nil {
		t.Fatalf("ListByRepo: %v", err)
	}
	if results == nil {
		t.Error("ListByRepo returned nil, want non-nil empty slice")
	}
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0", len(results))
	}
}

func TestSQLiteStore_FindActiveByTicket_Match(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "horde.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	repo := "github.com/org/repo.git"
	ticket := "PROJ-42"

	// active with matching repo+ticket — the one we expect returned
	run1 := newTestRun()
	run1.ID = "run-active-match"
	run1.Status = StatusPending

	// completed with same repo+ticket — should NOT be returned
	run2 := newTestRun()
	run2.ID = "run-completed-match"
	run2.Status = StatusSuccess

	// active with same repo but different ticket — should NOT be returned
	run3 := newTestRun()
	run3.ID = "run-active-other-ticket"
	run3.Ticket = "PROJ-99"
	run3.Status = StatusRunning

	for _, r := range []*Run{run1, run2, run3} {
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatalf("CreateRun(%s): %v", r.ID, err)
		}
	}

	results, err := s.FindActiveByTicket(ctx, repo, ticket)
	if err != nil {
		t.Fatalf("FindActiveByTicket: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].ID != "run-active-match" {
		t.Errorf("results[0].ID = %q, want %q", results[0].ID, "run-active-match")
	}
}

func TestSQLiteStore_FindActiveByTicket_NoMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "horde.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	run := newTestRun()
	run.Status = StatusSuccess
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	results, err := s.FindActiveByTicket(ctx, run.Repo, run.Ticket)
	if err != nil {
		t.Fatalf("FindActiveByTicket: %v", err)
	}
	if results == nil {
		t.Error("FindActiveByTicket returned nil, want non-nil empty slice")
	}
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0", len(results))
	}
}

func TestSQLiteStore_FindActiveByTicket_RepoMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "horde.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	// active run for a different repo, but same ticket "PROJ-42"
	run := newTestRun()
	run.Repo = "github.com/other/project.git"
	run.Status = StatusRunning
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	results, err := s.FindActiveByTicket(ctx, "github.com/org/repo.git", "PROJ-42")
	if err != nil {
		t.Fatalf("FindActiveByTicket: %v", err)
	}
	if results == nil {
		t.Error("FindActiveByTicket returned nil, want non-nil empty slice")
	}
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0 (repo must match)", len(results))
	}
}

func TestSQLiteStore_CountActive_Empty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "horde.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	count, err := s.CountActive(ctx)
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	if count != 0 {
		t.Errorf("CountActive = %d, want 0", count)
	}
}

func TestSQLiteStore_CountActive_MixedStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "horde.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	now := time.Now().Truncate(time.Second)

	statuses := []struct {
		id     string
		status Status
	}{
		{"run-pending", StatusPending},
		{"run-running", StatusRunning},
		{"run-success", StatusSuccess},
		{"run-failed", StatusFailed},
		{"run-killed", StatusKilled},
	}

	for _, tc := range statuses {
		run := newTestRun()
		run.ID = tc.id
		run.Status = tc.status
		run.StartedAt = now
		run.TimeoutAt = now.Add(60 * time.Minute)
		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun(%s): %v", tc.id, err)
		}
	}

	count, err := s.CountActive(ctx)
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	if count != 2 {
		t.Errorf("CountActive = %d, want 2 (pending + running)", count)
	}
}

func TestSQLiteStore_CountActive_CrossRepo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "horde.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	now := time.Now().Truncate(time.Second)

	run1 := newTestRun()
	run1.ID = "run-repo-a"
	run1.Repo = "github.com/org/repo-a.git"
	run1.Status = StatusPending
	run1.StartedAt = now
	run1.TimeoutAt = now.Add(60 * time.Minute)

	run2 := newTestRun()
	run2.ID = "run-repo-b"
	run2.Repo = "github.com/org/repo-b.git"
	run2.Status = StatusRunning
	run2.StartedAt = now
	run2.TimeoutAt = now.Add(60 * time.Minute)

	for _, r := range []*Run{run1, run2} {
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatalf("CreateRun(%s): %v", r.ID, err)
		}
	}

	count, err := s.CountActive(ctx)
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	if count != 2 {
		t.Errorf("CountActive = %d, want 2 (one from each repo)", count)
	}
}
