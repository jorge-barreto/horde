package config

import (
	"fmt"
	"sort"
	"strings"
)

// Provider kind identifiers used by SecretSpec.ValidateForProvider.
const (
	ProviderDocker = "docker"
	ProviderECS    = "aws-ecs"
)

// Canonical secret names auto-seeded with default sources.
const (
	SecretClaudeCodeOauthToken = "CLAUDE_CODE_OAUTH_TOKEN"
	SecretGitToken             = "GIT_TOKEN"
)

// canonicalAWSSecretName returns the bootstrap-stack Secrets Manager name
// for a canonical secret of the given project slug. Mirrors the names
// hardcoded in internal/bootstrap/templates/stack.yaml.tmpl. The slug is
// not known at config-load time, so the auto-seeded ECS source uses the
// short name "horde-worker/<lower>" as a placeholder; the bootstrap
// renderer treats the two canonicals specially anyway.
func canonicalAWSSecretName(name string) string {
	return "horde-worker/" + strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

// SecretSource declares how to resolve a single secret per provider kind.
// Sources are additive — a single entry can carry multiple kinds, and the
// active provider picks the matching one. Empty fields mean "no source for
// that provider"; ValidateForProvider rejects entries that lack a source
// matching the active provider.
type SecretSource struct {
	// Env is the host env-var name (read from .env) used by the docker
	// provider. When empty the secret has no docker source.
	Env string `yaml:"env,omitempty"`

	// AWSSecret is the Secrets Manager secret name (or ARN) used by the
	// ECS provider. When empty the secret has no ECS source.
	AWSSecret string `yaml:"aws-secret,omitempty"`
}

// SecretSpec maps env-var names (as seen inside the worker container) to
// their per-provider sources. The map key is the container env-var name.
type SecretSpec map[string]SecretSource

// DefaultSecrets returns the two canonical secrets with their default
// sources: read from .env on docker, and from the bootstrap stack's
// Secrets Manager entries on ECS. The bootstrap renderer recognizes the
// canonical names and wires them through stack-managed CF parameters; the
// AWSSecret field here is informational for ValidateForProvider only.
func DefaultSecrets() SecretSpec {
	return SecretSpec{
		SecretClaudeCodeOauthToken: {
			Env:       SecretClaudeCodeOauthToken,
			AWSSecret: canonicalAWSSecretName(SecretClaudeCodeOauthToken),
		},
		SecretGitToken: {
			Env:       SecretGitToken,
			AWSSecret: canonicalAWSSecretName(SecretGitToken),
		},
	}
}

// MergeSecrets seeds DefaultSecrets() and applies caller overrides on top.
// A caller entry replaces the default entry's sources entirely (no field-
// level merge) — overriding the canonical name means the caller takes
// full responsibility for both source kinds.
func MergeSecrets(fromConfig SecretSpec) SecretSpec {
	out := DefaultSecrets()
	for name, src := range fromConfig {
		out[name] = src
	}
	return out
}

// IsCanonical reports whether name is one of the two auto-seeded secrets.
func IsCanonical(name string) bool {
	return name == SecretClaudeCodeOauthToken || name == SecretGitToken
}

// ValidateForProvider returns an error if any entry in s lacks a source
// matching the given provider kind. The error message lists every
// unsourced secret so the caller can fix the config in one pass.
func (s SecretSpec) ValidateForProvider(kind string) error {
	var missing []string
	for name, src := range s {
		switch kind {
		case ProviderDocker:
			if src.Env == "" {
				missing = append(missing, name)
			}
		case ProviderECS:
			if src.AWSSecret == "" {
				missing = append(missing, name)
			}
		default:
			return fmt.Errorf("validating secrets: unknown provider kind %q", kind)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("validating secrets: provider %q has no source for: %s", kind, strings.Join(missing, ", "))
}

// EnvKeys returns the set of host env-var names referenced by the spec's
// docker sources. Used by ValidateEnvFileFor to assert .env coverage.
// Returned slice is sorted for stable error messages.
func (s SecretSpec) EnvKeys() []string {
	seen := map[string]struct{}{}
	for _, src := range s {
		if src.Env != "" {
			seen[src.Env] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ExtraAWSSecretNames returns the Secrets Manager names referenced by
// non-canonical entries. The bootstrap renderer uses this to add IAM
// grants and task-definition Secrets entries beyond the two canonicals.
// Returned slice is sorted for deterministic template output.
func (s SecretSpec) ExtraAWSSecretNames() []ExtraAWSSecret {
	var out []ExtraAWSSecret
	for name, src := range s {
		if IsCanonical(name) {
			continue
		}
		if src.AWSSecret == "" {
			continue
		}
		out = append(out, ExtraAWSSecret{EnvName: name, SecretName: src.AWSSecret})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EnvName < out[j].EnvName })
	return out
}

// ExtraAWSSecret describes one caller-declared ECS secret beyond the two
// canonicals. EnvName is the env-var name inside the worker container;
// SecretName is the AWS Secrets Manager secret name (or ARN) the caller
// pre-created.
type ExtraAWSSecret struct {
	EnvName    string
	SecretName string
}
