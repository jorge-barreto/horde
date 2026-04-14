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

// HordeConfig holds ECS infrastructure configuration discovered from SSM.
// JSON tags match the parameter written by the CDK construct at /horde/config.
type HordeConfig struct {
	ClusterARN            string   `json:"cluster_arn"`
	TaskDefinitionARN     string   `json:"task_definition_arn"`
	Subnets               []string `json:"subnets"`
	SecurityGroup         string   `json:"security_group"`
	LogGroup              string   `json:"log_group"`
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
