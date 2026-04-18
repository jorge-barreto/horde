package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

const DockerImage = "horde-worker:latest"

type dockerInspectState struct {
	Running    bool   `json:"Running"`
	ExitCode   int    `json:"ExitCode"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

type DockerProvider struct {
	Image string // run image tag; defaults to DockerImage
}

func NewDockerProvider() *DockerProvider {
	img := DockerImage
	if v := os.Getenv("HORDE_DOCKER_IMAGE"); v != "" {
		img = v
	}
	return &DockerProvider{Image: img}
}

var _ Provider = (*DockerProvider)(nil)

// Start restarts a stopped container. The entrypoint re-runs on the
// preserved filesystem — it detects the existing workspace and skips clone.
func (p *DockerProvider) Start(ctx context.Context, instanceID string) error {
	cmd := exec.CommandContext(ctx, "docker", "start", instanceID)
	if _, err := cmd.Output(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if strings.Contains(strings.ToLower(stderr), "no such") {
				return fmt.Errorf("starting container: container not found: %s", instanceID)
			}
			return fmt.Errorf("starting container: %s", stderr)
		}
		return fmt.Errorf("starting container: %w", err)
	}
	return nil
}

// logReadCloser wraps a pipe reader and its associated docker logs process.
// Closing kills the process and closes the pipe.
type logReadCloser struct {
	io.ReadCloser
	cmd  *exec.Cmd
	done <-chan struct{} // closed by background goroutine after cmd.Wait() returns
}

func (l *logReadCloser) Close() error {
	if l.cmd.Process != nil {
		// Kill unconditionally. If the process has already exited,
		// os.ErrProcessDone is returned and we ignore it: the goal is
		// to ensure the process is gone before <-l.done. Reading
		// cmd.ProcessState here would race with the Wait goroutine.
		if err := l.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			// Best-effort — the pipe close below is the primary
			// cleanup path and the caller doesn't need this signal.
			_ = err
		}
		<-l.done // wait for background goroutine to reap the child process
	}
	return l.ReadCloser.Close()
}

func (p *DockerProvider) Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error) {
	// Create persistent workspace and sessions directories and prepend them
	// to mounts. Both dirs are per-run and persist across retries so orc can
	// resume the workspace tree AND the agent's session history from
	// /root/.claude (Claude CLI writes session JSON there). If the dirs
	// already exist (retry), MkdirAll is a no-op.
	var allMounts []string
	if opts.HomeDir != "" && opts.RunID != "" {
		wsDir := WorkspacePath(opts.HomeDir, opts.RunID)
		if err := os.MkdirAll(wsDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating workspace directory: %w", err)
		}
		allMounts = append(allMounts, wsDir+":/workspace")

		sessionsDir := SessionsPath(opts.HomeDir, opts.RunID)
		if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating sessions directory: %w", err)
		}
		allMounts = append(allMounts, sessionsDir+":/root/.claude")
	}
	allMounts = append(allMounts, opts.Mounts...)

	args := []string{
		"run", "-d", "--init",
		"-e", "REPO_URL=" + opts.Repo,
		"-e", "TICKET=" + opts.Ticket,
		"-e", "BRANCH=" + opts.Branch,
		"-e", "WORKFLOW=" + opts.Workflow,
		"-e", "RUN_ID=" + opts.RunID,
	}
	if len(opts.OrcArgs) > 0 {
		args = append(args, "-e", "ORC_EXTRA_ARGS="+strings.Join(opts.OrcArgs, " "))
	}
	if opts.EnvFile != "" {
		args = append(args, "--env-file", opts.EnvFile)
	}
	for _, mount := range allMounts {
		args = append(args, "-v", mount)
	}
	args = append(args, p.Image)

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("launching docker container: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("launching docker container: %w", err)
	}

	containerID := strings.TrimSpace(string(out))
	if containerID == "" {
		return nil, fmt.Errorf("launching docker container: empty container ID")
	}
	return &LaunchResult{InstanceID: containerID}, nil
}

func (p *DockerProvider) Status(ctx context.Context, instanceID string) (*InstanceStatus, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .State}}", instanceID)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if strings.Contains(strings.ToLower(string(exitErr.Stderr)), "no such") {
				return &InstanceStatus{State: StateUnknown}, nil
			}
			return nil, fmt.Errorf("inspecting container: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("inspecting container: %w", err)
	}

	var state dockerInspectState
	if err := json.Unmarshal(out, &state); err != nil {
		return nil, fmt.Errorf("parsing container state: %w", err)
	}

	startedAt, err := time.Parse(time.RFC3339Nano, state.StartedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing container start time: %w", err)
	}

	if state.Running {
		return &InstanceStatus{State: StateRunning, StartedAt: startedAt}, nil
	}

	finishedAt, err := time.Parse(time.RFC3339Nano, state.FinishedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing container finish time: %w", err)
	}
	return &InstanceStatus{
		State:      StateStopped,
		ExitCode:   &state.ExitCode,
		StartedAt:  startedAt,
		FinishedAt: &finishedAt,
	}, nil
}

func (p *DockerProvider) copyFromContainer(ctx context.Context, containerID, containerPath, hostPath string) error {
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		return fmt.Errorf("creating destination directory: %w", err)
	}
	cmd := exec.CommandContext(ctx, "docker", "cp", containerID+":"+containerPath, hostPath)
	if _, err := cmd.Output(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("copying from container: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return fmt.Errorf("copying from container: %w", err)
	}
	return nil
}

func (p *DockerProvider) RemoveContainer(ctx context.Context, containerID string) error {
	cmd := exec.CommandContext(ctx, "docker", "rm", containerID)
	if _, err := cmd.Output(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("removing container: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return fmt.Errorf("removing container: %w", err)
	}
	return nil
}

func (p *DockerProvider) Logs(ctx context.Context, instanceID string, follow bool) (io.ReadCloser, error) {
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, instanceID)

	cmd := exec.CommandContext(ctx, "docker", args...)

	if !follow {
		out, err := cmd.CombinedOutput()
		if err != nil {
			outStr := strings.TrimSpace(string(out))
			if strings.Contains(strings.ToLower(outStr), "no such") {
				return nil, fmt.Errorf("reading container logs: container not found: %s", instanceID)
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return nil, fmt.Errorf("reading container logs: %s", outStr)
			}
			return nil, fmt.Errorf("reading container logs: %w", err)
		}
		return io.NopCloser(bytes.NewReader(out)), nil
	}

	// Follow mode: verify container exists before streaming
	checkCmd := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.ID}}", instanceID)
	if _, err := checkCmd.Output(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if strings.Contains(strings.ToLower(string(exitErr.Stderr)), "no such") {
				return nil, fmt.Errorf("reading container logs: container not found: %s", instanceID)
			}
			return nil, fmt.Errorf("reading container logs: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("reading container logs: %w", err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("creating log pipe: %w", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("reading container logs: %w", err)
	}

	done := make(chan struct{})
	go func() {
		cmd.Wait()
		pw.Close()
		close(done)
	}()

	return &logReadCloser{ReadCloser: pr, cmd: cmd, done: done}, nil
}

func (p *DockerProvider) Stop(ctx context.Context, opts StopOpts) error {
	// Stop container directly — no pre-check to avoid TOCTOU race.
	// docker stop succeeds on already-stopped containers and fails with
	// "No such container" for nonexistent ones.
	cmd := exec.CommandContext(ctx, "docker", "stop", opts.InstanceID)
	if _, err := cmd.Output(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if strings.Contains(strings.ToLower(stderr), "no such") {
				return fmt.Errorf("stopping container: container not found: %s", opts.InstanceID)
			}
			return fmt.Errorf("stopping container: %s", stderr)
		}
		return fmt.Errorf("stopping container: %w", err)
	}

	// Best-effort copy of audit and artifacts — errors are intentionally ignored.
	// Container is preserved for retry/shell — not removed here.
	if opts.ResultsDir != "" {
		p.copyFromContainer(ctx, opts.InstanceID, "/workspace/.orc/audit/.", filepath.Join(opts.ResultsDir, "audit"))
		p.copyFromContainer(ctx, opts.InstanceID, "/workspace/.orc/artifacts/.", filepath.Join(opts.ResultsDir, "artifacts"))
	}

	return nil
}

// ReadContainerFile reads a file from a running container via docker exec.
func (p *DockerProvider) ReadContainerFile(ctx context.Context, instanceID, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", instanceID, "cat", path)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ExecInContainer runs a shell command in a container and returns stdout.
func (p *DockerProvider) ExecInContainer(ctx context.Context, instanceID, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", instanceID, "sh", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (p *DockerProvider) ReadFile(ctx context.Context, opts ReadFileOpts) ([]byte, error) {
	relPath, err := validateReadFileOpts(opts)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}
	resultsDir := LocalResultsDir(homeDir, opts.RunID)
	fullPath := filepath.Join(resultsDir, relPath)

	cleanResults := filepath.Clean(resultsDir) + string(filepath.Separator)
	cleanFull := filepath.Clean(fullPath)
	if !strings.HasPrefix(cleanFull, cleanResults) {
		return nil, fmt.Errorf("reading file: path escapes results directory")
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &FileNotFoundError{Path: opts.Path, Err: err}
		}
		return nil, fmt.Errorf("reading file: %w", err)
	}
	return data, nil
}

func mapExitCode(code int) store.Status {
	switch code {
	case 0:
		return store.StatusSuccess
	case 5:
		return store.StatusKilled
	default:
		return store.StatusFailed
	}
}

type dockerRunResult struct {
	TotalCostUSD *float64 `json:"total_cost_usd"`
	ExitCode     *int     `json:"exit_code"`
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if path == src {
				return err // propagate root errors (e.g. nonexistent source)
			}
			return nil // skip inaccessible entries but continue walking
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("creating directory %s: %w", target, err)
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil // skip unreadable source files (container fs may be partial)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("creating parent directory for %s: %w", target, err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", target, err)
		}
		return nil
	})
}

// Finalize checks whether a pending/running Docker container has completed
// or timed out. For completed instances it collects artifacts and populates
// the run's terminal fields (Status, ExitCode, CompletedAt, TotalCostUSD)
// in-place. If the instance is still running (and not timed out), the run
// is left unchanged. The caller is responsible for persisting any changes
// to the store.
func (p *DockerProvider) Finalize(ctx context.Context, run *store.Run, homeDir string) error {
	if run.Status != store.StatusPending && run.Status != store.StatusRunning {
		return nil
	}
	if run.InstanceID == "" {
		return nil
	}

	instanceStatus, err := p.Status(ctx, run.InstanceID)
	if err != nil {
		return fmt.Errorf("checking instance status: %w", err)
	}

	switch instanceStatus.State {
	case StateRunning:
		// Check if orc finished first (marker file on host workspace — container
		// stays alive via sleep infinity). This must come before the timeout
		// check: if orc completed, its exit code is authoritative regardless of
		// whether the timeout window has passed.
		markerPath := filepath.Join(WorkspacePath(homeDir, run.ID), ".horde-exit-code")
		exitData, err := os.ReadFile(markerPath)
		if err == nil {
			exitCode := 1 // default to failure
			if n, _ := fmt.Sscanf(strings.TrimSpace(string(exitData)), "%d", &exitCode); n == 0 {
				fmt.Fprintf(os.Stderr, "warning: could not parse exit code from marker file for run %s, defaulting to 1\n", run.ID)
			}

			resultsDir := LocalResultsDir(homeDir, run.ID)
			if err := p.copyFromContainer(ctx, run.InstanceID, "/workspace/.orc/audit/.", filepath.Join(resultsDir, "audit")); err != nil {
				fmt.Fprintf(os.Stderr, "warning: copying results for run %s: %v\n", run.ID, err)
			}
			if err := p.copyFromContainer(ctx, run.InstanceID, "/workspace/.orc/artifacts/.", filepath.Join(resultsDir, "artifacts")); err != nil {
				fmt.Fprintf(os.Stderr, "warning: copying artifacts for run %s: %v\n", run.ID, err)
			}

			if logs, err := p.Logs(ctx, run.InstanceID, false); err == nil {
				if logData, err := io.ReadAll(logs); err == nil && len(logData) > 0 {
					os.MkdirAll(resultsDir, 0o755)
					os.WriteFile(filepath.Join(resultsDir, "container.log"), logData, 0o644)
				}
				logs.Close()
			}

			resultPath := filepath.Join(resultsDir, AuditRelPath(run.Workflow, run.Ticket, "run-result.json"))
			var cost *float64
			if data, err := os.ReadFile(resultPath); err == nil {
				var rr dockerRunResult
				if json.Unmarshal(data, &rr) == nil {
					cost = rr.TotalCostUSD
				}
			}

			now := time.Now()
			run.Status = mapExitCode(exitCode)
			run.ExitCode = &exitCode
			run.CompletedAt = &now
			run.TotalCostUSD = cost
			return nil
		}

		// Orc still running — check timeout
		if time.Now().After(run.TimeoutAt) {
			resultsDir := LocalResultsDir(homeDir, run.ID)
			if err := p.Stop(ctx, StopOpts{InstanceID: run.InstanceID, ResultsDir: resultsDir}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: stopping timed-out container: %v\n", err)
				return nil
			}

			var cost *float64
			var exitCode *int
			resultPath := filepath.Join(resultsDir, AuditRelPath(run.Workflow, run.Ticket, "run-result.json"))
			if data, err := os.ReadFile(resultPath); err == nil {
				var rr dockerRunResult
				if json.Unmarshal(data, &rr) == nil {
					cost = rr.TotalCostUSD
					exitCode = rr.ExitCode
				}
			}

			now := time.Now()
			run.Status = store.StatusKilled
			run.ExitCode = exitCode
			run.CompletedAt = &now
			run.TotalCostUSD = cost
			return nil
		}

		return nil // orc still running, nothing to do

	case StateStopped:
		resultsDir := LocalResultsDir(homeDir, run.ID)
		if err := p.copyFromContainer(ctx, run.InstanceID, "/workspace/.orc/audit/.", filepath.Join(resultsDir, "audit")); err != nil {
			fmt.Fprintf(os.Stderr, "warning: copying results for run %s: %v\n", run.ID, err)
		}
		if err := p.copyFromContainer(ctx, run.InstanceID, "/workspace/.orc/artifacts/.", filepath.Join(resultsDir, "artifacts")); err != nil {
			fmt.Fprintf(os.Stderr, "warning: copying artifacts for run %s: %v\n", run.ID, err)
		}

		if logs, err := p.Logs(ctx, run.InstanceID, false); err == nil {
			if logData, err := io.ReadAll(logs); err == nil && len(logData) > 0 {
				os.MkdirAll(resultsDir, 0o755)
				os.WriteFile(filepath.Join(resultsDir, "container.log"), logData, 0o644)
			}
			logs.Close()
		}

		resultPath := filepath.Join(resultsDir, AuditRelPath(run.Workflow, run.Ticket, "run-result.json"))
		// Stopped branch reads TotalCostUSD from run-result.json but
		// deliberately ignores rr.ExitCode: for natural completion the
		// authoritative exit code comes from the host .horde-exit-code
		// marker (orc's own exit), with docker inspect as a fallback.
		// The timeout and kill branches, by contrast, use
		// rr.ExitCode because the container was force-stopped and
		// neither the marker nor docker inspect reflects orc's result.
		var cost *float64
		if data, err := os.ReadFile(resultPath); err == nil {
			var rr dockerRunResult
			if json.Unmarshal(data, &rr) == nil {
				cost = rr.TotalCostUSD
			}
		}

		// Check orc's exit marker on host workspace first — docker's exit code
		// reflects how the container process (sleep infinity) died, not orc's result.
		var newStatus store.Status
		var exitCode *int
		markerPath := filepath.Join(WorkspacePath(homeDir, run.ID), ".horde-exit-code")
		if data, err := os.ReadFile(markerPath); err == nil {
			code := 1
			if n, _ := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &code); n == 0 {
				fmt.Fprintf(os.Stderr, "warning: could not parse exit code from marker file for run %s, defaulting to 1\n", run.ID)
			}
			exitCode = &code
			newStatus = mapExitCode(code)
		} else if instanceStatus.ExitCode != nil {
			exitCode = instanceStatus.ExitCode
			newStatus = mapExitCode(*instanceStatus.ExitCode)
		} else {
			newStatus = store.StatusFailed
		}

		now := time.Now()
		run.Status = newStatus
		run.ExitCode = exitCode
		run.CompletedAt = &now
		run.TotalCostUSD = cost

	case StateUnknown:
		var cost *float64
		workspaceDir := WorkspacePath(homeDir, run.ID)
		if _, err := os.Stat(workspaceDir); err == nil {
			fmt.Fprintf(os.Stderr, "warning: container for run %s vanished — workspace preserved at %s\n", run.ID, workspaceDir)
			fmt.Fprintf(os.Stderr, "  use 'horde retry %s' to resume or 'horde shell %s' to inspect\n", run.ID, run.ID)

			resultsDir := LocalResultsDir(homeDir, run.ID)
			auditSrc := filepath.Join(workspaceDir, ".orc", "audit")
			artifactsSrc := filepath.Join(workspaceDir, ".orc", "artifacts")
			if _, err := os.Stat(auditSrc); err == nil {
				if err := copyDir(auditSrc, filepath.Join(resultsDir, "audit")); err != nil {
					fmt.Fprintf(os.Stderr, "warning: copying audit artifacts for run %s: %v\n", run.ID, err)
				}
			}
			if _, err := os.Stat(artifactsSrc); err == nil {
				if err := copyDir(artifactsSrc, filepath.Join(resultsDir, "artifacts")); err != nil {
					fmt.Fprintf(os.Stderr, "warning: copying artifacts for run %s: %v\n", run.ID, err)
				}
			}

			resultPath := filepath.Join(workspaceDir, ".orc", AuditRelPath(run.Workflow, run.Ticket, "run-result.json"))
			if data, err := os.ReadFile(resultPath); err == nil {
				var rr dockerRunResult
				if json.Unmarshal(data, &rr) == nil {
					cost = rr.TotalCostUSD
				}
			}
		}

		// Check orc's exit marker on host workspace — if orc completed before the
		// container vanished, the marker has the authoritative exit code.
		var newStatus store.Status
		var exitCode *int
		markerPath := filepath.Join(WorkspacePath(homeDir, run.ID), ".horde-exit-code")
		if data, err := os.ReadFile(markerPath); err == nil {
			code := 1
			if n, _ := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &code); n == 0 {
				fmt.Fprintf(os.Stderr, "warning: could not parse exit code from marker file for run %s, defaulting to 1\n", run.ID)
			}
			exitCode = &code
			newStatus = mapExitCode(code)
		} else {
			newStatus = store.StatusFailed
		}

		now := time.Now()
		run.Status = newStatus
		run.ExitCode = exitCode
		run.CompletedAt = &now
		run.TotalCostUSD = cost
	}

	return nil
}

// HydrateRun copies a run's audit and artifacts trees from the local results
// store to the caller-supplied destination dirs. The source is rooted at
// ~/.horde/results/<run-id>/{audit,artifacts}/[<workflow>/]<ticket>/ —
// i.e. the specific subtree orc wrote for this run — so that DestAuditDir
// ends up containing the run's audit files directly, without orc's native
// <workflow>/<ticket>/ nesting.
//
// Returns *FileNotFoundError if the run's results dir does not exist.
// A missing audit or artifacts subtree individually is treated as empty
// (some runs don't produce both).
func (p *DockerProvider) HydrateRun(ctx context.Context, opts HydrateOpts) error {
	if err := ValidateRunID(opts.RunID); err != nil {
		return fmt.Errorf("hydrating run: %w", err)
	}
	if opts.Ticket == "" {
		return fmt.Errorf("hydrating run: ticket is required")
	}
	if strings.ContainsAny(opts.Ticket, "/\\") || strings.Contains(opts.Ticket, "..") {
		return fmt.Errorf("hydrating run: invalid ticket")
	}
	if strings.ContainsAny(opts.Workflow, "/\\") || strings.Contains(opts.Workflow, "..") {
		return fmt.Errorf("hydrating run: invalid workflow")
	}
	if opts.DestAuditDir == "" || opts.DestArtifactsDir == "" {
		return fmt.Errorf("hydrating run: destination directories are required")
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("hydrating run: %w", err)
	}
	resultsDir := LocalResultsDir(homeDir, opts.RunID)
	if _, err := os.Stat(resultsDir); err != nil {
		if os.IsNotExist(err) {
			return &FileNotFoundError{Path: resultsDir, Err: err}
		}
		return fmt.Errorf("hydrating run: %w", err)
	}

	srcAudit := orcSubdir(filepath.Join(resultsDir, "audit"), opts.Workflow, opts.Ticket)
	if _, err := os.Stat(srcAudit); err == nil {
		if err := copyLocalTree(srcAudit, opts.DestAuditDir); err != nil {
			return fmt.Errorf("hydrating audit: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("hydrating audit: %w", err)
	}

	srcArtifacts := orcSubdir(filepath.Join(resultsDir, "artifacts"), opts.Workflow, opts.Ticket)
	if _, err := os.Stat(srcArtifacts); err == nil {
		if err := copyLocalTree(srcArtifacts, opts.DestArtifactsDir); err != nil {
			return fmt.Errorf("hydrating artifacts: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("hydrating artifacts: %w", err)
	}

	if opts.DestConfigDir != "" {
		workspaceOrc := filepath.Join(WorkspacePath(homeDir, opts.RunID), ".orc")
		if err := copyOrcConfig(workspaceOrc, opts.DestConfigDir); err != nil {
			return fmt.Errorf("hydrating config: %w", err)
		}
	}

	return nil
}

// copyOrcConfig copies every entry under src (the run's workspace .orc/ dir)
// into dst, except the reserved per-run dirs "audit" and "artifacts". A
// missing src is not an error — the run's workspace may have been purged.
func copyOrcConfig(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", src, err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "audit" || name == "artifacts" {
			continue
		}
		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", srcPath, err)
		}
		if info.IsDir() {
			if err := copyLocalTree(srcPath, dstPath); err != nil {
				return fmt.Errorf("copying %s: %w", srcPath, err)
			}
			continue
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dst, err)
		}
		if err := copyRegularFile(srcPath, dstPath, info.Mode().Perm()); err != nil {
			return fmt.Errorf("copying %s: %w", srcPath, err)
		}
	}
	return nil
}

// orcSubdir appends orc's <workflow>/<ticket> layout (or just <ticket> for
// the default workflow) to a base directory.
func orcSubdir(base, workflow, ticket string) string {
	if workflow == "" {
		return filepath.Join(base, ticket)
	}
	return filepath.Join(base, workflow, ticket)
}
