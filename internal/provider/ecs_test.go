package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cloudwatchlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/jorge-barreto/horde/internal/config"
)

type fakeECSClient struct {
	runTaskInput  *ecs.RunTaskInput
	runTaskOutput *ecs.RunTaskOutput
	runTaskErr    error

	describeTasksInput   *ecs.DescribeTasksInput
	describeTasksOutput  *ecs.DescribeTasksOutput
	describeTasksErr     error
	describeTasksInputs  []*ecs.DescribeTasksInput
	describeTasksOutputs []*ecs.DescribeTasksOutput

	stopTaskInput  *ecs.StopTaskInput
	stopTaskOutput *ecs.StopTaskOutput
	stopTaskErr    error
}

func (f *fakeECSClient) RunTask(ctx context.Context, params *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	f.runTaskInput = params
	if f.runTaskErr != nil {
		return nil, f.runTaskErr
	}
	if f.runTaskOutput != nil {
		return f.runTaskOutput, nil
	}
	return &ecs.RunTaskOutput{}, nil
}
func (f *fakeECSClient) DescribeTasks(ctx context.Context, params *ecs.DescribeTasksInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	f.describeTasksInput = params
	f.describeTasksInputs = append(f.describeTasksInputs, params)
	if f.describeTasksErr != nil {
		return nil, f.describeTasksErr
	}
	if len(f.describeTasksOutputs) > 0 {
		idx := len(f.describeTasksInputs) - 1
		if idx < len(f.describeTasksOutputs) {
			return f.describeTasksOutputs[idx], nil
		}
		return f.describeTasksOutputs[len(f.describeTasksOutputs)-1], nil
	}
	return f.describeTasksOutput, nil
}
func (f *fakeECSClient) StopTask(ctx context.Context, params *ecs.StopTaskInput, optFns ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	f.stopTaskInput = params
	if f.stopTaskErr != nil {
		return nil, f.stopTaskErr
	}
	if f.stopTaskOutput != nil {
		return f.stopTaskOutput, nil
	}
	return &ecs.StopTaskOutput{}, nil
}

type fakeCloudWatchLogsClient struct {
	getLogEventsInputs  []*cloudwatchlogs.GetLogEventsInput
	getLogEventsOutputs []*cloudwatchlogs.GetLogEventsOutput
	getLogEventsErr     error
	getLogEventsErrs    []error
}

func (f *fakeCloudWatchLogsClient) GetLogEvents(ctx context.Context, params *cloudwatchlogs.GetLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.getLogEventsInputs = append(f.getLogEventsInputs, params)
	idx := len(f.getLogEventsInputs) - 1
	if idx < len(f.getLogEventsErrs) && f.getLogEventsErrs[idx] != nil {
		return nil, f.getLogEventsErrs[idx]
	}
	if f.getLogEventsErr != nil {
		return nil, f.getLogEventsErr
	}
	if idx < len(f.getLogEventsOutputs) {
		return f.getLogEventsOutputs[idx], nil
	}
	return &cloudwatchlogs.GetLogEventsOutput{}, nil
}

type failReader struct{ err error }

func (f *failReader) Read(p []byte) (int, error) { return 0, f.err }

type fakeS3Client struct {
	getObjectInput  *s3.GetObjectInput
	getObjectOutput *s3.GetObjectOutput
	getObjectErr    error
}

func (f *fakeS3Client) GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.getObjectInput = params
	if f.getObjectErr != nil {
		return nil, f.getObjectErr
	}
	if f.getObjectOutput != nil {
		return f.getObjectOutput, nil
	}
	return nil, nil
}

func testHordeConfig() *config.HordeConfig {
	return &config.HordeConfig{
		ClusterARN:            "arn:aws:ecs:us-east-1:123456789012:cluster/horde",
		TaskDefinitionARN:     "arn:aws:ecs:us-east-1:123456789012:task-definition/horde-worker:1",
		Subnets:               []string{"subnet-abc"},
		SecurityGroup:         "sg-123",
		LogGroup:              "/ecs/horde-worker",
		LogStreamPrefix:       "ecs",
		ArtifactsBucket:       "my-horde-artifacts",
		RunsTable:             "horde-runs",
		MaxConcurrent:         5,
		DefaultTimeoutMinutes: 1440,
	}
}

func TestNewECSProvider(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	if p == nil {
		t.Error("NewECSProvider() returned nil")
	}
}

func TestECSProvider_InterfaceCompliance(t *testing.T) {
	t.Parallel()
	var p Provider = NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	if p == nil {
		t.Error("NewECSProvider() returned nil")
	}
}

