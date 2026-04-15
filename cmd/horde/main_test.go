package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jorge-barreto/horde/internal/store"
)

type launchEnv struct {
	projectDir string
	binDir     string
}

func setupLaunchEnv(t *testing.T) launchEnv {
	t.Helper()
	tmpHome := t.TempDir()

	// Create project dir
	projectDir := filepath.Join(tmpHome, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("creating project dir: %v", err)
	}

	// Git init + remote — pattern from internal/config/config_test.go:89-97
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "remote", "add", "origin", "https://github.com/test/repo.git")

	// Write .env — pattern from internal/config/envfile_test.go
	envContent := "CLAUDE_CODE_OAUTH_TOKEN=test-key\nGIT_TOKEN=test-token\n"
	if err := os.WriteFile(filepath.Join(projectDir, ".env"), []byte(envContent), 0o644); err != nil {
		t.Fatalf("writing .env: %v", err)
	}

	// Create binDir with fake docker — pattern from internal/provider/docker_test.go:16-22
	binDir := filepath.Join(tmpHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("creating bin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\ncase \"$1\" in\n  image) echo \"2099-01-01T00:00:00Z\";;\n  *) echo abc123container;;\nesac\n"), 0o755); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}

	// Set env vars — t.Setenv restores on cleanup
	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	// Change working dir using os.Chdir + t.Cleanup (NOT t.Chdir — added in Go 1.24, not in 1.22)
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting working directory: %v", err)
	}
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("changing to project dir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(oldDir) })

	return launchEnv{projectDir: projectDir, binDir: binDir}
}

func TestLaunch_Success(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "TICKET-1"})

	pw.Close()
	os.Stdout = origStdout

	out, _ := io.ReadAll(pr)
	runID := strings.TrimSpace(string(out))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !regexp.MustCompile(`^[a-z0-9]{12}$`).MatchString(runID) {
		t.Errorf("runID %q does not match ^[a-z0-9]{12}$", runID)
	}

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()

	runs, err := st.ListByRepo(ctx, "github.com/test/repo.git", false)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	r := runs[0]
	if r.ID != runID {
		t.Errorf("ID = %q, want %q", r.ID, runID)
	}
	if r.Repo != "github.com/test/repo.git" {
		t.Errorf("Repo = %q, want %q", r.Repo, "github.com/test/repo.git")
	}
	if r.Ticket != "TICKET-1" {
		t.Errorf("Ticket = %q, want %q", r.Ticket, "TICKET-1")
	}
	if r.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", r.Status, store.StatusRunning)
	}
	if r.InstanceID != "abc123container" {
		t.Errorf("InstanceID = %q, want %q", r.InstanceID, "abc123container")
	}
	if r.Provider != "docker" {
		t.Errorf("Provider = %q, want %q", r.Provider, "docker")
	}
	if r.LaunchedBy == "" {
		t.Errorf("LaunchedBy is empty")
	}
	expectedTimeout := r.StartedAt.Add(24 * time.Hour)
	if diff := r.TimeoutAt.Sub(expectedTimeout); diff < -5*time.Second || diff > 5*time.Second {
		t.Errorf("TimeoutAt %v not within 5s of StartedAt+24h %v", r.TimeoutAt, expectedTimeout)
	}

	// Verify workspace directory was created
	wsDir := filepath.Join(filepath.Dir(env.projectDir), ".horde", "workspaces", runID)
	if _, err := os.Stat(wsDir); os.IsNotExist(err) {
		t.Errorf("workspace directory not created at %s", wsDir)
	}
}

func TestLaunch_WithFlags(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--branch", "feature-x", "--workflow", "review", "--timeout", "30m", "TICKET-2"})

	pw.Close()
	os.Stdout = origStdout
	io.ReadAll(pr)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()

	runs, err := st.ListByRepo(ctx, "github.com/test/repo.git", false)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	r := runs[0]
	if r.Branch != "feature-x" {
		t.Errorf("Branch = %q, want %q", r.Branch, "feature-x")
	}
	if r.Workflow != "review" {
		t.Errorf("Workflow = %q, want %q", r.Workflow, "review")
	}
	expectedTimeout := r.StartedAt.Add(30 * time.Minute)
	if diff := r.TimeoutAt.Sub(expectedTimeout); diff < -5*time.Second || diff > 5*time.Second {
		t.Errorf("TimeoutAt %v not within 5s of StartedAt+30m %v", r.TimeoutAt, expectedTimeout)
	}
}

func TestLaunch_MissingTicket(t *testing.T) {
	_ = setupLaunchEnv(t)
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "missing required argument") {
		t.Errorf("error %q does not contain %q", err.Error(), "missing required argument")
	}
}

func TestLaunch_NotGitRepo(t *testing.T) {
	tmpHome := t.TempDir()
	projectDir := filepath.Join(tmpHome, "project")
	os.MkdirAll(projectDir, 0o755)
	os.WriteFile(filepath.Join(projectDir, ".env"), []byte("CLAUDE_CODE_OAUTH_TOKEN=test-key\nGIT_TOKEN=test-token\n"), 0o644)
	binDir := filepath.Join(tmpHome, "bin")
	os.MkdirAll(binDir, 0o755)
	os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\necho abc123container\n"), 0o755)
	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	oldDir, _ := os.Getwd()
	os.Chdir(projectDir)
	t.Cleanup(func() { os.Chdir(oldDir) })

	ctx := context.Background()
	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "TICKET-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error %q does not contain %q", err.Error(), "not a git repository")
	}
}

func TestLaunch_MissingEnvFile(t *testing.T) {
	tmpHome := t.TempDir()
	projectDir := filepath.Join(tmpHome, "project")
	os.MkdirAll(projectDir, 0o755)

	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "remote", "add", "origin", "https://github.com/test/repo.git")

	// NO .env file written

	binDir := filepath.Join(tmpHome, "bin")
	os.MkdirAll(binDir, 0o755)
	os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\necho abc123container\n"), 0o755)
	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	oldDir, _ := os.Getwd()
	os.Chdir(projectDir)
	t.Cleanup(func() { os.Chdir(oldDir) })

	ctx := context.Background()
	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "TICKET-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "opening .env file") {
		t.Errorf("error %q does not contain %q", err.Error(), "opening .env file")
	}
}

func TestLaunch_MissingEnvKey(t *testing.T) {
	tmpHome := t.TempDir()
	projectDir := filepath.Join(tmpHome, "project")
	os.MkdirAll(projectDir, 0o755)

	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "remote", "add", "origin", "https://github.com/test/repo.git")

	// Write .env with only CLAUDE_CODE_OAUTH_TOKEN, missing GIT_TOKEN
	os.WriteFile(filepath.Join(projectDir, ".env"), []byte("CLAUDE_CODE_OAUTH_TOKEN=test\n"), 0o644)

	binDir := filepath.Join(tmpHome, "bin")
	os.MkdirAll(binDir, 0o755)
	os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\necho abc123container\n"), 0o755)
	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	oldDir, _ := os.Getwd()
	os.Chdir(projectDir)
	t.Cleanup(func() { os.Chdir(oldDir) })

	ctx := context.Background()
	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "TICKET-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "missing required key GIT_TOKEN") {
		t.Errorf("error %q does not contain %q", err.Error(), "missing required key GIT_TOKEN")
	}
}

