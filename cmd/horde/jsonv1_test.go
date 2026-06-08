package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jorge-barreto/horde/internal/store"
)

func TestParseOrcDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   float64
		wantOK bool
	}{
		{"12m 34s", 754, true},
		{"4m 57s", 297, true},
		{"1h 2m 3s", 3723, true},
		{"45s", 45, true},
		{"2m 30s", 150, true},
		{"  9m 34s  ", 574, true}, // surrounding whitespace tolerated
		{"", 0, false},
		{"bogus", 0, false},
		{"12 minutes", 0, false},
		// Known wart of the space-collapsing stopgap: malformed "2 30s"
		// collapses to "230s" and parses as 230s. We don't guard against it
		// here because this whole parser goes away once orc emits a numeric
		// duration (jorge-barreto/orc#5); real orc output never has bare
		// number-number spacing, so it isn't reachable in practice
		// (jorge-barreto/orc#3 tracks removing this parser entirely).
		{"2 30s", 230, true},
	}
	for _, tc := range cases {
		got, ok := parseOrcDuration(tc.in)
		if ok != tc.wantOK {
			t.Errorf("parseOrcDuration(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseOrcDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func ptrf(f float64) *float64 { return &f }

func TestStatusToV1_Labels(t *testing.T) {
	t.Parallel()
	run := &store.Run{
		ID:        "r1",
		Ticket:    "KS-1",
		Status:    store.StatusRunning,
		StartedAt: time.Now(),
		Labels:    map[string]string{"epic": "KS-100"},
	}
	v := statusToV1(run)
	if v.Labels["epic"] != "KS-100" {
		t.Errorf("Labels[epic] = %q, want %q", v.Labels["epic"], "KS-100")
	}
}

func TestStatusToV1_LabelsOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	run := &store.Run{ID: "r1", Ticket: "KS-1", Status: store.StatusRunning, StartedAt: time.Now()}
	b, err := json.Marshal(statusToV1(run))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "labels") {
		t.Errorf("expected labels omitted when empty, got %s", b)
	}
}

func TestListToV1_Labels(t *testing.T) {
	t.Parallel()
	runs := []*store.Run{
		{ID: "r1", Ticket: "KS-1", Status: store.StatusSuccess, StartedAt: time.Now(), Labels: map[string]string{"epic": "KS-100"}},
	}
	v := listToV1(runs)
	if len(v.Runs) != 1 {
		t.Fatalf("len = %d, want 1", len(v.Runs))
	}
	if v.Runs[0].Labels["epic"] != "KS-100" {
		t.Errorf("Labels[epic] = %q, want %q", v.Runs[0].Labels["epic"], "KS-100")
	}
}

func TestListToV1_Summary(t *testing.T) {
	t.Parallel()
	now := time.Now()
	runs := []*store.Run{
		{ID: "r1", Status: store.StatusSuccess, StartedAt: now, TotalCostUSD: ptrf(1.50)},
		{ID: "r2", Status: store.StatusSuccess, StartedAt: now, TotalCostUSD: ptrf(2.32)},
		{ID: "r3", Status: store.StatusRunning, StartedAt: now}, // nil cost contributes 0
	}
	v := listToV1(runs)
	if v.Summary.Count != 3 {
		t.Errorf("Summary.Count = %d, want 3", v.Summary.Count)
	}
	if got := v.Summary.TotalCostUSD; got < 3.81 || got > 3.83 {
		t.Errorf("Summary.TotalCostUSD = %v, want ~3.82", got)
	}
}

func TestListToV1_SummaryEmpty(t *testing.T) {
	t.Parallel()
	v := listToV1(nil)
	if v.Summary.Count != 0 {
		t.Errorf("Summary.Count = %d, want 0", v.Summary.Count)
	}
	if v.Summary.TotalCostUSD != 0 {
		t.Errorf("Summary.TotalCostUSD = %v, want 0", v.Summary.TotalCostUSD)
	}
	// runs must always be a (possibly empty) array, never null.
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"runs":[]`) {
		t.Errorf("expected empty runs array, got %s", b)
	}
	if !strings.Contains(string(b), `"summary"`) {
		t.Errorf("expected summary present, got %s", b)
	}
}

func TestStatusToV1_Tokens(t *testing.T) {
	run := &store.Run{
		ID: "r1", Ticket: "T-1", Status: store.StatusSuccess,
		StartedAt: time.Now(),
		Tokens: &store.TokenUsage{
			InputTokens: 1, OutputTokens: 2, CacheCreationTokens: 3, CacheReadTokens: 4, Turns: 5,
		},
	}
	v := statusToV1(run)
	if v.Tokens == nil {
		t.Fatal("StatusV1.Tokens: got nil, want non-nil")
	}
	if v.Tokens.Input != 1 || v.Tokens.Output != 2 || v.Tokens.CacheCreation != 3 ||
		v.Tokens.CacheRead != 4 || v.Tokens.Turns != 5 {
		t.Errorf("StatusV1.Tokens: got %+v", *v.Tokens)
	}
}

func TestStatusToV1_NoTokens(t *testing.T) {
	run := &store.Run{ID: "r1", Ticket: "T-1", Status: store.StatusRunning, StartedAt: time.Now()}
	if v := statusToV1(run); v.Tokens != nil {
		t.Errorf("StatusV1.Tokens: want nil, got %+v", v.Tokens)
	}
}

func TestListToV1_SummarySumsTokens(t *testing.T) {
	now := time.Now()
	runs := []*store.Run{
		{ID: "r1", StartedAt: now, Tokens: &store.TokenUsage{InputTokens: 1, OutputTokens: 2, Turns: 1}},
		{ID: "r2", StartedAt: now, Tokens: &store.TokenUsage{InputTokens: 10, OutputTokens: 20, Turns: 3}},
		{ID: "r3", StartedAt: now}, // no tokens — must not break the sum
	}
	v := listToV1(runs)
	if v.Summary.Tokens == nil {
		t.Fatal("ListSummary.Tokens: got nil, want non-nil")
	}
	if v.Summary.Tokens.Input != 11 || v.Summary.Tokens.Output != 22 || v.Summary.Tokens.Turns != 4 {
		t.Errorf("ListSummary.Tokens: got %+v", *v.Summary.Tokens)
	}
	// r3 has no tokens; its per-run object must be omitted.
	if v.Runs[2].Tokens != nil {
		t.Errorf("ListRunV1[r3].Tokens: want nil, got %+v", v.Runs[2].Tokens)
	}
}
