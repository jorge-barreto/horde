package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var hordeBin string // set by TestMain

func TestMain(m *testing.M) {
	// Build horde binary
	tmp, err := os.MkdirTemp("", "horde-integration-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	hordeBin = filepath.Join(tmp, "horde")
	if runtime.GOOS == "windows" {
		hordeBin += ".exe"
	}

	// Resolve repo root (test/integration/ -> repo root)
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving repo root: %v\n", err)
		os.Exit(1)
	}

	cmd := exec.Command("go", "build", "-o", hordeBin, "./cmd/horde/")
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "building horde binary: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

const testImage = "horde-worker-test:latest"

type harness struct {
	t        *testing.T
	homeDir  string // unique temp HOME for this test
	workDir  string // project directory (cwd for horde commands)
	repoRoot string // horde repo root
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}

	homeDir, err := os.MkdirTemp("", "horde-integ-*")
	if err != nil {
		t.Fatalf("creating temp home: %v", err)
	}
	t.Cleanup(func() {
		// Workspace dirs contain root-owned files created by Docker containers.
		// Use a container to remove them before os.RemoveAll.
		hordeDir := filepath.Join(homeDir, ".horde")
		exec.Command("docker", "run", "--rm",
			"-v", hordeDir+":/cleanup",
			"horde-worker-base:latest", "rm", "-rf", "/cleanup",
		).Run()
		os.RemoveAll(homeDir)
		// Remove the test-specific worker image so it doesn't linger.
		exec.Command("docker", "rmi", testImage).Run()
	})

	// Create project directory with git remote
	workDir := filepath.Join(homeDir, "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("creating project dir: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "remote", "add", "origin", "https://github.com/test/repo.git")
	run("git", "config", "user.name", "integration-test")
	run("git", "config", "user.email", "test@test.com")

	// Write .env with required keys (values are dummy — script workflows don't use them)
	envContent := "CLAUDE_CODE_OAUTH_TOKEN=test-token\nGIT_TOKEN=test-token\n"
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte(envContent), 0o644); err != nil {
		t.Fatalf("writing .env: %v", err)
	}

	// Prepare worker/ directory with project Dockerfile
	workerDir := filepath.Join(workDir, "worker")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatalf("creating worker dir: %v", err)
	}

	// Initialize a git repo from the test fixtures and copy to worker/test-repo/
	fixtureDir := filepath.Join(repoRoot, "test", "fixtures", "test-repo")
	testRepoDir := filepath.Join(workerDir, "test-repo")
	if err := copyDir(fixtureDir, testRepoDir); err != nil {
		t.Fatalf("copying test fixtures: %v", err)
	}
	runIn := func(dir string, args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v in %s failed: %v\n%s", args, dir, err, out)
		}
	}
	runIn(testRepoDir, "git", "init")
	runIn(testRepoDir, "git", "config", "user.name", "test")
	runIn(testRepoDir, "git", "config", "user.email", "test@test.com")
	runIn(testRepoDir, "git", "add", "-A")
	runIn(testRepoDir, "git", "commit", "-m", "initial")

	// Copy test-entrypoint.sh and Dockerfile into worker/
	integrationDir := filepath.Join(repoRoot, "test", "integration")
	copyFile(t, filepath.Join(integrationDir, "test-entrypoint.sh"), filepath.Join(workerDir, "test-entrypoint.sh"))
	copyFile(t, filepath.Join(integrationDir, "worker-dockerfile.tmpl"), filepath.Join(workerDir, "Dockerfile"))

	return &harness{
		t:        t,
		homeDir:  homeDir,
		workDir:  workDir,
		repoRoot: repoRoot,
	}
}

// copyDir recursively copies src to dst.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
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
	for _, e := range env {
		if strings.HasPrefix(e, "HOME=") || strings.HasPrefix(e, "HORDE_DOCKER_IMAGE=") {
			continue
		}
		filtered = append(filtered, e)
	}
	return append(filtered, "HOME="+h.homeDir, "HORDE_DOCKER_IMAGE="+testImage)
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

// Launch runs horde launch and returns the run ID.
func (h *harness) Launch(ticket, workflow string, timeout time.Duration) string {
	h.t.Helper()
	args := []string{"--provider", "docker", "launch", "--timeout", timeout.String()}
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

	// Register cleanup to stop and remove the container
	h.t.Cleanup(func() {
		cid := h.ContainerID(runID)
		if cid != "" {
			exec.Command("docker", "rm", "-f", cid).Run()
		}
	})

	return runID
}

// Status runs horde status and returns stdout.
func (h *harness) Status(runID string) string {
	h.t.Helper()
	out, err := h.runHorde("--provider", "docker", "status", runID)
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
	_, err := h.runHorde("--provider", "docker", "kill", runID)
	return err
}

