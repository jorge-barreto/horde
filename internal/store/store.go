package store

import (
	"context"
	"errors"
	"fmt"
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
	// StatusQueued is a launch parked in the server-side backlog
	// (`horde launch --enqueue`), waiting for a concurrency slot and budget
	// headroom. It is NON-terminal and NOT "active" — it consumes no slot
	// (CountActive counts only pending+running). It drains to pending.
	StatusQueued Status = "queued"
	// StatusCancelled is a queued run cancelled before it ever ran
	// (`horde queue cancel`). Terminal. Distinct from StatusKilled, which
	// means a RUNNING task was stopped and may have committed work / spent
	// money; a cancelled run definitionally did neither.
	StatusCancelled Status = "cancelled"
)

// Priority is the queue drain-order lever set at enqueue and adjustable via
// `horde queue prioritize`. The drain picks highest priority first, then
// oldest enqueued_at within a level. It is purely mechanical: horde never
// decides priority from ticket meaning — the operator (or an agent above
// horde) curates the backlog.
type Priority string

const (
	PriorityLowest  Priority = "lowest"
	PriorityLow     Priority = "low"
	PriorityMed     Priority = "med"
	PriorityHigh    Priority = "high"
	PriorityHighest Priority = "highest"
)

var priorityOrdinals = map[Priority]int{
	PriorityLowest: 0, PriorityLow: 1, PriorityMed: 2, PriorityHigh: 3, PriorityHighest: 4,
}

// Ordinal is the sortable rank; higher drains first. Unknown priorities sort
// as med so a malformed stored value never starves the queue.
func (p Priority) Ordinal() int {
	if o, ok := priorityOrdinals[p]; ok {
		return o
	}
	return priorityOrdinals[PriorityMed]
}

// ParsePriority validates a user-supplied priority string. Empty defaults to
// med. Unknown values are an error (surfaced to the CLI user).
func ParsePriority(s string) (Priority, error) {
	if s == "" {
		return PriorityMed, nil
	}
	p := Priority(s)
	if _, ok := priorityOrdinals[p]; !ok {
		return "", fmt.Errorf("invalid priority %q: want one of lowest, low, med, high, highest", s)
	}
	return p, nil
}

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
	case StatusSuccess, StatusFailed, StatusKilled, StatusTimedOut, StatusRateLimited, StatusCancelled:
		return true
	}
	return false
}

// TokenUsage is the per-run token telemetry orc reports, as run-level totals
// summed across phases. Zero counts are meaningful (a run can legitimately use
// 0 cache-read tokens), so presence is tracked by the enclosing *TokenUsage
// being nil — mirroring the TotalCostUSD *float64 "nil = not yet known"
// convention. Turns is the agent-invocation count summed across phases.
type TokenUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	Turns               int
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
	// EnqueuedAt is set when a run enters the backlog via --enqueue; it is the
	// zero value for directly-launched runs. It is the drain-order tiebreaker
	// (oldest first within a priority level). Distinct from StartedAt, which is
	// set when the run actually begins running (at drain time for queued runs).
	EnqueuedAt time.Time
	// Priority is the drain-order lever; empty for directly-launched runs.
	Priority Priority
	// Tokens is nil until orc reports usage (pre-finalize, or an orc old
	// enough that neither costs.json nor run-result.json carried token totals).
	Tokens *TokenUsage
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
	Tokens       *TokenUsage
	TimeoutAt    *time.Time
	Priority     *Priority // nil = don't update
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
