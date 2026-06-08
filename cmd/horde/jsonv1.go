package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

// errJSONEmitted is a sentinel: a command that has already written its own
// JSON output to stdout wraps it around a returned error so the root
// ExitErrHandler does not print a second, conflicting error envelope.
var errJSONEmitted = errors.New("json envelope already emitted")

// emittedExit lets a command exit non-zero while signalling (via errJSONEmitted)
// that it already emitted JSON. It satisfies urfave/cli's ExitCoder so the
// process exit code is preserved.
type emittedExit struct{ code int }

func (e emittedExit) Error() string        { return "" }
func (e emittedExit) ExitCode() int        { return e.code }
func (e emittedExit) Is(target error) bool { return target == errJSONEmitted }

// ErrorV1 is the JSON error envelope emitted on stdout (under --json) when a
// command fails for a real error. The human-readable "error: <msg>" line is
// still written to stderr by main().
type ErrorV1 struct {
	Status string `json:"status"` // always "error"
	Reason string `json:"reason"`
}

func errorEnvelopeV1(err error) ErrorV1 {
	return ErrorV1{Status: "error", Reason: err.Error()}
}

// TokensV1 is the per-run token telemetry surfaced under --json. It is a
// pointer on its parent structs and omitempty, so it is absent entirely when
// horde has no token data for the run (pre-finalize on ECS, or an orc old
// enough not to emit tokens). Field names are the stable contract.
type TokensV1 struct {
	Input         int `json:"input"`
	Output        int `json:"output"`
	CacheCreation int `json:"cache_creation"`
	CacheRead     int `json:"cache_read"`
	Turns         int `json:"turns"`
}

// tokensToV1 maps a store.TokenUsage to the JSON contract shape, or nil.
func tokensToV1(t *store.TokenUsage) *TokensV1 {
	if t == nil {
		return nil
	}
	return &TokensV1{
		Input:         t.InputTokens,
		Output:        t.OutputTokens,
		CacheCreation: t.CacheCreationTokens,
		CacheRead:     t.CacheReadTokens,
		Turns:         t.Turns,
	}
}

type StatusV1 struct {
	ID           string            `json:"id"`
	Ticket       string            `json:"ticket"`
	Workflow     string            `json:"workflow,omitempty"`
	Branch       string            `json:"branch"`
	Status       string            `json:"status"`
	InstanceID   string            `json:"instance_id,omitempty"`
	ExitCode     *int              `json:"exit_code,omitempty"`
	DurationSecs float64           `json:"duration_seconds"`
	TotalCostUSD *float64          `json:"total_cost_usd,omitempty"`
	Tokens       *TokensV1         `json:"tokens,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	LaunchedBy   string            `json:"launched_by"`
	StartedAt    string            `json:"started_at"`
	CompletedAt  string            `json:"completed_at,omitempty"`
}

type ListV1 struct {
	Runs    []ListRunV1 `json:"runs"`
	Summary ListSummary `json:"summary"`
}

// ListSummary aggregates the filtered result set so a label cohort's spend is
// readable without client-side summing. Always present (even for an empty
// result) so the shape is stable.
type ListSummary struct {
	Count        int       `json:"count"`
	TotalCostUSD float64   `json:"total_cost_usd"`
	Tokens       *TokensV1 `json:"tokens,omitempty"`
}

// ListRunV1 is the per-run subset of StatusV1: it carries every field
// horde knows from a run record, so `horde list --json` no longer forces
// callers into N+1 `horde status` lookups to learn the workflow, exit code,
// or timestamps. DurationSecs is non-pointer because horde always knows the
// wall-clock duration of a run it recorded.
type ListRunV1 struct {
	ID           string            `json:"id"`
	Ticket       string            `json:"ticket"`
	Workflow     string            `json:"workflow,omitempty"`
	Branch       string            `json:"branch"`
	Status       string            `json:"status"`
	InstanceID   string            `json:"instance_id,omitempty"`
	ExitCode     *int              `json:"exit_code,omitempty"`
	DurationSecs float64           `json:"duration_seconds"`
	TotalCostUSD *float64          `json:"total_cost_usd,omitempty"`
	Tokens       *TokensV1         `json:"tokens,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	LaunchedBy   string            `json:"launched_by"`
	StartedAt    string            `json:"started_at"`
	CompletedAt  string            `json:"completed_at,omitempty"`
}

