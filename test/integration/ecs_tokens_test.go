package integration

import (
	"testing"
	"time"
)

// TestECSTokenTelemetry verifies per-run token telemetry (#24/#49) on ECS: a
// real agent turn produces non-zero token usage + cost, the status Lambda
// promotes them into the run row at finalize, and `horde status --json`
// surfaces them as the nested `tokens` object + `total_cost_usd`.
//
// This is the ONLY e2e test that spends real model money — the token-probe
// workflow runs one minimal haiku turn ("reply DONE and stop").
//
// "Fail for the right reason": if the status Lambda did not read costs.json
// from S3 / did not write the token attributes, StoreTokens returns nil and
// the input>0 assertion fails.
func TestECSTokenTelemetry(t *testing.T) {
	t.Parallel()
	h := newECSHarness(t)

	ticket := uniqueTicket("tokens")
	// Generous timeout: agent cold start + one turn. Mirrors the resume test.
	runID := h.LaunchBranch(ticket, "token-probe", e2eFixtureBranch(), 15*time.Minute)
	h.TrackRunForCleanup(runID)

	status := waitForECSTerminal(t, h, runID, 15*time.Minute)
	if status != "success" {
		t.Fatalf("token-probe run reached %q, want success", status)
	}

	d := ecsd(t, h)

	// --- Store side: token attributes promoted at finalize ---
	tok := d.StoreTokens(runID)
	if tok == nil {
		t.Fatalf("StoreTokens(%s) = nil; expected non-nil token usage from a real agent turn", runID)
	}
	if tok.InputTokens <= 0 {
		t.Errorf("token input = %d, want > 0", tok.InputTokens)
	}
	if cost := d.StoreCostUSD(runID); cost == nil || *cost <= 0 {
		t.Errorf("StoreCostUSD = %v, want > 0", cost)
	}

	// --- JSON contract side: tokens object + total_cost_usd surfaced ---
	s := h.StatusJSON(runID)
	if s.Tokens == nil {
		t.Fatalf("--json status tokens object is nil; want populated")
	}
	if s.Tokens.Input <= 0 {
		t.Errorf("--json status tokens.input = %d, want > 0", s.Tokens.Input)
	}
	if s.TotalCostUSD == nil || *s.TotalCostUSD <= 0 {
		t.Errorf("--json status total_cost_usd = %v, want > 0", s.TotalCostUSD)
	}
}
