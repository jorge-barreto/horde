package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	smithy "github.com/aws/smithy-go"
)

func validHordeFields() map[string]interface{} {
	return map[string]interface{}{
		"cluster_arn":             "arn:aws:ecs:us-east-1:123456789012:cluster/horde",
		"task_definition_arn":     "arn:aws:ecs:us-east-1:123456789012:task-definition/horde-worker:1",
		"subnets":                 []string{"subnet-abc", "subnet-def"},
		"security_group":          "sg-123",
		"log_group":               "/ecs/horde-worker",
		"artifacts_bucket":        "my-horde-artifacts",
		"runs_table":              "horde-runs",
		"max_concurrent":          5,
		"default_timeout_minutes": 60,
	}
}

func marshalFields(t *testing.T, fields map[string]interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshaling fields: %v", err)
	}
	return data
}

type fakeSSMClient struct {
	output *ssm.GetParameterOutput
	err    error
}

func (f *fakeSSMClient) GetParameter(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	return f.output, f.err
}

func TestLoadFromSSM_Success(t *testing.T) {
	t.Parallel()
	fields := validHordeFields()
	data := marshalFields(t, fields)
	val := string(data)
	client := &fakeSSMClient{
		output: &ssm.GetParameterOutput{
			Parameter: &ssmtypes.Parameter{Value: aws.String(val)},
		},
	}
	cfg, err := LoadFromSSM(context.Background(), client, "/horde/config")
	if err != nil {
		t.Fatalf("LoadFromSSM() unexpected error: %v", err)
	}
	if cfg.ClusterARN != "arn:aws:ecs:us-east-1:123456789012:cluster/horde" {
		t.Errorf("ClusterARN = %q, want %q", cfg.ClusterARN, "arn:aws:ecs:us-east-1:123456789012:cluster/horde")
	}
	if cfg.MaxConcurrent != 5 {
		t.Errorf("MaxConcurrent = %d, want 5", cfg.MaxConcurrent)
	}
}

