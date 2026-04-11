package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const dockerImage = "horde-worker:latest"

type dockerInspectState struct {
	Running    bool   `json:"Running"`
	ExitCode   int    `json:"ExitCode"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

type DockerProvider struct{}

func NewDockerProvider() *DockerProvider {
	return &DockerProvider{}
}

var _ Provider = (*DockerProvider)(nil)

func (p *DockerProvider) Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error) {
	args := []string{
		"run", "-d",
		"-e", "REPO_URL=" + opts.Repo,
		"-e", "TICKET=" + opts.Ticket,
		"-e", "BRANCH=" + opts.Branch,
		"-e", "WORKFLOW=" + opts.Workflow,
		"-e", "RUN_ID=" + opts.RunID,
	}
	if opts.EnvFile != "" {
		args = append(args, "--env-file", opts.EnvFile)
	}
	args = append(args, dockerImage)

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
			if strings.Contains(string(exitErr.Stderr), "No such") {
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

	startedAt, _ := time.Parse(time.RFC3339Nano, state.StartedAt)

	if state.Running {
		return &InstanceStatus{State: "running", StartedAt: startedAt}, nil
	}

	finishedAt, _ := time.Parse(time.RFC3339Nano, state.FinishedAt)
	return &InstanceStatus{
		State:      "stopped",
		ExitCode:   &state.ExitCode,
		StartedAt:  startedAt,
		FinishedAt: &finishedAt,
	}, nil
}

func (p *DockerProvider) Logs(ctx context.Context, instanceID string, follow bool) (io.ReadCloser, error) {
	return nil, fmt.Errorf("docker provider: Logs not implemented")
}

func (p *DockerProvider) Kill(ctx context.Context, instanceID string) error {
	return fmt.Errorf("docker provider: Kill not implemented")
}

func (p *DockerProvider) ReadFile(ctx context.Context, opts ReadFileOpts) ([]byte, error) {
	return nil, fmt.Errorf("docker provider: ReadFile not implemented")
}