func TestECSProvider_Launch_Success(t *testing.T) {
	t.Parallel()
	taskARN := "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123"
	fake := &fakeECSClient{
		runTaskOutput: &ecs.RunTaskOutput{
			Tasks: []ecstypes.Task{
				{TaskArn: aws.String(taskARN)},
			},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	opts := LaunchOpts{
		Repo:     "github.com/org/repo.git",
		Ticket:   "PROJ-123",
		Branch:   "main",
		Workflow: "default",
		RunID:    "k7m2xp4qr9n3",
	}
	result, err := p.Launch(context.Background(), opts)
	if err != nil {
		t.Fatalf("Launch() error = %v, want nil", err)
	}
	if result.InstanceID != taskARN {
		t.Errorf("InstanceID = %q, want %q", result.InstanceID, taskARN)
	}
	if result.Metadata["cluster_arn"] != "arn:aws:ecs:us-east-1:123456789012:cluster/horde" {
		t.Errorf("Metadata[cluster_arn] = %q", result.Metadata["cluster_arn"])
	}
	if result.Metadata["log_group"] != "/ecs/horde-worker" {
		t.Errorf("Metadata[log_group] = %q", result.Metadata["log_group"])
	}
	if result.Metadata["artifacts_bucket"] != "my-horde-artifacts" {
		t.Errorf("Metadata[artifacts_bucket] = %q", result.Metadata["artifacts_bucket"])
	}

	// Verify RunTaskInput construction
	in := fake.runTaskInput
	if in == nil {
		t.Fatal("RunTask was not called")
	}
	if *in.TaskDefinition != "arn:aws:ecs:us-east-1:123456789012:task-definition/horde-worker:1" {
		t.Errorf("TaskDefinition = %q", *in.TaskDefinition)
	}
	if *in.Cluster != "arn:aws:ecs:us-east-1:123456789012:cluster/horde" {
		t.Errorf("Cluster = %q", *in.Cluster)
	}
	if in.LaunchType != ecstypes.LaunchTypeFargate {
		t.Errorf("LaunchType = %v", in.LaunchType)
	}
	if *in.Count != 1 {
		t.Errorf("Count = %d, want 1", *in.Count)
	}
	vpc := in.NetworkConfiguration.AwsvpcConfiguration
	if len(vpc.Subnets) == 0 || vpc.Subnets[0] != "subnet-abc" {
		t.Errorf("Subnets = %v", vpc.Subnets)
	}
	if len(vpc.SecurityGroups) == 0 || vpc.SecurityGroups[0] != "sg-123" {
		t.Errorf("SecurityGroups = %v", vpc.SecurityGroups)
	}
	if vpc.AssignPublicIp != ecstypes.AssignPublicIpEnabled {
		t.Errorf("AssignPublicIp = %v", vpc.AssignPublicIp)
	}
	overrides := in.Overrides.ContainerOverrides
	if len(overrides) != 1 {
		t.Fatalf("ContainerOverrides len = %d, want 1", len(overrides))
	}
	if *overrides[0].Name != "horde-worker" {
		t.Errorf("ContainerOverride Name = %q", *overrides[0].Name)
	}
	envMap := make(map[string]string)
	for _, kv := range overrides[0].Environment {
		envMap[*kv.Name] = *kv.Value
	}
	wantEnv := map[string]string{
		"REPO_URL":         "github.com/org/repo.git",
		"TICKET":           "PROJ-123",
		"BRANCH":           "main",
		"WORKFLOW":         "default",
		"RUN_ID":           "k7m2xp4qr9n3",
		"ARTIFACTS_BUCKET": "my-horde-artifacts",
	}
	for k, v := range wantEnv {
		if envMap[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, envMap[k], v)
		}
	}
	tagMap := make(map[string]string)
	for _, tag := range in.Tags {
		tagMap[*tag.Key] = *tag.Value
	}
	if tagMap["horde-run-id"] != "k7m2xp4qr9n3" {
		t.Errorf("tag horde-run-id = %q", tagMap["horde-run-id"])
	}
	if tagMap["horde-ticket"] != "PROJ-123" {
		t.Errorf("tag horde-ticket = %q", tagMap["horde-ticket"])
	}
}

func TestECSProvider_Launch_RunTaskError(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{runTaskErr: fmt.Errorf("AccessDeniedException: not authorized")}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Launch(context.Background(), LaunchOpts{})
	if err == nil {
		t.Fatal("Launch() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "launching ECS task") {
		t.Errorf("error = %q, want it to contain \"launching ECS task\"", err.Error())
	}
}

func TestECSProvider_Launch_Failure(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		runTaskOutput: &ecs.RunTaskOutput{
			Failures: []ecstypes.Failure{{Reason: aws.String("RESOURCE:ENI")}},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Launch(context.Background(), LaunchOpts{})
	if err == nil {
		t.Fatal("Launch() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "launching ECS task") {
		t.Errorf("error = %q, want it to contain \"launching ECS task\"", err.Error())
	}
	if !strings.Contains(err.Error(), "RESOURCE:ENI") {
		t.Errorf("error = %q, want it to contain \"RESOURCE:ENI\"", err.Error())
	}
}

func TestECSProvider_Launch_NoTasks(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		runTaskOutput: &ecs.RunTaskOutput{Tasks: []ecstypes.Task{}},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Launch(context.Background(), LaunchOpts{})
	if err == nil {
		t.Fatal("Launch() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "no task returned") {
		t.Errorf("error = %q, want it to contain \"no task returned\"", err.Error())
	}
}

func TestECSProvider_Launch_NilTaskArn(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		runTaskOutput: &ecs.RunTaskOutput{
			Tasks: []ecstypes.Task{{}}, // TaskArn is nil
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Launch(context.Background(), LaunchOpts{})
	if err == nil {
		t.Fatal("Launch() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "task ARN is nil") {
		t.Errorf("error = %q, want it to contain \"task ARN is nil\"", err.Error())
	}
}

func TestECSProvider_Launch_EmptyOpts(t *testing.T) {
	t.Parallel()
	taskARN := "arn:aws:ecs:us-east-1:123456789012:task/horde/empty"
	fake := &fakeECSClient{
		runTaskOutput: &ecs.RunTaskOutput{
			Tasks: []ecstypes.Task{{TaskArn: aws.String(taskARN)}},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Launch(context.Background(), LaunchOpts{})
	if err != nil {
		t.Fatalf("Launch() error = %v, want nil", err)
	}
	if result.InstanceID != taskARN {
		t.Errorf("InstanceID = %q, want %q", result.InstanceID, taskARN)
	}
	// Provider does not validate opts — empty strings are passed through
	in := fake.runTaskInput
	envMap := make(map[string]string)
	for _, kv := range in.Overrides.ContainerOverrides[0].Environment {
		envMap[*kv.Name] = *kv.Value
	}
	for _, key := range []string{"REPO_URL", "TICKET", "BRANCH", "WORKFLOW", "RUN_ID"} {
		if envMap[key] != "" {
			t.Errorf("env[%s] = %q, want empty string", key, envMap[key])
		}
	}
	// ARTIFACTS_BUCKET comes from config, not opts
	if envMap["ARTIFACTS_BUCKET"] != "my-horde-artifacts" {
		t.Errorf("env[ARTIFACTS_BUCKET] = %q, want \"my-horde-artifacts\"", envMap["ARTIFACTS_BUCKET"])
	}
}

func TestECSProvider_Status_Running(t *testing.T) {
	t.Parallel()
	started := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	instanceID := "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123"
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{
				{
					LastStatus: aws.String("RUNNING"),
					StartedAt:  &started,
				},
			},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Status(context.Background(), instanceID)
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if result.State != "running" {
		t.Errorf("State = %q, want \"running\"", result.State)
	}
	if result.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil", result.ExitCode)
	}
	if result.StartedAt != started {
		t.Errorf("StartedAt = %v, want %v", result.StartedAt, started)
	}
	if result.FinishedAt != nil {
		t.Errorf("FinishedAt = %v, want nil", result.FinishedAt)
	}
	// Verify DescribeTasksInput
	if fake.describeTasksInput == nil {
		t.Fatal("DescribeTasks was not called")
	}
	if *fake.describeTasksInput.Cluster != testHordeConfig().ClusterARN {
		t.Errorf("Cluster = %q, want %q", *fake.describeTasksInput.Cluster, testHordeConfig().ClusterARN)
	}
	if len(fake.describeTasksInput.Tasks) != 1 || fake.describeTasksInput.Tasks[0] != instanceID {
		t.Errorf("Tasks = %v, want [%q]", fake.describeTasksInput.Tasks, instanceID)
	}
}

func TestECSProvider_Status_Stopped(t *testing.T) {
	t.Parallel()
	started := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	stopped := time.Date(2025, 1, 15, 11, 0, 0, 0, time.UTC)
	exitCode := int32(0)
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{
				{
					LastStatus: aws.String("STOPPED"),
					StartedAt:  &started,
					StoppedAt:  &stopped,
					Containers: []ecstypes.Container{
						{ExitCode: &exitCode},
					},
				},
			},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Status(context.Background(), "task-arn")
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if result.State != "stopped" {
		t.Errorf("State = %q, want \"stopped\"", result.State)
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", result.ExitCode)
	}
	if result.FinishedAt == nil || *result.FinishedAt != stopped {
		t.Errorf("FinishedAt = %v, want %v", result.FinishedAt, stopped)
	}
}

func TestECSProvider_Status_StoppedNonZeroExit(t *testing.T) {
	t.Parallel()
	exitCode := int32(1)
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{
				{
					LastStatus: aws.String("STOPPED"),
					Containers: []ecstypes.Container{
						{ExitCode: &exitCode},
					},
				},
			},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Status(context.Background(), "task-arn")
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if result.ExitCode == nil || *result.ExitCode != 1 {
		t.Errorf("ExitCode = %v, want 1", result.ExitCode)
	}
}

func TestECSProvider_Status_UnknownStatus(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{
				{LastStatus: aws.String("PROVISIONING")},
			},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Status(context.Background(), "task-arn")
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if result.State != "unknown" {
		t.Errorf("State = %q, want \"unknown\"", result.State)
	}
}

func TestECSProvider_Status_NilLastStatus(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{
				{LastStatus: nil},
			},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Status(context.Background(), "task-arn")
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if result.State != "unknown" {
		t.Errorf("State = %q, want \"unknown\"", result.State)
	}
}

func TestECSProvider_Status_NoContainers(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{
				{
					LastStatus: aws.String("STOPPED"),
					Containers: []ecstypes.Container{},
				},
			},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Status(context.Background(), "task-arn")
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if result.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil", result.ExitCode)
	}
}

func TestECSProvider_Status_DescribeTasksError(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{describeTasksErr: fmt.Errorf("AccessDeniedException: not authorized")}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Status(context.Background(), "task-arn")
	if err == nil {
		t.Fatal("Status() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "describing ECS task") {
		t.Errorf("error = %q, want it to contain \"describing ECS task\"", err.Error())
	}
}

func TestECSProvider_Status_Failure(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Failures: []ecstypes.Failure{{Reason: aws.String("MISSING")}},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Status(context.Background(), "task-arn")
	if err == nil {
		t.Fatal("Status() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "describing ECS task") {
		t.Errorf("error = %q, want it to contain \"describing ECS task\"", err.Error())
	}
	if !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("error = %q, want it to contain \"MISSING\"", err.Error())
	}
}

func TestECSProvider_Status_FailureNilReason(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Failures: []ecstypes.Failure{{}},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Status(context.Background(), "task-arn")
	if err == nil {
		t.Fatal("Status() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "describing ECS task") {
		t.Errorf("error = %q, want it to contain \"describing ECS task\"", err.Error())
	}
}

func TestECSProvider_Status_NoTasks(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks:    []ecstypes.Task{},
			Failures: []ecstypes.Failure{},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Status(context.Background(), "task-arn")
	if err == nil {
		t.Fatal("Status() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "task not found") {
		t.Errorf("error = %q, want it to contain \"task not found\"", err.Error())
	}
}

func TestECSProvider_Status_NilResponse(t *testing.T) {
	t.Parallel()
	// Both describeTasksOutput and describeTasksErr are zero (nil) — tests nil response guard.
	fake := &fakeECSClient{}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Status(context.Background(), "task-arn")
	if err == nil {
		t.Fatal("Status() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "describing ECS task") {
		t.Errorf("error = %q, want it to contain \"describing ECS task\"", err.Error())
	}
	if !strings.Contains(err.Error(), "nil response") {
		t.Errorf("error = %q, want it to contain \"nil response\"", err.Error())
	}
}

func TestECSProvider_Status_DescribeTasksTypedError(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksErr: &ecstypes.ClusterNotFoundException{Message: aws.String("cluster not found")},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Status(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/nonexistent")
	if err == nil {
		t.Fatal("Status() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "describing ECS task") {
		t.Errorf("error = %q, want it to contain \"describing ECS task\"", err.Error())
	}
	var cnf *ecstypes.ClusterNotFoundException
	if !errors.As(err, &cnf) {
		t.Errorf("error type = %T, want *ecstypes.ClusterNotFoundException", err)
	}
}

func TestECSProvider_Logs_Success(t *testing.T) {
	t.Parallel()
	token := "token-1"
	fake := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events: []cwltypes.OutputLogEvent{
					{Message: aws.String("line 1\n")},
					{Message: aws.String("line 2\n")},
				},
				NextForwardToken: aws.String(token),
			},
			{
				// Second call returns same token — signals end of pagination
				Events:           []cwltypes.OutputLogEvent{},
				NextForwardToken: aws.String(token),
			},
		},
	}
	p := NewECSProvider(&fakeECSClient{}, fake, &fakeS3Client{}, testHordeConfig())
	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", false)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	want := "line 1\nline 2\n"
	if string(data) != want {
		t.Errorf("Logs output = %q, want %q", string(data), want)
	}
	// Verify GetLogEventsInput on first call
	if len(fake.getLogEventsInputs) < 1 {
		t.Fatal("GetLogEvents was not called")
	}
	in := fake.getLogEventsInputs[0]
	if *in.LogGroupName != "/ecs/horde-worker" {
		t.Errorf("LogGroupName = %q, want %q", *in.LogGroupName, "/ecs/horde-worker")
	}
	if *in.LogStreamName != "ecs/horde-worker/abc123" {
		t.Errorf("LogStreamName = %q, want %q", *in.LogStreamName, "ecs/horde-worker/abc123")
	}
	if !*in.StartFromHead {
		t.Error("StartFromHead = false, want true")
	}
	if in.NextToken != nil {
		t.Errorf("first call NextToken = %v, want nil", in.NextToken)
	}
}

func TestECSProvider_Logs_Pagination(t *testing.T) {
	t.Parallel()
	token1 := "token-1"
	token2 := "token-2"
	fake := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events: []cwltypes.OutputLogEvent{
					{Message: aws.String("page 1\n")},
				},
				NextForwardToken: aws.String(token1),
			},
			{
				Events: []cwltypes.OutputLogEvent{
					{Message: aws.String("page 2\n")},
				},
				NextForwardToken: aws.String(token2),
			},
			{
				Events:           []cwltypes.OutputLogEvent{},
				NextForwardToken: aws.String(token2), // same as sent — done
			},
		},
	}
	p := NewECSProvider(&fakeECSClient{}, fake, &fakeS3Client{}, testHordeConfig())
	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123:task/c/task1", false)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	want := "page 1\npage 2\n"
	if string(data) != want {
		t.Errorf("Logs output = %q, want %q", string(data), want)
	}
	// Verify second call forwarded the token
	if len(fake.getLogEventsInputs) < 2 {
		t.Fatalf("GetLogEvents called %d times, want >= 2", len(fake.getLogEventsInputs))
	}
	if fake.getLogEventsInputs[1].NextToken == nil || *fake.getLogEventsInputs[1].NextToken != token1 {
		t.Errorf("second call NextToken = %v, want %q", fake.getLogEventsInputs[1].NextToken, token1)
	}
}

func TestECSProvider_Logs_Empty(t *testing.T) {
	t.Parallel()
	fake := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{Events: []cwltypes.OutputLogEvent{}},
		},
	}
	p := NewECSProvider(&fakeECSClient{}, fake, &fakeS3Client{}, testHordeConfig())
	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123:task/c/task1", false)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if len(data) != 0 {
		t.Errorf("Logs output = %q, want empty", string(data))
	}
}

func TestECSProvider_Logs_APIError(t *testing.T) {
	t.Parallel()
	fake := &fakeCloudWatchLogsClient{
		getLogEventsErr: fmt.Errorf("ResourceNotFoundException: log group does not exist"),
	}
	p := NewECSProvider(&fakeECSClient{}, fake, &fakeS3Client{}, testHordeConfig())
	_, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123:task/c/task1", false)
	if err == nil {
		t.Fatal("Logs() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "reading logs") {
		t.Errorf("error = %q, want it to contain \"reading logs\"", err.Error())
	}
}

func TestECSProvider_Logs_NilResponse(t *testing.T) {
	t.Parallel()
	fake := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{nil},
	}
	p := NewECSProvider(&fakeECSClient{}, fake, &fakeS3Client{}, testHordeConfig())
	_, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123:task/c/task1", false)
	if err == nil {
		t.Fatal("Logs() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "reading logs") {
		t.Errorf("error = %q, want it to contain \"reading logs\"", err.Error())
	}
	if !strings.Contains(err.Error(), "nil response") {
		t.Errorf("error = %q, want it to contain \"nil response\"", err.Error())
	}
}

func TestECSProvider_Logs_LogStreamNotCreated(t *testing.T) {
	t.Parallel()
	fake := &fakeCloudWatchLogsClient{
		getLogEventsErr: &cwltypes.ResourceNotFoundException{Message: aws.String("The specified log stream does not exist.")},
	}
	p := NewECSProvider(&fakeECSClient{}, fake, &fakeS3Client{}, testHordeConfig())
	_, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123:task/c/task1", false)
	if err == nil {
		t.Fatal("Logs() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "reading logs") {
		t.Errorf("error = %q, want it to contain \"reading logs\"", err.Error())
	}
	var rnf *cwltypes.ResourceNotFoundException
	if !errors.As(err, &rnf) {
		t.Errorf("error type = %T, want *cwltypes.ResourceNotFoundException", err)
	}
}

func TestECSProvider_Logs_Follow_LogStreamNotCreated(t *testing.T) {
	t.Parallel()
	rnf := &cwltypes.ResourceNotFoundException{Message: aws.String("The specified log stream does not exist.")}
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsErrs: []error{rnf, rnf, nil},
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			nil, // ignored — error takes precedence
			nil, // ignored — error takes precedence
			{
				Events: []cwltypes.OutputLogEvent{{Message: aws.String("hello from container\n")}},
			},
		},
	}
	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("RUNNING")}}},
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("RUNNING")}}},
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("STOPPED")}}},
		},
	}
	p := NewECSProvider(fakeECS, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v, want nil", err)
	}
	if !strings.Contains(string(data), "hello from container") {
		t.Errorf("output = %q, want it to contain \"hello from container\"", string(data))
	}
	if len(fakeLogs.getLogEventsInputs) != 3 {
		t.Errorf("GetLogEvents called %d times, want 3", len(fakeLogs.getLogEventsInputs))
	}
}

