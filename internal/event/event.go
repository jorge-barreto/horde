// Package event defines the horde run-lifecycle event contract and the
// Emitter that publishes it to the EventBridge bus. Events are the
// notification-of-truth: the store write is authoritative and happens first;
// a failed emit is logged, never fatal.
package event

import (
	"context"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

// DetailType values (EventBridge DetailType). Stable public contract.
const (
	TypeRunStarted               = "run.started"
	TypeRunTerminal              = "run.terminal"
	TypeRunCostThresholdExceeded = "run.cost-threshold-exceeded"
)

// Source is the EventBridge event Source for all horde events.
const Source = "horde"

// DetailVersion is the schema version embedded in every Detail. Bump on any
// breaking change to the field set; consumers key off it.
const DetailVersion = 1

// Detail is the JSON payload of a horde run-lifecycle event. Field names are
// the stable contract — horde vocabulary, never raw ECS shape.
type Detail struct {
	Version      int               `json:"version"`
	RunID        string            `json:"run_id"`
	Repo         string            `json:"repo"`
	Ticket       string            `json:"ticket"`
	Workflow     string            `json:"workflow"`
	Branch       string            `json:"branch"`
	Status       string            `json:"status"`
	ExitCode     *int              `json:"exit_code"`
	TotalCostUSD *float64          `json:"total_cost_usd"`
	Labels       map[string]string `json:"labels"`
	StartedAt    *time.Time        `json:"started_at"`
	CompletedAt  *time.Time        `json:"completed_at"`
	StopCode     string            `json:"stop_code,omitempty"`
	StopReason   string            `json:"stop_reason,omitempty"`
}

// DetailFromRun builds a Detail from a store.Run.
func DetailFromRun(r *store.Run) Detail {
	d := Detail{
		Version: DetailVersion, RunID: r.ID, Repo: r.Repo, Ticket: r.Ticket,
		Workflow: r.Workflow, Branch: r.Branch, Status: string(r.Status),
		ExitCode: r.ExitCode, TotalCostUSD: r.TotalCostUSD, Labels: r.Labels,
		StopCode: r.Metadata["stop_code"], StopReason: r.Metadata["stop_reason"],
	}
	if !r.StartedAt.IsZero() {
		t := r.StartedAt.UTC()
		d.StartedAt = &t
	}
	d.CompletedAt = r.CompletedAt
	return d
}

// Emitter publishes run-lifecycle events. Implementations must treat emission
// as best-effort from the caller's perspective: the caller logs an error but
// does not fail its operation.
type Emitter interface {
	Emit(ctx context.Context, detailType string, detail Detail) error
}

// NopEmitter is used when no bus is configured (e.g. docker, or ECS deployments
// predating the bus). It discards events.
type NopEmitter struct{}

// Emit discards the event and never errors.
func (NopEmitter) Emit(context.Context, string, Detail) error { return nil }
