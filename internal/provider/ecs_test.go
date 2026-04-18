package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	"github.com/jorge-barreto/horde/internal/store"
)

type fakeECSClient struct {
	runTaskInput      *ecs.RunTaskInput
	runTaskOutput     *ecs.RunTaskOutput
	runTaskReturnNil  bool // if true, RunTask returns (nil, nil)
	runTaskErr        error

	describeTasksInput   *ecs.DescribeTasksInput
	describeTasksOutput  *ecs.DescribeTasksOutput
	describeTasksErr     error
	describeTasksErrs    []error // per-call errors; indexed by call number
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
	if f.runTaskReturnNil {
		return nil, nil
	}
	if f.runTaskOutput != nil {
		return f.runTaskOutput, nil
	}
	return &ecs.RunTaskOutput{}, nil
}
func (f *fakeECSClient) DescribeTasks(ctx context.Context, params *ecs.DescribeTasksInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	f.describeTasksInput = params
	f.describeTasksInputs = append(f.describeTasksInputs, params)
	idx := len(f.describeTasksInputs) - 1
	if idx < len(f.describeTasksErrs) && f.describeTasksErrs[idx] != nil {
		return nil, f.describeTasksErrs[idx]
	}
	if f.describeTasksErr != nil {
		return nil, f.describeTasksErr
	}
	if len(f.describeTasksOutputs) > 0 {
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

	// ListObjectsV2: keyed by prefix for simple per-prefix responses.
	// Values are the flat list of keys that live under that prefix.
	listKeys map[string][]string
	listErr  error

	// getObjectByKey overrides getObjectOutput for specific keys during hydrate tests.
	getObjectByKey map[string]string
}

func (f *fakeS3Client) GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.getObjectInput = params
	if f.getObjectErr != nil {
		return nil, f.getObjectErr
	}
	if f.getObjectByKey != nil {
		if body, ok := f.getObjectByKey[aws.ToString(params.Key)]; ok {
			return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(body))}, nil
		}
	}
	if f.getObjectOutput != nil {
		return f.getObjectOutput, nil
	}
	return nil, nil
}

func (f *fakeS3Client) ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	prefix := aws.ToString(params.Prefix)
	keys := f.listKeys[prefix]
	out := &s3.ListObjectsV2Output{}
	for _, k := range keys {
		k := k
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
	}
	return out, nil
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
	if result.Metadata["log_stream_prefix"] != "ecs" {
		t.Errorf("Metadata[log_stream_prefix] = %q, want %q", result.Metadata["log_stream_prefix"], "ecs")
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

func TestECSProvider_Launch_FailureNilReason(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		runTaskOutput: &ecs.RunTaskOutput{
			Failures: []ecstypes.Failure{{}},
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
	if !strings.Contains(err.Error(), "unknown failure") {
		t.Errorf("error = %q, want it to contain \"unknown failure\"", err.Error())
	}
}

// TestECSProvider_Launch_NilResponse covers the ecs.go nil-response
// guard in Launch. Regression guard for horde-ukr.
func TestECSProvider_Launch_NilResponse(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{runTaskReturnNil: true}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Launch(context.Background(), LaunchOpts{RunID: "abc"})
	if err == nil {
		t.Fatal("Launch() error = nil, want non-nil (nil RunTask response must surface as error)")
	}
	if !strings.Contains(err.Error(), "launching ECS task") {
		t.Errorf("error = %q, want it to contain \"launching ECS task\"", err.Error())
	}
	if !strings.Contains(err.Error(), "nil response") {
		t.Errorf("error = %q, want it to contain \"nil response\"", err.Error())
	}
}

func TestECSProvider_Launch_AssignPublicIp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		configVal  string
		wantAssign ecstypes.AssignPublicIp
	}{
		{"default empty maps to ENABLED", "", ecstypes.AssignPublicIpEnabled},
		{"explicit ENABLED", "ENABLED", ecstypes.AssignPublicIpEnabled},
		{"DISABLED for private subnets", "DISABLED", ecstypes.AssignPublicIpDisabled},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testHordeConfig()
			cfg.AssignPublicIp = tc.configVal
			fake := &fakeECSClient{
				runTaskOutput: &ecs.RunTaskOutput{
					Tasks: []ecstypes.Task{{TaskArn: aws.String("arn:aws:ecs:us-east-1:123:task/horde/abc")}},
				},
			}
			p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, cfg)
			if _, err := p.Launch(context.Background(), LaunchOpts{RunID: "abc", Ticket: "T"}); err != nil {
				t.Fatalf("Launch() error = %v", err)
			}
			if fake.runTaskInput == nil {
				t.Fatal("RunTask was not called")
			}
			got := fake.runTaskInput.NetworkConfiguration.AwsvpcConfiguration.AssignPublicIp
			if got != tc.wantAssign {
				t.Errorf("AssignPublicIp = %q, want %q", got, tc.wantAssign)
			}
		})
	}
}