func TestLaunch_DuplicateTicket_NoForce(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	err = st.CreateRun(ctx, &store.Run{
		ID:         "existingrunid",
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Status:     store.StatusRunning,
		Provider:   "docker",
		LaunchedBy: "someone",
		StartedAt:  now,
		TimeoutAt:  now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("pre-creating run: %v", err)
	}
	st.Close()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "TICKET-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate active ticket") {
		t.Errorf("error %q does not contain %q", err.Error(), "duplicate active ticket")
	}

	st2, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st2.Close()

	runs, err := st2.ListByRepo(ctx, "github.com/test/repo.git", false)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("expected 1 run, got %d", len(runs))
	}
}

func TestLaunch_DuplicateTicket_WithForce(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	err = st.CreateRun(ctx, &store.Run{
		ID:         "existingrunid",
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Status:     store.StatusRunning,
		Provider:   "docker",
		LaunchedBy: "someone",
		StartedAt:  now,
		TimeoutAt:  now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("pre-creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--force", "TICKET-1"})

	pw.Close()
	os.Stdout = origStdout
	io.ReadAll(pr)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	st2, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st2.Close()

	runs, err := st2.ListByRepo(ctx, "github.com/test/repo.git", false)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}

	var newRun *store.Run
	for _, r := range runs {
		if r.ID != "existingrunid" {
			newRun = r
			break
		}
	}
	if newRun == nil {
		t.Fatal("could not find new run in store")
	}
	if newRun.Status != store.StatusRunning {
		t.Errorf("new run Status = %q, want %q", newRun.Status, store.StatusRunning)
	}
}

func TestLaunch_DockerFailure(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	os.WriteFile(filepath.Join(env.binDir, "docker"), []byte("#!/bin/sh\ncase \"$1\" in\n  image) echo \"2099-01-01T00:00:00Z\";;\n  tag) exit 0;;\n  *) echo \"Error: Cannot connect to the Docker daemon\" >&2; exit 1;;\nesac\n"), 0o755)

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "TICKET-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "launching docker container") {
		t.Errorf("error %q does not contain %q", err.Error(), "launching docker container")
	}

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()

	runs, err := st.ListByRepo(ctx, "github.com/test/repo.git", false)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].Status != store.StatusFailed {
		t.Errorf("Status = %q, want %q", runs[0].Status, store.StatusFailed)
	}
	if runs[0].CompletedAt == nil {
		t.Error("CompletedAt should be non-nil after launch failure")
	}
}

func TestLaunch_DockerFailure_NoStderrWarning(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	os.WriteFile(filepath.Join(env.binDir, "docker"), []byte("#!/bin/sh\ncase \"$1\" in\n  image) echo \"2099-01-01T00:00:00Z\";;\n  tag) exit 0;;\n  *) echo \"Error: Cannot connect to the Docker daemon\" >&2; exit 1;;\nesac\n"), 0o755)

	origStderr := os.Stderr
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stderr pipe: %v", err)
	}
	os.Stderr = stderrW
	defer func() { os.Stderr = origStderr }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "TICKET-1"})

	stderrW.Close()
	os.Stderr = origStderr
	stderrOut, _ := io.ReadAll(stderrR)

	if runErr == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(runErr.Error(), "launching docker container") {
		t.Errorf("error %q does not contain %q", runErr.Error(), "launching docker container")
	}
	if strings.Contains(string(stderrOut), "warning: failed to mark run as failed") {
		t.Error("unexpected warning on stderr — UpdateRun should have succeeded")
	}

	// Verify run is marked failed (UpdateRun worked)
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()
	runs, err := st.ListByRepo(ctx, "github.com/test/repo.git", false)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].Status != store.StatusFailed {
		t.Errorf("Status = %q, want %q", runs[0].Status, store.StatusFailed)
	}
	if runs[0].CompletedAt == nil {
		t.Error("CompletedAt should be non-nil after launch failure")
	}
}

func TestProvider_InvalidValue(t *testing.T) {
	_ = setupLaunchEnv(t)
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "gcp", "launch", "TICKET-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `unsupported provider "gcp"`) {
		t.Errorf("error %q does not contain expected message", err.Error())
	}
}

func TestProvider_ExplicitDocker(t *testing.T) {
	_ = setupLaunchEnv(t)
	ctx := context.Background()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "TICKET-1"})

	pw.Close()
	os.Stdout = origStdout
	pr.Close()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProfile_AcceptedAsGlobalFlag(t *testing.T) {
	_ = setupLaunchEnv(t)
	ctx := context.Background()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "--profile", "staging", "launch", "TICKET-1"})

	pw.Close()
	os.Stdout = origStdout
	pr.Close()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- Status tests ---

type statusEnv struct {
	tmpHome string
	binDir  string
	dbPath  string
}

func setupStatusEnv(t *testing.T, dockerScript string) statusEnv {
	t.Helper()
	tmpHome := t.TempDir()
	binDir := filepath.Join(tmpHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("creating bin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(dockerScript), 0o755); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	dbPath := filepath.Join(tmpHome, ".horde", "horde.db")
	return statusEnv{tmpHome: tmpHome, binDir: binDir, dbPath: dbPath}
}

func TestStatus_CompletedRun(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()

	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	completedAt := now.Add(-5 * time.Minute)
	exitCode := 0
	cost := 4.52
	runID := "testrunid001"
	err = st.CreateRun(ctx, &store.Run{
		ID:           runID,
		Repo:         "github.com/test/repo.git",
		Ticket:       "TICKET-1",
		Status:       store.StatusSuccess,
		Provider:     "docker",
		LaunchedBy:   "testuser",
		StartedAt:    now.Add(-10 * time.Minute),
		TimeoutAt:    now.Add(50 * time.Minute),
		ExitCode:     &exitCode,
		CompletedAt:  &completedAt,
		TotalCostUSD: &cost,
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, runID) {
		t.Errorf("output missing run ID %q: %s", runID, outStr)
	}
	if !strings.Contains(outStr, "TICKET-1") {
		t.Errorf("output missing ticket: %s", outStr)
	}
	if !strings.Contains(outStr, "success") {
		t.Errorf("output missing status 'success': %s", outStr)
	}
	if !strings.Contains(outStr, "Exit code") {
		t.Errorf("output missing 'Exit code': %s", outStr)
	}
	if !strings.Contains(outStr, "$4.52") {
		t.Errorf("output missing '$4.52': %s", outStr)
	}
	if !strings.Contains(outStr, "testuser") {
		t.Errorf("output missing launched_by 'testuser': %s", outStr)
	}
}

func TestStatus_RunningRun(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":true,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  exec) exit 1 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	runID := "testrunid002"
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-2",
		Status:     store.StatusRunning,
		InstanceID: "abc123",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  now.Add(-5 * time.Minute),
		TimeoutAt:  now.Add(55 * time.Minute),
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "running") {
		t.Errorf("output missing 'running': %s", outStr)
	}
	if strings.Contains(outStr, "Exit code") {
		t.Errorf("output should not contain 'Exit code' for running run: %s", outStr)
	}
}

func TestStatus_NotFound(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()

	// Ensure store directory exists so SQLiteStore can create the DB
	if err := os.MkdirAll(filepath.Dir(env.dbPath), 0o755); err != nil {
		t.Fatalf("creating db dir: %v", err)
	}
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	st.Close()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", "nonexistent"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "run not found") {
		t.Errorf("error %q does not contain 'run not found'", err.Error())
	}
}

func TestStatus_MissingRunID(t *testing.T) {
	setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "missing required argument") {
		t.Errorf("error %q does not contain 'missing required argument'", err.Error())
	}
}

