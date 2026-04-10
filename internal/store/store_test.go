package store

import (
	"encoding/json"
	"testing"
)

func TestRunMetadata_JSONRoundTrip(t *testing.T) {
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