func TestECSProvider_Logs_Follow_LogStreamNotCreatedTaskStopped(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsErr: &cwltypes.ResourceNotFoundException{Message: aws.String("The specified log stream does not exist.")},
	}
	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("STOPPED")}}},
		},
	}
	p := NewECSProvider(fakeECS, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v, want nil (goroutine must exit cleanly when task stops during RNF)", err)
	}
	if len(data) != 0 {
		t.Errorf("output = %q, want empty (no log stream was ever created)", string(data))
	}
	if len(fakeLogs.getLogEventsInputs) != 1 {
		t.Errorf("GetLogEvents called %d times, want 1", len(fakeLogs.getLogEventsInputs))
	}
	if len(fakeECS.describeTasksInputs) != 1 {
		t.Errorf("DescribeTasks called %d times, want 1", len(fakeECS.describeTasksInputs))
	}
}

func TestECSProvider_Logs_Follow_LogStreamNotCreatedContextCancel(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsErr: &cwltypes.ResourceNotFoundException{Message: aws.String("The specified log stream does not exist.")},
	}
	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("RUNNING")}}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := NewECSProvider(fakeECS, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(ctx, "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}

	// Let the goroutine make a few RNF + DescribeTasks iterations, then cancel.
	time.Sleep(10 * time.Millisecond)
	cancel()

	// ReadAll must complete (goroutine must not hang).
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Logf("ReadAll() error = %v (acceptable — context cancelled)", err)
	}
	_ = data

	// reader.Close() must complete without hanging.
	reader.Close()
}