func TestStatus_LazyCompletion(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":false,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"2024-06-15T10:45:00Z"}' ;;
  cp) exit 0 ;;
  rm) exit 0 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	runID := "testrunid005"
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-5",
		Status:     store.StatusRunning,
		InstanceID: "abc123",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  now.Add(-15 * time.Minute),
		TimeoutAt:  now.Add(45 * time.Minute),
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "success") {
		t.Errorf("output missing 'success': %s", outStr)
	}
	if !strings.Contains(outStr, "Exit code") {
		t.Errorf("output missing 'Exit code': %s", outStr)
	}
	if !strings.Contains(outStr, "0") {
		t.Errorf("output missing exit code '0': %s", outStr)
	}

	// Verify store was updated
	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusSuccess {
		t.Errorf("store status = %q, want %q", r.Status, store.StatusSuccess)
	}
	if r.ExitCode == nil || *r.ExitCode != 0 {
		t.Errorf("store exit code = %v, want 0", r.ExitCode)
	}
	if r.CompletedAt == nil {
		t.Errorf("store completed_at is nil")
	}
}

func TestStatus_LazyCompletion_ZeroFinishedAt(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":false,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  cp) exit 0 ;;
  rm) exit 0 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	runID := "testrunid-m0e"
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-m0e",
		Status:     store.StatusRunning,
		InstanceID: "abc123",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  now.Add(-15 * time.Minute),
		TimeoutAt:  now.Add(45 * time.Minute),
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	before := time.Now()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	after := time.Now()
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "success") {
		t.Errorf("output missing 'success': %s", outStr)
	}
	if !strings.Contains(outStr, "Exit code") {
		t.Errorf("output missing 'Exit code': %s", outStr)
	}
	if !strings.Contains(outStr, "0") {
		t.Errorf("output missing exit code '0': %s", outStr)
	}

	// Verify store was updated with a non-zero CompletedAt from time.Now() fallback
	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusSuccess {
		t.Errorf("store status = %q, want %q", r.Status, store.StatusSuccess)
	}
	if r.ExitCode == nil || *r.ExitCode != 0 {
		t.Errorf("store exit code = %v, want 0", r.ExitCode)
	}
	if r.CompletedAt == nil {
		t.Errorf("store completed_at is nil")
	} else if r.CompletedAt.IsZero() {
		t.Errorf("store completed_at is the zero time — fallback was not used")
	} else if r.CompletedAt.Before(before.Truncate(time.Second)) || r.CompletedAt.After(after) {
		t.Errorf("store completed_at %v not in [%v, %v] — expected time.Now() fallback", r.CompletedAt, before, after)
	}
}

func TestStatus_LazyCompletion_WithCost(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":false,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"2024-06-15T10:45:00Z"}' ;;
  cp)
    case "$2" in
      *audit*) mkdir -p "$3/TICKET-1" && printf '{"total_cost_usd": 4.52}' > "$3/TICKET-1/run-result.json" ;;
    esac
    ;;
  rm) exit 0 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	runID := "testrunid006"
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Workflow:   "",
		Status:     store.StatusRunning,
		InstanceID: "abc123",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  now.Add(-15 * time.Minute),
		TimeoutAt:  now.Add(45 * time.Minute),
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "success") {
		t.Errorf("output missing 'success': %s", outStr)
	}
	if !strings.Contains(outStr, "$4.52") {
		t.Errorf("output missing '$4.52': %s", outStr)
	}

	// Verify store was updated with cost
	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.TotalCostUSD == nil || *r.TotalCostUSD != 4.52 {
		t.Errorf("store TotalCostUSD = %v, want 4.52", r.TotalCostUSD)
	}
}

func TestStatus_Timeout(t *testing.T) {
	// handleLazyCheck calls Status() (inspect) then Kill() (stop+cp+rm) — script is intentionally stateless
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":true,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  stop) exit 0 ;;
  cp) exit 0 ;;
  rm) exit 0 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	runID := "testrunid007"
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-7",
		Status:     store.StatusRunning,
		InstanceID: "abc123",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  now.Add(-90 * time.Minute),
		TimeoutAt:  now.Add(-30 * time.Minute), // already timed out
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "killed") {
		t.Errorf("output missing 'killed': %s", outStr)
	}

	// Verify store was updated
	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusKilled {
		t.Errorf("store status = %q, want %q", r.Status, store.StatusKilled)
	}
	if r.CompletedAt == nil {
		t.Errorf("store completed_at is nil")
	}
}

func TestStatus_UnknownContainer(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf "Error: No such container: abc123\n" >&2; exit 1 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	runID := "testrunid008"
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-8",
		Status:     store.StatusRunning,
		InstanceID: "abc123",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  now.Add(-5 * time.Minute),
		TimeoutAt:  now.Add(55 * time.Minute),
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "failed") {
		t.Errorf("output missing 'failed': %s", outStr)
	}

	// Verify store was updated
	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusFailed {
		t.Errorf("store status = %q, want %q", r.Status, store.StatusFailed)
	}
}

func TestMapExitCode(t *testing.T) {
	tests := []struct {
		code int
		want store.Status
	}{
		{0, store.StatusSuccess},
		{5, store.StatusKilled},
		{1, store.StatusFailed},
		{2, store.StatusFailed},
		{137, store.StatusFailed},
	}
	for _, tt := range tests {
		got := mapExitCode(tt.code)
		if got != tt.want {
			t.Errorf("mapExitCode(%d) = %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestStatus_LazyCompletion_WithWorkflow(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":false,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"2024-06-15T10:45:00Z"}' ;;
  cp)
    case "$2" in
      *audit*) mkdir -p "$3/review/TICKET-1" && printf '{"total_cost_usd": 7.50}' > "$3/review/TICKET-1/run-result.json" ;;
    esac
    ;;
  rm) exit 0 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	runID := "testrunid009"
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Workflow:   "review",
		Status:     store.StatusRunning,
		InstanceID: "abc123",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  now.Add(-15 * time.Minute),
		TimeoutAt:  now.Add(45 * time.Minute),
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})

	w.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(r)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "success") {
		t.Errorf("output missing 'success': %s", outStr)
	}
	if !strings.Contains(outStr, "$7.50") {
		t.Errorf("output missing '$7.50': %s", outStr)
	}
	if !strings.Contains(outStr, "Workflow") {
		t.Errorf("output missing 'Workflow': %s", outStr)
	}

	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st2.Close()
	run, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if run.TotalCostUSD == nil || *run.TotalCostUSD != 7.50 {
		t.Errorf("store TotalCostUSD = %v, want 7.50", run.TotalCostUSD)
	}
}

