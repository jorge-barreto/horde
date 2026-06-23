package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestExecLocalAllowed_DockerOK verifies the pure guard accepts docker.
func TestExecLocalAllowed_DockerOK(t *testing.T) {
	t.Parallel()
	if err := execLocalAllowed("docker"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestExecLocalAllowed_ECSRejected verifies the pure guard rejects aws-ecs
// and mentions docker in the message.
func TestExecLocalAllowed_ECSRejected(t *testing.T) {
	t.Parallel()
	err := execLocalAllowed("aws-ecs")
	if err == nil {
		t.Fatal("expected error for aws-ecs, got nil")
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Errorf("expected error to mention docker, got: %v", err)
	}
}

// TestExec_RequiresOrcArgs verifies that `horde exec` with no orc args
// after -- returns a usage error mentioning "orc".
func TestExec_RequiresOrcArgs(t *testing.T) {
	setupLaunchEnv(t)
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "exec"})
	if err == nil || !strings.Contains(err.Error(), "orc") {
		t.Fatalf("expected usage error about missing orc args, got %v", err)
	}
}

// TestExecCappedV1_Marshal guards the capped JSON path: status must be
// "capped", run_id must be null, and the reason must appear.
func TestExecCappedV1_Marshal(t *testing.T) {
	t.Parallel()
	v := execCappedV1([]string{"eval", "x"}, false, "max concurrent runs reached (5/5)")
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"status":"capped"`) {
		t.Errorf("missing \"status\":\"capped\" in %s", got)
	}
	if !strings.Contains(got, `"run_id":null`) {
		t.Errorf("missing \"run_id\":null in %s", got)
	}
	if !strings.Contains(got, "max concurrent") {
		t.Errorf("missing reason in %s", got)
	}
}