// List runs horde list --all and returns stdout.
func (h *harness) List() string {
	h.t.Helper()
	out, err := h.runHorde("--provider", "docker", "list", "--all")
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

// WaitForOrc polls the container until it exits or the timeout expires.
// Detects orc completion by checking if the container has stopped.
func (h *harness) WaitForOrc(runID string, timeout time.Duration) {
	h.t.Helper()
	cid := h.ContainerID(runID)
	if cid == "" {
		h.t.Fatalf("WaitForOrc: no container ID found for run %s", runID)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", cid).Output()
		if err == nil && strings.TrimSpace(string(out)) == "false" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.t.Fatalf("WaitForOrc: container %s still running after %v", cid, timeout)
}

// ContainerID reads the instance_id from the SQLite store for the given run.
func (h *harness) ContainerID(runID string) string {
	h.t.Helper()
	dbPath := filepath.Join(h.homeDir, ".horde", "horde.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return ""
	}
	defer db.Close()
	var instanceID string
	err = db.QueryRow("SELECT instance_id FROM runs WHERE id = ?", runID).Scan(&instanceID)
	if err != nil {
		return ""
	}
	return instanceID
}

// DockerLogs returns the container's Docker logs.
func (h *harness) DockerLogs(containerID string) (string, error) {
	h.t.Helper()
	out, err := exec.Command("docker", "logs", containerID).CombinedOutput()
	return string(out), err
}

// StoreStatus reads the status from the SQLite store for the given run.
func (h *harness) StoreStatus(runID string) string {
	h.t.Helper()
	dbPath := filepath.Join(h.homeDir, ".horde", "horde.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		h.t.Fatalf("opening store: %v", err)
	}
	defer db.Close()
	var status string
	err = db.QueryRow("SELECT status FROM runs WHERE id = ?", runID).Scan(&status)
	if err != nil {
		h.t.Fatalf("reading status from store: %v", err)
	}
	return status
}

// Retry runs horde retry and returns stdout and any error.
func (h *harness) Retry(runID string, orcArgs ...string) (string, error) {
	h.t.Helper()
	args := []string{"--provider", "docker", "retry", runID}
	if len(orcArgs) > 0 {
		args = append(args, "--")
		args = append(args, orcArgs...)
	}
	return h.runHorde(args...)
}

// WaitForFile polls for a file inside the workspace until it appears or timeout expires.
func (h *harness) WaitForFile(runID, relPath string, timeout time.Duration) {
	h.t.Helper()
	fullPath := filepath.Join(h.homeDir, ".horde", "workspaces", runID, relPath)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(fullPath); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.t.Fatalf("WaitForFile: %s not found after %v", relPath, timeout)
}

// ReadWorkspaceFile reads a file from the workspace, returning its contents.
func (h *harness) ReadWorkspaceFile(runID, relPath string) (string, error) {
	h.t.Helper()
	fullPath := filepath.Join(h.homeDir, ".horde", "workspaces", runID, relPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// OrcStatePath returns the path to orc's state.json on the host for a given run.
// When workflow is non-empty, orc namespaces artifacts under .orc/artifacts/<workflow>/<ticket>/.
func (h *harness) OrcStatePath(runID, workflow, ticket string) string {
	h.t.Helper()
	if workflow != "" {
		return filepath.Join(h.homeDir, ".horde", "workspaces", runID, ".orc", "artifacts", workflow, ticket, "state.json")
	}
	return filepath.Join(h.homeDir, ".horde", "workspaces", runID, ".orc", "artifacts", ticket, "state.json")
}

// WaitForPhaseIndex polls orc's state.json in the workspace until phase_index >= minIndex.
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

// StoreExitCode reads the exit_code from the SQLite store. Returns nil if NULL.
func (h *harness) StoreExitCode(runID string) *int {
	h.t.Helper()
	dbPath := filepath.Join(h.homeDir, ".horde", "horde.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		h.t.Fatalf("opening store: %v", err)
	}
	defer db.Close()
	var code sql.NullInt64
	err = db.QueryRow("SELECT exit_code FROM runs WHERE id = ?", runID).Scan(&code)
	if err != nil {
		h.t.Fatalf("reading exit_code from store: %v", err)
	}
	if !code.Valid {
		return nil
	}
	v := int(code.Int64)
	return &v
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

// Logs runs horde logs and returns stdout.
func (h *harness) Logs(runID string) string {
	h.t.Helper()
	out, err := h.runHorde("--provider", "docker", "logs", runID)
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
	out, err := h.runHorde("--provider", "docker", "results", runID)
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
	args := []string{"--provider", "docker", "clean"}
	if purge {
		args = append(args, "--purge")
	}
	args = append(args, runID)
	return h.runHorde(args...)
}

// CleanAll runs horde clean (all terminal runs) and returns stdout.
func (h *harness) CleanAll(purge bool) string {
	h.t.Helper()
	args := []string{"--provider", "docker", "clean"}
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

// ListActive runs horde list (active only, no --all) and returns stdout.
func (h *harness) ListActive() string {
	h.t.Helper()
	out, err := h.runHorde("--provider", "docker", "list")
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

// WorkspaceDir returns the host path to a run's workspace.
func (h *harness) WorkspaceDir(runID string) string {
	return filepath.Join(h.homeDir, ".horde", "workspaces", runID)
}

// WorkspaceExists checks whether the workspace directory exists.
func (h *harness) WorkspaceExists(runID string) bool {
	_, err := os.Stat(h.WorkspaceDir(runID))
	return err == nil
}

// ResultsDir returns the host path to a run's results directory.
func (h *harness) ResultsDir(runID string) string {
	return filepath.Join(h.homeDir, ".horde", "results", runID)
}

// SavedLogsExist checks whether container.log was saved in the results dir.
func (h *harness) SavedLogsExist(runID string) bool {
	_, err := os.Stat(filepath.Join(h.ResultsDir(runID), "container.log"))
	return err == nil
}

// ContainerRunning checks if a container is currently running via docker inspect.
func (h *harness) ContainerRunning(containerID string) bool {
	h.t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", containerID).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// ContainerExists checks if a Docker container exists (running or stopped).
func (h *harness) ContainerExists(containerID string) bool {
	return exec.Command("docker", "inspect", containerID).Run() == nil
}

// RemoveContainerExternally removes a container via docker rm -f (not through horde).
func (h *harness) RemoveContainerExternally(containerID string) {
	h.t.Helper()
	exec.Command("docker", "rm", "-f", containerID).Run()
}
