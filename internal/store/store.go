package store

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrRunNotFound = errors.New("run not found")

type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusSuccess Status = "success"
	StatusFailed  Status = "failed"
	StatusKilled  Status = "killed"
	// StatusTimedOut and StatusRateLimited are terminal but recoverable:
	// the run stopped for a transient reason (orc phase timeout / Anthropic
	// rate-limit exhaustion) rather than a genuine failure, so its sunk work
	// is worth resuming. They are distinct from StatusFailed so that
	// `horde list` makes them legible as "waiting, not broken" and so a
	// future scheduler can auto-resume them. `horde retry` accepts them
	// like failed/killed. Source: orc exit code 2 (timeout) → StatusTimedOut,
	// exit code 4 (cost/rate limit) → StatusRateLimited (see mapExitCode).
	StatusTimedOut    Status = "timed_out"
	StatusRateLimited Status = "rate_limited"
)

// matchesFilter reports whether a run satisfies the non-repo dimensions of a
// RunFilter (Repo scoping is handled by the query that fetched the run). It is
// the single source of truth for filter semantics, shared by the SQLite store
// (which fetches repo rows then filters in Go) and the conformance test fake.
// The DynamoDB store mirrors this logic in a server-side FilterExpression; the
// conformance suite runs the same cases against both, guarding against drift.
func matchesFilter(run *Run, f RunFilter) bool {
	if len(f.Statuses) > 0 {
		ok := false
		for _, st := range f.Statuses {
			if run.Status == st {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if f.Workflow != "" && run.Workflow != f.Workflow {
		return false
	}
	if f.Ticket != "" && run.Ticket != f.Ticket {
		return false
	}
	for k, v := range f.Labels {
		if run.Labels[k] != v {
			return false
		}
	}
	if f.Since != nil && run.StartedAt.Before(*f.Since) {
		return false
	}
	if f.Until != nil && run.StartedAt.After(*f.Until) {
		return false
	}
	return true
}

// IsTerminal reports whether a run has reached a final state and will not
// change further. New statuses must be classified here — callers use this
// to decide "active vs done" without enumerating statuses inline.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusSuccess, StatusFailed, StatusKilled, StatusTimedOut, StatusRateLimited:
		return true
	}
	return false
}

type Run struct {
	ID         string
	Repo       string
	Ticket     string
	Branch     string
	Workflow   string
	Provider   string
	InstanceID string
	Metadata   map[string]string
	// Labels are user-supplied key/value tags set once at launch (via
	// `horde launch --label k=v`) and queried via `horde list` filters. They
	// are deliberately separate from Metadata: Metadata holds provider-internal
	// data (ECS cluster_arn, log_group, …) written by the provider, whereas
	// Labels are the caller's own taxonomy (epic, prompt variant, dispatched-by).
	// Keeping them apart means user keys can never collide with reserved
	// provider keys, and the two evolve independently.
	Labels       map[string]string
	Status       Status
	ExitCode     *int
	LaunchedBy   string
	StartedAt    time.Time
	CompletedAt  *time.Time
	TimeoutAt    time.Time
	TotalCostUSD *float64
}

// RunUpdate holds fields to update on an existing run.
// Pointer fields: nil means "don't update", non-nil means "set to this value".
type RunUpdate struct {
	Status       *Status
	InstanceID   *string
	Metadata     map[string]string // nil = don't update; non-nil (even empty) = overwrite
	ExitCode     *int
	CompletedAt  *time.Time
	TotalCostUSD *float64
	TimeoutAt    *time.Time
}

// RunFilter scopes a ListRuns query. Repo is always required (listing is
// repo-scoped). The remaining fields are AND-combined; a zero/empty field is
// "no constraint on this dimension":
//   - Statuses: run status must be in this set (empty = any status).
//   - Workflow / Ticket: exact match (empty = any).
//   - Labels: every key/value pair must match the run's labels exactly
//     (AND across keys; empty/nil = no label constraint).
//   - Since / Until: bounds on StartedAt, inclusive (nil = unbounded).
type RunFilter struct {
	Repo     string
	Statuses []Status
	Workflow string
	Ticket   string
	Labels   map[string]string
	Since    *time.Time
	Until    *time.Time
}

type Store interface {
	io.Closer
	CreateRun(ctx context.Context, run *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	UpdateRun(ctx context.Context, id string, update *RunUpdate) error
	ListByRepo(ctx context.Context, repo string, activeOnly bool) ([]*Run, error)
	// ListRuns returns runs matching the filter, scoped to filter.Repo, sorted
	// by started_at descending (newest first). It is the general-purpose query
	// behind `horde list`; ListByRepo is the active-only/all special case.
	ListRuns(ctx context.Context, filter RunFilter) ([]*Run, error)
	FindActiveByTicket(ctx context.Context, repo string, ticket string) ([]*Run, error)
	CountActive(ctx context.Context) (int, error)
	// ListActive returns all runs in pending or running status across every
	// repo, sorted by started_at descending (newest first). The pending /
	// running statuses are interleaved by start time — implementations that
	// fetch them in separate queries must merge-sort before returning so
	// callers can rely on a stable cross-status ordering.
	ListActive(ctx context.Context) ([]*Run, error)
}
