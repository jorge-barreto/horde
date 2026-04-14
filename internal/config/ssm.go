package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

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
