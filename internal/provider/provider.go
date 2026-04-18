package provider

import (
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

// Provider abstracts container/task lifecycle operations.
//
// Status contract: when the instance cannot be located (not-found by
// the underlying backend), Status must return (&InstanceStatus{State:
// "unknown"}, nil). Callers rely on the "unknown" sentinel to branch
// between "still running" and "gone" paths without a provider-specific
// error type switch. Real transport/API errors should still be
// returned as (nil, error).
type Provider interface {
	Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error)
	Status(ctx context.Context, instanceID string) (*InstanceStatus, error)
	Logs(ctx context.Context, instanceID string, follow bool) (io.ReadCloser, error)
	Stop(ctx context.Context, opts StopOpts) error
	ReadFile(ctx context.Context, opts ReadFileOpts) ([]byte, error)
	HydrateRun(ctx context.Context, opts HydrateOpts) error
	// Finalize checks whether a pending/running instance has completed or
	// timed out. For completed instances, it collects artifacts and populates
	// the run's terminal fields (Status, ExitCode, CompletedAt, TotalCostUSD)
	// in-place. If the instance is still running (and not timed out), the run
	// is left unchanged. Returns nil on success or when no finalization was needed.
	Finalize(ctx context.Context, run *store.Run, homeDir string) error
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

// ValidateRunID rejects empty run IDs and IDs that would enable path
// traversal when interpolated into filesystem paths or S3 keys. Providers
// must call this before using a RunID to build any storage path.
func ValidateRunID(id string) error {
	if id == "" {
		return fmt.Errorf("run ID is required")
	}
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return fmt.Errorf("invalid run ID")
	}
	return nil
}

// orcPathPrefix is the single allowed logical prefix for ReadFile paths.
// All readable files live under the .orc/ tree rooted at the workspace.
const orcPathPrefix = ".orc/"

// validateReadFileOpts centralizes the input validation shared between
// Docker and ECS ReadFile implementations: RunID sanity, Path
// non-emptiness, required .orc/ prefix, non-empty filename, and
// logical-path traversal check. Returns the cleaned relative path
// (stripped of the .orc/ prefix) on success. Callers still wrap any
// returned error with their own action verb ("reading file: ...").
func validateReadFileOpts(opts ReadFileOpts) (relPath string, err error) {
	if err := ValidateRunID(opts.RunID); err != nil {
		return "", err
	}
	if opts.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(opts.Path, orcPathPrefix) {
		return "", fmt.Errorf("path must start with %q", orcPathPrefix)
	}
	rel := strings.TrimPrefix(opts.Path, orcPathPrefix)
	if rel == "" {
		return "", fmt.Errorf("path must include a filename after %q", orcPathPrefix)
	}
	// Logical-path traversal check: forward-slash path.Clean is the
	// right call for both flat S3 keys and filesystem sub-paths.
	cleaned := path.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned == "." || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("path escapes %q prefix", orcPathPrefix)
	}
	return cleaned, nil
}

// WorkspacePath returns the host path for a run's persistent workspace.
func WorkspacePath(homeDir, runID string) string {
	return filepath.Join(homeDir, ".horde", "workspaces", runID)
}

// LocalResultsDir returns the host path where a run's results (audit,
// artifacts, container.log) are staged on the local filesystem. Both the
// provider and CLI call this to avoid drift in the results layout.
func LocalResultsDir(homeDir, runID string) string {
	return filepath.Join(homeDir, ".horde", "results", runID)
}

// AuditRelPath builds the audit-tree path suffix for a per-run file:
// "audit/[workflow/]ticket/<filename>". Callers prefix this with the
// appropriate base (host results dir, .orc/ logical path, or the
// container's /workspace/.orc path) using filepath.Join or path.Join.
func AuditRelPath(workflow, ticket, filename string) string {
	parts := []string{"audit"}
	if workflow != "" {
		parts = append(parts, workflow)
	}
	parts = append(parts, ticket, filename)
	return filepath.Join(parts...)
}

// SessionsPath returns the host path for a run's persistent agent session
// state (bind-mounted to /home/horde/.claude inside the container). Keeping it
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
// Instance state sentinels. Providers must use these strings so callers can
// switch on State without a provider-specific type switch.
const (
	StateRunning = "running"
	StateStopped = "stopped"
	StateUnknown = "unknown"
)

type InstanceStatus struct {
	State      string // see StateRunning/StateStopped/StateUnknown
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
// trees — and the project's orc configuration — to local destination
// directories.
//
// Providers locate the source tree using RunID plus the orc-native
// (Workflow, Ticket) path components. Workflow may be empty — in which
// case the source path omits the workflow segment. DestAuditDir and
// DestArtifactsDir are the final on-disk destinations for the bytes
// under the run's <workflow>/<ticket>/ subtree (caller-assembled).
//
// DestConfigDir, if set, is where the provider copies the project's
// orc configuration surface — i.e. everything under the run's .orc/
// directory except audit/ and artifacts/ (those two are per-run and
// reserved by orc). An empty DestConfigDir skips config hydration.
// A missing config source is NOT an error; it degrades gracefully.
// This lets orc improve / orc doctor find config.yaml, phases/,
// scripts/, workflows/, prompts/, etc. alongside the per-run data.
//
// If the per-run source data does not exist, implementations return
// *FileNotFoundError with Path set to a description of what was missing.
type HydrateOpts struct {
	RunID            string            // run ID (used to resolve the per-run source)
	Workflow         string            // orc workflow name, or "" for default workflow
	Ticket           string            // orc ticket name
	InstanceID       string            // container ID or ECS task ARN
	Metadata         map[string]string // provider-specific metadata from LaunchResult
	DestAuditDir     string            // absolute destination for audit content
	DestArtifactsDir string            // absolute destination for artifacts content
	DestConfigDir    string            // absolute destination for orc config surface; "" skips
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