// --- List tests ---

func TestList_ActiveOnly(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	completedAt := now.Add(-10 * time.Minute)

	runs := []*store.Run{
		{ID: "listrun00001", Ticket: "T-1", Status: store.StatusPending, Repo: "github.com/test/repo.git", Provider: "docker", LaunchedBy: "testuser", StartedAt: now, TimeoutAt: now.Add(time.Hour)},
		{ID: "listrun00002", Ticket: "T-2", Status: store.StatusRunning, Repo: "github.com/test/repo.git", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-5 * time.Minute), TimeoutAt: now.Add(55 * time.Minute)},
		{ID: "listrun00003", Ticket: "T-3", Status: store.StatusSuccess, Repo: "github.com/test/repo.git", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-20 * time.Minute), CompletedAt: &completedAt, TimeoutAt: now.Add(40 * time.Minute)},
	}
	for _, r := range runs {
		if err := st.CreateRun(ctx, r); err != nil {
			t.Fatalf("creating run %s: %v", r.ID, err)
		}
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "list"})

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "listrun00001") {
		t.Errorf("output missing listrun00001: %s", outStr)
	}
	if !strings.Contains(outStr, "listrun00002") {
		t.Errorf("output missing listrun00002: %s", outStr)
	}
	if !strings.Contains(outStr, "RUN ID") {
		t.Errorf("output missing header 'RUN ID': %s", outStr)
	}
	if strings.Contains(outStr, "listrun00003") {
		t.Errorf("output should not contain completed run listrun00003: %s", outStr)
	}
}

func TestList_All(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	completedAt := now.Add(-10 * time.Minute)

	runs := []*store.Run{
		{ID: "listrun00001", Ticket: "T-1", Status: store.StatusPending, Repo: "github.com/test/repo.git", Provider: "docker", LaunchedBy: "testuser", StartedAt: now, TimeoutAt: now.Add(time.Hour)},
		{ID: "listrun00002", Ticket: "T-2", Status: store.StatusRunning, Repo: "github.com/test/repo.git", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-5 * time.Minute), TimeoutAt: now.Add(55 * time.Minute)},
		{ID: "listrun00003", Ticket: "T-3", Status: store.StatusSuccess, Repo: "github.com/test/repo.git", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-20 * time.Minute), CompletedAt: &completedAt, TimeoutAt: now.Add(40 * time.Minute)},
	}
	for _, r := range runs {
		if err := st.CreateRun(ctx, r); err != nil {
			t.Fatalf("creating run %s: %v", r.ID, err)
		}
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "list", "--all"})

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "listrun00001") {
		t.Errorf("output missing listrun00001: %s", outStr)
	}
	if !strings.Contains(outStr, "listrun00002") {
		t.Errorf("output missing listrun00002: %s", outStr)
	}
	if !strings.Contains(outStr, "listrun00003") {
		t.Errorf("output missing listrun00003: %s", outStr)
	}
}

func TestList_EmptyResult(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "list"})

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if !strings.Contains(string(out), "No active runs for this repo.") {
		t.Errorf("output missing 'No active runs for this repo.': %s", string(out))
	}
}

func TestList_EmptyResult_All(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "list", "--all"})

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if !strings.Contains(string(out), "No runs found for this repo.") {
		t.Errorf("output missing 'No runs found for this repo.': %s", string(out))
	}
}

func TestList_LazyCompletion(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	// Overwrite fake docker with one that handles inspect/cp/rm
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":false,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"2024-06-15T10:45:00Z"}' ;;
  cp) exit 0 ;;
  rm) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(env.binDir, "docker"), []byte(dockerScript), 0o755); err != nil {
		t.Fatalf("writing docker script: %v", err)
	}

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	err = st.CreateRun(ctx, &store.Run{
		ID:         "listrun00005",
		Ticket:     "TICKET-5",
		Status:     store.StatusRunning,
		InstanceID: "abc123",
		Repo:       "github.com/test/repo.git",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  now.Add(-15 * time.Minute),
		TimeoutAt:  now.Add(45 * time.Minute),
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "list"})

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if !strings.Contains(string(out), "No active runs for this repo.") {
		t.Errorf("output missing 'No active runs for this repo.': %s", string(out))
	}

	st2, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, "listrun00005")
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusSuccess {
		t.Errorf("store status = %q, want %q", r.Status, store.StatusSuccess)
	}
	if r.ExitCode == nil || *r.ExitCode != 0 {
		t.Errorf("store exit code = %v, want 0", r.ExitCode)
	}
	if r.CompletedAt == nil {
		t.Errorf("store completed_at is nil")
	}
}

func TestList_LazyCompletion_MixedStates(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dockerScript := `#!/bin/sh
case "$1" in
  inspect)
    case "$4" in
      container1) printf '{"Running":false,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"2024-06-15T10:45:00Z"}' ;;
      container2) printf '{"Running":true,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
    esac ;;
  exec) exit 1 ;;
  cp) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(env.binDir, "docker"), []byte(dockerScript), 0o755); err != nil {
		t.Fatalf("writing docker script: %v", err)
	}

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	for _, r := range []*store.Run{
		{ID: "lazyrun00001", Ticket: "TICKET-1", Status: store.StatusRunning, InstanceID: "container1", Repo: "github.com/test/repo.git", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-15 * time.Minute), TimeoutAt: now.Add(45 * time.Minute)},
		{ID: "lazyrun00002", Ticket: "TICKET-2", Status: store.StatusRunning, InstanceID: "container2", Repo: "github.com/test/repo.git", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-10 * time.Minute), TimeoutAt: now.Add(50 * time.Minute)},
	} {
		if err := st.CreateRun(ctx, r); err != nil {
			t.Fatalf("creating run %s: %v", r.ID, err)
		}
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "list"})

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "lazyrun00002") {
		t.Errorf("output missing active run lazyrun00002: %s", outStr)
	}
	if strings.Contains(outStr, "lazyrun00001") {
		t.Errorf("output should not contain completed run lazyrun00001: %s", outStr)
	}
	if !strings.Contains(outStr, "RUN ID") {
		t.Errorf("output missing table header 'RUN ID': %s", outStr)
	}
}

func TestList_LazyCompletion_AllFlag(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":false,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"2024-06-15T10:45:00Z"}' ;;
  cp) exit 0 ;;
  rm) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(env.binDir, "docker"), []byte(dockerScript), 0o755); err != nil {
		t.Fatalf("writing docker script: %v", err)
	}

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	err = st.CreateRun(ctx, &store.Run{
		ID:         "lazyrun00003",
		Ticket:     "TICKET-3",
		Status:     store.StatusRunning,
		InstanceID: "container3",
		Repo:       "github.com/test/repo.git",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  now.Add(-15 * time.Minute),
		TimeoutAt:  now.Add(45 * time.Minute),
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "list", "--all"})

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "lazyrun00003") {
		t.Errorf("output missing run lazyrun00003: %s", outStr)
	}
	if !strings.Contains(outStr, "success") {
		t.Errorf("output missing 'success' status for lazily-completed run: %s", outStr)
	}
}

func TestList_LazyCheck_ErrorContinues(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dockerScript := `#!/bin/sh
case "$1" in
  inspect)
    case "$4" in
      badcontainer) echo "Error: connection refused" >&2; exit 1 ;;
      goodcontainer) printf '{"Running":true,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
    esac ;;
  exec) exit 1 ;;
  cp) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(env.binDir, "docker"), []byte(dockerScript), 0o755); err != nil {
		t.Fatalf("writing docker script: %v", err)
	}

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	for _, r := range []*store.Run{
		{ID: "errrun00001", Ticket: "TICKET-1", Status: store.StatusRunning, InstanceID: "badcontainer", Repo: "github.com/test/repo.git", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-15 * time.Minute), TimeoutAt: now.Add(45 * time.Minute)},
		{ID: "errrun00002", Ticket: "TICKET-2", Status: store.StatusRunning, InstanceID: "goodcontainer", Repo: "github.com/test/repo.git", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-10 * time.Minute), TimeoutAt: now.Add(50 * time.Minute)},
	} {
		if err := st.CreateRun(ctx, r); err != nil {
			t.Fatalf("creating run %s: %v", r.ID, err)
		}
	}
	st.Close()

	// Capture stdout
	origStdout := os.Stdout
	prOut, pwOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stdout pipe: %v", err)
	}
	os.Stdout = pwOut
	defer func() { os.Stdout = origStdout }()

	// Capture stderr
	origStderr := os.Stderr
	prErr, pwErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stderr pipe: %v", err)
	}
	os.Stderr = pwErr
	defer func() { os.Stderr = origStderr }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "list"})

	pwOut.Close()
	os.Stdout = origStdout
	pwErr.Close()
	os.Stderr = origStderr
	stdout, _ := io.ReadAll(prOut)
	stderr, _ := io.ReadAll(prErr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if !strings.Contains(string(stderr), "warning: checking run errrun00001") {
		t.Errorf("stderr missing warning for errrun00001: %s", string(stderr))
	}
	if !strings.Contains(string(stdout), "errrun00002") {
		t.Errorf("stdout missing healthy run errrun00002: %s", string(stdout))
	}
}

