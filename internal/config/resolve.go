package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Resolver resolves the project-identity inputs that horde commands need —
// the canonical repo (bucket key), the project config, and where to look for
// the .env file — with a single, consistent precedence: explicit override
// (flag or env, already folded by the caller) wins over filesystem discovery,
// and a missing required value is an error rather than a silent guess.
//
// The override fields collapse the flag>env precedence at construction time
// (urfave's flag Sources do this for free), so the Resolver itself only has to
// express override>discovery. When an override is present, the corresponding
// resolution touches no filesystem and shells out to git for nothing — this is
// what lets `horde launch` run from an empty directory with only env vars set.
type Resolver struct {
	// RepoOverride is the --repo / HORDE_REPO_URL value. When set, it is the
	// bucket key (after canonicalization) and git discovery is skipped.
	RepoOverride string
	// ConfigOverride is the --config / HORDE_CONFIG_PATH value. It may point at
	// a config.yaml file directly, or at a directory containing
	// .horde/config.yaml.
	ConfigOverride string
	// SSMOverride is the --ssm-path / HORDE_SSM_PATH value. Consumed by the
	// cmd/horde factory (which owns slug derivation to avoid an import cycle).
	SSMOverride string
	// Dir is the discovery working directory. An empty Dir means no filesystem
	// discovery is available (e.g. os.Getwd failed); resolution then relies
	// entirely on overrides and errors if a required one is absent.
	Dir string
}

// CanonicalRepo resolves the bucket key. Precedence: RepoOverride (normalized
// the same way as a git remote, so ".git"/case/scheme variants collapse) >
// git discovery in Dir > error. No filesystem access when RepoOverride is set.
func (r *Resolver) CanonicalRepo() (string, error) {
	if r.RepoOverride != "" {
		return CanonicalRepo(r.RepoOverride)
	}
	if r.Dir != "" {
		return RepoURL(r.Dir)
	}
	return "", fmt.Errorf("no repo URL: set --repo or HORDE_REPO_URL, or run inside a git repository")
}

// ProjectConfig loads the project config. Precedence: ConfigOverride (a
// config.yaml file or a directory containing .horde/config.yaml) > Dir's
// .horde/config.yaml > empty config. A missing file is never an error,
// matching LoadProjectConfig's behavior.
func (r *Resolver) ProjectConfig() (*ProjectConfig, error) {
	if r.ConfigOverride != "" {
		return loadProjectConfigOverride(r.ConfigOverride)
	}
	if r.Dir != "" {
		return LoadProjectConfig(r.Dir)
	}
	return &ProjectConfig{}, nil
}

// EnvFileDir returns the directory to search for the .env file. When a config
// override is given, the .env is expected alongside it (the override's
// directory) so a relocated config and its secrets stay together. Otherwise
// the discovery Dir is used.
func (r *Resolver) EnvFileDir() string {
	if r.ConfigOverride != "" {
		return configOverrideDir(r.ConfigOverride)
	}
	return r.Dir
}

// loadProjectConfigOverride loads a project config from an explicit override
// path. If the path looks like a YAML file it is loaded directly; otherwise it
// is treated as a directory and LoadProjectConfig appends .horde/config.yaml.
func loadProjectConfigOverride(path string) (*ProjectConfig, error) {
	if isYAMLPath(path) {
		return loadProjectConfigFile(path)
	}
	return LoadProjectConfig(path)
}

// configOverrideDir returns the directory that holds the .env for a config
// override: the file's parent when the override is a YAML file, else the
// override directory itself.
func configOverrideDir(path string) string {
	if isYAMLPath(path) {
		return filepath.Dir(path)
	}
	return path
}

func isYAMLPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}
