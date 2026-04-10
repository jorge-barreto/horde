package store

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
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
