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
		l.cmd.Process.Kill()
		<-l.done // wait for background goroutine to reap the child process
	}
	return l.ReadCloser.Close()
}

func (p *DockerProvider) Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error) {
	// Create persistent workspace directory and prepend it to mounts.
	// If the directory already exists (retry), this is a no-op.
	var allMounts []string
	if opts.HomeDir != "" && opts.RunID != "" {
		wsDir := WorkspacePath(opts.HomeDir, opts.RunID)
		if err := os.MkdirAll(wsDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating workspace directory: %w", err)
		}
		allMounts = append(allMounts, wsDir+":/workspace")
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
				return &InstanceStatus{State: "unknown"}, nil
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
		return &InstanceStatus{State: "running", StartedAt: startedAt}, nil
	}

	finishedAt, err := time.Parse(time.RFC3339Nano, state.FinishedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing container finish time: %w", err)
	}
	return &InstanceStatus{
		State:      "stopped",
		ExitCode:   &state.ExitCode,
		StartedAt:  startedAt,
		FinishedAt: &finishedAt,
	}, nil
}

func (p *DockerProvider) CopyFromContainer(ctx context.Context, containerID, containerPath, hostPath string) error {
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
				return nil, fmt.Errorf("container not found: %s", instanceID)
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
				return nil, fmt.Errorf("container not found: %s", instanceID)
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
		p.CopyFromContainer(ctx, opts.InstanceID, "/workspace/.orc/audit/.", filepath.Join(opts.ResultsDir, "audit"))
		p.CopyFromContainer(ctx, opts.InstanceID, "/workspace/.orc/artifacts/.", filepath.Join(opts.ResultsDir, "artifacts"))
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
	if opts.RunID == "" {
		return nil, fmt.Errorf("reading file: run ID is required")
	}
	if strings.ContainsAny(opts.RunID, "/\\") || strings.Contains(opts.RunID, "..") {
		return nil, fmt.Errorf("reading file: invalid run ID")
	}
	if opts.Path == "" {
		return nil, fmt.Errorf("reading file: path is required")
	}

	const orcPrefix = ".orc/"
	if !strings.HasPrefix(opts.Path, orcPrefix) {
		return nil, fmt.Errorf("reading file: path must start with %q", orcPrefix)
	}
	relPath := strings.TrimPrefix(opts.Path, orcPrefix)
	if relPath == "" {
		return nil, fmt.Errorf("reading file: path must include a filename after %q", orcPrefix)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}
	resultsDir := filepath.Join(homeDir, ".horde", "results", opts.RunID)
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
