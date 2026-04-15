package integration

import (
	"context"
	"database/sql"
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
	// Override HOME so horde uses our temp store/workspace
	filtered := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, "HOME=") {
			continue
		}
		filtered = append(filtered, e)
	}
	return append(filtered, "HOME="+h.homeDir)
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

// WaitForOrc polls the host-side workspace for .horde-exit-code until it appears
// or the timeout expires. This detects orc completion WITHOUT going through horde's
// status detection — critical for testing what horde reports independently.
func (h *harness) WaitForOrc(runID string, timeout time.Duration) {
	h.t.Helper()
	markerPath := filepath.Join(h.homeDir, ".horde", "workspaces", runID, ".horde-exit-code")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(markerPath); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.t.Fatalf("WaitForOrc: marker file not found after %v at %s", timeout, markerPath)
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