func TestECSProvider_Launch_FailureIncludesArnAndDetail(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		runTaskOutput: &ecs.RunTaskOutput{
			Failures: []ecstypes.Failure{{
				Reason: aws.String("RESOURCE:ENI"),
				Arn:    aws.String("arn:aws:ecs:us-east-1:123:task/abc"),
				Detail: aws.String("no ENIs available in subnet"),
			}},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	_, err := p.Launch(context.Background(), LaunchOpts{})
	if err == nil {
		t.Fatal("Launch() error = nil, want non-nil")
	}
	msg := err.Error()
	for _, want := range []string{"RESOURCE:ENI", "arn:aws:ecs:us-east-1:123:task/abc", "no ENIs available in subnet"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to contain %q", msg, want)
		}
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

func TestECSProvider_Status_Provisioning(t *testing.T) {
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
	if result.State != "pending" {
		t.Errorf("State = %q, want \"pending\"", result.State)
	}
}

func TestECSProvider_Status_Pending(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{
				{LastStatus: aws.String("PENDING")},
			},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Status(context.Background(), "task-arn")
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if result.State != "pending" {
		t.Errorf("State = %q, want \"pending\"", result.State)
	}
}

func TestECSProvider_Status_Activating(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{
				{LastStatus: aws.String("ACTIVATING")},
			},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Status(context.Background(), "task-arn")
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if result.State != "pending" {
		t.Errorf("State = %q, want \"pending\"", result.State)
	}
}

func TestECSProvider_Status_Deprovisioning(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{
				{LastStatus: aws.String("DEPROVISIONING")},
			},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Status(context.Background(), "task-arn")
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if result.State != "stopping" {
		t.Errorf("State = %q, want \"stopping\"", result.State)
	}
}

func TestECSProvider_Status_Stopping(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{
				{LastStatus: aws.String("STOPPING")},
			},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Status(context.Background(), "task-arn")
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if result.State != "stopping" {
		t.Errorf("State = %q, want \"stopping\"", result.State)
	}
}

func TestECSProvider_Status_UnrecognizedStatus(t *testing.T) {
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{
				{LastStatus: aws.String("SOME_FUTURE_STATE")},
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

func TestECSProvider_Status_FailureMissing_ReturnsUnknown(t *testing.T) {
	// Per the Provider contract (see provider.go), a not-found instance
	// must surface as (&InstanceStatus{State: "unknown"}, nil) — matching
	// the Docker provider's behavior. AWS signals not-found with a
	// Failure.Reason of "MISSING".
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Failures: []ecstypes.Failure{{Reason: aws.String("MISSING")}},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	st, err := p.Status(context.Background(), "task-arn")
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if st == nil || st.State != "unknown" {
		t.Errorf("Status = %+v, want State=\"unknown\"", st)
	}
}

func TestECSProvider_Status_Failure_NonMissing_ReturnsError(t *testing.T) {
	// Any non-MISSING Failure must still surface as a real error.
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Failures: []ecstypes.Failure{{Reason: aws.String("ACCESS_DENIED")}},
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
	if !strings.Contains(err.Error(), "ACCESS_DENIED") {
		t.Errorf("error = %q, want it to contain \"ACCESS_DENIED\"", err.Error())
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
	if !strings.Contains(err.Error(), "unknown failure") {
		t.Errorf("error = %q, want it to contain \"unknown failure\"", err.Error())
	}
}

func TestECSProvider_Status_NoTasks_ReturnsUnknown(t *testing.T) {
	// Per the Provider contract, not-found must return the unknown
	// sentinel rather than an error.
	t.Parallel()
	fake := &fakeECSClient{
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks:    []ecstypes.Task{},
			Failures: []ecstypes.Failure{},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	st, err := p.Status(context.Background(), "task-arn")
	if err != nil {
		t.Fatalf("Status() error = %v, want nil", err)
	}
	if st == nil || st.State != "unknown" {
		t.Errorf("Status = %+v, want State=\"unknown\"", st)
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
	if len(fakeLogs.getLogEventsInputs) != 4 {
		t.Errorf("GetLogEvents called %d times, want 4 (3 poll + 1 drain)", len(fakeLogs.getLogEventsInputs))
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
	if len(fakeLogs.getLogEventsInputs) != 2 {
		t.Errorf("GetLogEvents called %d times, want 2 (1 poll + 1 drain)", len(fakeLogs.getLogEventsInputs))
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

func TestECSProvider_Logs_Follow_DrainAfterStopped(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			// Call 0 (main loop): one line, token advances
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("line before stop\n")}},
				NextForwardToken: aws.String("tok1"),
			},
			// Call 1 (main loop): no new data yet, same token
			{
				Events:           []cwltypes.OutputLogEvent{},
				NextForwardToken: aws.String("tok1"),
			},
			// Call 2 (drain, 1st): late-arriving line, token advances
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("late line 1\n")}},
				NextForwardToken: aws.String("tok2"),
			},
			// Call 3 (drain, 2nd): another late line, same token → drain exits
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("late line 2\n")}},
				NextForwardToken: aws.String("tok2"),
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
	want := "line before stop\nlate line 1\nlate line 2\n"
	if string(data) != want {
		t.Errorf("Logs output = %q, want %q", string(data), want)
	}
	if len(fakeLogs.getLogEventsInputs) != 4 {
		t.Errorf("GetLogEvents called %d times, want 4 (2 poll + 2 drain)", len(fakeLogs.getLogEventsInputs))
	}
	if len(fakeECS.describeTasksInputs) != 2 {
		t.Errorf("DescribeTasks called %d times, want 2", len(fakeECS.describeTasksInputs))
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

// TestECSProvider_Logs_Follow_DrainSurvivesContextCancel verifies that
// when the follow context is cancelled after STOPPED detection (e.g. the
// user hits Ctrl-C), the final-drain phase still completes and delivers
// late-arriving log events. Regression guard for horde-g86.
func TestECSProvider_Logs_Follow_DrainSurvivesContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Custom CloudWatch fake: cancels the follow ctx on the first drain
	// call, then continues to return late-arriving events. If the drain
	// loop used followCtx, the cancel would abort it and the late lines
	// would be lost.
	script := []*cloudwatchlogs.GetLogEventsOutput{
		// Call 0 (main loop): initial line, token advances.
		{
			Events:           []cwltypes.OutputLogEvent{{Message: aws.String("before stop\n")}},
			NextForwardToken: aws.String("tok1"),
		},
		// Call 1 (main loop): no new data, same token.
		{NextForwardToken: aws.String("tok1")},
		// Call 2 (drain, 1st): late line AND we cancel the follow ctx here.
		{
			Events:           []cwltypes.OutputLogEvent{{Message: aws.String("late 1\n")}},
			NextForwardToken: aws.String("tok2"),
		},
		// Call 3 (drain, 2nd): another late line, token advances.
		{
			Events:           []cwltypes.OutputLogEvent{{Message: aws.String("late 2\n")}},
			NextForwardToken: aws.String("tok3"),
		},
		// Call 4 (drain, 3rd): same token → drain exits.
		{NextForwardToken: aws.String("tok3")},
	}
	fakeLogs := &cancellingCWClient{script: script, cancelOnCall: 2, cancel: cancel}

	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("RUNNING")}}},
			{Tasks: []ecstypes.Task{{LastStatus: aws.String("STOPPED")}}},
		},
	}
	p := NewECSProvider(fakeECS, fakeLogs, &fakeS3Client{}, testHordeConfig())
	p.pollInterval = time.Millisecond

	reader, err := p.Logs(ctx, "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123", true)
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	want := "before stop\nlate 1\nlate 2\n"
	if string(data) != want {
		t.Errorf("Logs output = %q, want %q (drain must survive follow-ctx cancel)", string(data), want)
	}
}