type ResultsV1 struct {
	ID                string    `json:"id"`
	Ticket            string    `json:"ticket"`
	Workflow          string    `json:"workflow,omitempty"`
	Status            string    `json:"status"`
	OrcStatus         string    `json:"orc_status,omitempty"`
	ExitCode          *int      `json:"exit_code,omitempty"`
	TotalCostUSD      *float64  `json:"total_cost_usd,omitempty"`
	Tokens            *TokensV1 `json:"tokens,omitempty"`
	TotalDuration     string    `json:"total_duration,omitempty"`
	TotalDurationSecs *float64  `json:"total_duration_seconds,omitempty"`
	Phases            []PhaseV1 `json:"phases,omitempty"`
	Partial           bool      `json:"partial"`
}

type PhaseV1 struct {
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	CostUSD      float64  `json:"cost_usd"`
	Duration     string   `json:"duration"`
	DurationSecs *float64 `json:"duration_seconds,omitempty"`
}

func runDuration(run *store.Run) time.Duration {
	var d time.Duration
	if run.CompletedAt != nil {
		d = run.CompletedAt.Sub(run.StartedAt)
	} else {
		d = time.Since(run.StartedAt)
	}
	return d.Truncate(time.Second)
}

func statusToV1(run *store.Run) StatusV1 {
	d := runDuration(run)
	v := StatusV1{
		ID:           run.ID,
		Ticket:       run.Ticket,
		Workflow:     run.Workflow,
		Branch:       run.Branch,
		Status:       string(run.Status),
		InstanceID:   run.InstanceID,
		ExitCode:     run.ExitCode,
		DurationSecs: d.Seconds(),
		TotalCostUSD: run.TotalCostUSD,
		Tokens:       tokensToV1(run.Tokens),
		Labels:       run.Labels,
		LaunchedBy:   run.LaunchedBy,
		StartedAt:    run.StartedAt.Format(time.RFC3339),
	}
	if run.CompletedAt != nil {
		v.CompletedAt = run.CompletedAt.Format(time.RFC3339)
	}
	return v
}

func listToV1(runs []*store.Run) ListV1 {
	items := make([]ListRunV1, len(runs))
	var totalCost float64
	var tokenSum store.TokenUsage
	var anyTokens bool
	for i, run := range runs {
		// Derive from statusToV1 so list and status never drift on field
		// values or timestamp formatting.
		s := statusToV1(run)
		items[i] = ListRunV1{
			ID:           s.ID,
			Ticket:       s.Ticket,
			Workflow:     s.Workflow,
			Branch:       s.Branch,
			Status:       s.Status,
			InstanceID:   s.InstanceID,
			ExitCode:     s.ExitCode,
			DurationSecs: s.DurationSecs,
			TotalCostUSD: s.TotalCostUSD,
			Tokens:       s.Tokens,
			Labels:       s.Labels,
			LaunchedBy:   s.LaunchedBy,
			StartedAt:    s.StartedAt,
			CompletedAt:  s.CompletedAt,
		}
		if run.TotalCostUSD != nil {
			totalCost += *run.TotalCostUSD
		}
		if run.Tokens != nil {
			anyTokens = true
			tokenSum.InputTokens += run.Tokens.InputTokens
			tokenSum.OutputTokens += run.Tokens.OutputTokens
			tokenSum.CacheCreationTokens += run.Tokens.CacheCreationTokens
			tokenSum.CacheReadTokens += run.Tokens.CacheReadTokens
			tokenSum.Turns += run.Tokens.Turns
		}
	}
	return ListV1{
		Runs: items,
		Summary: ListSummary{
			Count:        len(runs),
			TotalCostUSD: totalCost,
			Tokens:       summaryTokens(anyTokens, tokenSum),
		},
	}
}

