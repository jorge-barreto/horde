package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

// validListStatuses is the set of statuses accepted by `horde list --status`.
var validListStatuses = map[string]store.Status{
	string(store.StatusPending):     store.StatusPending,
	string(store.StatusRunning):     store.StatusRunning,
	string(store.StatusSuccess):     store.StatusSuccess,
	string(store.StatusFailed):      store.StatusFailed,
	string(store.StatusKilled):      store.StatusKilled,
	string(store.StatusTimedOut):    store.StatusTimedOut,
	string(store.StatusRateLimited): store.StatusRateLimited,
}

// parseStatuses validates and de-duplicates --status values into a status set.
// An unknown status is an error so typos surface rather than silently matching
// nothing. Empty input returns nil (no status constraint).
func parseStatuses(vals []string) ([]store.Status, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	seen := make(map[store.Status]bool, len(vals))
	out := make([]store.Status, 0, len(vals))
	for _, v := range vals {
		st, ok := validListStatuses[v]
		if !ok {
			return nil, fmt.Errorf("invalid --status %q: valid values are pending, running, success, failed, killed, timed_out, rate_limited", v)
		}
		if seen[st] {
			continue
		}
		seen[st] = true
		out = append(out, st)
	}
	return out, nil
}

// parseWhen interprets a --since/--until value as either an absolute time
// (RFC3339, with or without the clock component) or a duration-ago relative to
// now (e.g. "1h", "30m", "7d"). Empty input returns nil (no bound). The result
// is in UTC. Duration support adds a "d" = 24h unit on top of Go's
// time.ParseDuration, which has no day unit.
func parseWhen(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}

	// Absolute timestamps first.
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			u := t.UTC()
			return &u, nil
		}
	}

	// Duration-ago. Translate a trailing "d" (days) into hours, then defer to
	// time.ParseDuration for the rest (h/m/s). The result is truncated to the
	// second: StartedAt is persisted at second precision in both stores, so a
	// sub-second bound would be compared inconsistently (SQLite filters in Go at
	// full precision, DynamoDB compares truncated RFC3339 strings). Truncating
	// here keeps both stores in agreement.
	if d, err := parseDurationWithDays(s); err == nil {
		if d < 0 {
			return nil, fmt.Errorf("invalid duration %q: must not be negative", s)
		}
		t := time.Now().Add(-d).UTC().Truncate(time.Second)
		return &t, nil
	}

	return nil, fmt.Errorf("invalid time %q: expected RFC3339 (2026-04-01 or 2026-04-01T12:00:00Z) or a duration-ago (1h, 30m, 7d)", s)
}

// maxDurationDays bounds the "d" suffix so the days→nanoseconds multiply can't
// silently overflow int64 (which would wrap a huge value to a bogus near-now
// bound). ~25,000 years of days is far beyond any real query window.
const maxDurationDays = 10_000_000

// parseDurationWithDays parses a Go duration that may use a "d" (day = 24h)
// suffix on a leading integer, e.g. "7d", "7d12h". Plain Go durations ("1h30m")
// pass straight through.
func parseDurationWithDays(s string) (time.Duration, error) {
	if i := strings.IndexByte(s, 'd'); i >= 0 {
		days, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, fmt.Errorf("invalid day count in %q", s)
		}
		if days < -maxDurationDays || days > maxDurationDays {
			return 0, fmt.Errorf("day count in %q is out of range", s)
		}
		rest := s[i+1:]
		dur := time.Duration(days) * 24 * time.Hour
		if rest == "" {
			return dur, nil
		}
		extra, err := time.ParseDuration(rest)
		if err != nil {
			return 0, err
		}
		return dur + extra, nil
	}
	return time.ParseDuration(s)
}
