package integration

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRetryResumesAgentPhase verifies the full retry flow with real claude:
//  1. Launch a workflow with a script phase followed by an agent phase
//  2. Wait for orc to reach the agent phase (phase_index=1 in state.json)
//  3. Kill the run — docker stop + tini forwards SIGTERM so orc saves state
//  4. Verify orc saved interrupted state with a session ID
//  5. Retry — orc restarts the agent phase fresh
//  6. Verify the run completes successfully
//
// Reads credentials from the project's .env file.
func TestRetryResumesAgentPhase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	envVars := loadEnvFile(t, filepath.Join(repoRoot, ".env"))
	if envVars["CLAUDE_CODE_OAUTH_TOKEN"] == "" {
		t.Skip("skipping: CLAUDE_CODE_OAUTH_TOKEN not set in .env")
	}

	h := newHarness(t)

	// Write real credentials to the test project's .env
	if err := os.WriteFile(filepath.Join(h.workDir, ".env"), []byte(
		"CLAUDE_CODE_OAUTH_TOKEN="+envVars["CLAUDE_CODE_OAUTH_TOKEN"]+"\n"+
			"GIT_TOKEN=not-needed\n",
	), 0o644); err != nil {
		t.Fatalf("writing .env: %v", err)
	}

	const workflow = "agent-slow"
	const ticket = "TEST-retry-agent"

	runID := h.Launch(ticket, workflow, 10*time.Minute)

	// Wait for orc to reach the agent phase (phase_index=1 means setup is done)
	h.WaitForPhaseIndex(runID, workflow, ticket, 1, 2*time.Minute)

	// Give the agent a few seconds to start and for orc to save the session ID
	time.Sleep(10 * time.Second)

	// Kill the run — docker stop sends SIGTERM via tini to orc
	if err := h.Kill(runID); err != nil {
		t.Fatalf("horde kill failed: %v", err)
	}

	// Verify orc saved state with interrupted status and a session ID
	statePath := h.OrcStatePath(runID, workflow, ticket)
	stateJSON, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading state.json after kill: %v", err)
	}
	var state struct {
		PhaseIndex     int    `json:"phase_index"`
		Status         string `json:"status"`
		PhaseSessionID string `json:"phase_session_id"`
	}
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		t.Fatalf("parsing state.json: %v\nraw: %s", err, stateJSON)
	}
	if state.Status != "interrupted" {
		t.Errorf("state status after kill: got %q, want %q\nstate.json: %s", state.Status, "interrupted", stateJSON)
	}
	if state.PhaseSessionID == "" {
		t.Errorf("state has no session ID after kill — signal propagation likely broken\nstate.json: %s", stateJSON)
	}
	if state.PhaseIndex != 1 {
		t.Errorf("state phase_index after kill: got %d, want 1 (agent phase)\nstate.json: %s", state.PhaseIndex, stateJSON)
	}

	// Retry — restart agent phase fresh (skips archive gate via --retry)
	out, err := h.Retry(runID, "--retry", "work")
	if err != nil {
		t.Fatalf("horde retry failed: %v\nstdout: %s", err, out)
	}

	// Wait for the retried run to complete
	h.WaitForOrc(runID, 5*time.Minute)

	// Trigger lazy detection and verify success
	statusOut := h.Status(runID)
	if !strings.Contains(statusOut, "success") {
		t.Errorf("expected status 'success' after retry, got:\n%s", statusOut)
	}

	storeStatus := h.StoreStatus(runID)
	if storeStatus != "success" {
		t.Errorf("store status after retry: got %q, want %q", storeStatus, "success")
	}
}

// TestRetryFailedRun verifies that a failed script-only run can be retried.
// The retry should launch a new container against the preserved workspace.
func TestRetryFailedRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-retry-fail", "quick-fail", 5*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)

	// Confirm the run failed
	h.Status(runID) // trigger prov.Finalize
	storeStatus := h.StoreStatus(runID)
	if storeStatus != "failed" {
		t.Fatalf("precondition: expected status 'failed' before retry, got %q", storeStatus)
	}

	originalCID := h.ContainerID(runID)

	out, err := h.Retry(runID)
	if err != nil {
		t.Fatalf("retry failed: %v\noutput: %s", err, out)
	}

	newCID := h.ContainerID(runID)
	if newCID == originalCID {
		t.Error("retry should launch a new container (different container ID)")
	}

	// Clean up both containers
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", originalCID).Run()
	})

	// Wait for retried run to complete (quick-fail exits immediately)
	h.WaitForOrc(runID, 2*time.Minute)

	// Workspace should persist through the retry
	if !h.WorkspaceExists(runID) {
		t.Error("workspace should persist through retry")
	}
}

// TestRetryRefusesRunningRun verifies that retry rejects a run that is
// still running — the user should kill it first.
func TestRetryRefusesRunningRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-retry-running", "slow", 5*time.Minute)

	_, err := h.Retry(runID)
	if err == nil {
		t.Fatal("expected error retrying a running run, got nil")
	}
}

// TestRetryRefusesSucceededRun verifies that retry rejects a run that
// already completed successfully — there's nothing to retry.
func TestRetryRefusesSucceededRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	runID := h.Launch("TEST-retry-success", "quick-success", 5*time.Minute)
	h.WaitForOrc(runID, 2*time.Minute)

	_, err := h.Retry(runID)
	if err == nil {
		t.Fatal("expected error retrying a successful run, got nil")
	}
}

// loadEnvFile reads a KEY=VALUE .env file and returns a map.
func loadEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()

	env := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, "="); idx > 0 {
			env[line[:idx]] = line[idx+1:]
		}
	}
	return env
}
