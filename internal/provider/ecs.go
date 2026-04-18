package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/jorge-barreto/horde/internal/config"
	"github.com/jorge-barreto/horde/internal/store"
)

// containerName is the container name in the ECS task definition.
// Must match the name set by the @horde/cdk construct.
const containerName = "horde-worker"

// maxConsecutiveDescribeFailures is the number of consecutive DescribeTasks
// errors tolerated in follow mode before stopping the log poll loop.
const maxConsecutiveDescribeFailures = 5

// writeLogEvents writes CloudWatch log events to w, skipping events
// with a nil Message pointer and appending a trailing newline when
// absent. Returns the first write error encountered so follow-mode
// callers can stop the poll loop.
func writeLogEvents(w io.Writer, events []cwltypes.OutputLogEvent) error {
	for _, event := range events {
		if event.Message == nil {
			continue
		}
		msg := *event.Message
		if !strings.HasSuffix(msg, "\n") {
			msg += "\n"
		}
		if _, err := io.WriteString(w, msg); err != nil {
			return err
		}
	}
	return nil
}

// assignPublicIp maps a HordeConfig.AssignPublicIp string to the AWS enum.
// Empty string defaults to ENABLED for backward-compatible public-subnet
// topology; "DISABLED" is required for private-subnet deployments.
func assignPublicIp(v string) ecstypes.AssignPublicIp {
	if v == "DISABLED" {
		return ecstypes.AssignPublicIpDisabled
	}
	return ecstypes.AssignPublicIpEnabled
}

// ecsFailureReason renders an ECS Failure struct as a human-readable
// error fragment. Empty Reason defaults to "unknown failure". When
// present, Arn and Detail are appended for debuggability.
func ecsFailureReason(f ecstypes.Failure) string {
	reason := "unknown failure"
	if f.Reason != nil && *f.Reason != "" {
		reason = *f.Reason
	}
	var extras []string
	if f.Arn != nil && *f.Arn != "" {
		extras = append(extras, "arn="+*f.Arn)
	}
	if f.Detail != nil && *f.Detail != "" {
		extras = append(extras, "detail="+*f.Detail)
	}
	if len(extras) > 0 {
		reason += " (" + strings.Join(extras, ", ") + ")"
	}
	return reason
}

