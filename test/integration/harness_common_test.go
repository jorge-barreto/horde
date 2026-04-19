package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// instanceDriver abstracts the provider-specific operations the test harness
// uses to inspect worker instances. Docker and ECS implementations live in
// their own files.
type instanceDriver interface {
	// InstanceID returns the container ID (Docker) or task ARN (ECS)
	// that horde recorded for the given run.
	InstanceID(runID string) string
	// InstanceRunning reports whether the worker instance is still alive.
	InstanceRunning(instanceID string) bool
	// FetchContainerLogs returns the full logs for the worker instance
	// (via 'docker logs' for Docker; via CloudWatch Logs for ECS).
	FetchContainerLogs(instanceID string) (string, error)
	// StoreStatus returns the run's current status from the harness's
	// store of record (SQLite for Docker, DynamoDB for ECS).
	StoreStatus(runID string) string
	// StoreExitCode returns the run's exit_code column (nil if null).
	StoreExitCode(runID string) *int
	// TearDown removes any instance-level state created by the driver.
	// Called from t.Cleanup. Can be a no-op.
	TearDown()
}

// harness is the shared test harness. Provider-specific behavior is delegated
// to the driver field; provider-neutral methods live on this struct.
type harness struct {
	t             *testing.T
	homeDir       string // unique temp HOME for this test
	workDir       string // project directory (cwd for horde commands)
	repoRoot      string // horde repo root
	driver        instanceDriver
	hordeProvider string // "docker" or "aws-ecs"; prepended to every horde invocation
}

// TrackRunForCleanup registers a run for driver-specific teardown. For the
// ECS driver this appends to the DynamoDB delete list; for Docker this is a
// no-op (SQLite lives in the temp HOME that gets removed wholesale).
func (h *harness) TrackRunForCleanup(runID string) {
	if d, ok := h.driver.(*ecsDriver); ok {
		d.runsToClean = append(d.runsToClean, runID)
	}
}

// providerArgs returns the --provider flag set to h.hordeProvider (if any).
func (h *harness) providerArgs() []string {
	if h.hordeProvider == "" {
		return nil
	}
	return []string{"--provider", h.hordeProvider}
}

// copyFile copies a single file from src to dst.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", dst, err)
	}
}

// env returns the environment for horde subprocess calls.
func (h *harness) env() []string {
	env := os.Environ()
	// Override HOME so horde uses our temp store/workspace.
	// Override HORDE_DOCKER_IMAGE so tests don't clobber the real project image.
	filtered := env[:0]
	hasConfigFile := false
	hasCredsFile := false
	for _, e := range env {
		switch {
		case strings.HasPrefix(e, "HOME="):
			continue
		case strings.HasPrefix(e, "HORDE_DOCKER_IMAGE="):
			continue
		case strings.HasPrefix(e, "AWS_CONFIG_FILE="):
			hasConfigFile = true
		case strings.HasPrefix(e, "AWS_SHARED_CREDENTIALS_FILE="):
			hasCredsFile = true
		}
		filtered = append(filtered, e)
	}
	filtered = append(filtered, "HOME="+h.homeDir, "HORDE_DOCKER_IMAGE="+testImage)

	// For ECS we need the horde subprocess to reach AWS — keep the real
	// user's AWS config/credentials and SSO cache discoverable under the
	// overridden HOME. Docker runs ignore these vars, so it's harmless.
	if h.hordeProvider == "aws-ecs" {
		realHome, _ := os.UserHomeDir()
		if realHome != "" {
			if !hasConfigFile {
				filtered = append(filtered, "AWS_CONFIG_FILE="+realHome+"/.aws/config")
			}
			if !hasCredsFile {
				filtered = append(filtered, "AWS_SHARED_CREDENTIALS_FILE="+realHome+"/.aws/credentials")
			}
			fakeSSO := h.homeDir + "/.aws/sso"
			if _, err := os.Stat(fakeSSO); os.IsNotExist(err) {
				_ = os.MkdirAll(h.homeDir+"/.aws", 0o755)
				_ = os.Symlink(realHome+"/.aws/sso", fakeSSO)
			}
		}
	}
	return filtered
}

// runHorde executes the horde binary with args, returning stdout and any error.
func (h *harness) runHorde(args ...string) (string, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, hordeBin, args...)
	cmd.Dir = h.workDir
	cmd.Env = h.env()
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// runHordeFull executes horde and returns stdout, stderr, and any error separately.
func (h *harness) runHordeFull(args ...string) (string, string, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, hordeBin, args...)
	cmd.Dir = h.workDir
	cmd.Env = h.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