func TestECSProvider_Logs_Follow_LogStreamNotCreatedOtherErrorStillFatal(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsErr: fmt.Errorf("ThrottlingException: rate exceeded"),
	}
	p := NewECSProvider(&fakeECSClient{}, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()

	_, err = io.ReadAll(reader)
	if err == nil {
		t.Fatal("ReadAll() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "reading logs") {
		t.Errorf("error = %q, want it to contain \"reading logs\"", err.Error())
	}
}

func TestECSProvider_Logs_Follow_TaskStops(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("follow line 1\n")}},
				NextForwardToken: aws.String("tok1"),
			},
			{
				// Same token — follow mode does NOT stop on same token
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("follow line 2\n")}},
				NextForwardToken: aws.String("tok1"),
			},
		},
	}
	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("RUNNING")}}},
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("STOPPED")}}},
		},
	}
	p := NewECSProvider(fakeECS, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	want := "follow line 1\nfollow line 2\n"
	if string(data) != want {
		t.Errorf("Logs output = %q, want %q", string(data), want)
	}

	if len(fakeLogs.getLogEventsInputs) < 1 {
		t.Fatal("GetLogEvents was not called")
	}
	in := fakeLogs.getLogEventsInputs[0]
	if *in.LogGroupName != "/ecs/horde-worker" {
		t.Errorf("LogGroupName = %q, want %q", *in.LogGroupName, "/ecs/horde-worker")
	}
	if *in.LogStreamName != "ecs/horde-worker/abc123" {
		t.Errorf("LogStreamName = %q, want %q", *in.LogStreamName, "ecs/horde-worker/abc123")
	}
}

