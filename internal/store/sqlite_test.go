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
