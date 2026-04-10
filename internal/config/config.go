package config

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeRepoURL converts any git remote URL format to "host/path" (no scheme, no userinfo).
// Supported formats: https://, http://, ssh://, git://, and SSH SCP-style (git@host:path).
func NormalizeRepoURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("empty remote URL")
	}

	var normalized string

	if !strings.Contains(rawURL, "://") {
		// SCP-style: [user@]host:path
		// e.g. git@github.com:org/repo.git
		colon := strings.Index(rawURL, ":")
		if colon < 0 {
			return "", fmt.Errorf("cannot normalize remote URL: %s", rawURL)
		}
		host := rawURL[:colon]
		path := rawURL[colon+1:]
		// strip user@ prefix from host
		if at := strings.Index(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		normalized = host + "/" + path
	} else {
		// URL with scheme: https://, http://, ssh://, git://
		u, err := url.Parse(rawURL)
		if err != nil {
			return "", fmt.Errorf("cannot normalize remote URL: %s", rawURL)
		}
		// u.Host is "host" or "host:port"; u.Path is "/org/repo.git"
		normalized = u.Host + u.Path
		// strip any trailing slash
		normalized = strings.TrimSuffix(normalized, "/")
	}

	if normalized == "" || !strings.Contains(normalized, "/") {
		return "", fmt.Errorf("cannot normalize remote URL: %s", rawURL)
	}

	return normalized, nil
}