func TestECSProvider_Logs_Follow_ContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("line 1\n")}},
				NextForwardToken: aws.String("tok1"),
			},
			// remaining calls return empty (default)
		},
	}
	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("RUNNING")}}},
		},
	}
	p := NewECSProvider(fakeECS, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(ctx, "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}

	buf := make([]byte, 64)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if string(buf[:n]) != "line 1\n" {
		t.Errorf("Read() = %q, want %q", string(buf[:n]), "line 1\n")
	}

	cancel()

	_, err = io.ReadAll(reader)
	if err != nil {
		t.Errorf("ReadAll() after cancel error = %v, want nil", err)
	}

	// reader.Close() must complete without hanging
	reader.Close()
}

func TestECSProvider_Logs_Follow_GetLogEventsError(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("line 1\n")}},
				NextForwardToken: aws.String("tok1"),
			},
		},
		getLogEventsErrs: []error{nil, fmt.Errorf("throttling")},
	}
	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("RUNNING")}}},
		},
	}
	p := NewECSProvider(fakeECS, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err == nil {
		t.Fatal("ReadAll() error = nil, want non-nil")
	}
	if !strings.Contains(string(data), "line 1") {
		t.Errorf("data = %q, want it to contain \"line 1\"", string(data))
	}
	if !strings.Contains(err.Error(), "reading logs") {
		t.Errorf("error = %q, want it to contain \"reading logs\"", err.Error())
	}
}

