package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

type StatusV1 struct {
	ID           string   `json:"id"`
	Ticket       string   `json:"ticket"`
	Workflow     string   `json:"workflow,omitempty"`
	Branch       string   `json:"branch"`
	Status       string   `json:"status"`
	InstanceID   string   `json:"instance_id,omitempty"`
	ExitCode     *int     `json:"exit_code,omitempty"`
	DurationSecs float64  `json:"duration_seconds"`
	TotalCostUSD *float64 `json:"total_cost_usd,omitempty"`
	LaunchedBy   string   `json:"launched_by"`
	StartedAt    string   `json:"started_at"`
	CompletedAt  string   `json:"completed_at,omitempty"`
}

type ListV1 struct {
	Runs []ListRunV1 `json:"runs"`
}

type ListRunV1 struct {
	ID           string   `json:"id"`
	Ticket       string   `json:"ticket"`
	Branch       string   `json:"branch"`
	Status       string   `json:"status"`
	DurationSecs float64  `json:"duration_seconds"`
	TotalCostUSD *float64 `json:"total_cost_usd,omitempty"`
}

type ResultsV1 struct {
	ID            string    `json:"id"`
	Ticket        string    `json:"ticket"`
	Workflow      string    `json:"workflow,omitempty"`
	Status        string    `json:"status"`
	OrcStatus     string    `json:"orc_status,omitempty"`
	ExitCode      *int      `json:"exit_code,omitempty"`
	TotalCostUSD  *float64  `json:"total_cost_usd,omitempty"`
	TotalDuration string    `json:"total_duration,omitempty"`
	Phases        []PhaseV1 `json:"phases,omitempty"`
	Partial       bool      `json:"partial"`
}

type PhaseV1 struct {
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	CostUSD  float64 `json:"cost_usd"`
	Duration string  `json:"duration"`
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
	for i, run := range runs {
		d := runDuration(run)
		items[i] = ListRunV1{
			ID:           run.ID,
			Ticket:       run.Ticket,
			Branch:       run.Branch,
			Status:       string(run.Status),
			DurationSecs: d.Seconds(),
			TotalCostUSD: run.TotalCostUSD,
		}
	}
	return ListV1{Runs: items}
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
		TotalDuration: result.TotalDuration,
		Partial:       false,
	}
	if len(result.Phases) > 0 {
		v.Phases = make([]PhaseV1, len(result.Phases))
		for i, p := range result.Phases {
			v.Phases[i] = PhaseV1{
				Name:     p.Name,
				Status:   p.Status,
				CostUSD:  p.CostUSD,
				Duration: p.Duration,
			}
		}
	}
	return v
}

func partialResultsToV1(run *store.Run) ResultsV1 {
	return ResultsV1{
		ID:           run.ID,
		Ticket:       run.Ticket,
		Workflow:     run.Workflow,
		Status:       string(run.Status),
		ExitCode:     run.ExitCode,
		TotalCostUSD: run.TotalCostUSD,
		Partial:      true,
	}
}

func writeJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	return nil
}
