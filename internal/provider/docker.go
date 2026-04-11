package provider

import (
	"context"
	"fmt"
	"io"
)

const dockerImage = "horde-worker:latest"

type DockerProvider struct{}

func NewDockerProvider() *DockerProvider {
	return &DockerProvider{}
}

var _ Provider = (*DockerProvider)(nil)

func (p *DockerProvider) Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error) {
	return nil, fmt.Errorf("docker provider: Launch not implemented")
}

func (p *DockerProvider) Status(ctx context.Context, instanceID string) (*InstanceStatus, error) {
	return nil, fmt.Errorf("docker provider: Status not implemented")
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
