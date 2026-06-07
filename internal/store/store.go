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
	ID           string
	Repo         string
	Ticket       string
	Branch       string
	Workflow     string
	Provider     string
	InstanceID   string
	Metadata     map[string]string
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

type Store interface {
	io.Closer
	CreateRun(ctx context.Context, run *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	UpdateRun(ctx context.Context, id string, update *RunUpdate) error
	ListByRepo(ctx context.Context, repo string, activeOnly bool) ([]*Run, error)
	FindActiveByTicket(ctx context.Context, repo string, ticket string) ([]*Run, error)
	CountActive(ctx context.Context) (int, error)
	// ListActive returns all runs in pending or running status across every
	// repo, sorted by started_at descending (newest first). The pending /
	// running statuses are interleaved by start time — implementations that
	// fetch them in separate queries must merge-sort before returning so
	// callers can rely on a stable cross-status ordering.
	ListActive(ctx context.Context) ([]*Run, error)
}
