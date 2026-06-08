package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

func newDrainTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "horde.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDrainOnceRespectsCapacity(t *testing.T) {
	st := newDrainTestStore(t)
	ctx := context.Background()
	now := time.Now()
	// At capacity: one running + maxConcurrent=1 → no drain.
	_ = st.CreateRun(ctx, &store.Run{ID: "run", Repo: "r", Ticket: "A", Workflow: "w", Provider: "aws-ecs", Status: store.StatusRunning, LaunchedBy: "me", StartedAt: now, TimeoutAt: now.Add(time.Hour)})
	_ = st.CreateRun(ctx, &store.Run{ID: "q", Repo: "r", Ticket: "B", Workflow: "w", Provider: "aws-ecs", Status: store.StatusQueued, Priority: store.PriorityMed, LaunchedBy: "me", EnqueuedAt: now, TimeoutAt: now.Add(time.Hour)})

	launched := false
	drainOnce(ctx, drainDeps{
		store: st, repo: "r", maxConcurrent: 1,
		launch:  func(_ context.Context, _ *store.Run) error { launched = true; return nil },
		spendOK: func(_ context.Context) (bool, error) { return true, nil },
		emit:    func(string, *store.Run) {},
	})
	if launched {
		t.Error("drained despite being at capacity")
	}
}

func TestDrainOnceLaunchesWhenSlotFree(t *testing.T) {
	st := newDrainTestStore(t)
	ctx := context.Background()
	now := time.Now()
	_ = st.CreateRun(ctx, &store.Run{ID: "q", Repo: "r", Ticket: "B", Workflow: "w", Provider: "aws-ecs", Status: store.StatusQueued, Priority: store.PriorityMed, LaunchedBy: "me", EnqueuedAt: now, TimeoutAt: now.Add(time.Hour)})

	var launchedID, emittedType string
	drainOnce(ctx, drainDeps{
		store: st, repo: "r", maxConcurrent: 5,
		launch:  func(_ context.Context, run *store.Run) error { launchedID = run.ID; return nil },
		spendOK: func(_ context.Context) (bool, error) { return true, nil },
		emit:    func(dt string, _ *store.Run) { emittedType = dt },
	})
	if launchedID != "q" {
		t.Errorf("expected to drain q, launched %q", launchedID)
	}
	if emittedType != "run.started" {
		t.Errorf("expected run.started emitted, got %q", emittedType)
	}
}

func TestDrainOnceHeldByBudgetEmitsThreshold(t *testing.T) {
	st := newDrainTestStore(t)
	ctx := context.Background()
	now := time.Now()
	_ = st.CreateRun(ctx, &store.Run{ID: "q", Repo: "r", Ticket: "B", Workflow: "w", Provider: "aws-ecs", Status: store.StatusQueued, Priority: store.PriorityMed, LaunchedBy: "me", EnqueuedAt: now, TimeoutAt: now.Add(time.Hour)})

	launched := false
	var emittedType string
	drainOnce(ctx, drainDeps{
		store: st, repo: "r", maxConcurrent: 5,
		launch:  func(_ context.Context, _ *store.Run) error { launched = true; return nil },
		spendOK: func(_ context.Context) (bool, error) { return false, nil }, // over budget
		emit:    func(dt string, _ *store.Run) { emittedType = dt },
	})
	if launched {
		t.Error("drained despite being over budget")
	}
	if emittedType != "run.cost-threshold-exceeded" {
		t.Errorf("expected run.cost-threshold-exceeded emitted, got %q", emittedType)
	}
}

func TestDrainOnceLaunchFailureMarksFailed(t *testing.T) {
	st := newDrainTestStore(t)
	ctx := context.Background()
	now := time.Now()
	_ = st.CreateRun(ctx, &store.Run{ID: "q", Repo: "r", Ticket: "B", Workflow: "w", Provider: "aws-ecs", Status: store.StatusQueued, Priority: store.PriorityMed, LaunchedBy: "me", EnqueuedAt: now, TimeoutAt: now.Add(time.Hour)})

	drainOnce(ctx, drainDeps{
		store: st, repo: "r", maxConcurrent: 5,
		launch:  func(_ context.Context, _ *store.Run) error { return context.DeadlineExceeded },
		spendOK: func(_ context.Context) (bool, error) { return true, nil },
		emit:    func(string, *store.Run) {},
	})
	got, err := st.GetRun(ctx, "q")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusFailed {
		t.Errorf("status = %q, want failed (launch failed after claim, not re-queued)", got.Status)
	}
}
