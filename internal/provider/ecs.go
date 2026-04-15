package provider

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jorge-barreto/horde/internal/config"
)

// ECSClient is the subset of the ECS API used by ECSProvider.
type ECSClient interface {
	RunTask(ctx context.Context, params *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
	DescribeTasks(ctx context.Context, params *ecs.DescribeTasksInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	StopTask(ctx context.Context, params *ecs.StopTaskInput, optFns ...func(*ecs.Options)) (*ecs.StopTaskOutput, error)
}

// CloudWatchLogsClient is the subset of the CloudWatch Logs API used by ECSProvider.
type CloudWatchLogsClient interface {
	GetLogEvents(ctx context.Context, params *cloudwatchlogs.GetLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error)
}

// S3Client is the subset of the S3 API used by ECSProvider.
type S3Client interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// ECSProvider implements Provider using AWS ECS Fargate.
type ECSProvider struct {
	ecs    ECSClient
	logs   CloudWatchLogsClient
	s3     S3Client
	config *config.HordeConfig
}

// NewECSProvider constructs an ECSProvider with the given AWS clients and config.
func NewECSProvider(ecsClient ECSClient, logsClient CloudWatchLogsClient, s3Client S3Client, cfg *config.HordeConfig) *ECSProvider {
	return &ECSProvider{
		ecs:    ecsClient,
		logs:   logsClient,
		s3:     s3Client,
		config: cfg,
	}
}

var _ Provider = (*ECSProvider)(nil)

func (p *ECSProvider) Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error) {
	return nil, fmt.Errorf("ECSProvider.Launch not implemented")
}

func (p *ECSProvider) Status(ctx context.Context, instanceID string) (*InstanceStatus, error) {
	return nil, fmt.Errorf("ECSProvider.Status not implemented")
}

func (p *ECSProvider) Logs(ctx context.Context, instanceID string, follow bool) (io.ReadCloser, error) {
	return nil, fmt.Errorf("ECSProvider.Logs not implemented")
}

func (p *ECSProvider) Stop(ctx context.Context, opts StopOpts) error {
	return fmt.Errorf("ECSProvider.Stop not implemented")
}

func (p *ECSProvider) ReadFile(ctx context.Context, opts ReadFileOpts) ([]byte, error) {
	return nil, fmt.Errorf("ECSProvider.ReadFile not implemented")
}