type cancellingCWClient struct {
	script       []*cloudwatchlogs.GetLogEventsOutput
	cancelOnCall int
	cancel       context.CancelFunc
	calls        int
}

func (c *cancellingCWClient) GetLogEvents(ctx context.Context, params *cloudwatchlogs.GetLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	// Respect ctx cancellation (mirrors real AWS SDK behavior). This
	// ensures the regression test fails if the drain loop uses the
	// cancelled follow ctx instead of an independent drain ctx.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	idx := c.calls
	c.calls++
	if idx == c.cancelOnCall {
		c.cancel()
	}
	if idx < len(c.script) {
		return c.script[idx], nil
	}
	return &cloudwatchlogs.GetLogEventsOutput{}, nil
}

// TestECSProvider_Logs_Follow_DescribeFailuresResetOnSuccess verifies that
// the describeFailures counter resets after a successful DescribeTasks
// call, so that non-consecutive intermittent failures don't prematurely
// terminate follow mode. Regression guard for horde-5du.
func TestECSProvider_Logs_Follow_DescribeFailuresResetOnSuccess(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("alive\n")}},
				NextForwardToken: aws.String("tok1"),
			},
			// Subsequent calls return empty (default).
		},
	}
	transientErr := fmt.Errorf("ThrottlingException: Rate exceeded")

	// Pattern: 4 errors, 1 success (RUNNING), 4 errors, then STOPPED.
	// Without reset, cumulative failures hit 5 after call 6 and follow
	// terminates early. With reset, consecutive failures never hit 5,
	// so follow continues to the STOPPED signal on call 10.
	describeErrs := []error{
		transientErr, transientErr, transientErr, transientErr,
		nil, // success — must reset counter
		transientErr, transientErr, transientErr, transientErr,
		nil, // STOPPED delivered here
	}
	describeOuts := []*ecs.DescribeTasksOutput{
		nil, nil, nil, nil,
		{Tasks: []ecstypes.Task{{LastStatus: aws.String("RUNNING")}}},
		nil, nil, nil, nil,
		{Tasks: []ecstypes.Task{{LastStatus: aws.String("STOPPED")}}},
	}
	fakeECS := &fakeECSClient{
		describeTasksErrs:    describeErrs,
		describeTasksOutputs: describeOuts,
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
	if !strings.Contains(string(data), "alive") {
		t.Errorf("data = %q, want it to contain \"alive\"", string(data))
	}
	if strings.Contains(string(data), "unable to determine task completion") {
		t.Errorf("data = %q, follow must not terminate early (counter should reset on success)", string(data))
	}
	// Must reach call #10 (STOPPED) — if follow terminated early at call 5,
	// DescribeTasks would stop being invoked.
	if len(fakeECS.describeTasksInputs) < 10 {
		t.Errorf("DescribeTasks calls = %d, want >= 10 (follow terminated prematurely)", len(fakeECS.describeTasksInputs))
	}
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

func TestECSProvider_Logs_Follow_DrainTransientErrorRetrySucceeds(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			// Call 0 (poll): line delivered, token advances
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("line before stop\n")}},
				NextForwardToken: aws.String("tok1"),
			},
			// Call 1 (drain, first attempt): output ignored — error takes precedence
			{},
			// Call 2 (drain, retry): late line delivered, token advances
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("late line\n")}},
				NextForwardToken: aws.String("tok2"),
			},
			// Call 3 (drain, next iteration): empty + same token → drain exits
			{
				Events:           []cwltypes.OutputLogEvent{},
				NextForwardToken: aws.String("tok2"),
			},
		},
		getLogEventsErrs: []error{nil, fmt.Errorf("ThrottlingException: rate exceeded")},
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
		t.Fatalf("ReadAll() error = %v, want nil", err)
	}
	if !strings.Contains(string(data), "line before stop") {
		t.Errorf("data = %q, want it to contain \"line before stop\"", string(data))
	}
	if !strings.Contains(string(data), "late line") {
		t.Errorf("data = %q, want it to contain \"late line\"", string(data))
	}
	if strings.Contains(string(data), "WARNING") {
		t.Errorf("data = %q, want no WARNING", string(data))
	}
	if len(fakeLogs.getLogEventsInputs) != 4 {
		t.Errorf("GetLogEvents called %d times, want 4 (1 poll + 1 failed drain + 1 retry + 1 drain-exit)", len(fakeLogs.getLogEventsInputs))
	}
}

