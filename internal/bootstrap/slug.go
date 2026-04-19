// Package bootstrap provides helpers for deriving identifiers used when
// bootstrapping cloud resources for a horde project (e.g. CloudFormation
// stack names, S3 bucket names, ECS cluster names).
package bootstrap

import (
	"fmt"
	"strings"

	"github.com/jorge-barreto/horde/internal/config"
)

// MaxSlugLen caps the length of a project slug.
//
// The slug is embedded in AWS resource names with hard length limits. The
// tightest known consumer is the S3 artifacts bucket, named as
// "horde-artifacts-{slug}-{account-id}", which S3 caps at 63 characters.
// With 16 characters for "horde-artifacts-", 12 for the AWS account ID, and
// 2 for the hyphen separators, the slug itself is capped at 33. We use 30
// for a small safety margin against future prefix changes.
const MaxSlugLen = 30

// Slug derives a CloudFormation-safe project slug from a git remote URL.
//
// The slug is the repo path (everything after the host), lowercased, with
// runs of non-alphanumeric characters collapsed to single '-' separators,
// trimmed of leading/trailing '-', and truncated to MaxSlugLen.
//
// Slug does NOT guarantee the result starts with a letter — callers that
// need that property (e.g. CloudFormation stack names) are expected to
// prepend a fixed prefix such as "horde-".
//
// Examples:
//
//	https://github.com/jorge-barreto/horde.git -> "jorge-barreto-horde"
//	git@github.com:org/repo.git                -> "org-repo"
//	https://gitlab.com/group/subgroup/repo.git -> "group-subgroup-repo"
func Slug(remoteURL string) (string, error) {
	if strings.TrimSpace(remoteURL) == "" {
		return "", fmt.Errorf("deriving project slug: input is empty")
	}

	// Accept either a raw remote URL or a pre-normalized "host/path" form
	// (config.RepoURL returns the latter). If the input has no scheme and
	// no scp-style "git@host:" prefix but does contain a '/', assume it's
	// already normalized and skip re-normalization.
	normalized, err := config.NormalizeRepoURL(remoteURL)
	if err != nil {
		if isAlreadyNormalized(remoteURL) {
			normalized = remoteURL
		} else {
			return "", fmt.Errorf("deriving project slug: %w", err)
		}
	}

	// Strip the host: take everything after the first '/'.
	slash := strings.Index(normalized, "/")
	if slash < 0 || slash == len(normalized)-1 {
		return "", fmt.Errorf("deriving project slug: %q has no repo path", remoteURL)
	}
	path := normalized[slash+1:]

	path = strings.TrimSuffix(path, ".git")
	path = strings.ToLower(path)

	var b strings.Builder
	b.Grow(len(path))
	for _, r := range path {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		// Collapse runs of separators: skip if builder is empty or the last
		// byte written is already '-'.
		if b.Len() == 0 {
			continue
		}
		s := b.String()
		if s[len(s)-1] == '-' {
			continue
		}
		b.WriteByte('-')
	}

	slug := strings.Trim(b.String(), "-")

	if len(slug) > MaxSlugLen {
		slug = slug[:MaxSlugLen]
		slug = strings.TrimRight(slug, "-")
	}

	if slug == "" {
		return "", fmt.Errorf("deriving project slug: %q sanitizes to empty string", remoteURL)
	}

	return slug, nil
}

// isAlreadyNormalized reports whether s looks like the "host/path" output
// of config.NormalizeRepoURL (no scheme, no scp-style userinfo, at least
// one "/", and a non-empty host segment).
func isAlreadyNormalized(s string) bool {
	if strings.Contains(s, "://") || strings.Contains(s, "@") {
		return false
	}
	slash := strings.Index(s, "/")
	return slash > 0 && slash < len(s)-1
}
