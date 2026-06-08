package event

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jorge-barreto/horde/internal/store"
)

func TestDetailMarshalShape(t *testing.T) {
	cost := 1.25
	d := DetailFromRun(&store.Run{
		ID: "r1", Repo: "repo", Ticket: "T-1", Workflow: "w", Branch: "b",
		Status: store.StatusSuccess, TotalCostUSD: &cost,
		Labels: map[string]string{"epic": "E1"},
	})
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "run_id", "repo", "ticket", "workflow", "status", "total_cost_usd", "labels"} {
		if _, ok := got[k]; !ok {
			t.Errorf("detail missing key %q", k)
		}
	}
	if got["version"] != float64(1) {
		t.Errorf("version = %v, want 1", got["version"])
	}
	if got["run_id"] != "r1" {
		t.Errorf("run_id = %v, want r1", got["run_id"])
	}
	if got["repo"] != "repo" {
		t.Errorf("repo = %v, want repo", got["repo"])
	}
}

func TestNopEmitterNeverErrors(t *testing.T) {
	e := NopEmitter{}
	if err := e.Emit(context.Background(), TypeRunStarted, Detail{}); err != nil {
		t.Errorf("nop emit: %v", err)
	}
}