func TestECSProvider_Logs_Follow_NilResponse(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{nil},
	}
	p := NewECSProvider(&fakeECSClient{}, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()

	_, err = io.ReadAll(reader)
	if err == nil {
		t.Fatal("ReadAll() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "reading logs") {
		t.Errorf("error = %q, want it to contain \"reading logs\"", err.Error())
	}
	if !strings.Contains(err.Error(), "nil response") {
		t.Errorf("error = %q, want it to contain \"nil response\"", err.Error())
	}
}

func TestECSProvider_Logs_FollowDescribeTasksError(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		// No outputs configured — GetLogEvents returns empty output on every call,
		// keeping the goroutine looping without producing log events.
	}
	fakeECS := &fakeECSClient{
		describeTasksErr: fmt.Errorf("ExpiredTokenException: security token has expired"),
	}
	p := NewECSProvider(fakeECS, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v, want nil (goroutine should close pipe normally)", err)
	}
	got := string(data)
	if !strings.Contains(got, "unable to determine task completion") {
		t.Errorf("output = %q, want it to contain \"unable to determine task completion\"", got)
	}
	if !strings.Contains(got, "stopping follow") {
		t.Errorf("output = %q, want it to contain \"stopping follow\"", got)
	}
	// Verify DescribeTasks was called exactly maxConsecutiveDescribeFailures times (5).
	if len(fakeECS.describeTasksInputs) != 5 {
		t.Errorf("DescribeTasks called %d times, want %d", len(fakeECS.describeTasksInputs), 5)
	}
}

func TestECSProvider_Logs_Follow_DescribeTasksFailures(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("follow line 1\n")}},
				NextForwardToken: aws.String("tok1"),
			},
		},
	}
	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{
			{Failures: []ecstypes.Failure{{Reason: aws.String("MISSING")}}, Tasks: []ecstypes.Task{}},
		},
	}
	p := NewECSProvider(fakeECS, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v, want nil (pipe must close cleanly)", err)
	}
	got := string(data)
	if !strings.Contains(got, "follow line 1") {
		t.Errorf("output = %q, want it to contain \"follow line 1\"", got)
	}
	if !strings.Contains(got, "task no longer available") {
		t.Errorf("output = %q, want it to contain \"task no longer available\"", got)
	}
	if !strings.Contains(got, "MISSING") {
		t.Errorf("output = %q, want it to contain \"MISSING\"", got)
	}
	if len(fakeECS.describeTasksInputs) != 1 {
		t.Errorf("DescribeTasks called %d times, want 1", len(fakeECS.describeTasksInputs))
	}
}

func TestECSProvider_Logs_Follow_DescribeTasksFailuresNilReason(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("follow line 1\n")}},
				NextForwardToken: aws.String("tok1"),
			},
		},
	}
	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{
			{Failures: []ecstypes.Failure{{}}, Tasks: []ecstypes.Task{}},
		},
	}
	p := NewECSProvider(fakeECS, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v, want nil (pipe must close cleanly)", err)
	}
	got := string(data)
	if !strings.Contains(got, "task no longer available") {
		t.Errorf("output = %q, want it to contain \"task no longer available\"", got)
	}
	if len(fakeECS.describeTasksInputs) != 1 {
		t.Errorf("DescribeTasks called %d times, want 1", len(fakeECS.describeTasksInputs))
	}
}

func TestECSProvider_Logs_Follow_EmptyTaskID(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	result, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123:task/horde/", true)
	if result != nil {
		t.Errorf("Logs() result = %v, want nil", result)
	}
	if err == nil {
		t.Fatal("Logs() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "empty task ID") {
		t.Errorf("error = %q, want it to contain \"empty task ID\"", err.Error())
	}
}

func TestECSProvider_Logs_Follow_NewlineHandling(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events: []cwltypes.OutputLogEvent{
					{Message: aws.String("no newline")},
					{Message: aws.String("has newline\n")},
				},
				// nil NextForwardToken
			},
		},
	}
	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("STOPPED")}}},
		},
	}
	p := NewECSProvider(fakeECS, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	want := "no newline\nhas newline\n"
	if string(data) != want {
		t.Errorf("Logs output = %q, want %q", string(data), want)
	}
}

func TestECSProvider_Logs_Follow_CloseStopsGoroutine(t *testing.T) {
	t.Parallel()
	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("RUNNING")}}},
		},
	}
	p := NewECSProvider(fakeECS, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}

	// Close must return without hanging — confirms goroutine exits
	reader.Close()
}

func TestECSProvider_Logs_BareTaskID(t *testing.T) {
	t.Parallel()
	fake := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{Events: []cwltypes.OutputLogEvent{}},
		},
	}
	p := NewECSProvider(&fakeECSClient{}, fake, &fakeS3Client{}, testHordeConfig())
	_, err := p.Logs(context.Background(), "abc123", false)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	if len(fake.getLogEventsInputs) < 1 {
		t.Fatal("GetLogEvents was not called")
	}
	// bare ID is used directly as task ID
	if *fake.getLogEventsInputs[0].LogStreamName != "ecs/horde-worker/abc123" {
		t.Errorf("LogStreamName = %q, want %q", *fake.getLogEventsInputs[0].LogStreamName, "ecs/horde-worker/abc123")
	}
}

