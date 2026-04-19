package integration

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

const testImage = "horde-worker-test:latest"

// dockerDriver implements instanceDriver for the Docker provider.
// Instance state lives in Docker (containers) and SQLite (run rows under
// $HOME/.horde/horde.db).
type dockerDriver struct {
	t       *testing.T
	homeDir string
}

// InstanceID reads instance_id from the SQLite store for the given run.
func (d *dockerDriver) InstanceID(runID string) string {
	d.t.Helper()
	dbPath := filepath.Join(d.homeDir, ".horde", "horde.db")
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

// InstanceRunning checks if a container is currently running via docker inspect.
func (d *dockerDriver) InstanceRunning(instanceID string) bool {
	d.t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", instanceID).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// FetchContainerLogs returns the container's Docker logs.
func (d *dockerDriver) FetchContainerLogs(instanceID string) (string, error) {
	d.t.Helper()
	out, err := exec.Command("docker", "logs", instanceID).CombinedOutput()
	return string(out), err
}

// StoreStatus reads the status from the SQLite store for the given run.
func (d *dockerDriver) StoreStatus(runID string) string {
	d.t.Helper()
	dbPath := filepath.Join(d.homeDir, ".horde", "horde.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		d.t.Fatalf("opening store: %v", err)
	}
	defer db.Close()
	var status string
	err = db.QueryRow("SELECT status FROM runs WHERE id = ?", runID).Scan(&status)
	if err != nil {
		d.t.Fatalf("reading status from store: %v", err)
	}
	return status
}

// StoreExitCode reads the exit_code from the SQLite store. Returns nil if NULL.
func (d *dockerDriver) StoreExitCode(runID string) *int {
	d.t.Helper()
	dbPath := filepath.Join(d.homeDir, ".horde", "horde.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		d.t.Fatalf("opening store: %v", err)
	}
	defer db.Close()
	var code sql.NullInt64
	err = db.QueryRow("SELECT exit_code FROM runs WHERE id = ?", runID).Scan(&code)
	if err != nil {
		d.t.Fatalf("reading exit_code from store: %v", err)
	}
	if !code.Valid {
		return nil
	}
	v := int(code.Int64)
	return &v
}

// TearDown removes container-owned files and the test-specific worker image.
func (d *dockerDriver) TearDown() {
	// Workspace dirs contain root-owned files created by Docker containers.
	// Use a container to remove them before os.RemoveAll.
	hordeDir := filepath.Join(d.homeDir, ".horde")
	exec.Command("docker", "run", "--rm",
		"-v", hordeDir+":/cleanup",
		"horde-worker-base:latest", "rm", "-rf", "/cleanup",
	).Run()
	os.RemoveAll(d.homeDir)
	// Remove the test-specific worker image so it doesn't linger.
	exec.Command("docker", "rmi", testImage).Run()
}

// newHarness builds a Docker-backed harness with a temp HOME, a project dir
// containing a git remote and .env, and a worker/ directory with the test
// Dockerfile and entrypoint.
func newHarness(t *testing.T) *harness {
	t.Helper()
	if !dockerAvailable {
		t.Skip("docker not available; skipping Docker-backed integration test")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}

	homeDir, err := os.MkdirTemp("", "horde-integ-*")
	if err != nil {
		t.Fatalf("creating temp home: %v", err)
	}
	driver := &dockerDriver{t: t, homeDir: homeDir}
	t.Cleanup(driver.TearDown)

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
		t:             t,
		homeDir:       homeDir,
		workDir:       workDir,
		repoRoot:      repoRoot,
		driver:        driver,
		hordeProvider: "docker",
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

// ---------------------------------------------------------------------------
// Docker-specific harness methods. These are convenience wrappers used by
// Docker tests directly; provider-neutral code should go through h.driver.
// ---------------------------------------------------------------------------

// ContainerID reads the instance_id from the SQLite store for the given run.
func (h *harness) ContainerID(runID string) string {
	return h.driver.InstanceID(runID)
}

// ContainerRunning checks if a container is currently running via docker inspect.
func (h *harness) ContainerRunning(containerID string) bool {
	return h.driver.InstanceRunning(containerID)
}

// DockerLogs returns the container's Docker logs.
func (h *harness) DockerLogs(containerID string) (string, error) {
	return h.driver.FetchContainerLogs(containerID)
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

// WorkspaceDir returns the host path to a run's workspace.
func (h *harness) WorkspaceDir(runID string) string {
	return filepath.Join(h.homeDir, ".horde", "workspaces", runID)
}

// WorkspaceExists checks whether the workspace directory exists.
func (h *harness) WorkspaceExists(runID string) bool {
	_, err := os.Stat(h.WorkspaceDir(runID))
	return err == nil
}

// SessionsDir returns the host path to a run's sessions directory
// (bind-mounted to /home/horde/.claude in the container).
func (h *harness) SessionsDir(runID string) string {
	return filepath.Join(h.homeDir, ".horde", "workspaces", runID+"-sessions")
}

// SessionsExists checks whether the sessions directory exists.
func (h *harness) SessionsExists(runID string) bool {
	_, err := os.Stat(h.SessionsDir(runID))
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

// ReadWorkspaceFile reads a file from the workspace, returning its contents.
// Docker-only: workspaces only exist on-host for the Docker provider.
func (h *harness) ReadWorkspaceFile(runID, relPath string) (string, error) {
	h.t.Helper()
	fullPath := filepath.Join(h.WorkspaceDir(runID), relPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// OrcStatePath returns the path to orc's state.json on the host for a given run.
// When workflow is non-empty, orc namespaces artifacts under
// .orc/artifacts/<workflow>/<ticket>/.
// Docker-only: depends on on-host workspace access.
func (h *harness) OrcStatePath(runID, workflow, ticket string) string {
	h.t.Helper()
	if workflow != "" {
		return filepath.Join(h.WorkspaceDir(runID), ".orc", "artifacts", workflow, ticket, "state.json")
	}
	return filepath.Join(h.WorkspaceDir(runID), ".orc", "artifacts", ticket, "state.json")
}