func TestLoadFromSSM_NotFound(t *testing.T) {
	t.Parallel()
	client := &fakeSSMClient{
		err: &ssmtypes.ParameterNotFound{Message: aws.String("not found")},
	}
	_, err := LoadFromSSM(context.Background(), client, "/horde/config")
	if err == nil {
		t.Fatal("LoadFromSSM() expected error, got nil")
	}
	var target *NotFoundError
	if !errors.As(err, &target) {
		t.Errorf("expected *NotFoundError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "/horde/config") {
		t.Errorf("error = %q, want it to contain path", err.Error())
	}
}

func TestLoadFromSSM_AccessDenied(t *testing.T) {
	t.Parallel()
	client := &fakeSSMClient{
		err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "User is not authorized"},
	}
	_, err := LoadFromSSM(context.Background(), client, "/horde/config")
	if err == nil {
		t.Fatal("LoadFromSSM() expected error, got nil")
	}
	var target *AccessDeniedError
	if !errors.As(err, &target) {
		t.Errorf("expected *AccessDeniedError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "access denied")
	}
}

func TestLoadFromSSM_ParseError_InvalidJSON(t *testing.T) {
	t.Parallel()
	client := &fakeSSMClient{
		output: &ssm.GetParameterOutput{
			Parameter: &ssmtypes.Parameter{Value: aws.String("{not json}")},
		},
	}
	_, err := LoadFromSSM(context.Background(), client, "/horde/config")
	if err == nil {
		t.Fatal("LoadFromSSM() expected error, got nil")
	}
	var target *ParseError
	if !errors.As(err, &target) {
		t.Errorf("expected *ParseError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "/horde/config") {
		t.Errorf("error = %q, want it to contain path", err.Error())
	}
}

func TestLoadFromSSM_ParseError_ValidationFailure(t *testing.T) {
	t.Parallel()
	fields := validHordeFields()
	fields["max_concurrent"] = 0
	data := marshalFields(t, fields)
	client := &fakeSSMClient{
		output: &ssm.GetParameterOutput{
			Parameter: &ssmtypes.Parameter{Value: aws.String(string(data))},
		},
	}
	_, err := LoadFromSSM(context.Background(), client, "/horde/config")
	if err == nil {
		t.Fatal("LoadFromSSM() expected error, got nil")
	}
	var target *ParseError
	if !errors.As(err, &target) {
		t.Errorf("expected *ParseError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "max_concurrent must be at least 1") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "max_concurrent must be at least 1")
	}
}

func TestLoadFromSSM_NilValue(t *testing.T) {
	t.Parallel()
	client := &fakeSSMClient{
		output: &ssm.GetParameterOutput{
			Parameter: &ssmtypes.Parameter{},
		},
	}
	_, err := LoadFromSSM(context.Background(), client, "/horde/config")
	if err == nil {
		t.Fatal("LoadFromSSM() expected error, got nil")
	}
	var target *ParseError
	if !errors.As(err, &target) {
		t.Errorf("expected *ParseError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "nil")
	}
}

func TestLoadFromSSM_UnclassifiedError(t *testing.T) {
	t.Parallel()
	client := &fakeSSMClient{
		err: errors.New("connection refused"),
	}
	_, err := LoadFromSSM(context.Background(), client, "/horde/config")
	if err == nil {
		t.Fatal("LoadFromSSM() expected error, got nil")
	}
	var nf *NotFoundError
	if errors.As(err, &nf) {
		t.Errorf("unexpected *NotFoundError")
	}
	var ad *AccessDeniedError
	if errors.As(err, &ad) {
		t.Errorf("unexpected *AccessDeniedError")
	}
	var pe *ParseError
	if errors.As(err, &pe) {
		t.Errorf("unexpected *ParseError")
	}
	if !strings.Contains(err.Error(), "reading ssm parameter") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "reading ssm parameter")
	}
	if !strings.Contains(err.Error(), "/horde/config") {
		t.Errorf("error = %q, want it to contain path", err.Error())
	}
}

func TestParseHordeConfig_Valid(t *testing.T) {
	t.Parallel()
	cfg, err := ParseHordeConfig(marshalFields(t, validHordeFields()))
	if err != nil {
		t.Fatalf("ParseHordeConfig() error: %v", err)
	}
	if cfg.ClusterARN != "arn:aws:ecs:us-east-1:123456789012:cluster/horde" {
		t.Errorf("ClusterARN = %q, want %q", cfg.ClusterARN, "arn:aws:ecs:us-east-1:123456789012:cluster/horde")
	}
	if cfg.TaskDefinitionARN != "arn:aws:ecs:us-east-1:123456789012:task-definition/horde-worker:1" {
		t.Errorf("TaskDefinitionARN = %q, want %q", cfg.TaskDefinitionARN, "arn:aws:ecs:us-east-1:123456789012:task-definition/horde-worker:1")
	}
	if len(cfg.Subnets) != 2 {
		t.Fatalf("Subnets length = %d, want 2", len(cfg.Subnets))
	}
	if cfg.Subnets[0] != "subnet-abc" {
		t.Errorf("Subnets[0] = %q, want %q", cfg.Subnets[0], "subnet-abc")
	}
	if cfg.Subnets[1] != "subnet-def" {
		t.Errorf("Subnets[1] = %q, want %q", cfg.Subnets[1], "subnet-def")
	}
	if cfg.SecurityGroup != "sg-123" {
		t.Errorf("SecurityGroup = %q, want %q", cfg.SecurityGroup, "sg-123")
	}
	if cfg.LogGroup != "/ecs/horde-worker" {
		t.Errorf("LogGroup = %q, want %q", cfg.LogGroup, "/ecs/horde-worker")
	}
	if cfg.ArtifactsBucket != "my-horde-artifacts" {
		t.Errorf("ArtifactsBucket = %q, want %q", cfg.ArtifactsBucket, "my-horde-artifacts")
	}
	if cfg.RunsTable != "horde-runs" {
		t.Errorf("RunsTable = %q, want %q", cfg.RunsTable, "horde-runs")
	}
	if cfg.MaxConcurrent != 5 {
		t.Errorf("MaxConcurrent = %d, want 5", cfg.MaxConcurrent)
	}
	if cfg.DefaultTimeoutMinutes != 60 {
		t.Errorf("DefaultTimeoutMinutes = %d, want 60", cfg.DefaultTimeoutMinutes)
	}
}

func TestParseHordeConfig_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg := HordeConfig{
		ClusterARN:            "arn:aws:ecs:us-east-1:123:cluster/horde",
		TaskDefinitionARN:     "arn:aws:ecs:us-east-1:123:task-definition/horde-worker:1",
		Subnets:               []string{"subnet-abc", "subnet-def"},
		SecurityGroup:         "sg-123",
		LogGroup:              "/ecs/horde-worker",
		ArtifactsBucket:       "my-horde-artifacts",
		RunsTable:             "horde-runs",
		MaxConcurrent:         5,
		DefaultTimeoutMinutes: 60,
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal() error: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal() error: %v", err)
	}
	wantKeys := []string{
		"cluster_arn", "task_definition_arn", "subnets", "security_group",
		"log_group", "artifacts_bucket", "runs_table", "max_concurrent",
		"default_timeout_minutes",
	}
	for _, key := range wantKeys {
		if _, ok := m[key]; !ok {
			t.Errorf("JSON output missing key %q", key)
		}
	}
}

func TestParseHordeConfig_MissingFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(map[string]interface{})
		wantErr []string
	}{
		{
			name:    "missing cluster_arn",
			mutate:  func(m map[string]interface{}) { delete(m, "cluster_arn") },
			wantErr: []string{"missing required fields: cluster_arn"},
		},
		{
			name:    "missing task_definition_arn",
			mutate:  func(m map[string]interface{}) { delete(m, "task_definition_arn") },
			wantErr: []string{"missing required fields: task_definition_arn"},
		},
		{
			name:    "missing subnets",
			mutate:  func(m map[string]interface{}) { delete(m, "subnets") },
			wantErr: []string{"missing required fields: subnets"},
		},
		{
			name:    "empty subnets",
			mutate:  func(m map[string]interface{}) { m["subnets"] = []string{} },
			wantErr: []string{"missing required fields: subnets"},
		},
		{
			name:    "missing security_group",
			mutate:  func(m map[string]interface{}) { delete(m, "security_group") },
			wantErr: []string{"missing required fields: security_group"},
		},
		{
			name:    "missing log_group",
			mutate:  func(m map[string]interface{}) { delete(m, "log_group") },
			wantErr: []string{"missing required fields: log_group"},
		},
		{
			name:    "missing artifacts_bucket",
			mutate:  func(m map[string]interface{}) { delete(m, "artifacts_bucket") },
			wantErr: []string{"missing required fields: artifacts_bucket"},
		},
		{
			name:    "missing runs_table",
			mutate:  func(m map[string]interface{}) { delete(m, "runs_table") },
			wantErr: []string{"missing required fields: runs_table"},
		},
		{
			name: "multiple missing",
			mutate: func(m map[string]interface{}) {
				delete(m, "cluster_arn")
				delete(m, "log_group")
			},
			wantErr: []string{"cluster_arn", "log_group"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fields := validHordeFields()
			tc.mutate(fields)
			_, err := ParseHordeConfig(marshalFields(t, fields))
			if err == nil {
				t.Fatalf("ParseHordeConfig() expected error, got nil")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestParseHordeConfig_InvalidInts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		field   string
		value   int
		wantErr string
	}{
		{"max_concurrent zero", "max_concurrent", 0, "max_concurrent must be at least 1"},
		{"max_concurrent negative", "max_concurrent", -1, "max_concurrent must be at least 1"},
		{"default_timeout_minutes zero", "default_timeout_minutes", 0, "default_timeout_minutes must be at least 1"},
		{"default_timeout_minutes negative", "default_timeout_minutes", -5, "default_timeout_minutes must be at least 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fields := validHordeFields()
			fields[tc.field] = tc.value
			_, err := ParseHordeConfig(marshalFields(t, fields))
			if err == nil {
				t.Fatalf("ParseHordeConfig() expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestParseHordeConfig_InvalidJSON(t *testing.T) {
	t.Parallel()
	_, err := ParseHordeConfig([]byte("{not json"))
	if err == nil {
		t.Fatal("ParseHordeConfig() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "parsing horde config") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "parsing horde config")
	}
}

func TestParseHordeConfig_ExtraFieldsIgnored(t *testing.T) {
	t.Parallel()
	fields := validHordeFields()
	fields["new_future_field"] = "value"
	_, err := ParseHordeConfig(marshalFields(t, fields))
	if err != nil {
		t.Fatalf("ParseHordeConfig() with extra field error: %v", err)
	}
}

func TestDiagnostic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		want   []string
		noWant []string
	}{
		{
			name: "not found",
			err:  &NotFoundError{Path: "/horde/config", Err: fmt.Errorf("underlying")},
			want: []string{"/horde/config", "deploy the @horde/cdk construct"},
		},
		{
			name: "access denied",
			err:  &AccessDeniedError{Path: "/horde/config", Err: fmt.Errorf("underlying")},
			want: []string{"/horde/config", "attach the horde CLI user managed policy", "@horde/cdk construct"},
		},
		{
			name: "parse error",
			err:  &ParseError{Path: "/horde/config", Err: fmt.Errorf("invalid json")},
			want: []string{"/horde/config", "malformed", "Re-deploy", "invalid json"},
		},
		{
			name:   "unrecognized error",
			err:    fmt.Errorf("connection refused"),
			want:   []string{"connection refused"},
			noWant: []string{"hint:"},
		},
		{
			name: "wrapped not found",
			err:  fmt.Errorf("wrapped: %w", &NotFoundError{Path: "/horde/config", Err: fmt.Errorf("underlying")}),
			want: []string{"/horde/config", "deploy the @horde/cdk construct"},
		},
		{
			name: "wrapped access denied",
			err:  fmt.Errorf("wrapped: %w", &AccessDeniedError{Path: "/horde/config", Err: fmt.Errorf("underlying")}),
			want: []string{"/horde/config", "attach the horde CLI user managed policy", "@horde/cdk construct"},
		},
		{
			name: "wrapped parse error",
			err:  fmt.Errorf("wrapped: %w", &ParseError{Path: "/horde/config", Err: fmt.Errorf("invalid json")}),
			want: []string{"/horde/config", "malformed", "Re-deploy", "invalid json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Diagnostic(tc.err)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("Diagnostic() = %q, want it to contain %q", got, w)
				}
			}
			for _, nw := range tc.noWant {
				if strings.Contains(got, nw) {
					t.Errorf("Diagnostic() = %q, should not contain %q", got, nw)
				}
			}
		})
	}
	t.Run("nil error", func(t *testing.T) {
		t.Parallel()
		got := Diagnostic(nil)
		if got != "" {
			t.Errorf("Diagnostic(nil) = %q, want empty string", got)
		}
	})
}

func TestHordeConfig_Validate_AllFieldsPresent(t *testing.T) {
	t.Parallel()
	cfg := HordeConfig{
		ClusterARN:            "arn:aws:ecs:us-east-1:123:cluster/horde",
		TaskDefinitionARN:     "arn:aws:ecs:us-east-1:123:task-definition/horde-worker:1",
		Subnets:               []string{"subnet-abc"},
		SecurityGroup:         "sg-123",
		LogGroup:              "/ecs/horde-worker",
		ArtifactsBucket:       "my-bucket",
		RunsTable:             "horde-runs",
		MaxConcurrent:         1,
		DefaultTimeoutMinutes: 1,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error: %v", err)
	}
}
