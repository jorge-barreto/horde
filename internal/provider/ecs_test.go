package provider

import (
	"context"
	"strings"
	"testing"

	cloudwatchlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jorge-barreto/horde/internal/config"
)

type fakeECSClient struct{}

func (f *fakeECSClient) RunTask(ctx context.Context, params *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	return nil, nil
}
func (f *fakeECSClient) DescribeTasks(ctx context.Context, params *ecs.DescribeTasksInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	return nil, nil
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

func TestECSProvider_Launch_Stub(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Launch(context.Background(), LaunchOpts{})
	if result != nil {
		t.Errorf("Launch() result = %v, want nil", result)
	}
	if err == nil {
		t.Fatal("Launch() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("Launch() error = %q, want it to contain \"not implemented\"", err.Error())
	}
}

func TestECSProvider_Status_Stub(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	result, err := p.Status(context.Background(), "")
	if result != nil {
		t.Errorf("Status() result = %v, want nil", result)
	}
	if err == nil {
		t.Fatal("Status() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("Status() error = %q, want it to contain \"not implemented\"", err.Error())
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
