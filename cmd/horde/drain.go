package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jorge-barreto/horde/internal/event"
	"github.com/jorge-barreto/horde/internal/store"
)

// sortByDrainOrder orders runs the way the drain claims them: highest priority
// first, then oldest enqueued_at. Used by `horde queue list` for an accurate
// preview of what launches next, and by the lazy drain's peek.
func sortByDrainOrder(runs []*store.Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		oi, oj := runs[i].Priority.Ordinal(), runs[j].Priority.Ordinal()
		if oi != oj {
			return oi > oj
		}
		return runs[i].EnqueuedAt.Before(runs[j].EnqueuedAt)
	})
}

// timeNow is a seam for tests.
var timeNow = func() time.Time { return time.Now() }

// drainDeps are the injected dependencies of drainOnce, so it is testable
// without a live provider or AWS. launch starts the run; spendOK is the
// realized spend-rate gate; emit fires lifecycle events (run.started /
// run.cost-threshold-exceeded).
type drainDeps struct {
	store         store.Store
	repo          string
	maxConcurrent int
	launch        func(ctx context.Context, run *store.Run) error
	spendOK       func(ctx context.Context) (bool, error)
	emit          func(detailType string, run *store.Run)
}

// drainOnce drains at most one queued run for the repo if capacity and budget
// allow. It is the lazy CLI backstop (invoked from launch/list) and mirrors the
// drain Lambda's logic. Best-effort: errors are logged, never returned to the
// caller's command (a failed drain must not fail an unrelated `horde list`).
func drainOnce(ctx context.Context, d drainDeps) {
	active, err := d.store.CountActive(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: drain capacity check: %v\n", err)
		return
	}
	if active >= d.maxConcurrent {
		return // no slot
	}
	ok, err := d.spendOK(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: drain spend check: %v\n", err)
		return
	}
	if !ok {
		// Held by budget: signal it, leave runs queued.
		if next, _ := peekNextQueued(ctx, d.store, d.repo); next != nil {
			d.emit(event.TypeRunCostThresholdExceeded, next)
		}
		return
	}
	run, err := d.store.ClaimNextQueued(ctx, d.repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: claiming queued run: %v\n", err)
		return
	}
	if run == nil {
		return // empty queue
	}
	if err := d.launch(ctx, run); err != nil {
		// Launch failed after claim: mark failed (NOT re-queued — avoid a poison
		// loop). Surfaces in `horde list` for a human/agent to retry.
		failed := store.StatusFailed
		now := timeNow()
		md := map[string]string{"drain_launch_error": err.Error()}
		if uerr := d.store.UpdateRun(ctx, run.ID, &store.RunUpdate{Status: &failed, CompletedAt: &now, Metadata: md}); uerr != nil {
			fmt.Fprintf(os.Stderr, "warning: marking drained run failed: %v\n", uerr)
		}
		fmt.Fprintf(os.Stderr, "warning: launching drained run %s: %v\n", run.ID, err)
		return
	}
	d.emit(event.TypeRunStarted, run)
}

// peekNextQueued returns the next run the drain WOULD claim, without claiming.
func peekNextQueued(ctx context.Context, st store.Store, repo string) (*store.Run, error) {
	runs, err := st.ListRuns(ctx, store.RunFilter{Repo: repo, Statuses: []store.Status{store.StatusQueued}})
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	sortByDrainOrder(runs)
	return runs[0], nil
}
