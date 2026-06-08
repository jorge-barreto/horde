package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// normalizeVersion strips a cli-v / v prefix and returns a bare x.y.z, or ""
// for anything that isn't a clean three-part numeric version (dev builds,
// git-describe strings, garbage). Callers treat "" as "not comparable".
func normalizeVersion(v string) string {
	v = strings.TrimPrefix(v, "cli-v")
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return ""
	}
	for _, p := range parts {
		if p == "" {
			return ""
		}
		if _, err := strconv.Atoi(p); err != nil {
			return ""
		}
	}
	return v
}

// isNewer reports whether latest > current under x.y.z ordering. Inputs are
// normalized first; if either is not a clean version, returns false (a dev
// build never auto-claims an upgrade is available).
func isNewer(current, latest string) bool {
	c := normalizeVersion(current)
	l := normalizeVersion(latest)
	if c == "" || l == "" {
		return false
	}
	cp := strings.Split(c, ".")
	lp := strings.Split(l, ".")
	for i := 0; i < 3; i++ {
		cn, _ := strconv.Atoi(cp[i])
		ln, _ := strconv.Atoi(lp[i])
		if ln != cn {
			return ln > cn
		}
	}
	return false
}

// parseLatestTag extracts tag_name from a GitHub releases/latest response.
func parseLatestTag(body []byte) (string, error) {
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("parsing release JSON: %w", err)
	}
	if r.TagName == "" {
		return "", fmt.Errorf("no tag_name in release response")
	}
	return r.TagName, nil
}