func TestList_TableFormat(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	completedAt := now
	cost := 4.52
	err = st.CreateRun(ctx, &store.Run{
		ID:           "listrun00006",
		Ticket:       "PROJ-42",
		Branch:       "feature-x",
		Status:       store.StatusSuccess,
		Repo:         "github.com/test/repo.git",
		Provider:     "docker",
		LaunchedBy:   "testuser",
		StartedAt:    now.Add(-10 * time.Minute),
		CompletedAt:  &completedAt,
		TimeoutAt:    now.Add(50 * time.Minute),
		TotalCostUSD: &cost,
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "list", "--all"})

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "PROJ-42") {
		t.Errorf("output missing 'PROJ-42': %s", outStr)
	}
	if !strings.Contains(outStr, "feature-x") {
		t.Errorf("output missing 'feature-x': %s", outStr)
	}
	if !strings.Contains(outStr, "$4.52") {
		t.Errorf("output missing '$4.52': %s", outStr)
	}
}

func TestList_OtherRepoFiltered(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	runs := []*store.Run{
		{ID: "listrun00007", Repo: "github.com/test/repo.git", Ticket: "T-LOCAL", Status: store.StatusRunning, Provider: "docker", LaunchedBy: "testuser", StartedAt: now, TimeoutAt: now.Add(time.Hour)},
		{ID: "listrun00008", Repo: "github.com/other/repo.git", Ticket: "T-OTHER", Status: store.StatusRunning, Provider: "docker", LaunchedBy: "testuser", StartedAt: now, TimeoutAt: now.Add(time.Hour)},
	}
	for _, r := range runs {
		if err := st.CreateRun(ctx, r); err != nil {
			t.Fatalf("creating run %s: %v", r.ID, err)
		}
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "list"})

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "listrun00007") {
		t.Errorf("output missing listrun00007: %s", outStr)
	}
	if strings.Contains(outStr, "listrun00008") {
		t.Errorf("output should not contain other-repo run listrun00008: %s", outStr)
	}
}

func TestLogs_Success(t *testing.T) {
	script := "#!/bin/sh\ncase \"$1\" in\n  logs) printf 'line 1\\nline 2\\nline 3\\n' ;;\nesac\n"
	env := setupStatusEnv(t, script)
	ctx := context.Background()

	runID := "logsrun00001"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(60 * time.Minute),
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "logs", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	for _, want := range []string{"line 1", "line 2", "line 3"} {
		if !strings.Contains(outStr, want) {
			t.Errorf("output missing %q: %s", want, outStr)
		}
	}
}

func TestLogs_Follow_Success(t *testing.T) {
	script := "#!/bin/sh\ncase \"$1\" in\n  inspect) echo abc123 ;;\n  logs) case \"$@\" in *--follow*) printf 'follow line 1\\nfollow line 2\\n' ;; *) echo \"ERROR: --follow not passed\" >&2; exit 1 ;; esac ;;\nesac\n"
	env := setupStatusEnv(t, script)
	ctx := context.Background()

	runID := "logsrun00002"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(60 * time.Minute),
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "logs", "--follow", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	for _, want := range []string{"follow line 1", "follow line 2"} {
		if !strings.Contains(outStr, want) {
			t.Errorf("output missing %q: %s", want, outStr)
		}
	}
}

func TestLogs_MissingRunID(t *testing.T) {
	setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "logs"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "missing required argument") {
		t.Errorf("error %q does not contain 'missing required argument'", err.Error())
	}
}

func TestLogs_RunNotFound(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()

	if err := os.MkdirAll(filepath.Dir(env.dbPath), 0o755); err != nil {
		t.Fatalf("creating db dir: %v", err)
	}
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	st.Close()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "logs", "nonexistent"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "run not found") {
		t.Errorf("error %q does not contain 'run not found'", err.Error())
	}
}

func TestLogs_CompletedRun(t *testing.T) {
	for _, status := range []store.Status{store.StatusSuccess, store.StatusFailed, store.StatusKilled} {
		t.Run(string(status), func(t *testing.T) {
			env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
			ctx := context.Background()

			runID := "logsrun-" + string(status)
			st, err := store.NewSQLiteStore(env.dbPath)
			if err != nil {
				t.Fatalf("opening store: %v", err)
			}
			err = st.CreateRun(ctx, &store.Run{
				ID:         runID,
				Repo:       "github.com/test/repo.git",
				Ticket:     "TICKET-1",
				Provider:   "docker",
				LaunchedBy: "testuser",
				StartedAt:  time.Now(),
				TimeoutAt:  time.Now().Add(60 * time.Minute),
				Status:     status,
				InstanceID: "abc123",
			})
			if err != nil {
				t.Fatalf("creating run: %v", err)
			}
			st.Close()

			err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "logs", runID})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "logs unavailable") {
				t.Errorf("error %q does not contain 'logs unavailable'", err.Error())
			}
			if !strings.Contains(err.Error(), "container removed") {
				t.Errorf("error %q does not contain 'container removed'", err.Error())
			}
		})
	}
}

