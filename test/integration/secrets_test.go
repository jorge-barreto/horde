package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLaunchExtraSecretInjected verifies that a third secret declared in
// .horde/config.yaml is injected into the worker container alongside the
// two canonical secrets. The worker dumps its environment to a file in
// the workspace; the test reads that file from the host and asserts the
// value lands under the container env-var name.
func TestLaunchExtraSecretInjected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)

	// Two extras: STRIPE_API_KEY exercises the identity-mapping path
	// (matching host/container names flow through --env-file unchanged)
	// and REVIEW_GIT_TOKEN exercises the remap path (host .env key
	// MY_REVIEW_TOKEN, container env-var REVIEW_GIT_TOKEN, injected via
	// `docker run -e REVIEW_GIT_TOKEN`). With --env-file in play the
	// host-side name (MY_REVIEW_TOKEN) is also forwarded into the
	// container under its own name; that is documented v1 behavior and
	// not asserted against here.
	envContent := "CLAUDE_CODE_OAUTH_TOKEN=test-token\n" +
		"GIT_TOKEN=test-token\n" +
		"MY_REVIEW_TOKEN=review-token-value-xyz\n" +
		"STRIPE_API_KEY=stripe-value-abc\n"
	if err := os.WriteFile(filepath.Join(h.workDir, ".env"), []byte(envContent), 0o644); err != nil {
		t.Fatalf("writing .env: %v", err)
	}

	configContent := `secrets:
  REVIEW_GIT_TOKEN:
    env: MY_REVIEW_TOKEN
    aws-secret: horde/review-git-token
  STRIPE_API_KEY:
    env: STRIPE_API_KEY
    aws-secret: prepdesk/stripe-api-key
`
	hordeDir := filepath.Join(h.workDir, ".horde")
	if err := os.MkdirAll(hordeDir, 0o755); err != nil {
		t.Fatalf("creating .horde dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hordeDir, "config.yaml"), []byte(configContent), 0o644); err != nil {
		t.Fatalf("writing .horde/config.yaml: %v", err)
	}

	// Inject the host env vars into the test process so the docker
	// provider's SecretEnvRemap path can copy them via os.Setenv +
	// docker -e <name>. (The .env file already covers the
	// matching-name case; the remap path needs the value in the
	// horde process env at launch time. The non-test horde binary
	// gets this via config.ApplyDotEnvToProcess at startup.)
	t.Setenv("MY_REVIEW_TOKEN", "review-token-value-xyz")
	t.Setenv("STRIPE_API_KEY", "stripe-value-abc")

	runID := h.Launch("TEST-secrets", "env-dump", 1*time.Minute)
	h.WaitForOrc(runID, 1*time.Minute)

	// env-dump.yaml writes env to .orc/artifacts/<ticket>/env-dump.txt
	envDumpPath := filepath.Join(h.WorkspaceDir(runID), ".orc", "artifacts", "TEST-secrets", "env-dump.txt")
	deadline := time.Now().Add(15 * time.Second)
	var dump string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(envDumpPath)
		if err == nil {
			dump = string(data)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if dump == "" {
		t.Fatalf("env-dump.txt not produced at %s", envDumpPath)
	}

	// Canonical secrets always appear.
	if !strings.Contains(dump, "CLAUDE_CODE_OAUTH_TOKEN=test-token") {
		t.Errorf("env dump missing CLAUDE_CODE_OAUTH_TOKEN=test-token; dump:\n%s", dump)
	}
	if !strings.Contains(dump, "GIT_TOKEN=test-token") {
		t.Errorf("env dump missing GIT_TOKEN=test-token; dump:\n%s", dump)
	}

	// Remap path: container env REVIEW_GIT_TOKEN gets the value of host
	// .env key MY_REVIEW_TOKEN.
	if !strings.Contains(dump, "REVIEW_GIT_TOKEN=review-token-value-xyz") {
		t.Errorf("env dump missing REVIEW_GIT_TOKEN=review-token-value-xyz; dump:\n%s", dump)
	}

	// Identity path: matching container/host name flows through --env-file.
	if !strings.Contains(dump, "STRIPE_API_KEY=stripe-value-abc") {
		t.Errorf("env dump missing STRIPE_API_KEY=stripe-value-abc; dump:\n%s", dump)
	}

	// Note: MY_REVIEW_TOKEN (the host-side name) is also visible inside
	// the container because docker --env-file forwards every key in .env.
	// We intentionally do not assert against that — it is documented v1
	// behavior. The contract is that the container *can* read the value
	// under the spec-declared name (REVIEW_GIT_TOKEN); leakage of the
	// host name is not part of the secret-isolation guarantee.
}

// TestLaunchSecretMissingFromEnvFails asserts that horde refuses to launch
// when .horde/config.yaml declares a docker secret whose env: source is
// not present in .env.
func TestLaunchSecretMissingFromEnvFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)

	// .env from harness has only CLAUDE_CODE_OAUTH_TOKEN + GIT_TOKEN.
	configContent := `secrets:
  STRIPE_API_KEY:
    env: STRIPE_API_KEY
    aws-secret: prepdesk/stripe-api-key
`
	hordeDir := filepath.Join(h.workDir, ".horde")
	if err := os.MkdirAll(hordeDir, 0o755); err != nil {
		t.Fatalf("creating .horde dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hordeDir, "config.yaml"), []byte(configContent), 0o644); err != nil {
		t.Fatalf("writing .horde/config.yaml: %v", err)
	}

	_, stderr, err := h.runHordeFull("--provider", "docker", "launch", "--workflow", "env-dump", "TEST-missing-env-key")
	if err == nil {
		t.Fatal("expected launch to fail for missing STRIPE_API_KEY in .env")
	}
	if !strings.Contains(stderr, "STRIPE_API_KEY") {
		t.Errorf("stderr should name missing key STRIPE_API_KEY, got:\n%s", stderr)
	}
}
