package main

import (
	"testing"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

func TestParseStatuses(t *testing.T) {
	t.Parallel()
	t.Run("empty returns nil", func(t *testing.T) {
		t.Parallel()
		got, err := parseStatuses(nil)
		if err != nil || got != nil {
			t.Errorf("got (%v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("valid set", func(t *testing.T) {
		t.Parallel()
		got, err := parseStatuses([]string{"running", "success"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got[0] != store.StatusRunning || got[1] != store.StatusSuccess {
			t.Errorf("got %v", got)
		}
	})
	t.Run("dedup", func(t *testing.T) {
		t.Parallel()
		got, err := parseStatuses([]string{"failed", "failed"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("len = %d, want 1 (deduped)", len(got))
		}
	})
	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		if _, err := parseStatuses([]string{"bogus"}); err == nil {
			t.Error("expected error for invalid status, got nil")
		}
	})
}

func TestParseWhen(t *testing.T) {
	t.Parallel()
	t.Run("empty returns nil", func(t *testing.T) {
		t.Parallel()
		got, err := parseWhen("")
		if err != nil || got != nil {
			t.Errorf("got (%v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("date only", func(t *testing.T) {
		t.Parallel()
		got, err := parseWhen("2026-04-01")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("rfc3339", func(t *testing.T) {
		t.Parallel()
		got, err := parseWhen("2026-04-01T12:30:00Z")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 4, 1, 12, 30, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("duration hours ago", func(t *testing.T) {
		t.Parallel()
		got, err := parseWhen("2h")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// ~2 hours before now (allow generous slack for test scheduling).
		delta := time.Since(*got)
		if delta < 2*time.Hour-time.Minute || delta > 2*time.Hour+time.Minute {
			t.Errorf("delta = %v, want ~2h", delta)
		}
	})
	t.Run("duration days ago", func(t *testing.T) {
		t.Parallel()
		got, err := parseWhen("7d")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		delta := time.Since(*got)
		if delta < 7*24*time.Hour-time.Minute || delta > 7*24*time.Hour+time.Minute {
			t.Errorf("delta = %v, want ~7d", delta)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		if _, err := parseWhen("not-a-time"); err == nil {
			t.Error("expected error for invalid time, got nil")
		}
	})
	// StartedAt is persisted at second precision in both stores, so a
	// sub-second bound would be compared inconsistently (SQLite filters in Go
	// at full precision; DynamoDB compares truncated RFC3339 strings). parseWhen
	// must hand back a second-truncated bound so both stores agree.
	t.Run("duration-ago truncated to second", func(t *testing.T) {
		t.Parallel()
		got, err := parseWhen("1h")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Nanosecond() != 0 {
			t.Errorf("expected second-truncated time, got sub-second %v", got)
		}
	})
	t.Run("negative duration rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := parseWhen("-3d"); err == nil {
			t.Error("expected error for negative duration-ago, got nil")
		}
	})
	t.Run("absurd day count rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := parseWhen("999999999999d"); err == nil {
			t.Error("expected error for overflowing day count, got nil")
		}
	})
}