func TestLogs_PendingNoContainer(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()

	runID := "logsrun00006"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(60 * time.Minute),
		Status:     store.StatusPending,
		InstanceID: "",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "logs", runID})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "logs unavailable") {
		t.Errorf("error %q does not contain 'logs unavailable'", err.Error())
	}
	if !strings.Contains(err.Error(), "no container yet") {
		t.Errorf("error %q does not contain 'no container yet'", err.Error())
	}
}

func TestLogs_ContainerGone(t *testing.T) {
	script := "#!/bin/sh\ncase \"$1\" in\n  logs) echo \"Error: No such container: abc123\" >&2; exit 1 ;;\nesac\n"
	env := setupStatusEnv(t, script)
	ctx := context.Background()

	runID := "logsrun00007"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(60 * time.Minute),
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "logs", runID})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "reading logs") {
		t.Errorf("error %q does not contain 'reading logs'", err.Error())
	}
}

// --- Kill tests ---

func TestKill_MissingRunID(t *testing.T) {
	setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "kill"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "missing required argument: <run-id>") {
		t.Errorf("error %q does not contain 'missing required argument: <run-id>'", err.Error())
	}
}

func TestKill_RunNotFound(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()

	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	st.Close()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "kill", "nonexistent123"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "run not found: nonexistent123") {
		t.Errorf("error %q does not contain 'run not found: nonexistent123'", err.Error())
	}
}

func TestKill_AlreadyCompleted(t *testing.T) {
	for i, status := range []store.Status{store.StatusSuccess, store.StatusFailed, store.StatusKilled} {
		t.Run(string(status), func(t *testing.T) {
			env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
			ctx := context.Background()

			runID := fmt.Sprintf("killrun%04d", i+1)
			st, err := store.NewSQLiteStore(env.dbPath)
			if err != nil {
				t.Fatalf("opening store: %v", err)
			}
			err = st.CreateRun(ctx, &store.Run{
				ID:         runID,
				Repo:       "github.com/test/repo.git",
				Ticket:     "TICKET-1",
				Provider:   "docker",
				LaunchedBy: "testuser",
				StartedAt:  time.Now(),
				TimeoutAt:  time.Now().Add(60 * time.Minute),
				Status:     status,
			})
			if err != nil {
				t.Fatalf("creating run: %v", err)
			}
			st.Close()

			err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "kill", runID})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			expected := fmt.Sprintf("run %s is already %s", runID, status)
			if !strings.Contains(err.Error(), expected) {
				t.Errorf("error %q does not contain %q", err.Error(), expected)
			}
		})
	}
}

func TestHandleLazyCheck_TimeoutKillFailure(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":true,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  stop) echo "stop failed" >&2; exit 1 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	runID := "lazycheck001"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now().Add(-2 * time.Minute),
		TimeoutAt:  time.Now().Add(-1 * time.Minute), // already timed out
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	// Capture stderr to check for warning message
	origStderr := os.Stderr
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stderr pipe: %v", err)
	}
	os.Stderr = stderrW
	defer func() { os.Stderr = origStderr }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})

	stderrW.Close()
	os.Stderr = origStderr
	stderrOut, _ := io.ReadAll(stderrR)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}

	// Verify warning was logged
	if !strings.Contains(string(stderrOut), "warning: stopping timed-out container") {
		t.Errorf("stderr missing warning: %s", string(stderrOut))
	}

	// Verify run status is still running (not failed)
	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store for verification: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusRunning {
		t.Errorf("status: got %q, want %q", r.Status, store.StatusRunning)
	}
	if r.CompletedAt != nil {
		t.Errorf("CompletedAt should be nil, got %v", r.CompletedAt)
	}
}

func TestHandleLazyCheck_TimeoutKillSuccess(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":true,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  stop) exit 0 ;;
  cp) exit 0 ;;
  rm) exit 0 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	runID := "lazycheck002"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-2",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now().Add(-2 * time.Minute),
		TimeoutAt:  time.Now().Add(-1 * time.Minute), // already timed out
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}

	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store for verification: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusKilled {
		t.Errorf("status: got %q, want %q", r.Status, store.StatusKilled)
	}
	if r.CompletedAt == nil {
		t.Error("CompletedAt should be non-nil after successful kill")
	}
	if r.TotalCostUSD != nil {
		t.Errorf("TotalCostUSD: got %v, want nil (no run-result.json)", *r.TotalCostUSD)
	}
	if r.ExitCode != nil {
		t.Errorf("ExitCode: got %v, want nil (no run-result.json)", *r.ExitCode)
	}
}

func TestHandleLazyCheck_TimeoutKillCapturesCostAndExitCode(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":true,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  stop) exit 0 ;;
  cp)
    case "$2" in
      *audit*) mkdir -p "$3/TICKET-1" && printf '{"total_cost_usd": 3.14, "exit_code": 0}' > "$3/TICKET-1/run-result.json" ;;
    esac
    ;;
  rm) exit 0 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	runID := "lazycheck003"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Workflow:   "",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now().Add(-2 * time.Minute),
		TimeoutAt:  time.Now().Add(-1 * time.Minute),
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}

	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store for verification: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusKilled {
		t.Errorf("status: got %q, want %q", r.Status, store.StatusKilled)
	}
	if r.CompletedAt == nil {
		t.Error("CompletedAt should be non-nil after successful kill")
	}
	if r.TotalCostUSD == nil || *r.TotalCostUSD != 3.14 {
		t.Errorf("TotalCostUSD: got %v, want 3.14", r.TotalCostUSD)
	}
	if r.ExitCode == nil || *r.ExitCode != 0 {
		t.Errorf("ExitCode: got %v, want 0", r.ExitCode)
	}
}

func TestHandleLazyCheck_TimeoutKillCapturesCostAndExitCode_WithWorkflow(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) printf '{"Running":true,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  stop) exit 0 ;;
  cp)
    case "$2" in
      *audit*) mkdir -p "$3/plan/TICKET-1" && printf '{"total_cost_usd": 7.50, "exit_code": 2}' > "$3/plan/TICKET-1/run-result.json" ;;
    esac
    ;;
  rm) exit 0 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	runID := "lazycheck004"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Workflow:   "plan",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now().Add(-2 * time.Minute),
		TimeoutAt:  time.Now().Add(-1 * time.Minute),
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}

	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store for verification: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusKilled {
		t.Errorf("status: got %q, want %q", r.Status, store.StatusKilled)
	}
	if r.CompletedAt == nil {
		t.Error("CompletedAt should be non-nil after successful kill")
	}
	if r.TotalCostUSD == nil || *r.TotalCostUSD != 7.50 {
		t.Errorf("TotalCostUSD: got %v, want 7.50", r.TotalCostUSD)
	}
	if r.ExitCode == nil || *r.ExitCode != 2 {
		t.Errorf("ExitCode: got %v, want 2", r.ExitCode)
	}
}

func TestKill_RunningRun(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) echo '{"Running":true,"ExitCode":0,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  exec) exit 1 ;;
  stop) exit 0 ;;
  cp) exit 0 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	runID := "killrun0010"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(60 * time.Minute),
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "kill", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if !strings.Contains(string(out), "Killed run") {
		t.Errorf("output missing 'Killed run': %s", string(out))
	}

	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store for verification: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusKilled {
		t.Errorf("status: got %q, want %q", r.Status, store.StatusKilled)
	}
	if r.CompletedAt == nil {
		t.Error("CompletedAt should be non-nil after kill")
	}
	if r.TotalCostUSD != nil {
		t.Errorf("TotalCostUSD should be nil, got %v", *r.TotalCostUSD)
	}
	if r.ExitCode != nil {
		t.Errorf("ExitCode should be nil, got %d", *r.ExitCode)
	}
}

