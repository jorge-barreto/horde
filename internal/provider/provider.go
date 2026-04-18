package provider

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"
)

// Provider abstracts container/task lifecycle operations.
type Provider interface {
	Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error)
	Status(ctx context.Context, instanceID string) (*InstanceStatus, error)
	Logs(ctx context.Context, instanceID string, follow bool) (io.ReadCloser, error)
	Stop(ctx context.Context, opts StopOpts) error
	ReadFile(ctx context.Context, opts ReadFileOpts) ([]byte, error)
	HydrateRun(ctx context.Context, opts HydrateOpts) error
}

// LaunchOpts contains parameters for launching a worker instance.
type LaunchOpts struct {
	Repo     string
	Ticket   string
	Branch   string
	Workflow string
	RunID    string
	EnvFile  string   // path to .env file (docker provider)
	Mounts   []string // volume mounts in docker format (host:container)
	HomeDir  string   // home directory for workspace path resolution
	OrcArgs  []string // extra orc flags passed through via -- (opaque)
}

// WorkspacePath returns the host path for a run's persistent workspace.
func WorkspacePath(homeDir, runID string) string {
	return filepath.Join(homeDir, ".horde", "workspaces", runID)
}

// SessionsPath returns the host path for a run's persistent agent session
// state (bind-mounted to /root/.claude inside the container). Keeping it
// beside the workspace dir mirrors the per-run lifecycle.
func SessionsPath(homeDir, runID string) string {
	return filepath.Join(homeDir, ".horde", "workspaces", runID+"-sessions")
}

// LaunchResult contains the outcome of a successful launch.
type LaunchResult struct {
	InstanceID string            // container ID or ECS task ARN
	Metadata   map[string]string // provider-specific data (ECS: cluster_arn, log_group, etc.)
}

// InstanceStatus describes the current state of a running or completed instance.
type InstanceStatus struct {
	State      string // pending, running, stopping, stopped, unknown
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

// HydrateOpts contains parameters for copying a run's audit and artifacts
// trees to local destination directories.
//
// Providers locate the source tree using RunID plus the orc-native
// (Workflow, Ticket) path components. Workflow may be empty — in which
// case the source path omits the workflow segment. DestAuditDir and
// DestArtifactsDir are the final on-disk destinations for the bytes
// under the run's <workflow>/<ticket>/ subtree (caller-assembled).
//
// If the source data does not exist for this run, implementations return
// *FileNotFoundError with Path set to a description of what was missing.
type HydrateOpts struct {
	RunID            string            // run ID (used to resolve the per-run source)
	Workflow         string            // orc workflow name, or "" for default workflow
	Ticket           string            // orc ticket name
	InstanceID       string            // container ID or ECS task ARN
	Metadata         map[string]string // provider-specific metadata from LaunchResult
	DestAuditDir     string            // absolute destination for audit content
	DestArtifactsDir string            // absolute destination for artifacts content
}

// FileNotFoundError is returned when a requested file does not exist in the provider's storage.
type FileNotFoundError struct {
	Path string
	Err  error
}

func (e *FileNotFoundError) Error() string {
	return fmt.Sprintf("file not found: %s", e.Path)
}

func (e *FileNotFoundError) Unwrap() error { return e.Err }