func TestECSProvider_Logs_EmptyTaskID(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123:task/horde/", false)
	if err == nil {
		t.Fatal("Logs() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "empty task ID") {
		t.Errorf("error = %q, want it to contain \"empty task ID\"", err.Error())
	}
}

func TestECSProvider_Logs_NewlineHandling(t *testing.T) {
	t.Parallel()
	fake := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events: []cwltypes.OutputLogEvent{
					{Message: aws.String("no newline")},
					{Message: aws.String("has newline\n")},
				},
			},
		},
	}
	p := NewECSProvider(&fakeECSClient{}, fake, &fakeS3Client{}, testHordeConfig())
	reader, err := p.Logs(context.Background(), "arn:aws:ecs:us-east-1:123:task/c/t1", false)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	// Messages without trailing newlines get one added
	want := "no newline\nhas newline\n"
	if string(data) != want {
		t.Errorf("Logs output = %q, want %q", string(data), want)
	}
}

func TestECSProvider_Stop_Success(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	instanceID := "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123"
	err := p.Stop(context.Background(), StopOpts{
		InstanceID: instanceID,
	})
	if err != nil {
		t.Fatalf("Stop() error = %v, want nil", err)
	}
	if fake.stopTaskInput == nil {
		t.Fatal("StopTask was not called")
	}
	if *fake.stopTaskInput.Cluster != testHordeConfig().ClusterARN {
		t.Errorf("Cluster = %q, want %q", *fake.stopTaskInput.Cluster, testHordeConfig().ClusterARN)
	}
	if *fake.stopTaskInput.Task != instanceID {
		t.Errorf("Task = %q, want %q", *fake.stopTaskInput.Task, instanceID)
	}
	if *fake.stopTaskInput.Reason != "horde kill" {
		t.Errorf("Reason = %q, want %q", *fake.stopTaskInput.Reason, "horde kill")
	}
}

func TestECSProvider_Stop_Error(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		stopTaskErr: fmt.Errorf("AccessDeniedException: not authorized"),
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	err := p.Stop(context.Background(), StopOpts{
		InstanceID: "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123",
	})
	if err == nil {
		t.Fatal("Stop() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "stopping ECS task") {
		t.Errorf("error = %q, want it to contain \"stopping ECS task\"", err.Error())
	}
}

func TestECSProvider_Stop_IgnoresResultsDir(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	err := p.Stop(context.Background(), StopOpts{
		InstanceID: "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123",
		ResultsDir: "/some/path/that/should/be/ignored",
	})
	if err != nil {
		t.Fatalf("Stop() error = %v, want nil", err)
	}
	if fake.stopTaskInput == nil {
		t.Fatal("StopTask was not called")
	}
	if *fake.stopTaskInput.Task != "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123" {
		t.Errorf("Task = %q", *fake.stopTaskInput.Task)
	}
}

func TestECSProvider_Stop_AlreadyStopped(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		stopTaskErr: &ecstypes.InvalidParameterException{Message: aws.String("The referenced task was already stopped.")},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	err := p.Stop(context.Background(), StopOpts{
		InstanceID: "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123",
	})
	if err != nil {
		t.Fatalf("Stop() error = %v, want nil (already-stopped tasks should be treated as success)", err)
	}
}

func TestECSProvider_Stop_OtherInvalidParameterException(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		stopTaskErr: &ecstypes.InvalidParameterException{Message: aws.String("some other parameter error")},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	err := p.Stop(context.Background(), StopOpts{
		InstanceID: "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123",
	})
	if err == nil {
		t.Fatal("Stop() error = nil, want non-nil for non-already-stopped InvalidParameterException")
	}
	if !strings.Contains(err.Error(), "stopping ECS task") {
		t.Errorf("error = %q, want it to contain \"stopping ECS task\"", err.Error())
	}
	var ipe *ecstypes.InvalidParameterException
	if !errors.As(err, &ipe) {
		t.Errorf("error type = %T, want *ecstypes.InvalidParameterException", err)
	}
}

func TestECSProvider_ReadFile_Success(t *testing.T) {
	t.Parallel()
	fake := &fakeS3Client{
		getObjectOutput: &s3.GetObjectOutput{
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
	}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())
	data, err := p.ReadFile(context.Background(), ReadFileOpts{
		RunID:    "k7m2xp4qr9n3",
		Path:     ".orc/audit/PROJ-123/run-result.json",
		Metadata: map[string]string{"artifacts_bucket": "my-horde-artifacts"},
	})
	if err != nil {
		t.Fatalf("ReadFile() error = %v, want nil", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("ReadFile() data = %q, want %q", data, `{"ok":true}`)
	}
	if fake.getObjectInput == nil {
		t.Fatal("GetObject was not called")
	}
	if *fake.getObjectInput.Bucket != "my-horde-artifacts" {
		t.Errorf("Bucket = %q, want %q", *fake.getObjectInput.Bucket, "my-horde-artifacts")
	}
	if *fake.getObjectInput.Key != "horde-runs/k7m2xp4qr9n3/audit/PROJ-123/run-result.json" {
		t.Errorf("Key = %q", *fake.getObjectInput.Key)
	}
}

func TestECSProvider_ReadFile_ArtifactsPath(t *testing.T) {
	t.Parallel()
	fake := &fakeS3Client{
		getObjectOutput: &s3.GetObjectOutput{
			Body: io.NopCloser(strings.NewReader("hello")),
		},
	}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{
		RunID:    "run-002",
		Path:     ".orc/artifacts/output.txt",
		Metadata: map[string]string{"artifacts_bucket": "my-horde-artifacts"},
	})
	if err != nil {
		t.Fatalf("ReadFile() error = %v, want nil", err)
	}
	if fake.getObjectInput == nil {
		t.Fatal("GetObject was not called")
	}
	if *fake.getObjectInput.Key != "horde-runs/run-002/artifacts/output.txt" {
		t.Errorf("Key = %q, want %q", *fake.getObjectInput.Key, "horde-runs/run-002/artifacts/output.txt")
	}
}

func TestECSProvider_ReadFile_NoSuchKey(t *testing.T) {
	t.Parallel()
	fake := &fakeS3Client{
		getObjectErr: &s3types.NoSuchKey{Message: aws.String("key not found")},
	}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{
		RunID:    "run-001",
		Path:     ".orc/audit/foo.json",
		Metadata: map[string]string{"artifacts_bucket": "my-horde-artifacts"},
	})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	var notFound *FileNotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("ReadFile() error type = %T, want *FileNotFoundError", err)
	}
	if notFound.Path != ".orc/audit/foo.json" {
		t.Errorf("FileNotFoundError.Path = %q, want %q", notFound.Path, ".orc/audit/foo.json")
	}
}