// summaryTokens returns the cohort token total, or nil when no run in the set
// carried token data (so the summary object is omitted entirely).
func summaryTokens(any bool, sum store.TokenUsage) *TokensV1 {
	if !any {
		return nil
	}
	return tokensToV1(&sum)
}

// resultsTokens prefers the run's stored token usage (populated at finalize
// from costs.json) and falls back to run-result.json's forward token fields.
func resultsTokens(run *store.Run, result *fullRunResult) *TokensV1 {
	if run.Tokens != nil {
		return tokensToV1(run.Tokens)
	}
	if result.TotalInputTokens == nil && result.TotalOutputTokens == nil &&
		result.TotalCacheCreationTokens == nil && result.TotalCacheReadTokens == nil && result.Turns == nil {
		return nil
	}
	deref := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	return &TokensV1{
		Input:         deref(result.TotalInputTokens),
		Output:        deref(result.TotalOutputTokens),
		CacheCreation: deref(result.TotalCacheCreationTokens),
		CacheRead:     deref(result.TotalCacheReadTokens),
		Turns:         deref(result.Turns),
	}
}

func fullResultsToV1(run *store.Run, result *fullRunResult) ResultsV1 {
	v := ResultsV1{
		ID:            run.ID,
		Ticket:        run.Ticket,
		Workflow:      run.Workflow,
		Status:        string(run.Status),
		OrcStatus:     result.Status,
		ExitCode:      run.ExitCode,
		TotalCostUSD:  result.TotalCostUSD,
		Tokens:        resultsTokens(run, result),
		TotalDuration: result.TotalDuration,
		Partial:       false,
	}
	if secs, ok := parseOrcDuration(result.TotalDuration); ok {
		v.TotalDurationSecs = &secs
	}
	if len(result.Phases) > 0 {
		v.Phases = make([]PhaseV1, len(result.Phases))
		for i, p := range result.Phases {
			ph := PhaseV1{
				Name:     p.Name,
				Status:   p.Status,
				CostUSD:  p.CostUSD,
				Duration: p.Duration,
			}
			if secs, ok := parseOrcDuration(p.Duration); ok {
				ph.DurationSecs = &secs
			}
			v.Phases[i] = ph
		}
	}
	return v
}

// parseOrcDuration is a best-effort bridge: orc serializes durations in
// run-result.json as human strings ("12m 34s", "1h 2m 3s", "45s") rather than
// a number, so to expose numeric *_seconds we collapse the spaces and try
// time.ParseDuration. It returns ok=false for anything it can't confidently
// parse, so callers omit the numeric field rather than emit a wrong value.
// horde's own logic never depends on this — the string stays the source of
// truth.
//
// This whole function is a stopgap. The real fix is orc emitting a numeric
// duration in run-result.json (jorge-barreto/orc#3), after which the parser
// and the best-effort path should be deleted in favor of reading the number
// directly.
func parseOrcDuration(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	compact := strings.ReplaceAll(s, " ", "")
	if d, err := time.ParseDuration(compact); err == nil {
		return d.Seconds(), true
	}
	return 0, false
}

func partialResultsToV1(run *store.Run) ResultsV1 {
	return ResultsV1{
		ID:           run.ID,
		Ticket:       run.Ticket,
		Workflow:     run.Workflow,
		Status:       string(run.Status),
		ExitCode:     run.ExitCode,
		TotalCostUSD: run.TotalCostUSD,
		Tokens:       tokensToV1(run.Tokens),
		Partial:      true,
	}
}

