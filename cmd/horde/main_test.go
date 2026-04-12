package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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
	envContent := "ANTHROPIC_API_KEY=test-key\nGIT_TOKEN=test-token\n"
	if err := os.WriteFile(filepath.Join(projectDir, ".env"), []byte(envContent), 0o644); err != nil {
		t.Fatalf("writing .env: %v", err)
	}

	// Create binDir with fake docker — pattern from internal/provider/docker_test.go:16-22
	binDir := filepath.Join(tmpHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("creating bin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\necho abc123container\n"), 0o755); err != nil {
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
	pr, pw, _ := os.Pipe()
	os.Stdout = pw

	err := newApp().Run(ctx, []string{"horde", "launch", "TICKET-1"})

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
	expectedTimeout := r.StartedAt.Add(60 * time.Minute)
	if diff := r.TimeoutAt.Sub(expectedTimeout); diff < -5*time.Second || diff > 5*time.Second {
		t.Errorf("TimeoutAt %v not within 5s of StartedAt+60m %v", r.TimeoutAt, expectedTimeout)
	}
}

func TestLaunch_WithFlags(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	origStdout := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw

	err := newApp().Run(ctx, []string{"horde", "launch", "--branch", "feature-x", "--workflow", "review", "--timeout", "30m", "TICKET-2"})

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

	err := newApp().Run(ctx, []string{"horde", "launch"})
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
	os.WriteFile(filepath.Join(projectDir, ".env"), []byte("ANTHROPIC_API_KEY=test-key\nGIT_TOKEN=test-token\n"), 0o644)
	binDir := filepath.Join(tmpHome, "bin")
	os.MkdirAll(binDir, 0o755)
	os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\necho abc123container\n"), 0o755)
	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	oldDir, _ := os.Getwd()
	os.Chdir(projectDir)
	t.Cleanup(func() { os.Chdir(oldDir) })

	ctx := context.Background()
	err := newApp().Run(ctx, []string{"horde", "launch", "TICKET-1"})
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
	err := newApp().Run(ctx, []string{"horde", "launch", "TICKET-1"})
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

	// Write .env with only ANTHROPIC_API_KEY, missing GIT_TOKEN
	os.WriteFile(filepath.Join(projectDir, ".env"), []byte("ANTHROPIC_API_KEY=test\n"), 0o644)

	binDir := filepath.Join(tmpHome, "bin")
	os.MkdirAll(binDir, 0o755)
	os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\necho abc123container\n"), 0o755)
	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	oldDir, _ := os.Getwd()
	os.Chdir(projectDir)
	t.Cleanup(func() { os.Chdir(oldDir) })

	ctx := context.Background()
	err := newApp().Run(ctx, []string{"horde", "launch", "TICKET-1"})
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

	err = newApp().Run(ctx, []string{"horde", "launch", "TICKET-1"})
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
	pr, pw, _ := os.Pipe()
	os.Stdout = pw

	err = newApp().Run(ctx, []string{"horde", "launch", "--force", "TICKET-1"})

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

	os.WriteFile(filepath.Join(env.binDir, "docker"), []byte(`#!/bin/sh
echo "Error: Cannot connect to the Docker daemon" >&2
exit 1
`), 0o755)

	err := newApp().Run(ctx, []string{"horde", "launch", "TICKET-1"})
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
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	runErr := newApp().Run(ctx, []string{"horde", "status", runID})
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
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	runErr := newApp().Run(ctx, []string{"horde", "status", runID})
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

	err = newApp().Run(ctx, []string{"horde", "status", "nonexistent"})
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

	err := newApp().Run(ctx, []string{"horde", "status"})
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
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	runErr := newApp().Run(ctx, []string{"horde", "status", runID})
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
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	runErr := newApp().Run(ctx, []string{"horde", "status", runID})
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
	// Kill() calls Status() internally (docker.go:201), so inspect is called twice — script is intentionally stateless
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
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	runErr := newApp().Run(ctx, []string{"horde", "status", runID})
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
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	runErr := newApp().Run(ctx, []string{"horde", "status", runID})
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
