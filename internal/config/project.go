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
	Mounts []string `yaml:"mounts"` // volume mounts in host:container format (host side relative to project root)
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

// ResolveMounts resolves mount entries to absolute host:container paths.
// Each entry should be in host:container format (e.g. ".beads:/workspace/.beads").
// The host side is resolved relative to projectDir. Skips entries where the
// host path doesn't exist.
func (c *ProjectConfig) ResolveMounts(projectDir string) []string {
	var mounts []string
	for _, entry := range c.Mounts {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		hostRel, containerPath := parts[0], parts[1]
		hostPath := hostRel
		if !filepath.IsAbs(hostRel) {
			hostPath = filepath.Join(projectDir, hostRel)
		}
		if _, err := os.Stat(hostPath); err != nil {
			continue
		}
		mounts = append(mounts, hostPath+":"+containerPath)
	}
	return mounts
}
