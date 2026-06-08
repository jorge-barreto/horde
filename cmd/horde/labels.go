package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Label key/value bounds. Keys are constrained (k8s/Docker-style) so they stay
// safe in query, table-column, and metrics-dimension contexts; values are
// free-form (descriptive tags, commit-message-like text) but length-bounded.
const (
	maxLabelKeyLen   = 64
	maxLabelValueLen = 256
)

// labelKeyPattern restricts keys to letters, digits, and . _ - — non-empty,
// no spaces or path separators.
var labelKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// parseLabels turns repeated `key=value` CLI flag values into a map. It splits
// on the first '=' (values may contain '='), validates keys strictly and values
// loosely, and rejects duplicate keys (ambiguous). Returns nil for empty input
// so a label-less launch stores no labels rather than an empty map.
func parseLabels(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	labels := make(map[string]string, len(pairs))
	for _, p := range pairs {
		eq := strings.IndexByte(p, '=')
		if eq < 0 {
			return nil, fmt.Errorf("invalid label %q: must be key=value", p)
		}
		key := p[:eq]
		value := p[eq+1:]

		if key == "" {
			return nil, fmt.Errorf("invalid label %q: empty key", p)
		}
		if value == "" {
			return nil, fmt.Errorf("invalid label %q: empty value", p)
		}
		if len(key) > maxLabelKeyLen {
			return nil, fmt.Errorf("invalid label key %q: exceeds %d characters", key, maxLabelKeyLen)
		}
		if !labelKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("invalid label key %q: only letters, digits, and . _ - are allowed", key)
		}
		if len(value) > maxLabelValueLen {
			return nil, fmt.Errorf("invalid label value for key %q: exceeds %d characters", key, maxLabelValueLen)
		}
		if _, dup := labels[key]; dup {
			return nil, fmt.Errorf("duplicate label key %q", key)
		}
		labels[key] = value
	}
	return labels, nil
}
