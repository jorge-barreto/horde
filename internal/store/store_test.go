package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRunMetadata_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	run := Run{
		Metadata: map[string]string{
			"cluster_arn":      "arn:aws:ecs:us-east-1:123456789012:cluster/horde",
			"log_group":        "/ecs/horde-worker",
			"artifacts_bucket": "horde-artifacts-us-east-1",
		},
	}

	b, err := json.Marshal(run.Metadata)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got) != len(run.Metadata) {
		t.Errorf("length mismatch: got %d, want %d", len(got), len(run.Metadata))
	}
	for k, want := range run.Metadata {
		if v, ok := got[k]; !ok {
			t.Errorf("missing key %q", k)
		} else if v != want {
			t.Errorf("key %q: got %q, want %q", k, v, want)
		}
	}
}

func TestRunMetadata_NilRoundTrip(t *testing.T) {
	t.Parallel()
	run := Run{}

	b, err := json.Marshal(run.Metadata)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "null" {
		t.Errorf("marshaled nil map: got %q, want %q", string(b), "null")
	}

	var got map[string]string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != nil {
		t.Errorf("unmarshaled null: got %v, want nil", got)
	}
}

func TestRunMetadata_EmptyMapRoundTrip(t *testing.T) {
	t.Parallel()
	run := Run{
		Metadata: map[string]string{},
	}

	b, err := json.Marshal(run.Metadata)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "{}" {
		t.Errorf("marshaled empty map: got %q, want %q", string(b), "{}")
	}

	var got map[string]string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got == nil {
		t.Errorf("unmarshaled {}: got nil, want non-nil map")
	}
	if len(got) != 0 {
		t.Errorf("unmarshaled {}: got length %d, want 0", len(got))
	}
}

func TestRunUpdate_ZeroValue(t *testing.T) {
	t.Parallel()
	var u RunUpdate
	if u.Status != nil {
		t.Errorf("zero RunUpdate.Status: got %v, want nil", u.Status)
	}
	if u.InstanceID != nil {
		t.Errorf("zero RunUpdate.InstanceID: got %v, want nil", u.InstanceID)
	}
	if u.Metadata != nil {
		t.Errorf("zero RunUpdate.Metadata: got %v, want nil", u.Metadata)
	}
	if u.ExitCode != nil {
		t.Errorf("zero RunUpdate.ExitCode: got %v, want nil", u.ExitCode)
	}
	if u.CompletedAt != nil {
		t.Errorf("zero RunUpdate.CompletedAt: got %v, want nil", u.CompletedAt)
	}
	if u.TotalCostUSD != nil {
		t.Errorf("zero RunUpdate.TotalCostUSD: got %v, want nil", u.TotalCostUSD)
	}
}

func TestRunUpdate_PartialUpdate(t *testing.T) {
	t.Parallel()
	status := StatusRunning
	instanceID := "container-abc123"
	u := RunUpdate{
		Status:     &status,
		InstanceID: &instanceID,
	}
	if u.Status == nil || *u.Status != StatusRunning {
		t.Errorf("Status: got %v, want %v", u.Status, StatusRunning)
	}
	if u.InstanceID == nil || *u.InstanceID != "container-abc123" {
		t.Errorf("InstanceID: got %v, want %q", u.InstanceID, "container-abc123")
	}
	if u.ExitCode != nil {
		t.Errorf("ExitCode should be nil for partial update, got %v", u.ExitCode)
	}
	if u.CompletedAt != nil {
		t.Errorf("CompletedAt should be nil for partial update, got %v", u.CompletedAt)
	}
	if u.TotalCostUSD != nil {
		t.Errorf("TotalCostUSD should be nil for partial update, got %v", u.TotalCostUSD)
	}
}

func TestRunUpdate_CompletionUpdate(t *testing.T) {
	t.Parallel()
	status := StatusSuccess
	exitCode := 0
	now := time.Now()
	cost := 0.42
	u := RunUpdate{
		Status:       &status,
		ExitCode:     &exitCode,
		CompletedAt:  &now,
		TotalCostUSD: &cost,
	}
	if *u.Status != StatusSuccess {
		t.Errorf("Status: got %v, want %v", *u.Status, StatusSuccess)
	}
	if *u.ExitCode != 0 {
		t.Errorf("ExitCode: got %d, want 0", *u.ExitCode)
	}
	if !u.CompletedAt.Equal(now) {
		t.Errorf("CompletedAt: got %v, want %v", *u.CompletedAt, now)
	}
	if *u.TotalCostUSD != 0.42 {
		t.Errorf("TotalCostUSD: got %f, want 0.42", *u.TotalCostUSD)
	}
	if u.InstanceID != nil {
		t.Errorf("InstanceID should be nil for completion update, got %v", u.InstanceID)
	}
}
