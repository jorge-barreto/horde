package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
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
	ecs          ECSClient
	logs         CloudWatchLogsClient
	s3           S3Client
	config       *config.HordeConfig
	pollInterval time.Duration
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

// ecsLogFollower wraps a pipe reader for CloudWatch follow mode.
// Closing cancels the polling goroutine and waits for it to exit.
type ecsLogFollower struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   <-chan struct{}
}

func (f *ecsLogFollower) Close() error {
	f.cancel()
	err := f.PipeReader.Close()
	<-f.done
	return err
}

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
	out, err := p.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(p.config.ClusterARN),
		Tasks:   []string{instanceID},
	})
	if err != nil {
		return nil, fmt.Errorf("describing ECS task: %w", err)
	}
	if out == nil {
		return nil, fmt.Errorf("describing ECS task: nil response")
	}

	if len(out.Failures) > 0 {
		f := out.Failures[0]
		reason := ""
		if f.Reason != nil {
			reason = *f.Reason
		}
		return nil, fmt.Errorf("describing ECS task: %s", reason)
	}

	if len(out.Tasks) == 0 {
		return nil, fmt.Errorf("describing ECS task: task not found")
	}

	task := out.Tasks[0]

	state := "unknown"
	if task.LastStatus != nil {
		switch *task.LastStatus {
		case "RUNNING":
			state = "running"
		case "STOPPED":
			state = "stopped"
		}
	}

	status := &InstanceStatus{
		State: state,
	}

	if task.StartedAt != nil {
		status.StartedAt = *task.StartedAt
	}

	if task.StoppedAt != nil {
		status.FinishedAt = task.StoppedAt
	}

	if len(task.Containers) > 0 && task.Containers[0].ExitCode != nil {
		exitCode := int(*task.Containers[0].ExitCode)
		status.ExitCode = &exitCode
	}

	return status, nil
}

func (p *ECSProvider) Logs(ctx context.Context, instanceID string, follow bool) (io.ReadCloser, error) {
	taskID := instanceID
	if i := strings.LastIndex(instanceID, "/"); i >= 0 {
		taskID = instanceID[i+1:]
	}
	if taskID == "" {
		return nil, fmt.Errorf("reading logs: empty task ID from instance %q", instanceID)
	}

	logStream := p.config.LogStreamPrefix + "/" + containerName + "/" + taskID

	if !follow {
		var buf bytes.Buffer
		var nextToken *string
		for {
			input := &cloudwatchlogs.GetLogEventsInput{
				LogGroupName:  aws.String(p.config.LogGroup),
				LogStreamName: aws.String(logStream),
				StartFromHead: aws.Bool(true),
			}
			if nextToken != nil {
				input.NextToken = nextToken
			}

			out, err := p.logs.GetLogEvents(ctx, input)
			if err != nil {
				return nil, fmt.Errorf("reading logs: %w", err)
			}

			for _, event := range out.Events {
				if event.Message != nil {
					buf.WriteString(*event.Message)
					if !strings.HasSuffix(*event.Message, "\n") {
						buf.WriteByte('\n')
					}
				}
			}

			if out.NextForwardToken == nil || (nextToken != nil && *out.NextForwardToken == *nextToken) {
				break
			}
			nextToken = out.NextForwardToken
		}
		return io.NopCloser(&buf), nil
	}

	pr, pw := io.Pipe()
	followCtx, cancel := context.WithCancel(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close()

		interval := p.pollInterval
		if interval == 0 {
			interval = time.Second
		}

		var nextToken *string
		for {
			input := &cloudwatchlogs.GetLogEventsInput{
				LogGroupName:  aws.String(p.config.LogGroup),
				LogStreamName: aws.String(logStream),
				StartFromHead: aws.Bool(true),
			}
			if nextToken != nil {
				input.NextToken = nextToken
			}

			out, err := p.logs.GetLogEvents(followCtx, input)
			if err != nil {
				pw.CloseWithError(fmt.Errorf("reading logs: %w", err))
				return
			}

			for _, event := range out.Events {
				if event.Message != nil {
					msg := *event.Message
					if !strings.HasSuffix(msg, "\n") {
						msg += "\n"
					}
					if _, err := io.WriteString(pw, msg); err != nil {
						return
					}
				}
			}

			if out.NextForwardToken != nil {
				nextToken = out.NextForwardToken
			}

			// Check if task has stopped.
			taskOut, err := p.ecs.DescribeTasks(followCtx, &ecs.DescribeTasksInput{
				Cluster: aws.String(p.config.ClusterARN),
				Tasks:   []string{instanceID},
			})
			if err == nil && taskOut != nil && len(taskOut.Tasks) > 0 {
				if taskOut.Tasks[0].LastStatus != nil && *taskOut.Tasks[0].LastStatus == "STOPPED" {
					return
				}
			}

			select {
			case <-followCtx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()

	return &ecsLogFollower{PipeReader: pr, cancel: cancel, done: done}, nil
}

func (p *ECSProvider) Stop(ctx context.Context, opts StopOpts) error {
	_, err := p.ecs.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: aws.String(p.config.ClusterARN),
		Task:    aws.String(opts.InstanceID),
		Reason:  aws.String("horde kill"),
	})
	if err != nil {
		// StopTask returns InvalidParameterException when the task is already stopped.
		// Treat as success for idempotency — the desired state (task stopped) is achieved.
		var ipe *ecstypes.InvalidParameterException
		if errors.As(err, &ipe) && strings.Contains(strings.ToLower(ipe.ErrorMessage()), "already stopped") {
			return nil
		}
		return fmt.Errorf("stopping ECS task: %w", err)
	}
	return nil
}

func (p *ECSProvider) ReadFile(ctx context.Context, opts ReadFileOpts) ([]byte, error) {
	if opts.RunID == "" {
		return nil, fmt.Errorf("reading file: run ID is required")
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

	bucket := ""
	if opts.Metadata != nil {
		bucket = opts.Metadata["artifacts_bucket"]
	}
	if bucket == "" {
		return nil, fmt.Errorf("reading file: artifacts_bucket not found in metadata")
	}

	key := "horde-runs/" + opts.RunID + "/" + relPath

	out, err := p.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var noSuchKey *s3types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return nil, &FileNotFoundError{Path: opts.Path, Err: err}
		}
		return nil, fmt.Errorf("reading file from s3: %w", err)
	}
	if out == nil || out.Body == nil {
		return nil, fmt.Errorf("reading file from s3: nil response")
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("reading file from s3: %w", err)
	}
	return data, nil
}