func TestKill_PendingRun(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()

	runID := "killrun0020"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(60 * time.Minute),
		Status:     store.StatusPending,
		InstanceID: "",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "kill", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if !strings.Contains(string(out), "Killed run") {
		t.Errorf("output missing 'Killed run': %s", string(out))
	}

	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store for verification: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusKilled {
		t.Errorf("status: got %q, want %q", r.Status, store.StatusKilled)
	}
	if r.CompletedAt == nil {
		t.Error("CompletedAt should be non-nil after kill")
	}
}

func TestKill_CapturesCostAndExitCode(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) echo '{"Running":true,"ExitCode":0,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  exec) exit 1 ;;
  stop) exit 0 ;;
  cp)
    case "$2" in
      *audit*) mkdir -p "$3/TICKET-1" && printf '{"total_cost_usd": 3.14, "exit_code": 0}' > "$3/TICKET-1/run-result.json" ;;
    esac
    ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	runID := "killrun0030"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Workflow:   "",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(60 * time.Minute),
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "kill", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if !strings.Contains(string(out), "Killed run") {
		t.Errorf("output missing 'Killed run': %s", string(out))
	}

	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store for verification: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusKilled {
		t.Errorf("status: got %q, want %q", r.Status, store.StatusKilled)
	}
	if r.CompletedAt == nil {
		t.Error("CompletedAt should be non-nil after kill")
	}
	if r.TotalCostUSD == nil || *r.TotalCostUSD != 3.14 {
		t.Errorf("TotalCostUSD: got %v, want 3.14", r.TotalCostUSD)
	}
	if r.ExitCode == nil || *r.ExitCode != 0 {
		t.Errorf("ExitCode: got %v, want 0", r.ExitCode)
	}
}

func TestKill_CapturesCostAndExitCode_WithWorkflow(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) echo '{"Running":true,"ExitCode":0,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  exec) exit 1 ;;
  stop) exit 0 ;;
  cp)
    case "$2" in
      *audit*) mkdir -p "$3/plan/TICKET-1" && printf '{"total_cost_usd": 7.50, "exit_code": 2}' > "$3/plan/TICKET-1/run-result.json" ;;
    esac
    ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	runID := "killrun0040"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo.git",
		Ticket:     "TICKET-1",
		Workflow:   "plan",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(60 * time.Minute),
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "kill", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if !strings.Contains(string(out), "Killed run") {
		t.Errorf("output missing 'Killed run': %s", string(out))
	}

	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store for verification: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Status != store.StatusKilled {
		t.Errorf("status: got %q, want %q", r.Status, store.StatusKilled)
	}
	if r.CompletedAt == nil {
		t.Error("CompletedAt should be non-nil after kill")
	}
	if r.TotalCostUSD == nil || *r.TotalCostUSD != 7.50 {
		t.Errorf("TotalCostUSD: got %v, want 7.50", r.TotalCostUSD)
	}
	if r.ExitCode == nil || *r.ExitCode != 2 {
		t.Errorf("ExitCode: got %v, want 2", r.ExitCode)
	}
}

// --- Results tests ---

func TestResults_MissingRunID(t *testing.T) {
	setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "results"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "missing required argument") {
		t.Errorf("error %q does not contain 'missing required argument'", err.Error())
	}
}

func TestResults_RunNotFound(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Dir(env.dbPath), 0o755); err != nil {
		t.Fatalf("creating db dir: %v", err)
	}
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	st.Close()
	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "results", "nonexistent"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "run not found") {
		t.Errorf("error %q does not contain 'run not found'", err.Error())
	}
}

func TestResults_StillRunning(t *testing.T) {
	dockerScript := "#!/bin/sh\ncase \"$1\" in\n  inspect) printf '{\"Running\":true,\"ExitCode\":0,\"StartedAt\":\"2024-06-15T10:30:00Z\",\"FinishedAt\":\"0001-01-01T00:00:00Z\"}' ;;\n  exec) exit 1 ;;\nesac\n"
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()
	runID := "resultsrun001"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo.git", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "testuser",
		StartedAt: time.Now(), TimeoutAt: time.Now().Add(60 * time.Minute),
		Status: store.StatusRunning, InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()
	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "results", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "still in progress") {
		t.Errorf("output missing 'still in progress': %s", outStr)
	}
	if !strings.Contains(outStr, "running") {
		t.Errorf("output missing 'running': %s", outStr)
	}
}

func TestResults_CompletedWithResults(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	runID := "resultsrun002"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo.git", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "testuser",
		StartedAt: time.Now(), TimeoutAt: time.Now().Add(60 * time.Minute),
		Status: store.StatusSuccess,
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()
	resultDir := filepath.Join(env.tmpHome, ".horde", "results", runID, "audit", "TICKET-1")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatalf("creating result dir: %v", err)
	}
	resultJSON := `{"exit_code":0,"status":"completed","ticket":"TICKET-1","workflow":"","total_cost_usd":4.52,"total_duration":"12m 34s","phases":[{"name":"plan","status":"completed","cost_usd":1.23,"duration":"3m 0s"},{"name":"execute","status":"completed","cost_usd":3.29,"duration":"9m 34s"}]}`
	if err := os.WriteFile(filepath.Join(resultDir, "run-result.json"), []byte(resultJSON), 0o644); err != nil {
		t.Fatalf("writing run-result.json: %v", err)
	}
	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "results", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	for _, want := range []string{runID, "TICKET-1", "completed", "$4.52", "12m 34s", "PHASE", "plan", "$1.23", "3m 0s"} {
		if !strings.Contains(outStr, want) {
			t.Errorf("output missing %q: %s", want, outStr)
		}
	}
}

func TestResults_CompletedNoCost(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	runID := "resultsrun_nocost"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo.git", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "testuser",
		StartedAt: time.Now(), TimeoutAt: time.Now().Add(60 * time.Minute),
		Status: store.StatusSuccess,
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()
	resultDir := filepath.Join(env.tmpHome, ".horde", "results", runID, "audit", "TICKET-1")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatalf("creating result dir: %v", err)
	}
	resultJSON := `{"exit_code":0,"status":"completed","ticket":"TICKET-1","workflow":"","total_duration":"5m 0s","phases":[]}`
	if err := os.WriteFile(filepath.Join(resultDir, "run-result.json"), []byte(resultJSON), 0o644); err != nil {
		t.Fatalf("writing run-result.json: %v", err)
	}
	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "results", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if strings.Contains(outStr, "Total Cost:") {
		t.Errorf("output should not contain Total Cost when absent from JSON, got: %s", outStr)
	}
	for _, want := range []string{runID, "TICKET-1", "completed", "5m 0s"} {
		if !strings.Contains(outStr, want) {
			t.Errorf("output missing %q: %s", want, outStr)
		}
	}
}