func TestECSProvider_ReadFile_S3Error(t *testing.T) {
	t.Parallel()
	fake := &fakeS3Client{
		getObjectErr: fmt.Errorf("AccessDenied"),
	}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{
		RunID:    "run-001",
		Path:     ".orc/audit/foo.json",
		Metadata: map[string]string{"artifacts_bucket": "my-horde-artifacts"},
	})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "reading file from s3") {
		t.Errorf("ReadFile() error = %q, want it to contain \"reading file from s3\"", err.Error())
	}
	var notFound *FileNotFoundError
	if errors.As(err, &notFound) {
		t.Errorf("ReadFile() error = %T, should NOT be *FileNotFoundError", err)
	}
}

func TestECSProvider_ReadFile_BodyReadError(t *testing.T) {
	t.Parallel()
	fake := &fakeS3Client{
		getObjectOutput: &s3.GetObjectOutput{
			Body: io.NopCloser(&failReader{err: fmt.Errorf("connection reset")}),
		},
	}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{
		RunID:    "run-001",
		Path:     ".orc/audit/foo.json",
		Metadata: map[string]string{"artifacts_bucket": "my-horde-artifacts"},
	})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "reading file from s3") {
		t.Errorf("ReadFile() error = %q, want it to contain \"reading file from s3\"", err.Error())
	}
	var notFound *FileNotFoundError
	if errors.As(err, &notFound) {
		t.Errorf("ReadFile() error = %T, should NOT be *FileNotFoundError", err)
	}
}

func TestECSProvider_ReadFile_EmptyPath(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "run-001", Path: ""})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("ReadFile() error = %q, want it to contain \"path is required\"", err.Error())
	}
}

func TestECSProvider_ReadFile_InvalidPrefix(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "run-001", Path: "some/other/file.txt"})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "path must start with") {
		t.Errorf("ReadFile() error = %q, want it to contain \"path must start with\"", err.Error())
	}
}

func TestECSProvider_ReadFile_BareOrcPrefix(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "run-001", Path: ".orc/"})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "path must include a filename") {
		t.Errorf("ReadFile() error = %q, want it to contain \"path must include a filename\"", err.Error())
	}
}

func TestECSProvider_ReadFile_NilMetadata(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{
		RunID:    "run-001",
		Path:     ".orc/audit/foo.json",
		Metadata: nil,
	})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "artifacts_bucket not found in metadata") {
		t.Errorf("ReadFile() error = %q, want it to contain \"artifacts_bucket not found in metadata\"", err.Error())
	}
}

func TestECSProvider_ReadFile_MissingBucketInMetadata(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{
		RunID:    "run-001",
		Path:     ".orc/audit/foo.json",
		Metadata: map[string]string{"log_group": "foo"},
	})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "artifacts_bucket not found in metadata") {
		t.Errorf("ReadFile() error = %q, want it to contain \"artifacts_bucket not found in metadata\"", err.Error())
	}
}

func TestECSProvider_ReadFile_EmptyBucketInMetadata(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{
		RunID:    "run-001",
		Path:     ".orc/audit/foo.json",
		Metadata: map[string]string{"artifacts_bucket": ""},
	})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "artifacts_bucket not found in metadata") {
		t.Errorf("ReadFile() error = %q, want it to contain \"artifacts_bucket not found in metadata\"", err.Error())
	}
}

func TestECSProvider_ReadFile_EmptyRunID(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{
		RunID:    "",
		Path:     ".orc/audit/foo.json",
		Metadata: map[string]string{"artifacts_bucket": "my-horde-artifacts"},
	})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "run ID is required") {
		t.Errorf("ReadFile() error = %q, want it to contain \"run ID is required\"", err.Error())
	}
}

func TestECSProvider_ReadFile_NilResponse(t *testing.T) {
	t.Parallel()
	// fakeS3Client with zero fields returns (nil, nil) from GetObject — triggers nil-response guard.
	fake := &fakeS3Client{}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{
		RunID:    "run-001",
		Path:     ".orc/audit/foo.json",
		Metadata: map[string]string{"artifacts_bucket": "my-horde-artifacts"},
	})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "reading file from s3") {
		t.Errorf("error = %q, want it to contain \"reading file from s3\"", err.Error())
	}
	if !strings.Contains(err.Error(), "nil response") {
		t.Errorf("error = %q, want it to contain \"nil response\"", err.Error())
	}
}

func TestECSProvider_ReadFile_NilBody(t *testing.T) {
	t.Parallel()
	fake := &fakeS3Client{
		getObjectOutput: &s3.GetObjectOutput{Body: nil},
	}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())
	_, err := p.ReadFile(context.Background(), ReadFileOpts{
		RunID:    "run-001",
		Path:     ".orc/audit/foo.json",
		Metadata: map[string]string{"artifacts_bucket": "my-horde-artifacts"},
	})
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "reading file from s3") {
		t.Errorf("error = %q, want it to contain \"reading file from s3\"", err.Error())
	}
	if !strings.Contains(err.Error(), "nil response") {
		t.Errorf("error = %q, want it to contain \"nil response\"", err.Error())
	}
}