// drainTimeout bounds the final-drain phase after STOPPED detection.
// The drain uses an independent context (not the follow context) so that
// Ctrl-C while following does not abort drain and lose final output.
const drainTimeout = 30 * time.Second

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
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
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
				AssignPublicIp: assignPublicIp(p.config.AssignPublicIp),
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
		return nil, fmt.Errorf("launching ECS task: %s", ecsFailureReason(out.Failures[0]))
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
			"cluster_arn":       p.config.ClusterARN,
			"log_group":         p.config.LogGroup,
			"log_stream_prefix": p.config.LogStreamPrefix,
			"artifacts_bucket":  p.config.ArtifactsBucket,
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
		// "MISSING" is AWS's per-task signal for not-found. Treat it
		// like the Docker provider's "no such container" — return the
		// unknown sentinel rather than an error so callers don't need
		// provider-specific switches.
		if f := out.Failures[0]; f.Reason != nil && strings.EqualFold(*f.Reason, "MISSING") {
			return &InstanceStatus{State: StateUnknown}, nil
		}
		return nil, fmt.Errorf("describing ECS task: %s", ecsFailureReason(out.Failures[0]))
	}

	if len(out.Tasks) == 0 {
		return &InstanceStatus{State: StateUnknown}, nil
	}

	task := out.Tasks[0]

	state := StateUnknown
	if task.LastStatus != nil {
		switch *task.LastStatus {
		case "PROVISIONING", "PENDING", "ACTIVATING":
			state = "pending"
		case "RUNNING":
			state = StateRunning
		case "DEPROVISIONING", "STOPPING":
			state = "stopping"
		case "STOPPED":
			state = StateStopped
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
			if out == nil {
				return nil, fmt.Errorf("reading logs: nil response")
			}

			if err := writeLogEvents(&buf, out.Events); err != nil {
				return nil, fmt.Errorf("reading logs: %w", err)
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
		var describeFailures int
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
				var rnf *cwltypes.ResourceNotFoundException
				if !errors.As(err, &rnf) {
					pw.CloseWithError(fmt.Errorf("reading logs: %w", err))
					return
				}
				// ResourceNotFoundException: log stream not created yet — container
				// hasn't started writing output. Fall through to DescribeTasks check
				// so we detect task termination even when no log stream is ever created.
			} else if out == nil {
				pw.CloseWithError(fmt.Errorf("reading logs: nil response"))
				return
			} else {
				if err := writeLogEvents(pw, out.Events); err != nil {
					return
				}

				if out.NextForwardToken != nil {
					nextToken = out.NextForwardToken
				}
			}

			// Check if task has stopped.
			taskOut, err := p.ecs.DescribeTasks(followCtx, &ecs.DescribeTasksInput{
				Cluster: aws.String(p.config.ClusterARN),
				Tasks:   []string{instanceID},
			})
			if err != nil {
				describeFailures++
				if describeFailures >= maxConsecutiveDescribeFailures {
					fmt.Fprintf(pw, "WARNING: unable to determine task completion after %d consecutive failures, stopping follow\n", describeFailures)
					return
				}
			} else {
				describeFailures = 0
				if taskOut != nil && len(taskOut.Failures) > 0 {
					fmt.Fprintf(pw, "WARNING: task no longer available: %s\n", ecsFailureReason(taskOut.Failures[0]))
					return
				}
				if taskOut != nil && len(taskOut.Tasks) > 0 {
					if taskOut.Tasks[0].LastStatus != nil && *taskOut.Tasks[0].LastStatus == "STOPPED" {
						// Final drain: CloudWatch ingestion can lag task
						// termination by several seconds. Fetch remaining
						// events until NextForwardToken stops changing.
						// Use an independent, bounded context so Ctrl-C
						// during follow does not abort drain and lose
						// final output.
						drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
						defer drainCancel()
						for {
							drainInput := &cloudwatchlogs.GetLogEventsInput{
								LogGroupName:  aws.String(p.config.LogGroup),
								LogStreamName: aws.String(logStream),
								StartFromHead: aws.Bool(true),
							}
							if nextToken != nil {
								drainInput.NextToken = nextToken
							}
							drainOut, drainErr := p.logs.GetLogEvents(drainCtx, drainInput)
							if drainErr != nil {
								var rnf *cwltypes.ResourceNotFoundException
								if errors.As(drainErr, &rnf) {
									return // stream gone — nothing more to drain
								}
								// Transient error — retry once after a short backoff.
								select {
								case <-drainCtx.Done():
									return
								case <-time.After(interval):
								}
								drainOut, drainErr = p.logs.GetLogEvents(drainCtx, drainInput)
								if drainErr != nil {
									fmt.Fprintf(pw, "WARNING: output may be incomplete: %v\n", drainErr)
									pw.CloseWithError(fmt.Errorf("reading logs: %w", drainErr))
									return
								}
							}
							if drainOut == nil {
								return
							}
							if err := writeLogEvents(pw, drainOut.Events); err != nil {
								return
							}
							if drainOut.NextForwardToken == nil || (nextToken != nil && *drainOut.NextForwardToken == *nextToken) {
								return
							}
							nextToken = drainOut.NextForwardToken
						}
					}
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
	relPath, err := validateReadFileOpts(opts)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
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

// HydrateRun downloads the run's audit and artifacts trees from
// s3://<artifacts_bucket>/horde-runs/<run-id>/{audit,artifacts}/[<workflow>/]<ticket>/
// into the caller-supplied destination directories. Returns *FileNotFoundError
// if no objects are found under either prefix for this run.
func (p *ECSProvider) HydrateRun(ctx context.Context, opts HydrateOpts) error {
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
	bucket := ""
	if opts.Metadata != nil {
		bucket = opts.Metadata["artifacts_bucket"]
	}
	if bucket == "" {
		return fmt.Errorf("hydrating run: artifacts_bucket not found in metadata")
	}

	runPrefix := "horde-runs/" + opts.RunID + "/"
	auditPrefix := runPrefix + "audit/" + orcKeySuffix(opts.Workflow, opts.Ticket)
	artifactsPrefix := runPrefix + "artifacts/" + orcKeySuffix(opts.Workflow, opts.Ticket)

	audit, err := p.downloadS3Prefix(ctx, bucket, auditPrefix, opts.DestAuditDir)
	if err != nil {
		return fmt.Errorf("hydrating audit: %w", err)
	}
	artifacts, err := p.downloadS3Prefix(ctx, bucket, artifactsPrefix, opts.DestArtifactsDir)
	if err != nil {
		return fmt.Errorf("hydrating artifacts: %w", err)
	}
	if audit == 0 && artifacts == 0 {
		return &FileNotFoundError{Path: "s3://" + bucket + "/" + runPrefix}
	}

	// DestConfigDir is intentionally unhandled for ECS: the Fargate task is
	// already gone by the time hydrate runs, so the run's .orc/ config surface
	// is not retrievable live. To support this, the entrypoint must upload
	// .orc/ (minus audit/ and artifacts/) to s3://<bucket>/horde-runs/<run-id>/config/
	// at launch time, and this method would download that prefix to DestConfigDir.
	// Tracked separately — Docker parity only for now.

	return nil
}

// orcKeySuffix returns the "<workflow>/<ticket>/" (or "<ticket>/") suffix
// that orc uses for per-run audit/artifact paths.
func orcKeySuffix(workflow, ticket string) string {
	if workflow == "" {
		return ticket + "/"
	}
	return workflow + "/" + ticket + "/"
}

// downloadS3Prefix lists all objects under prefix and writes each to
// destDir, preserving the path layout under the prefix. Returns the number
// of objects downloaded. A prefix with no objects is not an error here —
// the caller decides how to treat an empty result.
func (p *ECSProvider) downloadS3Prefix(ctx context.Context, bucket, prefix, destDir string) (int, error) {
	var token *string
	count := 0
	for {
		out, err := p.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return count, fmt.Errorf("listing s3 prefix %q: %w", prefix, err)
		}
		for _, obj := range out.Contents {
			key := aws.ToString(obj.Key)
			rel := strings.TrimPrefix(key, prefix)
			if rel == "" || strings.HasSuffix(rel, "/") {
				continue
			}
			if err := p.downloadS3Object(ctx, bucket, key, filepath.Join(destDir, rel)); err != nil {
				return count, err
			}
			count++
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return count, nil
}

func (p *ECSProvider) downloadS3Object(ctx context.Context, bucket, key, destPath string) error {
	out, err := p.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("getting s3://%s/%s: %w", bucket, key, err)
	}
	if out == nil || out.Body == nil {
		return fmt.Errorf("getting s3://%s/%s: nil response", bucket, key)
	}
	defer out.Body.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("creating parent dir for %s: %w", destPath, err)
	}
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", destPath, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, out.Body); err != nil {
		return fmt.Errorf("writing %s: %w", destPath, err)
	}
	return nil
}

// Finalize is a no-op for ECS. The status Lambda handles finalization
// (updating DynamoDB on task completion via EventBridge).
func (p *ECSProvider) Finalize(ctx context.Context, run *store.Run, homeDir string) error {
	return nil
}
