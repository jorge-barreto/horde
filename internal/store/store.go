package store

import (
	"context"
	"time"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusSuccess Status = "success"
	StatusFailed  Status = "failed"
	StatusKilled  Status = "killed"
)

type Run struct {
	ID           string
	Repo         string
	Ticket       string
	Branch       string
	Workflow     string
	Provider     string
	InstanceID   string
	Status       Status
	ExitCode     *int
	LaunchedBy   string
	StartedAt    time.Time
	CompletedAt  *time.Time
	TimeoutAt    time.Time
	TotalCostUSD *float64
}

type Store interface {
	CreateRun(ctx context.Context, run *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	UpdateRun(ctx context.Context, id string, fields map[string]any) error
	ListByRepo(ctx context.Context, repo string, activeOnly bool) ([]*Run, error)
	FindActiveByTicket(ctx context.Context, repo string, ticket string) ([]*Run, error)
	CountActive(ctx context.Context) (int, error)
}
