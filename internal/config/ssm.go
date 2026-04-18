package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	smithy "github.com/aws/smithy-go"
)

const DefaultSSMPath = "/horde/config"

// SSMClient is the subset of the SSM API used by LoadFromSSM.
type SSMClient interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// NotFoundError is returned when the SSM parameter does not exist.
type NotFoundError struct {
	Path string
	Err  error
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("ssm parameter %q not found: %v", e.Path, e.Err)
}

func (e *NotFoundError) Unwrap() error { return e.Err }

// AccessDeniedError is returned when the caller lacks permission to read the SSM parameter.
type AccessDeniedError struct {
	Path string
	Err  error
}

func (e *AccessDeniedError) Error() string {
	return fmt.Sprintf("access denied reading ssm parameter %q: %v", e.Path, e.Err)
}

func (e *AccessDeniedError) Unwrap() error { return e.Err }

// ParseError is returned when the SSM parameter value cannot be parsed or validated.
type ParseError struct {
	Path string
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("parsing ssm parameter %q: %v", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// LoadFromSSM reads the horde configuration from an SSM parameter at the given path.
// It returns typed errors: *NotFoundError, *AccessDeniedError, or *ParseError.
func LoadFromSSM(ctx context.Context, client SSMClient, path string) (*HordeConfig, error) {
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{
		Name: &path,
	})
	if err != nil {
		var notFound *ssmtypes.ParameterNotFound
		if errors.As(err, &notFound) {
			return nil, &NotFoundError{Path: path, Err: err}
		}
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDeniedException" {
			return nil, &AccessDeniedError{Path: path, Err: err}
		}
		return nil, fmt.Errorf("reading ssm parameter %q: %w", path, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return nil, &ParseError{
			Path: path,
			Err:  fmt.Errorf("parameter value is nil"),
		}
	}
	cfg, err := ParseHordeConfig([]byte(*out.Parameter.Value))
	if err != nil {
		return nil, &ParseError{Path: path, Err: err}
	}
	return cfg, nil
}

// Diagnostic maps a typed SSM error to a user-friendly, actionable message.
func Diagnostic(err error) string {
	if err == nil {
		return ""
	}
	var nf *NotFoundError
	if errors.As(err, &nf) {
		return fmt.Sprintf("ssm parameter %q not found\n\nhint: deploy the @horde/cdk construct to create the SSM parameter.\nSee: https://github.com/jorge-barreto/horde/tree/main/cdk", nf.Path)
	}
	var ad *AccessDeniedError
	if errors.As(err, &ad) {
		return fmt.Sprintf("access denied reading ssm parameter %q\n\nhint: attach the horde CLI user managed policy to your IAM role or user.\nThe policy ARN is an output of the @horde/cdk construct.", ad.Path)
	}
	var pe *ParseError
	if errors.As(err, &pe) {
		return fmt.Sprintf("parsing ssm parameter %q: %v\n\nhint: the SSM parameter is malformed. Re-deploy the @horde/cdk construct to regenerate it.", pe.Path, pe.Err)
	}
	return err.Error()
}

// HordeConfig holds ECS infrastructure configuration discovered from SSM.
// JSON tags match the parameter written by the CDK construct at /horde/config.
type HordeConfig struct {
	ClusterARN            string   `json:"cluster_arn"`
	TaskDefinitionARN     string   `json:"task_definition_arn"`
	Subnets               []string `json:"subnets"`
	SecurityGroup         string   `json:"security_group"`
	// AssignPublicIp controls whether Fargate tasks get a public IP on their ENI.
	// Valid values: "ENABLED", "DISABLED", or "" (defaults to "ENABLED" for
	// backward-compatible public-subnet topology). Set to "DISABLED" for
	// private-subnet deployments that route egress through a NAT gateway.
	AssignPublicIp        string   `json:"assign_public_ip,omitempty"`
	LogGroup              string   `json:"log_group"`
	LogStreamPrefix       string   `json:"log_stream_prefix"`
	ArtifactsBucket       string   `json:"artifacts_bucket"`
	RunsTable             string   `json:"runs_table"`
	MaxConcurrent         int      `json:"max_concurrent"`
	DefaultTimeoutMinutes int      `json:"default_timeout_minutes"`
}

// Validate checks that all required fields are present and valid.
func (c *HordeConfig) Validate() error {
	var missing []string
	if c.ClusterARN == "" {
		missing = append(missing, "cluster_arn")
	}
	if c.TaskDefinitionARN == "" {
		missing = append(missing, "task_definition_arn")
	}
	if len(c.Subnets) == 0 {
		missing = append(missing, "subnets")
	}
	if c.SecurityGroup == "" {
		missing = append(missing, "security_group")
	}
	if c.LogGroup == "" {
		missing = append(missing, "log_group")
	}
	if c.LogStreamPrefix == "" {
		missing = append(missing, "log_stream_prefix")
	}
	if c.ArtifactsBucket == "" {
		missing = append(missing, "artifacts_bucket")
	}
	if c.RunsTable == "" {
		missing = append(missing, "runs_table")
	}
	if len(missing) > 0 {
		return fmt.Errorf("validating horde config: missing required fields: %s", strings.Join(missing, ", "))
	}
	if c.MaxConcurrent < 1 {
		return fmt.Errorf("validating horde config: max_concurrent must be at least 1, got %d", c.MaxConcurrent)
	}
	if c.DefaultTimeoutMinutes < 1 {
		return fmt.Errorf("validating horde config: default_timeout_minutes must be at least 1, got %d", c.DefaultTimeoutMinutes)
	}
	switch c.AssignPublicIp {
	case "", "ENABLED", "DISABLED":
	default:
		return fmt.Errorf("validating horde config: assign_public_ip must be \"ENABLED\" or \"DISABLED\", got %q", c.AssignPublicIp)
	}
	return nil
}

// ParseHordeConfig unmarshals JSON data into a HordeConfig and validates all fields.
func ParseHordeConfig(data []byte) (*HordeConfig, error) {
	var cfg HordeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing horde config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}