// LaunchV1 is the stable JSON contract for `horde launch --json`. The Status
// enum (launched|capped|duplicate|error) lets programmatic callers branch
// without grepping stderr wording. RunID and ExistingRunID are pointers so
// they serialize as JSON null when not applicable, giving consumers a stable
// key set. Capped and duplicate are protocol-level successes (exit 0 under
// --json); real errors use ErrorV1 via the root ExitErrHandler.
type LaunchV1 struct {
	Status        string  `json:"status"`
	RunID         *string `json:"run_id"`
	Ticket        string  `json:"ticket"`
	Workflow      string  `json:"workflow"`
	Branch        string  `json:"branch"`
	Reason        string  `json:"reason,omitempty"`
	ExistingRunID *string `json:"existing_run_id"`
}

func launchLaunchedV1(runID, ticket, workflow, branch string) LaunchV1 {
	return LaunchV1{Status: "launched", RunID: &runID, Ticket: ticket, Workflow: workflow, Branch: branch}
}

func launchCappedV1(ticket, workflow, branch, reason string) LaunchV1 {
	return LaunchV1{Status: "capped", Ticket: ticket, Workflow: workflow, Branch: branch, Reason: reason}
}

func launchDuplicateV1(ticket, workflow, branch, existingRunID, reason string) LaunchV1 {
	return LaunchV1{Status: "duplicate", Ticket: ticket, Workflow: workflow, Branch: branch, Reason: reason, ExistingRunID: &existingRunID}
}

// RetryV1 is the JSON contract for `horde retry --json`.
type RetryV1 struct {
	Status string `json:"status"` // "retrying"
	RunID  string `json:"run_id"`
	Ticket string `json:"ticket"`
}

func retryV1(runID, ticket string) RetryV1 {
	return RetryV1{Status: "retrying", RunID: runID, Ticket: ticket}
}

// KillV1 is the JSON contract for `horde kill --json`. ExitCode and cost are
// best-effort (orc may have crashed before writing run-result.json).
type KillV1 struct {
	Status       string   `json:"status"` // "killed"
	RunID        string   `json:"run_id"`
	ExitCode     *int     `json:"exit_code,omitempty"`
	TotalCostUSD *float64 `json:"total_cost_usd,omitempty"`
}

func killV1(runID string, exitCode *int, cost *float64) KillV1 {
	return KillV1{Status: "killed", RunID: runID, ExitCode: exitCode, TotalCostUSD: cost}
}

// CleanV1 is the JSON contract for `horde clean --json`. RemovedRunIDs is
// always a (possibly empty) array, never null.
type CleanV1 struct {
	Status        string   `json:"status"` // "cleaned"
	RemovedCount  int      `json:"removed_count"`
	RemovedRunIDs []string `json:"removed_run_ids"`
}

func cleanV1(ids []string) CleanV1 {
	if ids == nil {
		ids = []string{}
	}
	return CleanV1{Status: "cleaned", RemovedCount: len(ids), RemovedRunIDs: ids}
}

// PushV1 is the JSON contract for `horde push --json`.
type PushV1 struct {
	Status string `json:"status"` // "pushed"
	Image  string `json:"image"`
	Digest string `json:"digest,omitempty"`
}

func pushV1(image, digest string) PushV1 {
	return PushV1{Status: "pushed", Image: image, Digest: digest}
}

// HydrateV1 is the JSON contract for `horde hydrate --json`. It exposes the
// per-run outcomes alongside the aggregate counts so callers don't have to
// parse the human summary line.
type HydrateV1 struct {
	Status   string         `json:"status"` // "ok" if no failures, else "error"
	Hydrated int            `json:"hydrated"`
	Skipped  int            `json:"skipped"`
	Failed   int            `json:"failed"`
	Runs     []HydrateRunV1 `json:"runs"`
}

type HydrateRunV1 struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"` // hydrated|skipped|failed
	Reason string `json:"reason,omitempty"`
}

// writeJSONTo encodes v as indented JSON to the given writer. Tests pass a
// bytes.Buffer; production call sites pass cmd.Writer (defaults to os.Stdout)
// so JSON output never depends on swapping the global os.Stdout.
func writeJSONTo(w io.Writer, v interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	return nil
}