func TestECSProvider_Logs_Follow_DrainTransientErrorRetryFails(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			// Call 0 (poll): line delivered, token advances
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("line before stop\n")}},
				NextForwardToken: aws.String("tok1"),
			},
		},
		getLogEventsErrs: []error{
			nil,
			fmt.Errorf("ThrottlingException: rate exceeded"),
			fmt.Errorf("ThrottlingException: rate exceeded"),
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
	if err == nil {
		t.Fatal("ReadAll() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "reading logs") {
		t.Errorf("error = %q, want it to contain \"reading logs\"", err.Error())
	}
	if !strings.Contains(string(data), "line before stop") {
		t.Errorf("data = %q, want it to contain \"line before stop\"", string(data))
	}
	if !strings.Contains(string(data), "WARNING: output may be incomplete") {
		t.Errorf("data = %q, want it to contain \"WARNING: output may be incomplete\"", string(data))
	}
	if len(fakeLogs.getLogEventsInputs) != 3 {
		t.Errorf("GetLogEvents called %d times, want 3 (1 poll + 1 failed drain + 1 failed retry)", len(fakeLogs.getLogEventsInputs))
	}
}

func TestECSProvider_Logs_Follow_DrainRNFBreaksCleanly(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			// Call 0 (poll): line delivered, token advances
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("line before stop\n")}},
				NextForwardToken: aws.String("tok1"),
			},
		},
		getLogEventsErrs: []error{
			nil,
			&cwltypes.ResourceNotFoundException{Message: aws.String("log stream not found")},
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
		t.Fatalf("ReadAll() error = %v, want nil", err)
	}
	if !strings.Contains(string(data), "line before stop") {
		t.Errorf("data = %q, want it to contain \"line before stop\"", string(data))
	}
	if strings.Contains(string(data), "WARNING") {
		t.Errorf("data = %q, want no WARNING", string(data))
	}
	if len(fakeLogs.getLogEventsInputs) != 2 {
		t.Errorf("GetLogEvents called %d times, want 2 (1 poll + 1 drain with RNF)", len(fakeLogs.getLogEventsInputs))
	}
}

