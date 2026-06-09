package integration

import (
	"encoding/json"
	"errors"
	"os/exec"
	"time"
)

// Queue + JSON-contract harness helpers for the #36 / labels / token e2e
// tests. These mirror the cmd/horde/jsonv1.go contract structs; the field tags
// MUST stay in sync with that file (the --json output is a stable contract).

// launchV1 mirrors cmd/horde/jsonv1.go LaunchV1.
type launchV1 struct {
	Status        string  `json:"status"`
	RunID         *string `json:"run_id"`
	Ticket        string  `json:"ticket"`
	Workflow      string  `json:"workflow"`
	Branch        string  `json:"branch"`
	Reason        string  `json:"reason,omitempty"`
	ExistingRunID *string `json:"existing_run_id"`
	Priority      string  `json:"priority,omitempty"`
}

// queueListItemV1 / queueListV1 mirror QueueListItemV1 / QueueListV1.
type queueListItemV1 struct {
	RunID      string `json:"run_id"`
	Ticket     string `json:"ticket"`
	Workflow   string `json:"workflow"`
	Priority   string `json:"priority"`
	EnqueuedAt string `json:"enqueued_at"`
}

type queueListV1 struct {
	Status string            `json:"status"`
	Queued []queueListItemV1 `json:"queued"`
}

// tokensV1 mirrors TokensV1.
type tokensV1 struct {
	Input         int `json:"input"`
	Output        int `json:"output"`
	CacheCreation int `json:"cache_creation"`
	CacheRead     int `json:"cache_read"`
	Turns         int `json:"turns"`
}

// statusV1 mirrors the full StatusV1 contract (used by the deep-contract +
// token + label tests).
type statusV1 struct {
	ID           string            `json:"id"`
	Ticket       string            `json:"ticket"`
	Workflow     string            `json:"workflow,omitempty"`
	Branch       string            `json:"branch"`
	Status       string            `json:"status"`
	InstanceID   string            `json:"instance_id,omitempty"`
	ExitCode     *int              `json:"exit_code,omitempty"`
	DurationSecs float64           `json:"duration_seconds"`
	TotalCostUSD *float64          `json:"total_cost_usd,omitempty"`
	Tokens       *tokensV1         `json:"tokens,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	LaunchedBy   string            `json:"launched_by"`
	StartedAt    string            `json:"started_at"`
	CompletedAt  string            `json:"completed_at,omitempty"`
}

// listRunV1 / listV1 mirror ListRunV1 / ListV1 (the subset the filter tests use).
type listRunV1 struct {
	ID       string            `json:"id"`
	Ticket   string            `json:"ticket"`
	Workflow string            `json:"workflow,omitempty"`
	Status   string            `json:"status"`
	Labels   map[string]string `json:"labels,omitempty"`
}

type listV1 struct {
	Runs []listRunV1 `json:"runs"`
}

// EnqueueJSON runs `--json launch --enqueue` and returns the parsed LaunchV1.
// priority "" omits the flag. Registers the run for cleanup. Does NOT fatal on
// a non-zero exit for queued/capped/duplicate (those are exit 0); a real error
// fatals.
func (h *harness) EnqueueJSON(ticket, workflow, priority string, timeout time.Duration) launchV1 {
	h.t.Helper()
	args := append(h.providerArgs(), "--json", "launch", "--enqueue",
		"--workflow", workflow, "--timeout", timeout.String())
	if priority != "" {
		args = append(args, "--priority", priority)
	}
	args = append(args, ticket)
	out, err := h.runHorde(args...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde --json launch --enqueue failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	var v launchV1
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		h.t.Fatalf("invalid JSON from --json launch --enqueue: %v\nraw: %s", err, out)
	}
	if v.RunID != nil && *v.RunID != "" {
		h.TrackRunForCleanup(*v.RunID)
	}
	return v
}

// QueueListJSON runs `--json queue list` and returns the parsed QueueListV1.
func (h *harness) QueueListJSON() queueListV1 {
	h.t.Helper()
	out, err := h.runHorde(append(h.providerArgs(), "--json", "queue", "list")...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde --json queue list failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	var v queueListV1
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		h.t.Fatalf("invalid JSON from --json queue list: %v\nraw: %s", err, out)
	}
	return v
}

// QueuePrioritize runs `queue prioritize <id> --priority <level>`. Returns
// stdout + error (tests assert success or expect a specific failure).
func (h *harness) QueuePrioritize(runID, priority string) (string, error) {
	h.t.Helper()
	return h.runHorde(append(h.providerArgs(), "queue", "prioritize", runID, "--priority", priority)...)
}

// QueueCancel runs `queue cancel <id>`. Returns stdout + error.
func (h *harness) QueueCancel(runID string) (string, error) {
	h.t.Helper()
	return h.runHorde(append(h.providerArgs(), "queue", "cancel", runID)...)
}

// StatusJSON runs `--json status <id>` and returns the parsed StatusV1.
func (h *harness) StatusJSON(runID string) statusV1 {
	h.t.Helper()
	out, err := h.runHorde(append(h.providerArgs(), "--json", "status", runID)...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde --json status failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	var v statusV1
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		h.t.Fatalf("invalid JSON from --json status: %v\nraw: %s", err, out)
	}
	return v
}

// ListJSON runs `--json list --all <extraArgs>` and returns the parsed ListV1.
// extraArgs carries filters like --label/--status/--workflow/--ticket.
func (h *harness) ListJSON(extraArgs ...string) listV1 {
	h.t.Helper()
	args := append(h.providerArgs(), "--json", "list", "--all")
	args = append(args, extraArgs...)
	out, err := h.runHorde(args...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde --json list failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	var v listV1
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		h.t.Fatalf("invalid JSON from --json list: %v\nraw: %s", err, out)
	}
	return v
}

// listContains reports whether a ListV1 result set includes runID.
func listContains(v listV1, runID string) bool {
	for _, r := range v.Runs {
		if r.ID == runID {
			return true
		}
	}
	return false
}

// queueContains finds a queued item by run ID; returns it + whether found.
func queueContains(v queueListV1, runID string) (queueListItemV1, bool) {
	for _, it := range v.Queued {
		if it.RunID == runID {
			return it, true
		}
	}
	return queueListItemV1{}, false
}
