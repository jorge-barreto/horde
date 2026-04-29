package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLaunchMissingTicket verifies that launch without a ticket argument errors.
func TestLaunchMissingTicket(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	_, stderr, err := h.runHordeFull("--provider", "docker", "launch", "--workflow", "test-flow")
	if err == nil {
		t.Fatal("expected error for missing ticket, got nil")
	}
	if !strings.Contains(stderr, "missing required argument") {
		t.Errorf("expected 'missing required argument' in stderr, got:\n%s", stderr)
	}
}

// TestLaunchMissingEnvFile verifies that launch fails when .env is absent.
func TestLaunchMissingEnvFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	os.Remove(filepath.Join(h.workDir, ".env"))

	_, stderr, err := h.runHordeFull("--provider", "docker", "launch", "--workflow", "test-flow", "TEST-no-env")
	if err == nil {
		t.Fatal("expected error for missing .env, got nil")
	}
	if !strings.Contains(stderr, ".env") {
		t.Errorf("expected '.env' in error output, got:\n%s", stderr)
	}
}

// TestLaunchNotGitRepo verifies that launch fails outside a git repository.
func TestLaunchNotGitRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	nonGitDir := filepath.Join(h.homeDir, "not-a-repo")
	if err := os.MkdirAll(nonGitDir, 0o755); err != nil {
		t.Fatalf("creating non-git dir: %v", err)
	}
	os.WriteFile(filepath.Join(nonGitDir, ".env"),
		[]byte("CLAUDE_CODE_OAUTH_TOKEN=x\nGIT_TOKEN=x\n"), 0o644)
	h.workDir = nonGitDir

	_, stderr, err := h.runHordeFull("--provider", "docker", "launch", "--workflow", "test-flow", "TEST-no-git")
	if err == nil {
		t.Fatal("expected error for non-git directory, got nil")
	}
	if !strings.Contains(stderr, "git") {
		t.Errorf("expected 'git' in error output, got:\n%s", stderr)
	}
}

// TestLaunchDuplicateTicket verifies that launching the same ticket twice
// (while the first is still running) fails with a duplicate error.
func TestLaunchDuplicateTicket(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	_ = h.Launch("TEST-dup", "slow", 5*time.Minute)

	_, stderr, err := h.runHordeFull("--provider", "docker", "launch",
		"--workflow", "slow", "--timeout", "5m", "TEST-dup")
	if err == nil {
		t.Fatal("expected error for duplicate ticket, got nil")
	}
	if !strings.Contains(stderr, "duplicate") {
		t.Errorf("expected 'duplicate' in error output, got:\n%s", stderr)
	}
}

// TestLaunchForceOverridesDuplicate verifies that --force allows launching
// a ticket that already has an active run.
func TestLaunchForceOverridesDuplicate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newHarness(t)
	_ = h.Launch("TEST-force", "slow", 5*time.Minute)

	stdout, stderr, err := h.runHordeFull("--provider", "docker", "launch",
		"--workflow", "slow", "--timeout", "5m", "--force", "TEST-force")
	if err != nil {
		t.Fatalf("launch with --force failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	lines := strings.Split(stdout, "\n")
	runID := lines[len(lines)-1]
	if runID == "" {
		t.Fatal("launch with --force returned empty run ID")
	}

	t.Cleanup(func() {
		cid := h.ContainerID(runID)
		if cid != "" {
			exec.Command("docker", "rm", "-f", cid).Run()
		}
	})
}