// TestECSProvider_Logs_Follow_DescribeTasksNilResponse verifies the
// nil-guard in the follow-mode poll loop when DescribeTasks returns
// (nil, nil). The goroutine must not panic; it treats nil as "no
// information this tick" and continues polling until context is
// cancelled. Covers horde-l2g.
func TestECSProvider_Logs_Follow_DescribeTasksNilResponse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("line 1\n")}},
				NextForwardToken: aws.String("tok1"),
			},
			// Subsequent calls default to empty output.
		},
	}
	// describeTasksOutputs are all nil — each poll tick's DescribeTasks
	// returns (nil, nil). Follow must keep running (no panic, no
	// premature stop) until we cancel.
	fakeECS := &fakeECSClient{
		describeTasksOutputs: []*ecs.DescribeTasksOutput{nil, nil, nil},
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
	if !strings.Contains(string(buf[:n]), "line 1") {
		t.Errorf("Read() = %q, want it to contain \"line 1\"", string(buf[:n]))
	}

	// Let the goroutine iterate a few times with nil DescribeTasks
	// before we cancel. If the nil guard were missing, any of these
	// iterations would panic.
	time.Sleep(10 * time.Millisecond)

	cancel()
	_, _ = io.ReadAll(reader)
	reader.Close()

	if len(fakeECS.describeTasksInputs) < 1 {
		t.Error("DescribeTasks was never called")
	}
}

// TestECSProvider_Logs_Follow_DrainNilResponse verifies the nil-output
// guard in the drain loop after STOPPED detection. If GetLogEvents
// returns (nil, nil) during drain, the goroutine must exit cleanly
// without panic. Covers horde-dtv.
func TestECSProvider_Logs_Follow_DrainNilResponse(t *testing.T) {
	t.Parallel()
	fakeLogs := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			// Call 0 (poll): one line.
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("before stop\n")}},
				NextForwardToken: aws.String("tok1"),
			},
			// Call 1 (drain): nil response must not panic; goroutine must exit.
			nil,
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
	if !strings.Contains(string(data), "before stop") {
		t.Errorf("data = %q, want it to contain \"before stop\"", string(data))
	}
	if strings.Contains(string(data), "WARNING") {
		t.Errorf("data = %q, want no WARNING on nil drain response", string(data))
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

// TestECSProvider_ReadFile_PathTraversal verifies defense-in-depth
// rejection of path traversal attempts in opts.Path after the .orc/
// prefix check. Covers horde-t9i.
func TestECSProvider_ReadFile_PathTraversal(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())

	cases := []struct {
		name string
		path string
	}{
		{"escapes up", ".orc/../secret"},
		{"escapes deeper", ".orc/audit/../../other/secret"},
		{"absolute root", ".orc//etc/passwd"},
		{"dot-only relpath", ".orc/."},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.ReadFile(context.Background(), ReadFileOpts{
				RunID:    "run-001",
				Path:     tc.path,
				Metadata: map[string]string{"artifacts_bucket": "my-bucket"},
			})
			if err == nil {
				t.Fatal("ReadFile() error = nil, want non-nil")
			}
			if !strings.Contains(err.Error(), "escapes") {
				t.Errorf("ReadFile(%q) error = %q, want it to contain \"escapes\"", tc.path, err.Error())
			}
		})
	}
}