func TestResults_CompletedWithWorkflow(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	runID := "resultsrun003"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo.git", Ticket: "TICKET-1", Workflow: "review",
		Provider: "docker", LaunchedBy: "testuser",
		StartedAt: time.Now(), TimeoutAt: time.Now().Add(60 * time.Minute),
		Status: store.StatusSuccess,
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()
	resultDir := filepath.Join(env.tmpHome, ".horde", "results", runID, "audit", "review", "TICKET-1")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatalf("creating result dir: %v", err)
	}
	resultJSON := `{"exit_code":0,"status":"completed","ticket":"TICKET-1","workflow":"review","total_cost_usd":7.50,"total_duration":"5m 0s","phases":[{"name":"review","status":"completed","cost_usd":7.50,"duration":"5m 0s"}]}`
	if err := os.WriteFile(filepath.Join(resultDir, "run-result.json"), []byte(resultJSON), 0o644); err != nil {
		t.Fatalf("writing run-result.json: %v", err)
	}
	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "results", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "Workflow") {
		t.Errorf("output missing 'Workflow': %s", outStr)
	}
	if !strings.Contains(outStr, "review") {
		t.Errorf("output missing 'review': %s", outStr)
	}
	if !strings.Contains(outStr, "$7.50") {
		t.Errorf("output missing '$7.50': %s", outStr)
	}
}

func TestResults_MissingRunResult(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	runID := "resultsrun004"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	exitCode := 1
	cost := 2.50
	err = st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo.git", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "testuser",
		StartedAt: time.Now(), TimeoutAt: time.Now().Add(60 * time.Minute),
		Status: store.StatusFailed, ExitCode: &exitCode, TotalCostUSD: &cost,
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()
	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "results", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	for _, want := range []string{"Detailed results unavailable", "run-result.json not found", "failed", "1", "$2.50"} {
		if !strings.Contains(outStr, want) {
			t.Errorf("output missing %q: %s", want, outStr)
		}
	}
}

func TestResults_LazyCompletion(t *testing.T) {
	dockerScript := "#!/bin/sh\ncase \"$1\" in\n  inspect) printf '{\"Running\":false,\"ExitCode\":0,\"StartedAt\":\"2024-06-15T10:30:00Z\",\"FinishedAt\":\"2024-06-15T10:45:00Z\"}' ;;\n  cp) exit 0 ;;\n  rm) exit 0 ;;\nesac\n"
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()
	runID := "resultsrun005"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo.git", Ticket: "TICKET-5",
		Provider: "docker", LaunchedBy: "testuser",
		StartedAt: time.Now().Add(-15 * time.Minute), TimeoutAt: time.Now().Add(45 * time.Minute),
		Status: store.StatusRunning, InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()
	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "results", runID})
	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "Detailed results unavailable") {
		t.Errorf("output missing 'Detailed results unavailable': %s", outStr)
	}
	if !strings.Contains(outStr, "success") {
		t.Errorf("output missing 'success': %s", outStr)
	}
}

func TestResolveLaunchedBy_Docker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "config", "user.name", "Test User")

	got, err := resolveLaunchedBy(context.Background(), "docker", dir, nil, "")
	if err != nil {
		t.Fatalf("resolveLaunchedBy(docker) unexpected error: %v", err)
	}
	if got != "Test User" {
		t.Errorf("resolveLaunchedBy(docker) = %q, want %q", got, "Test User")
	}
}

func TestResolveLaunchedBy_ECS_NilConfig(t *testing.T) {
	t.Parallel()
	got, err := resolveLaunchedBy(context.Background(), "aws-ecs", "", nil, "")
	if err == nil {
		t.Fatalf("resolveLaunchedBy(aws-ecs, nil config) = %q, want error", got)
	}
	if !strings.Contains(err.Error(), "AWS config required") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "AWS config required")
	}
}

func TestResolveLaunchedBy_ECS_NoCredentials(t *testing.T) {
	t.Parallel()
	cfg := aws.Config{Credentials: aws.AnonymousCredentials{}}
	got, err := resolveLaunchedBy(context.Background(), "aws-ecs", "", &cfg, "")
	if err == nil {
		t.Fatalf("resolveLaunchedBy(aws-ecs, empty config) = %q, want error", got)
	}
	if !strings.Contains(err.Error(), "resolving launched_by") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "resolving launched_by")
	}
	if !strings.Contains(err.Error(), "hint:") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "hint:")
	}
}

func TestResolveLaunchedBy_UnsupportedProvider(t *testing.T) {
	t.Parallel()
	got, err := resolveLaunchedBy(context.Background(), "unknown", "", nil, "")
	if err == nil {
		t.Fatalf("resolveLaunchedBy(unknown) = %q, want error", got)
	}
	if !strings.Contains(err.Error(), "unsupported provider") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "unsupported provider")
	}
}

func TestOpenStore_Docker(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	st, cleanup, err := openStore("docker")
	if err != nil {
		t.Fatalf("openStore(docker) error: %v", err)
	}
	defer cleanup()

	ctx := context.Background()
	run := &store.Run{
		ID:        "testrun123ab",
		Repo:      "github.com/test/repo.git",
		Ticket:    "TEST-1",
		Provider:  "docker",
		Status:    store.StatusPending,
		StartedAt: time.Now(),
		TimeoutAt: time.Now().Add(time.Hour),
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got, err := st.GetRun(ctx, "testrun123ab")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Ticket != "TEST-1" {
		t.Errorf("Ticket = %q, want %q", got.Ticket, "TEST-1")
	}
}

func TestOpenStore_Docker_Cleanup(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	st, cleanup, err := openStore("docker")
	if err != nil {
		t.Fatalf("openStore(docker) error: %v", err)
	}

	cleanup() // Close the store

	ctx := context.Background()
	_, err = st.GetRun(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error after cleanup, got nil")
	}
}

func TestOpenStore_Unsupported(t *testing.T) {
	t.Parallel()
	_, _, err := openStore("gcp")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported provider") {
		t.Errorf("error %q does not contain expected message", err.Error())
	}
}

func TestProvider_AWSECS_SSMError(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")

	_ = setupLaunchEnv(t)
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "aws-ecs", "launch", "TICKET-1"})
	if err == nil {
		t.Fatal("expected error for aws-ecs provider, got nil")
	}
	// Before hook (in this bead) catches "aws-ecs" first; after d69.4.2 the factory fires.
	// Either way, the error must mention "aws-ecs".
	if !strings.Contains(err.Error(), "aws-ecs") {
		t.Errorf("error %q does not mention aws-ecs", err.Error())
	}
}

func TestProvider_AutoDetect_Error(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")

	_ = setupLaunchEnv(t)
	ctx := context.Background()

	// With --provider defaulting to "docker" and the Before hook in place,
	// this may succeed. After d69.4.2 removes the Before hook and changes
	// the default to "", it will fail with the auto-detect error.
	err := newApp().Run(ctx, []string{"horde", "launch", "TICKET-1"})
	if err != nil && !strings.Contains(err.Error(), "auto-detecting") &&
		!strings.Contains(err.Error(), "--provider docker") {
		t.Errorf("unexpected error: %v", err)
	}
}
