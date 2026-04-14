package provider

import (
	"context"
	"io"
	"time"
)

// Provider abstracts container/task lifecycle operations.
type Provider interface {
	Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error)
	Status(ctx context.Context, instanceID string) (*InstanceStatus, error)
	Logs(ctx context.Context, instanceID string, follow bool) (io.ReadCloser, error)
	Stop(ctx context.Context, opts StopOpts) error
	ReadFile(ctx context.Context, opts ReadFileOpts) ([]byte, error)
}

// LaunchOpts contains parameters for launching a worker instance.
type LaunchOpts struct {
	Repo       string
	Ticket     string
	Branch     string
	Workflow   string
	RunID      string
	EnvFile    string   // path to .env file (docker provider)
	Mounts []string // volume mounts in docker format (host:container)
}

// LaunchResult contains the outcome of a successful launch.
type LaunchResult struct {
	InstanceID string            // container ID or ECS task ARN
	Metadata   map[string]string // provider-specific data (ECS: cluster_arn, log_group, etc.)
}

// InstanceStatus describes the current state of a running or completed instance.
type InstanceStatus struct {
	State      string // running, stopped, unknown
	ExitCode   *int   // nil while running
	StartedAt  time.Time
	FinishedAt *time.Time // nil while running
}

// ReadFileOpts contains parameters for reading a file from a completed instance.
type ReadFileOpts struct {
	InstanceID string            // container ID or ECS task ARN
	Path       string            // logical path relative to project root
	RunID      string            // run ID (used by ECS provider to resolve S3 prefix)
	Metadata   map[string]string // provider-specific metadata from LaunchResult
}

// StopOpts contains parameters for stopping a running instance.
type StopOpts struct {
	InstanceID string // container ID or ECS task ARN
	ResultsDir string // per-run results directory for artifact copy (docker); empty to skip copy
}