func TestECSProvider_ReadFile_RunIDTraversal(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())

	cases := []struct {
		name  string
		runID string
	}{
		{"dot-dot", "../../other-user/secret"},
		{"forward-slash", "foo/bar"},
		{"backslash", "foo\\bar"},
		{"dot-dot-only", ".."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.ReadFile(context.Background(), ReadFileOpts{
				RunID:    tc.runID,
				Path:     ".orc/audit/foo.json",
				Metadata: map[string]string{"artifacts_bucket": "my-bucket"},
			})
			if err == nil {
				t.Fatal("ReadFile() error = nil, want non-nil")
			}
			if !strings.Contains(err.Error(), "invalid run ID") {
				t.Errorf("ReadFile() error = %q, want it to contain \"invalid run ID\"", err.Error())
			}
		})
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

func TestECSProvider_Finalize_NoOp(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	run := &store.Run{
		ID:         "abc123def456",
		InstanceID: "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123",
		Status:     store.StatusRunning,
	}
	err := p.Finalize(context.Background(), run, "/home/testuser")
	if err != nil {
		t.Fatalf("Finalize() error = %v, want nil", err)
	}
	if run.Status != store.StatusRunning {
		t.Errorf("run.Status = %q, want %q (Finalize must not mutate run for ECS)", run.Status, store.StatusRunning)
	}
	if run.CompletedAt != nil {
		t.Errorf("run.CompletedAt = %v, want nil", run.CompletedAt)
	}
}

