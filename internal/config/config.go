package config

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// NormalizeRepoURL converts any git remote URL format to "host/path" (no scheme, no userinfo).
// Supported formats: https://, http://, ssh://, git://, and SSH SCP-style (git@host:path).
func NormalizeRepoURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("normalizing remote URL: input is empty")
	}

	var normalized string

	if !strings.Contains(rawURL, "://") {
		// SCP-style: [user@]host:path
		// e.g. git@github.com:org/repo.git
		colon := strings.Index(rawURL, ":")
		if colon < 0 {
			return "", fmt.Errorf("normalizing remote URL %q: unrecognized format", rawURL)
		}
		host := rawURL[:colon]
		path := rawURL[colon+1:]
		// strip user@ prefix from host
		if at := strings.Index(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		if host == "" || path == "" {
			return "", fmt.Errorf("normalizing remote URL %q: unrecognized format", rawURL)
		}
		normalized = host + "/" + path
	} else {
		// URL with scheme: https://, http://, ssh://, git://
		u, err := url.Parse(rawURL)
		if err != nil {
			return "", fmt.Errorf("normalizing remote URL %q: %w", rawURL, err)
		}
		// u.Host is "host" or "host:port"; u.Path is "/org/repo.git"
		normalized = u.Host + u.Path
		// strip any trailing slash
		normalized = strings.TrimSuffix(normalized, "/")
	}

	if normalized == "" || !strings.Contains(normalized, "/") {
		return "", fmt.Errorf("normalizing remote URL %q: unrecognized format", rawURL)
	}

	return normalized, nil
}

// RepoURL runs "git remote get-url origin" in the given directory
// and normalizes the result via NormalizeRepoURL.
func RepoURL(dir string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if strings.Contains(stderr, "not a git repository") {
				return "", fmt.Errorf("resolving git remote: not a git repository")
			}
			if strings.Contains(stderr, "No such remote") {
				return "", fmt.Errorf("resolving git remote: no origin remote configured")
			}
			return "", fmt.Errorf("resolving git remote: %s: %w", stderr, exitErr)
		}
		return "", fmt.Errorf("running git: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	return NormalizeRepoURL(raw)
}

// LaunchedBy returns the current user's identity for run records.
// For v0.1, this is the local git user name from "git config user.name"
// resolved in the given directory. Returns "unknown" if git user.name is not configured.
func LaunchedBy(dir string) string {
	cmd := exec.Command("git", "config", "user.name")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "unknown"
	}
	return name
}