// Launch runs horde launch and returns the run ID.
func (h *harness) Launch(ticket, workflow string, timeout time.Duration) string {
	h.t.Helper()
	args := append(h.providerArgs(), "launch", "--timeout", timeout.String())
	if workflow != "" {
		args = append(args, "--workflow", workflow)
	}
	args = append(args, ticket)
	out, err := h.runHorde(args...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde launch failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	// Last line of stdout is the run ID
	lines := strings.Split(out, "\n")
	runID := lines[len(lines)-1]
	if runID == "" {
		h.t.Fatalf("horde launch returned empty run ID; stdout: %s", out)
	}

	// Register this run for driver-level cleanup (ECS: delete dynamo row).
	h.TrackRunForCleanup(runID)

	// Register cleanup to stop and remove the container (Docker only).
	h.t.Cleanup(func() {
		cid := h.driver.InstanceID(runID)
		if cid != "" && h.hordeProvider == "docker" {
			exec.Command("docker", "rm", "-f", cid).Run()
		}
	})

	return runID
}

// Status runs horde status and returns stdout.
func (h *harness) Status(runID string) string {
	h.t.Helper()
	out, err := h.runHorde(append(h.providerArgs(), "status", runID)...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde status failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	return out
}

// Kill runs horde kill. Returns error (does not fatal) since some tests expect failure.
func (h *harness) Kill(runID string) error {
	h.t.Helper()
	_, err := h.runHorde(append(h.providerArgs(), "kill", runID)...)
	return err
}

// List runs horde list --all and returns stdout.
func (h *harness) List() string {
	h.t.Helper()
	out, err := h.runHorde(append(h.providerArgs(), "list", "--all")...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde list failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	return out
}

// ListActive runs horde list (active only, no --all) and returns stdout.
func (h *harness) ListActive() string {
	h.t.Helper()
	out, err := h.runHorde(append(h.providerArgs(), "list")...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde list failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	return out
}

// Logs runs horde logs and returns stdout.
func (h *harness) Logs(runID string) string {
	h.t.Helper()
	out, err := h.runHorde(append(h.providerArgs(), "logs", runID)...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde logs failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	return out
}

// Results runs horde results and returns stdout.
func (h *harness) Results(runID string) string {
	h.t.Helper()
	out, err := h.runHorde(append(h.providerArgs(), "results", runID)...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde results failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	return out
}

// Clean runs horde clean on a specific run. Returns stdout and any error.
func (h *harness) Clean(runID string, purge bool) (string, error) {
	h.t.Helper()
	args := append(h.providerArgs(), "clean")
	if purge {
		args = append(args, "--purge")
	}
	args = append(args, runID)
	return h.runHorde(args...)
}

// CleanAll runs horde clean (all terminal runs) and returns stdout.
func (h *harness) CleanAll(purge bool) string {
	h.t.Helper()
	args := append(h.providerArgs(), "clean")
	if purge {
		args = append(args, "--purge")
	}
	out, err := h.runHorde(args...)
	if err != nil {
		var exitErr *exec.ExitError
		stderr := ""
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		h.t.Fatalf("horde clean failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	return out
}

// Retry runs horde retry and returns stdout and any error.
func (h *harness) Retry(runID string, orcArgs ...string) (string, error) {
	h.t.Helper()
	args := append(h.providerArgs(), "retry", runID)
	if len(orcArgs) > 0 {
		args = append(args, "--")
		args = append(args, orcArgs...)
	}
	return h.runHorde(args...)
}

// StoreStatus reads the status from the harness's store of record (delegated
// to the driver — SQLite for Docker, DynamoDB for ECS).
func (h *harness) StoreStatus(runID string) string {
	h.t.Helper()
	return h.driver.StoreStatus(runID)
}

// StoreExitCode reads the exit_code from the harness's store of record.
// Returns nil if NULL.
func (h *harness) StoreExitCode(runID string) *int {
	h.t.Helper()
	return h.driver.StoreExitCode(runID)
}

// WaitForOrc polls the worker instance until it exits or the timeout expires.
// Detects orc completion by checking if the instance has stopped.
func (h *harness) WaitForOrc(runID string, timeout time.Duration) {
	h.t.Helper()
	cid := h.driver.InstanceID(runID)
	if cid == "" {
		h.t.Fatalf("WaitForOrc: no instance ID found for run %s", runID)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !h.driver.InstanceRunning(cid) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.t.Fatalf("WaitForOrc: instance %s still running after %v", cid, timeout)
}

// WaitForFile polls for a file inside the workspace until it appears or timeout expires.
// Docker-only: workspaces only exist on-host for the Docker provider.
func (h *harness) WaitForFile(runID, relPath string, timeout time.Duration) {
	h.t.Helper()
	fullPath := h.WorkspaceDir(runID) + "/" + relPath
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(fullPath); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.t.Fatalf("WaitForFile: %s not found after %v", relPath, timeout)
}

// WaitForPhaseIndex polls orc's state.json in the workspace until phase_index >= minIndex.
// Docker-only: depends on on-host workspace access.
func (h *harness) WaitForPhaseIndex(runID, workflow, ticket string, minIndex int, timeout time.Duration) {
	h.t.Helper()
	statePath := h.OrcStatePath(runID, workflow, ticket)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(statePath)
		if err == nil {
			var state struct {
				PhaseIndex int `json:"phase_index"`
			}
			if json.Unmarshal(data, &state) == nil && state.PhaseIndex >= minIndex {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.t.Fatalf("WaitForPhaseIndex: phase_index never reached %d after %v at %s", minIndex, timeout, statePath)
}
