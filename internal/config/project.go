package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProjectConfig holds project-level horde settings from .horde/config.yaml.
type ProjectConfig struct {
	Mounts []string `yaml:"mounts"` // paths relative to project root to mount into /workspace/
}

// LoadProjectConfig reads .horde/config.yaml from dir.
// Returns an empty config (not an error) if the file doesn't exist.
func LoadProjectConfig(dir string) (*ProjectConfig, error) {
	path := filepath.Join(dir, ".horde", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ProjectConfig{}, nil
		}
		return nil, fmt.Errorf("reading project config: %w", err)
	}

	var cfg ProjectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing project config %s: %w", path, err)
	}
	return &cfg, nil
}

// ResolveMounts converts relative mount paths to docker volume mount strings
// (host:container format). Skips paths that don't exist on the host.
func (c *ProjectConfig) ResolveMounts(projectDir string) []string {
	var mounts []string
	for _, rel := range c.Mounts {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		hostPath := filepath.Join(projectDir, rel)
		if _, err := os.Stat(hostPath); err != nil {
			continue
		}
		mounts = append(mounts, hostPath+":/workspace/"+rel)
	}
	return mounts
}
