package provider

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jorge-barreto/horde/internal/config"
)

// containerName is the container name in the ECS task definition.
// Must match the name set by the @horde/cdk construct.
const containerName = "horde-worker"

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
	env := []ecstypes.KeyValuePair{
		{Name: aws.String("REPO_URL"), Value: aws.String(opts.Repo)},
		{Name: aws.String("TICKET"), Value: aws.String(opts.Ticket)},
		{Name: aws.String("BRANCH"), Value: aws.String(opts.Branch)},
		{Name: aws.String("WORKFLOW"), Value: aws.String(opts.Workflow)},
		{Name: aws.String("RUN_ID"), Value: aws.String(opts.RunID)},
		{Name: aws.String("ARTIFACTS_BUCKET"), Value: aws.String(p.config.ArtifactsBucket)},
	}

	input := &ecs.RunTaskInput{
		TaskDefinition: aws.String(p.config.TaskDefinitionARN),
		Cluster:        aws.String(p.config.ClusterARN),
		LaunchType:     ecstypes.LaunchTypeFargate,
		Count:          aws.Int32(1),
		NetworkConfiguration: &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
				Subnets:        p.config.Subnets,
				SecurityGroups: []string{p.config.SecurityGroup},
				AssignPublicIp: ecstypes.AssignPublicIpEnabled,
			},
		},
		Overrides: &ecstypes.TaskOverride{
			ContainerOverrides: []ecstypes.ContainerOverride{
				{
					Name:        aws.String(containerName),
					Environment: env,
				},
			},
		},
		Tags: []ecstypes.Tag{
			{Key: aws.String("horde-run-id"), Value: aws.String(opts.RunID)},
			{Key: aws.String("horde-ticket"), Value: aws.String(opts.Ticket)},
		},
	}

	out, err := p.ecs.RunTask(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("launching ECS task: %w", err)
	}
	if out == nil {
		return nil, fmt.Errorf("launching ECS task: nil response")
	}

	if len(out.Failures) > 0 {
		f := out.Failures[0]
		reason := ""
		if f.Reason != nil {
			reason = *f.Reason
		}
		return nil, fmt.Errorf("launching ECS task: %s", reason)
	}

	if len(out.Tasks) == 0 {
		return nil, fmt.Errorf("launching ECS task: no task returned")
	}

	task := out.Tasks[0]
	if task.TaskArn == nil {
		return nil, fmt.Errorf("launching ECS task: task ARN is nil")
	}

	return &LaunchResult{
		InstanceID: *task.TaskArn,
		Metadata: map[string]string{
			"cluster_arn":      p.config.ClusterARN,
			"log_group":        p.config.LogGroup,
			"artifacts_bucket": p.config.ArtifactsBucket,
		},
	}, nil
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
