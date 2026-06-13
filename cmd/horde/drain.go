package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jorge-barreto/horde/internal/config"
	"github.com/jorge-barreto/horde/internal/event"
	"github.com/jorge-barreto/horde/internal/provider"
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

// realizedSpendGate builds a spendOK function that sums TotalCostUSD of runs
// completed within the trailing window and reports whether the project is under
// the cap. Realized-only: in-flight runs contribute $0 until they finish (the
// concurrency limit is the blast-radius backstop; see follow-on #52). A cap of
// 0 or a zero window means "no spend cap configured" — always OK.
func realizedSpendGate(st store.Store, repo string, capUSD float64, window time.Duration, now func() time.Time) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		if capUSD <= 0 || window <= 0 {
			return true, nil
		}
		since := now().Add(-window)
		// The window is keyed on completed_at (when spend was realized), which
		// RunFilter.Since/Until do not filter on (they bound started_at), so
		// fetch the repo's runs and sum in-loop on CompletedAt.
		runs, err := st.ListRuns(ctx, store.RunFilter{Repo: repo})
		if err != nil {
			return false, fmt.Errorf("listing runs for spend window: %w", err)
		}
		var total float64
		for _, r := range runs {
			if r.TotalCostUSD == nil || r.CompletedAt == nil {
				continue
			}
			if r.CompletedAt.Before(since) {
				continue
			}
			total += *r.TotalCostUSD
		}
		return total < capUSD, nil
	}
}

// lazyDrainComponents bundles the live dependencies the CLI backstop drain
// needs. It is ECS-only (the queue/event backbone is ECS-only); on docker
// lazyDrain is a no-op.
type lazyDrainComponents struct {
	prov          provider.Provider
	store         store.Store
	resolver      *config.Resolver
	emitter       event.Emitter
	repo          string
	provName      string
	homeDir       string
	maxConcurrent int
	maxSpend      float64
	spendWindow   string // Go duration string; "" or unparsable → no spend cap
}

// lazyDrain runs one opportunistic drain pass using live components. It is the
// CLI backstop invoked after `horde launch`/`list` finish their own work, so a
// missed terminal event self-heals the next time anyone touches the project.
// Strictly best-effort: it never returns an error and must run AFTER the host
// command's output so a drain hiccup can't corrupt --json stdout.
func lazyDrain(ctx context.Context, lc lazyDrainComponents) {
	if lc.provName != "aws-ecs" {
		return // queue/drain is ECS-only; docker stays pull-based, no daemon
	}
	window, _ := time.ParseDuration(lc.spendWindow) // empty/invalid → 0 → no cap
	drainOnce(ctx, drainDeps{
		store:         lc.store,
		repo:          lc.repo,
		maxConcurrent: lc.maxConcurrent,
		spendOK:       realizedSpendGate(lc.store, lc.repo, lc.maxSpend, window, timeNow),
		launch:        launchQueuedRun(lc),
		emit: func(dt string, r *store.Run) {
			if err := lc.emitter.Emit(ctx, dt, event.DetailFromRun(r)); err != nil {
				fmt.Fprintf(os.Stderr, "warning: emitting %s: %v\n", dt, err)
			}
		},
	})
}

// launchQueuedRun is the lazy drain's launch closure: it re-resolves current
// secrets/mounts (so a run that waited in the queue launches against today's
// config) and starts the run via the provider, then transitions it to running.
// ECS-only — no docker EnsureImage. Mirrors the direct-launch path's
// prov.Launch + UpdateRun-to-running, deliberately kept separate from it so the
// well-tested direct path is not disturbed.
func launchQueuedRun(lc lazyDrainComponents) func(context.Context, *store.Run) error {
	return func(ctx context.Context, run *store.Run) error {
		envPath, _, secretRemap, err := resolveSecretsForLaunch(lc.provName, lc.resolver)
		if err != nil {
			return fmt.Errorf("resolving secrets: %w", err)
		}
		projCfg, err := lc.resolver.ProjectConfig()
		if err != nil {
			return fmt.Errorf("resolving project config: %w", err)
		}
		now := timeNow()
		timeoutAt := now.Add(run.TimeoutAt.Sub(run.StartedAt)) // preserve requested window if set
		if !run.TimeoutAt.After(run.StartedAt) {
			timeoutAt = now.Add(24 * time.Hour) // queued runs have no started_at yet; default window
		}
		result, err := lc.prov.Launch(ctx, provider.LaunchOpts{
			Repo:           run.Repo,
			Ticket:         run.Ticket,
			Branch:         run.Branch,
			Workflow:       run.Workflow,
			RunID:          run.ID,
			EnvFile:        envPath,
			Mounts:         projCfg.ResolveMounts(lc.resolver.EnvFileDir()),
			HomeDir:        lc.homeDir,
			SecretEnvRemap: secretRemap,
			Capacity:       string(run.Capacity),
		})
		if err != nil {
			return err // drainOnce marks the run failed (not re-queued)
		}
		running := store.StatusRunning
		if err := lc.store.UpdateRun(ctx, run.ID, &store.RunUpdate{
			Status:     &running,
			InstanceID: &result.InstanceID,
			Metadata:   result.Metadata,
			TimeoutAt:  &timeoutAt,
		}); err != nil {
			return fmt.Errorf("updating drained run to running: %w", err)
		}
		// Reflect running state on the in-memory run so the run.started event
		// Detail (emitted by drainOnce) carries instance_id/status/started_at.
		run.Status = store.StatusRunning
		run.InstanceID = result.InstanceID
		run.Metadata = result.Metadata
		run.StartedAt = now
		run.TimeoutAt = timeoutAt
		return nil
	}
}

// lazyDrainFromContext assembles lazyDrainComponents from the standard command
// locals shared by launch/list and runs one best-effort drain pass. Centralizes
// the wiring so call sites stay one line. No-op on docker (checked in lazyDrain).
func lazyDrainFromContext(ctx context.Context, prov provider.Provider, st store.Store, resolver *config.Resolver, hordeCfg *config.HordeConfig, repo, provName, homeDir string, maxConcurrent int, emitter event.Emitter) {
	lc := lazyDrainComponents{
		prov: prov, store: st, resolver: resolver, emitter: emitter,
		repo: repo, provName: provName, homeDir: homeDir, maxConcurrent: maxConcurrent,
	}
	if hordeCfg != nil {
		lc.maxSpend = hordeCfg.MaxSpendPerWindow
		lc.spendWindow = hordeCfg.SpendWindow
	}
	lazyDrain(ctx, lc)
}