func TestECSProvider_HydrateRun_SuccessDefaultWorkflow(t *testing.T) {
	t.Parallel()

	// Default workflow: S3 keys under horde-runs/<id>/audit/<ticket>/...
	fake := &fakeS3Client{
		listKeys: map[string][]string{
			"horde-runs/abc123/audit/PROJ-1/":     {"horde-runs/abc123/audit/PROJ-1/run-result.json", "horde-runs/abc123/audit/PROJ-1/nested/timing.json"},
			"horde-runs/abc123/artifacts/PROJ-1/": {"horde-runs/abc123/artifacts/PROJ-1/output.txt"},
		},
		getObjectByKey: map[string]string{
			"horde-runs/abc123/audit/PROJ-1/run-result.json":    `{"ok":true}`,
			"horde-runs/abc123/audit/PROJ-1/nested/timing.json": `{"phase":"plan"}`,
			"horde-runs/abc123/artifacts/PROJ-1/output.txt":     "bytes",
		},
	}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())

	destBase := t.TempDir()
	err := p.HydrateRun(context.Background(), HydrateOpts{
		RunID:            "abc123",
		Ticket:           "PROJ-1",
		Metadata:         map[string]string{"artifacts_bucket": "my-horde-artifacts"},
		DestAuditDir:     filepath.Join(destBase, "audit"),
		DestArtifactsDir: filepath.Join(destBase, "artifacts"),
	})
	if err != nil {
		t.Fatalf("HydrateRun: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destBase, "audit", "run-result.json"))
	if err != nil || string(got) != `{"ok":true}` {
		t.Errorf("audit: got %q err=%v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(destBase, "audit", "nested", "timing.json"))
	if err != nil || string(got) != `{"phase":"plan"}` {
		t.Errorf("nested audit: got %q err=%v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(destBase, "artifacts", "output.txt"))
	if err != nil || string(got) != "bytes" {
		t.Errorf("artifacts: got %q err=%v", got, err)
	}
}

func TestECSProvider_HydrateRun_SuccessNamedWorkflow(t *testing.T) {
	t.Parallel()

	// Named workflow: S3 keys under horde-runs/<id>/audit/<workflow>/<ticket>/...
	fake := &fakeS3Client{
		listKeys: map[string][]string{
			"horde-runs/abc123/audit/review/PROJ-1/": {"horde-runs/abc123/audit/review/PROJ-1/run-result.json"},
		},
		getObjectByKey: map[string]string{
			"horde-runs/abc123/audit/review/PROJ-1/run-result.json": `{"ok":true}`,
		},
	}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())

	destBase := t.TempDir()
	err := p.HydrateRun(context.Background(), HydrateOpts{
		RunID:            "abc123",
		Workflow:         "review",
		Ticket:           "PROJ-1",
		Metadata:         map[string]string{"artifacts_bucket": "my-horde-artifacts"},
		DestAuditDir:     filepath.Join(destBase, "audit"),
		DestArtifactsDir: filepath.Join(destBase, "artifacts"),
	})
	if err != nil {
		t.Fatalf("HydrateRun: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destBase, "audit", "run-result.json"))
	if err != nil || string(got) != `{"ok":true}` {
		t.Errorf("audit: got %q err=%v", got, err)
	}
}

func TestECSProvider_HydrateRun_NoObjects(t *testing.T) {
	t.Parallel()

	fake := &fakeS3Client{listKeys: map[string][]string{}}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())

	destBase := t.TempDir()
	err := p.HydrateRun(context.Background(), HydrateOpts{
		RunID:            "abc123",
		Ticket:           "PROJ-1",
		Metadata:         map[string]string{"artifacts_bucket": "my-horde-artifacts"},
		DestAuditDir:     filepath.Join(destBase, "audit"),
		DestArtifactsDir: filepath.Join(destBase, "artifacts"),
	})
	var nf *FileNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want *FileNotFoundError when both prefixes empty, got: %v", err)
	}
}

func TestECSProvider_HydrateRun_MissingBucket(t *testing.T) {
	t.Parallel()

	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	destBase := t.TempDir()
	err := p.HydrateRun(context.Background(), HydrateOpts{
		RunID:            "abc123",
		Ticket:           "PROJ-1",
		Metadata:         nil,
		DestAuditDir:     filepath.Join(destBase, "audit"),
		DestArtifactsDir: filepath.Join(destBase, "artifacts"),
	})
	if err == nil || !strings.Contains(err.Error(), "artifacts_bucket") {
		t.Fatalf("expected bucket-missing error, got: %v", err)
	}
}

func TestECSProvider_HydrateRun_InvalidRunID(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	for _, bad := range []string{"", "../etc", "a/b", "a\\b"} {
		err := p.HydrateRun(context.Background(), HydrateOpts{
			RunID:            bad,
			Ticket:           "PROJ-1",
			Metadata:         map[string]string{"artifacts_bucket": "b"},
			DestAuditDir:     "/tmp/x/a",
			DestArtifactsDir: "/tmp/x/b",
		})
		if err == nil {
			t.Errorf("run id %q should be rejected", bad)
		}
	}
}

func TestECSProvider_HydrateRun_InvalidTicketOrWorkflow(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	cases := []HydrateOpts{
		{RunID: "abc123", Ticket: "", Metadata: map[string]string{"artifacts_bucket": "b"}, DestAuditDir: "/tmp/x/a", DestArtifactsDir: "/tmp/x/b"},
		{RunID: "abc123", Ticket: "../etc", Metadata: map[string]string{"artifacts_bucket": "b"}, DestAuditDir: "/tmp/x/a", DestArtifactsDir: "/tmp/x/b"},
		{RunID: "abc123", Ticket: "a/b", Metadata: map[string]string{"artifacts_bucket": "b"}, DestAuditDir: "/tmp/x/a", DestArtifactsDir: "/tmp/x/b"},
		{RunID: "abc123", Ticket: "PROJ-1", Workflow: "../flow", Metadata: map[string]string{"artifacts_bucket": "b"}, DestAuditDir: "/tmp/x/a", DestArtifactsDir: "/tmp/x/b"},
		{RunID: "abc123", Ticket: "PROJ-1", Workflow: "a/b", Metadata: map[string]string{"artifacts_bucket": "b"}, DestAuditDir: "/tmp/x/a", DestArtifactsDir: "/tmp/x/b"},
	}
	for i, opts := range cases {
		if err := p.HydrateRun(context.Background(), opts); err == nil {
			t.Errorf("case %d: bad ticket/workflow should be rejected: %+v", i, opts)
		}
	}
}

// --- Cross-method integration tests (horde-vo2) ---
//
// These tests verify that the InstanceID (task ARN) returned by Launch
// flows correctly through Status, Logs, and Stop. They use the fake
// ECS/CloudWatch clients to detect interface contract drift between
// methods — e.g. a Launch that returns an ARN in one format which Logs
// cannot parse, or a Status call that looks up the wrong cluster.

func TestECSProvider_Integration_LaunchThenStatus(t *testing.T) {
	t.Parallel()
	const taskARN = "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123"
	fake := &fakeECSClient{
		runTaskOutput: &ecs.RunTaskOutput{
			Tasks: []ecstypes.Task{{TaskArn: aws.String(taskARN)}},
		},
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{{
				TaskArn:    aws.String(taskARN),
				LastStatus: aws.String("RUNNING"),
				StartedAt:  aws.Time(time.Now()),
			}},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())

	res, err := p.Launch(context.Background(), LaunchOpts{RunID: "abc123", Ticket: "PROJ-1"})
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	if res.InstanceID != taskARN {
		t.Fatalf("Launch InstanceID = %q, want %q", res.InstanceID, taskARN)
	}

	st, err := p.Status(context.Background(), res.InstanceID)
	if err != nil {
		t.Fatalf("Status(%q) error = %v", res.InstanceID, err)
	}
	if st.State != "running" {
		t.Errorf("Status.State = %q, want %q", st.State, "running")
	}
	if fake.describeTasksInput == nil {
		t.Fatal("DescribeTasks was not called")
	}
	if len(fake.describeTasksInput.Tasks) != 1 || fake.describeTasksInput.Tasks[0] != taskARN {
		t.Errorf("DescribeTasks.Tasks = %v, want [%q]", fake.describeTasksInput.Tasks, taskARN)
	}
	if aws.ToString(fake.describeTasksInput.Cluster) != testHordeConfig().ClusterARN {
		t.Errorf("DescribeTasks.Cluster = %q, want %q", aws.ToString(fake.describeTasksInput.Cluster), testHordeConfig().ClusterARN)
	}
}

func TestECSProvider_Integration_LaunchThenLogs(t *testing.T) {
	t.Parallel()
	const taskARN = "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123"
	const taskID = "abc123"
	token := "t1"
	ecsFake := &fakeECSClient{
		runTaskOutput: &ecs.RunTaskOutput{
			Tasks: []ecstypes.Task{{TaskArn: aws.String(taskARN)}},
		},
	}
	cwFake := &fakeCloudWatchLogsClient{
		getLogEventsOutputs: []*cloudwatchlogs.GetLogEventsOutput{
			{
				Events:           []cwltypes.OutputLogEvent{{Message: aws.String("hello\n")}},
				NextForwardToken: aws.String(token),
			},
			{
				Events:           []cwltypes.OutputLogEvent{},
				NextForwardToken: aws.String(token),
			},
		},
	}
	p := NewECSProvider(ecsFake, cwFake, &fakeS3Client{}, testHordeConfig())

	res, err := p.Launch(context.Background(), LaunchOpts{RunID: taskID, Ticket: "PROJ-1"})
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}

	reader, err := p.Logs(context.Background(), res.InstanceID, false)
	if err != nil {
		t.Fatalf("Logs(%q) error = %v", res.InstanceID, err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("Logs output = %q, want %q", string(data), "hello\n")
	}
	if len(cwFake.getLogEventsInputs) < 1 {
		t.Fatal("GetLogEvents was not called")
	}
	wantStream := "ecs/horde-worker/" + taskID
	if got := aws.ToString(cwFake.getLogEventsInputs[0].LogStreamName); got != wantStream {
		t.Errorf("LogStreamName = %q, want %q (Logs must accept the ARN returned by Launch and derive the task ID correctly)", got, wantStream)
	}
}

func TestECSProvider_Integration_LaunchStopStatus(t *testing.T) {
	t.Parallel()
	const taskARN = "arn:aws:ecs:us-east-1:123456789012:task/horde/abc123"
	fake := &fakeECSClient{
		runTaskOutput: &ecs.RunTaskOutput{
			Tasks: []ecstypes.Task{{TaskArn: aws.String(taskARN)}},
		},
		describeTasksOutput: &ecs.DescribeTasksOutput{
			Tasks: []ecstypes.Task{{
				TaskArn:    aws.String(taskARN),
				LastStatus: aws.String("STOPPED"),
				StoppedAt:  aws.Time(time.Now()),
				Containers: []ecstypes.Container{{ExitCode: aws.Int32(0)}},
			}},
		},
	}
	p := NewECSProvider(fake, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())

	res, err := p.Launch(context.Background(), LaunchOpts{RunID: "abc123", Ticket: "PROJ-1"})
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}

	if err := p.Stop(context.Background(), StopOpts{InstanceID: res.InstanceID}); err != nil {
		t.Fatalf("Stop(%q) error = %v", res.InstanceID, err)
	}
	if fake.stopTaskInput == nil {
		t.Fatal("StopTask was not called")
	}
	if aws.ToString(fake.stopTaskInput.Task) != taskARN {
		t.Errorf("StopTask.Task = %q, want %q", aws.ToString(fake.stopTaskInput.Task), taskARN)
	}

	st, err := p.Status(context.Background(), res.InstanceID)
	if err != nil {
		t.Fatalf("Status(%q) error = %v", res.InstanceID, err)
	}
	if st.State != "stopped" {
		t.Errorf("Status.State = %q, want %q", st.State, "stopped")
	}
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Errorf("Status.ExitCode = %v, want 0", st.ExitCode)
	}
}
