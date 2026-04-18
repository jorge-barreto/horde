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
)

// IsTerminal reports whether a run has reached a final state and will not
// change further. New statuses must be classified here — callers use this
// to decide "active vs done" without enumerating statuses inline.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusSuccess, StatusFailed, StatusKilled:
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
	ListActive(ctx context.Context) ([]*Run, error)
}
