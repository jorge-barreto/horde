package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cloudwatchlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jorge-barreto/horde/internal/config"
)

type fakeECSClient struct {
	runTaskInput  *ecs.RunTaskInput
	runTaskOutput *ecs.RunTaskOutput
	runTaskErr    error

	describeTasksInput  *ecs.DescribeTasksInput
	describeTasksOutput *ecs.DescribeTasksOutput
	describeTasksErr    error
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
	return f.describeTasksOutput, f.describeTasksErr
}
func (f *fakeECSClient) StopTask(ctx context.Context, params *ecs.StopTaskInput, optFns ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	return nil, nil
}

type fakeCloudWatchLogsClient struct{}

func (f *fakeCloudWatchLogsClient) GetLogEvents(ctx context.Context, params *cloudwatchlogs.GetLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	return nil, nil
}

type fakeS3Client struct{}

func (f *fakeS3Client) GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return nil, nil
}

func testHordeConfig() *config.HordeConfig {
	return &config.HordeConfig{
		ClusterARN:            "arn:aws:ecs:us-east-1:123456789012:cluster/horde",
		TaskDefinitionARN:     "arn:aws:ecs:us-east-1:123456789012:task-definition/horde-worker:1",
		Subnets:               []string{"subnet-abc"},
		SecurityGroup:         "sg-123",
		LogGroup:              "/ecs/horde-worker",
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

func TestECSProvider_Logs_Stub(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Logs(context.Background(), "", false)
	if result != nil {
		t.Errorf("Logs() result = %v, want nil", result)
	}
	if err == nil {
		t.Fatal("Logs() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("Logs() error = %q, want it to contain \"not implemented\"", err.Error())
	}
}

func TestECSProvider_Stop_Stub(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	err := p.Stop(context.Background(), StopOpts{})
	if err == nil {
		t.Fatal("Stop() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("Stop() error = %q, want it to contain \"not implemented\"", err.Error())
	}
}

func TestECSProvider_ReadFile_Stub(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.ReadFile(context.Background(), ReadFileOpts{})
	if result != nil {
		t.Errorf("ReadFile() result = %v, want nil", result)
	}
	if err == nil {
		t.Fatal("ReadFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("ReadFile() error = %q, want it to contain \"not implemented\"", err.Error())
	}
}
