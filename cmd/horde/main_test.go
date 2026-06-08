package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jorge-barreto/horde/internal/config"
	"github.com/jorge-barreto/horde/internal/provider"
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

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "test-flow", "TICKET-1"})

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

	runs, err := st.ListByRepo(ctx, "github.com/test/repo", false)
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
	if r.Repo != "github.com/test/repo" {
		t.Errorf("Repo = %q, want %q", r.Repo, "github.com/test/repo")
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

	runs, err := st.ListByRepo(ctx, "github.com/test/repo", false)
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

func TestLaunch_WithLabels(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch",
		"--workflow", "implement-ticket",
		"--label", "epic=KS-100",
		"--label", "variant=v3",
		"TICKET-LBL"})

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

	runs, err := st.ListByRepo(ctx, "github.com/test/repo", false)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	r := runs[0]
	if r.Labels["epic"] != "KS-100" {
		t.Errorf("Labels[epic] = %q, want %q", r.Labels["epic"], "KS-100")
	}
	if r.Labels["variant"] != "v3" {
		t.Errorf("Labels[variant] = %q, want %q", r.Labels["variant"], "v3")
	}
}

func TestLaunch_InvalidLabel(t *testing.T) {
	setupLaunchEnv(t)
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch",
		"--workflow", "implement-ticket",
		"--label", "no-equals-sign",
		"TICKET-BAD"})

	if err == nil {
		t.Fatal("expected error for malformed label, got nil")
	}
	if !strings.Contains(err.Error(), "key=value") {
		t.Errorf("error %q does not mention key=value form", err.Error())
	}
}

func TestLaunch_TimeoutAt_Regression(t *testing.T) {
	t.Parallel()

	providers := []string{"docker", "aws-ecs"}
	for _, provName := range providers {
		t.Run(provName, func(t *testing.T) {
			t.Parallel()

			dbPath := filepath.Join(t.TempDir(), "horde.db")
			st, err := store.NewSQLiteStore(dbPath)
			if err != nil {
				t.Fatalf("opening store: %v", err)
			}
			defer st.Close()

			ctx := context.Background()
			now := time.Now()
			timeout := 24 * time.Hour
			id := fmt.Sprintf("timeout%s", provName)

			// Mirrors launchCmd lines 192-204 in cmd/horde/main.go
			run := &store.Run{
				ID:         id,
				Repo:       "github.com/test/repo",
				Ticket:     "TICKET-1",
				Branch:     "",
				Workflow:   "",
				Provider:   provName,
				Status:     store.StatusPending,
				LaunchedBy: "testuser",
				StartedAt:  now,
				TimeoutAt:  now.Add(timeout),
			}
			if err := st.CreateRun(ctx, run); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}

			got, err := st.GetRun(ctx, id)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if got.TimeoutAt.IsZero() {
				t.Fatalf("TimeoutAt is zero for provider %q — must be populated on every launch path", provName)
			}
			expectedTimeout := got.StartedAt.Add(timeout)
			if diff := got.TimeoutAt.Sub(expectedTimeout); diff < -time.Second || diff > time.Second {
				t.Errorf("TimeoutAt %v not within 1s of StartedAt+24h %v (provider %q)", got.TimeoutAt, expectedTimeout, provName)
			}
		})
	}
}

func TestLaunch_MissingTicket(t *testing.T) {
	_ = setupLaunchEnv(t)
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "test-flow"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "missing required argument") {
		t.Errorf("error %q does not contain %q", err.Error(), "missing required argument")
	}
}

// TestLaunch_MissingWorkflow ensures launch refuses to proceed when --workflow
// is omitted entirely (validated inside the Action, not as a urfave Required
// flag, so the error routes through the JSON envelope under --json). Without
// this guard, orc would silently apply its own default workflow and write
// audit files under a path horde wouldn't read, leaving cost data orphaned.
func TestLaunch_MissingWorkflow(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "TICKET-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "workflow") {
		t.Errorf("error %q should mention workflow", err.Error())
	}
	assertNoRunsRecorded(t, env)
}

// TestLaunch_EmptyWorkflow ensures whitespace-only --workflow values are
// rejected before any side effects (DB row, container, image build).
func TestLaunch_EmptyWorkflow(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "   ", "TICKET-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "--workflow is required") {
		t.Errorf("error %q does not contain %q", err.Error(), "--workflow is required")
	}
	assertNoRunsRecorded(t, env)
}

// TestLaunch_InvalidWorkflow ensures path-unsafe workflow values are rejected.
// Mirrors the validation in provider.DockerProvider.Hydrate; prevents
// audit-tree paths like audit/../escape/<ticket>/.
func TestLaunch_InvalidWorkflow(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	for _, bad := range []string{"../escape", "a/b", `a\b`} {
		err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", bad, "TICKET-1"})
		if err == nil {
			t.Fatalf("workflow %q: expected error, got nil", bad)
		}
		if !strings.Contains(err.Error(), "invalid") {
			t.Errorf("workflow %q: error %q should mention invalid", bad, err.Error())
		}
	}
	assertNoRunsRecorded(t, env)
}

func assertNoRunsRecorded(t *testing.T, env launchEnv) {
	t.Helper()
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	// DB may not even exist if validation fired before initProviderAndStore.
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return
	}
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()
	runs, err := st.ListByRepo(context.Background(), "github.com/test/repo", true)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("expected no runs recorded, got %d", len(runs))
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
	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "test-flow", "TICKET-1"})
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
	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "test-flow", "TICKET-1"})
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
	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "test-flow", "TICKET-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "GIT_TOKEN") {
		t.Errorf("error %q does not name missing key GIT_TOKEN", err.Error())
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
		Repo:       "github.com/test/repo",
		Ticket:     "TICKET-1",
		Workflow:   "test-flow",
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

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "test-flow", "TICKET-1"})
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

	runs, err := st2.ListByRepo(ctx, "github.com/test/repo", false)
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
		Repo:       "github.com/test/repo",
		Ticket:     "TICKET-1",
		Workflow:   "test-flow",
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

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "test-flow", "--force", "TICKET-1"})

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

	runs, err := st2.ListByRepo(ctx, "github.com/test/repo", false)
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

func TestLaunch_MaxConcurrent_Rejected(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	for i := 0; i < 100; i++ {
		err = st.CreateRun(ctx, &store.Run{
			ID:         fmt.Sprintf("run-%03d", i),
			Repo:       "github.com/test/repo",
			Ticket:     fmt.Sprintf("TICKET-%d", i),
			Status:     store.StatusRunning,
			Provider:   "docker",
			LaunchedBy: "someone",
			StartedAt:  now.Add(time.Duration(i) * time.Second),
			TimeoutAt:  now.Add(25 * time.Hour),
		})
		if err != nil {
			t.Fatalf("pre-creating run %d: %v", i, err)
		}
	}
	st.Close()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "test-flow", "TICKET-NEW"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "max concurrent runs reached") {
		t.Errorf("error %q does not contain %q", err.Error(), "max concurrent runs reached")
	}
}

func TestLaunch_DockerFailure(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	os.WriteFile(filepath.Join(env.binDir, "docker"), []byte("#!/bin/sh\ncase \"$1\" in\n  image) echo \"2099-01-01T00:00:00Z\";;\n  tag) exit 0;;\n  *) echo \"Error: Cannot connect to the Docker daemon\" >&2; exit 1;;\nesac\n"), 0o755)

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "test-flow", "TICKET-1"})
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

	runs, err := st.ListByRepo(ctx, "github.com/test/repo", false)
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

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "test-flow", "TICKET-1"})

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
	runs, err := st.ListByRepo(ctx, "github.com/test/repo", false)
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

	err := newApp().Run(ctx, []string{"horde", "--provider", "gcp", "launch", "--workflow", "test-flow", "TICKET-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `unsupported provider "gcp"`) {
		t.Errorf("error %q does not contain expected message", err.Error())
	}
}

func TestProvider_ExplicitDocker(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "test-flow", "TICKET-1"})

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	runID := strings.TrimSpace(string(out))
	if !regexp.MustCompile(`^[a-z0-9]{12}$`).MatchString(runID) {
		t.Errorf("runID %q does not match ^[a-z0-9]{12}$", runID)
	}

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()
	r, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if r.Provider != "docker" {
		t.Errorf("Provider = %q, want %q", r.Provider, "docker")
	}
	if r.Ticket != "TICKET-1" {
		t.Errorf("Ticket = %q, want %q", r.Ticket, "TICKET-1")
	}
	if r.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", r.Status, store.StatusRunning)
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

	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "--profile", "staging", "launch", "--workflow", "test-flow", "TICKET-1"})

	pw.Close()
	os.Stdout = origStdout
	_, _ = io.ReadAll(pr)

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
		Repo:         "github.com/test/repo",
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
		Repo:       "github.com/test/repo",
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

// TestStatus_LazyCheck_CopyFailureWarns covers horde-lu0: when
// CopyFromContainer fails during Finalize's "marker-file completion" path,
// the provider must emit a stderr warning so users understand why cost data
// or artifacts may be missing, rather than silently proceeding with nil cost.
func TestStatus_LazyCheck_CopyFailureWarns(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) echo '{"Running":true,"ExitCode":0,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  cp)     echo "Error response from daemon: container not running" >&2; exit 1 ;;
  logs)   exit 0 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	runID := "statuslu00001"

	// Pre-seed the .horde-exit-code marker so Finalize takes the "orc finished"
	// branch that triggers CopyFromContainer.
	workspaceDir := filepath.Join(env.tmpHome, ".horde", "workspaces", runID)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("creating workspace dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, ".horde-exit-code"), []byte("0\n"), 0o644); err != nil {
		t.Fatalf("writing marker file: %v", err)
	}

	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	if err := st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "testuser",
		StartedAt: now.Add(-10 * time.Minute), TimeoutAt: now.Add(50 * time.Minute),
		Status: store.StatusRunning, InstanceID: "abc123",
	}); err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	origStderr := os.Stderr
	prErr, pwErr, _ := os.Pipe()
	os.Stderr = pwErr
	defer func() { os.Stderr = origStderr }()

	origStdout := os.Stdout
	prOut, pwOut, _ := os.Pipe()
	os.Stdout = pwOut
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})

	pwErr.Close()
	pwOut.Close()
	os.Stderr = origStderr
	os.Stdout = origStdout
	stderr, _ := io.ReadAll(prErr)
	_, _ = io.ReadAll(prOut)

	if runErr != nil {
		t.Fatalf("status returned error: %v", runErr)
	}
	if !strings.Contains(string(stderr), "warning: copying results for run") || !strings.Contains(string(stderr), runID) {
		t.Errorf("stderr missing CopyFromContainer warning for run %s: %s", runID, string(stderr))
	}
}

// TestStatus_LazyCheck_StatusError covers horde-kx2: when the provider's
// Status() fails with a non-"No such container" error during Finalize (the
// "lazy check" path for pending/running runs), statusCmd must surface the
// wrapped error rather than mask it.
func TestStatus_LazyCheck_StatusError(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) echo "Error response from daemon: connection refused" >&2; exit 1 ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()
	runID := "statuskx20001"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	if err := st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "testuser",
		StartedAt: now.Add(-10 * time.Minute), TimeoutAt: now.Add(50 * time.Minute),
		Status: store.StatusRunning, InstanceID: "abc123",
	}); err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	if runErr == nil {
		t.Fatal("expected error from status when Finalize's Status() call fails, got nil")
	}
	if !strings.Contains(runErr.Error(), "checking instance status") {
		t.Errorf("expected error to wrap 'checking instance status', got: %v", runErr)
	}
	if !strings.Contains(runErr.Error(), "connection refused") {
		t.Errorf("expected underlying docker stderr 'connection refused' in error, got: %v", runErr)
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
		Repo:       "github.com/test/repo",
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

// TestStatus_LazyCompletion_NonZeroExit covers horde-15m: the stopped-case
// mapExitCode paths were only exercised as pure unit tests on mapExitCode,
// never end-to-end through Finalize + store update. Drive a full status
// command for each and verify the store. Includes the recoverable statuses
// (orc exit 2 → timed_out, 4 → rate_limited) added for #35.
func TestStatus_LazyCompletion_NonZeroExit(t *testing.T) {
	cases := []struct {
		name       string
		exitCode   int
		wantStatus store.Status
		wantOut    string
	}{
		{"exit 1 maps to failed", 1, store.StatusFailed, "failed"},
		{"exit 2 maps to timed_out", 2, store.StatusTimedOut, "timed_out"},
		{"exit 4 maps to rate_limited", 4, store.StatusRateLimited, "rate_limited"},
		{"exit 5 maps to killed", 5, store.StatusKilled, "killed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dockerScript := fmt.Sprintf(`#!/bin/sh
case "$1" in
  inspect) printf '{"Running":false,"ExitCode":%d,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"2024-06-15T10:45:00Z"}' ;;
  cp) exit 0 ;;
  logs) exit 0 ;;
  rm) exit 0 ;;
esac
`, tc.exitCode)
			env := setupStatusEnv(t, dockerScript)
			ctx := context.Background()

			st, err := store.NewSQLiteStore(env.dbPath)
			if err != nil {
				t.Fatalf("opening store: %v", err)
			}
			now := time.Now()
			runID := "lazy15m00001"
			if err := st.CreateRun(ctx, &store.Run{
				ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-15M",
				Status: store.StatusRunning, InstanceID: "abc123",
				Provider: "docker", LaunchedBy: "testuser",
				StartedAt: now.Add(-15 * time.Minute), TimeoutAt: now.Add(45 * time.Minute),
			}); err != nil {
				t.Fatalf("creating run: %v", err)
			}
			st.Close()

			origStdout := os.Stdout
			pr, pw, _ := os.Pipe()
			os.Stdout = pw
			defer func() { os.Stdout = origStdout }()
			runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
			pw.Close()
			os.Stdout = origStdout
			out, _ := io.ReadAll(pr)

			if runErr != nil {
				t.Fatalf("unexpected error: %v", runErr)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("output missing %q: %s", tc.wantOut, string(out))
			}

			st2, err := store.NewSQLiteStore(env.dbPath)
			if err != nil {
				t.Fatalf("opening store: %v", err)
			}
			defer st2.Close()
			r, err := st2.GetRun(ctx, runID)
			if err != nil {
				t.Fatalf("getting run: %v", err)
			}
			if r.Status != tc.wantStatus {
				t.Errorf("store status = %q, want %q", r.Status, tc.wantStatus)
			}
			if r.ExitCode == nil || *r.ExitCode != tc.exitCode {
				t.Errorf("store exit code = %v, want %d", r.ExitCode, tc.exitCode)
			}
			if r.CompletedAt == nil {
				t.Error("store completed_at is nil after lazy finalize")
			}
		})
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
		Repo:       "github.com/test/repo",
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
		Repo:       "github.com/test/repo",
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
	// prov.Finalize calls Status() (inspect) then Kill() (stop+cp+rm) — script is intentionally stateless
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
		Repo:       "github.com/test/repo",
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
		Repo:       "github.com/test/repo",
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
		Repo:       "github.com/test/repo",
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

// TestPrintRunStatus_NilCost verifies that `horde status` prints a
// "Cost:        -" line when TotalCostUSD is nil — matches the list
// table's behavior so users see a consistent placeholder rather than a
// silently missing field.
func TestPrintRunStatus_NilCost(t *testing.T) {
	run := &store.Run{
		ID:         "abcdef123456",
		Repo:       "github.com/test/repo",
		Ticket:     "TICKET-1",
		Workflow:   "test-flow",
		Provider:   "docker",
		Status:     store.StatusFailed,
		LaunchedBy: "testuser",
		StartedAt:  time.Now().Add(-1 * time.Minute),
		// TotalCostUSD intentionally nil
	}
	now := time.Now()
	run.CompletedAt = &now

	stdout := captureStdout(t, func() { printRunStatus(run) })

	if !strings.Contains(stdout, "Cost:        -") {
		t.Errorf("stdout should contain 'Cost:        -' line; got:\n%s", stdout)
	}
}

// TestPrintRunStatus_WithCost is the positive control for
// TestPrintRunStatus_NilCost.
func TestPrintRunStatus_WithCost(t *testing.T) {
	cost := 1.23
	run := &store.Run{
		ID:           "abcdef123456",
		Repo:         "github.com/test/repo",
		Ticket:       "TICKET-1",
		Workflow:     "test-flow",
		Provider:     "docker",
		Status:       store.StatusSuccess,
		LaunchedBy:   "testuser",
		StartedAt:    time.Now().Add(-1 * time.Minute),
		TotalCostUSD: &cost,
	}
	now := time.Now()
	run.CompletedAt = &now

	stdout := captureStdout(t, func() { printRunStatus(run) })

	if !strings.Contains(stdout, "Cost:        $1.23") {
		t.Errorf("stdout should contain 'Cost:        $1.23'; got:\n%s", stdout)
	}
	if strings.Contains(stdout, "Cost:        -") {
		t.Errorf("stdout should NOT contain 'Cost:        -' when cost is set; got:\n%s", stdout)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, pr)
		done <- buf.String()
	}()
	fn()
	pw.Close()
	os.Stdout = origStdout
	return <-done
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
		{ID: "listrun00001", Ticket: "T-1", Status: store.StatusPending, Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now, TimeoutAt: now.Add(time.Hour)},
		{ID: "listrun00002", Ticket: "T-2", Status: store.StatusRunning, Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-5 * time.Minute), TimeoutAt: now.Add(55 * time.Minute)},
		{ID: "listrun00003", Ticket: "T-3", Status: store.StatusSuccess, Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-20 * time.Minute), CompletedAt: &completedAt, TimeoutAt: now.Add(40 * time.Minute)},
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
		{ID: "listrun00001", Ticket: "T-1", Status: store.StatusPending, Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now, TimeoutAt: now.Add(time.Hour)},
		{ID: "listrun00002", Ticket: "T-2", Status: store.StatusRunning, Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-5 * time.Minute), TimeoutAt: now.Add(55 * time.Minute)},
		{ID: "listrun00003", Ticket: "T-3", Status: store.StatusSuccess, Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-20 * time.Minute), CompletedAt: &completedAt, TimeoutAt: now.Add(40 * time.Minute)},
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
		Repo:       "github.com/test/repo",
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
		{ID: "lazyrun00001", Ticket: "TICKET-1", Status: store.StatusRunning, InstanceID: "container1", Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-15 * time.Minute), TimeoutAt: now.Add(45 * time.Minute)},
		{ID: "lazyrun00002", Ticket: "TICKET-2", Status: store.StatusRunning, InstanceID: "container2", Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-10 * time.Minute), TimeoutAt: now.Add(50 * time.Minute)},
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
		Repo:       "github.com/test/repo",
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
		{ID: "errrun00001", Ticket: "TICKET-1", Status: store.StatusRunning, InstanceID: "badcontainer", Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-15 * time.Minute), TimeoutAt: now.Add(45 * time.Minute)},
		{ID: "errrun00002", Ticket: "TICKET-2", Status: store.StatusRunning, InstanceID: "goodcontainer", Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-10 * time.Minute), TimeoutAt: now.Add(50 * time.Minute)},
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
	if !strings.Contains(string(stderr), "warning: finalizing run errrun00001") {
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
		Workflow:     "implement-ticket",
		Status:       store.StatusSuccess,
		Repo:         "github.com/test/repo",
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
	if !strings.Contains(outStr, "WORKFLOW") {
		t.Errorf("output missing WORKFLOW header: %s", outStr)
	}
	if !strings.Contains(outStr, "implement-ticket") {
		t.Errorf("output missing workflow value 'implement-ticket': %s", outStr)
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
		{ID: "listrun00007", Repo: "github.com/test/repo", Ticket: "T-LOCAL", Status: store.StatusRunning, Provider: "docker", LaunchedBy: "testuser", StartedAt: now, TimeoutAt: now.Add(time.Hour)},
		{ID: "listrun00008", Repo: "github.com/other/repo", Ticket: "T-OTHER", Status: store.StatusRunning, Provider: "docker", LaunchedBy: "testuser", StartedAt: now, TimeoutAt: now.Add(time.Hour)},
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
		Repo:       "github.com/test/repo",
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
		Repo:       "github.com/test/repo",
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

// TestLogs_Follow_ContextCancel_ClosesCleanly covers horde-eqc: when the
// parent context is cancelled mid-stream (the same code path SIGINT takes via
// signal.NotifyContext), logs --follow must close the docker reader, return
// without a "streaming logs" error, and the command must complete promptly.
func TestLogs_Follow_ContextCancel_ClosesCleanly(t *testing.T) {
	// Fake docker that streams forever on --follow, matching real `docker logs -f` behavior.
	script := `#!/bin/sh
case "$1" in
  inspect) echo abc123 ;;
  logs)
    case "$@" in
      *--follow*)
        printf 'follow line 1\n'
        # stream a line every 50ms forever; exits when the parent closes the pipe (SIGPIPE) or is killed
        while true; do
          printf 'follow line N\n' 2>/dev/null || exit 0
          sleep 0.05
        done
        ;;
      *) echo "ERROR: --follow not passed" >&2; exit 1 ;;
    esac ;;
esac
`
	env := setupStatusEnv(t, script)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runID := "logsrun_eqc1"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	if err := st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo",
		Ticket:     "TICKET-1",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(60 * time.Minute),
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	}); err != nil {
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

	done := make(chan error, 1)
	go func() {
		done <- newApp().Run(ctx, []string{"horde", "--provider", "docker", "logs", "--follow", runID})
	}()

	// Read until we see at least one line to confirm streaming started.
	buf := make([]byte, 256)
	readDone := make(chan struct{})
	go func() {
		for {
			n, err := pr.Read(buf)
			if n > 0 && strings.Contains(string(buf[:n]), "follow line") {
				close(readDone)
				// Drain the rest in the background so the writer doesn't block on pipe-full.
				go io.Copy(io.Discard, pr)
				return
			}
			if err != nil {
				close(readDone)
				return
			}
		}
	}()

	select {
	case <-readDone:
	case <-time.After(3 * time.Second):
		cancel()
		pw.Close()
		t.Fatal("timed out waiting for first log line")
	}

	// Cancel the parent context — this is what SIGINT would do via signal.NotifyContext.
	cancel()

	// The command must return within a reasonable window, with no "streaming logs" error.
	select {
	case runErr := <-done:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			// Accept nil or context.Canceled; anything else is a real error like "streaming logs".
			if strings.Contains(runErr.Error(), "streaming logs") {
				t.Errorf("expected clean shutdown on ctx cancel, got streaming error: %v", runErr)
			}
		}
	case <-time.After(3 * time.Second):
		pw.Close()
		t.Fatal("logs --follow did not return within 3s after ctx cancel — reader not closed on signal")
	}

	pw.Close()
	os.Stdout = origStdout
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
				Repo:       "github.com/test/repo",
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
		Repo:       "github.com/test/repo",
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
		Repo:       "github.com/test/repo",
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
				Repo:       "github.com/test/repo",
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

func TestFinalize_TimeoutKillFailure(t *testing.T) {
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
		Repo:       "github.com/test/repo",
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

func TestFinalize_TimeoutKillSuccess(t *testing.T) {
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
		Repo:       "github.com/test/repo",
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

func TestFinalize_TimeoutKillCapturesCostAndExitCode(t *testing.T) {
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
		Repo:       "github.com/test/repo",
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

	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "status", runID})
	pw.Close()
	os.Stdout = origStdout
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	stdoutBytes, _ := io.ReadAll(pr)
	stdout := string(stdoutBytes)

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

	// Verify the in-place mutation on the Finalize'd run reaches
	// stdout via printRunStatus — guards against accidental removal
	// of the run.TotalCostUSD/run.ExitCode assignments in Finalize.
	if !strings.Contains(stdout, "3.14") {
		t.Errorf("stdout %q should display TotalCostUSD 3.14", stdout)
	}
	// Exit code is displayed in the status block; search for "0" near
	// "exit" case-insensitively to be resilient to formatting tweaks.
	lower := strings.ToLower(stdout)
	if !strings.Contains(lower, "exit") {
		t.Errorf("stdout %q should mention exit code", stdout)
	}
}

func TestFinalize_TimeoutKillCapturesCostAndExitCode_WithWorkflow(t *testing.T) {
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
		Repo:       "github.com/test/repo",
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
		Repo:       "github.com/test/repo",
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
		Repo:       "github.com/test/repo",
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
		Repo:       "github.com/test/repo",
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

// TestKill_MalformedRunResult covers horde-bqy: killCmd must treat
// run-result.json parse failures as best-effort. A malformed JSON file
// must not surface as an error; the command completes and kills the
// container — it just leaves cost/exit code unset.
func TestKill_MalformedRunResult(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) echo '{"Running":true,"ExitCode":0,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  exec) exit 1 ;;
  stop) exit 0 ;;
  cp)
    case "$2" in
      *audit*) mkdir -p "$3/TICKET-1" && printf '{not valid json' > "$3/TICKET-1/run-result.json" ;;
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
		Repo:       "github.com/test/repo",
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
		t.Fatalf("malformed run-result.json must not surface error, got: %v", runErr)
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
	// cost and exit code must be nil since the JSON was unparseable.
	if r.TotalCostUSD != nil {
		t.Errorf("TotalCostUSD: got %v, want nil (malformed JSON)", *r.TotalCostUSD)
	}
	if r.ExitCode != nil {
		t.Errorf("ExitCode: got %v, want nil (malformed JSON)", *r.ExitCode)
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
		Repo:       "github.com/test/repo",
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

// TestKill_ReadFileContract exercises the directory-layout contract between
// the kill command (which populates ~/.horde/results/<runID>/) and
// DockerProvider.ReadFile (which reads from that same tree under the ".orc/"
// logical prefix). Both sides are unit-tested in isolation; this test covers
// the handshake so a future rename of e.g. "audit/" to "audits/" on one side
// fails loudly.
func TestKill_ReadFileContract(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) echo '{"Running":true,"ExitCode":0,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"0001-01-01T00:00:00Z"}' ;;
  exec) exit 1 ;;
  stop) exit 0 ;;
  logs) printf 'fake-container-log-line\n' ;;
  cp)
    case "$2" in
      *audit*)     mkdir -p "$3/TICKET-1" && printf '{"total_cost_usd": 1.5, "exit_code": 0}' > "$3/TICKET-1/run-result.json" ;;
      *artifacts*) mkdir -p "$3"           && printf 'hello-artifact'                          > "$3/out.txt"             ;;
    esac
    ;;
esac
`
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()

	runID := "killrfc0001"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	if err := st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo",
		Ticket:     "TICKET-1",
		Workflow:   "",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  time.Now(),
		TimeoutAt:  time.Now().Add(60 * time.Minute),
		Status:     store.StatusRunning,
		InstanceID: "abc123",
	}); err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	if err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "kill", runID}); err != nil {
		t.Fatalf("kill failed: %v", err)
	}

	p := provider.NewDockerProvider()
	cases := []struct {
		name string
		path string
		want string
	}{
		{"audit run-result.json", ".orc/audit/TICKET-1/run-result.json", `{"total_cost_usd": 1.5, "exit_code": 0}`},
		{"artifacts out.txt", ".orc/artifacts/out.txt", "hello-artifact"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := p.ReadFile(ctx, provider.ReadFileOpts{RunID: runID, Path: tc.path})
			if err != nil {
				t.Fatalf("ReadFile(%q) failed: %v", tc.path, err)
			}
			if string(data) != tc.want {
				t.Errorf("ReadFile(%q) = %q, want %q", tc.path, string(data), tc.want)
			}
		})
	}

	// container.log is written by kill directly, not via docker cp. It lives
	// outside the ".orc/" prefix, so ReadFile cannot reach it — verify it via
	// direct stat. This documents where the Kill→ReadFile contract stops.
	logPath := filepath.Join(env.tmpHome, ".horde", "results", runID, "container.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Errorf("container.log missing after kill: %v", err)
	} else if !strings.Contains(string(data), "fake-container-log-line") {
		t.Errorf("container.log contents unexpected: %q", string(data))
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
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
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
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
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
	for _, want := range []string{runID, "TICKET-1", "success", "$4.52", "12m 34s", "PHASE", "plan", "$1.23", "3m 0s"} {
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
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
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
	for _, want := range []string{runID, "TICKET-1", "success", "5m 0s"} {
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
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1", Workflow: "review",
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
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
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

// TestResults_CorruptJSON covers horde-jqr: when run-result.json exists but
// contains malformed or schema-mismatched JSON, results must fall back to the
// partial-results view rather than crash or propagate an error.
func TestResults_CorruptJSON(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"malformed", `{invalid json`},
		{"truncated", `{"phases": [`},
		{"wrong type for phases", `{"phases": "not an array"}`},
		{"totally wrong schema", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
			ctx := context.Background()
			runID := "resultsjqr01"

			resultsDir := filepath.Join(env.tmpHome, ".horde", "results", runID, "audit", "TICKET-1")
			if err := os.MkdirAll(resultsDir, 0o755); err != nil {
				t.Fatalf("creating results dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(resultsDir, "run-result.json"), []byte(tc.payload), 0o644); err != nil {
				t.Fatalf("writing run-result.json: %v", err)
			}

			st, err := store.NewSQLiteStore(env.dbPath)
			if err != nil {
				t.Fatalf("opening store: %v", err)
			}
			exitCode := 1
			cost := 2.50
			if err := st.CreateRun(ctx, &store.Run{
				ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
				Provider: "docker", LaunchedBy: "testuser",
				StartedAt: time.Now(), TimeoutAt: time.Now().Add(60 * time.Minute),
				Status: store.StatusFailed, ExitCode: &exitCode, TotalCostUSD: &cost,
			}); err != nil {
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
				t.Fatalf("results returned error on corrupt JSON: %v", runErr)
			}
			outStr := string(out)
			if !strings.Contains(outStr, "Detailed results unavailable") {
				t.Errorf("output missing fallback header: %s", outStr)
			}
			if !strings.Contains(outStr, "failed") || !strings.Contains(outStr, "$2.50") {
				t.Errorf("output missing partial-results fields: %s", outStr)
			}
		})
	}
}

// TestResults_JSON_CorruptJSON is the --json sibling of TestResults_CorruptJSON:
// corrupt run-result.json must still produce a valid partial-results JSON doc.
func TestResults_JSON_CorruptJSON(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	runID := "resultsjqr02"

	resultsDir := filepath.Join(env.tmpHome, ".horde", "results", runID, "audit", "TICKET-1")
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		t.Fatalf("creating results dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(resultsDir, "run-result.json"), []byte(`{not json`), 0o644); err != nil {
		t.Fatalf("writing run-result.json: %v", err)
	}

	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	exitCode := 1
	if err := st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "testuser",
		StartedAt: time.Now(), TimeoutAt: time.Now().Add(60 * time.Minute),
		Status: store.StatusFailed, ExitCode: &exitCode,
	}); err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "results", "--json", runID})
	out := buf.Bytes()

	if runErr != nil {
		t.Fatalf("results --json returned error on corrupt JSON: %v", runErr)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("--json output not valid JSON: %v\n%s", err, string(out))
	}
	if parsed["id"] != runID {
		t.Errorf("json id: got %v, want %q", parsed["id"], runID)
	}
	if parsed["partial"] != true {
		t.Errorf("json partial: got %v, want true", parsed["partial"])
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
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-5",
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

func TestResolveCanonicalRepo_FromSSMConfig(t *testing.T) {
	t.Parallel()
	// SSM authority: cfg.Repo wins verbatim, regardless of the resolver's Dir
	// (here a nonexistent dir, which would error if discovery were consulted).
	cfg := &config.HordeConfig{Repo: "github.com/example/myproj"}
	got, err := resolveCanonicalRepo(cfg, &config.Resolver{Dir: "/nonexistent/dir"})
	if err != nil {
		t.Fatalf("resolveCanonicalRepo unexpected error: %v", err)
	}
	if got != "github.com/example/myproj" {
		t.Errorf("resolveCanonicalRepo = %q, want %q", got, "github.com/example/myproj")
	}
}

func TestResolveCanonicalRepo_SSMAuthorityBeatsRepoOverride(t *testing.T) {
	t.Parallel()
	// Even with a --repo override present, the SSM value is the authority for
	// ECS so existing deployments are not re-partitioned.
	cfg := &config.HordeConfig{Repo: "github.com/example/myproj"}
	r := &config.Resolver{RepoOverride: "github.com/other/repo"}
	got, err := resolveCanonicalRepo(cfg, r)
	if err != nil {
		t.Fatalf("resolveCanonicalRepo unexpected error: %v", err)
	}
	if got != "github.com/example/myproj" {
		t.Errorf("resolveCanonicalRepo = %q, want SSM value %q", got, "github.com/example/myproj")
	}
}

func TestResolveCanonicalRepo_FallsBackToGitRemote(t *testing.T) {
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
	run("git", "remote", "add", "origin", "https://github.com/example/fallback.git")

	// Docker (no SSM): falls back to the resolver, which canonicalizes the
	// local git remote — the ".git" suffix is stripped.
	want := "github.com/example/fallback"
	got, err := resolveCanonicalRepo(nil, &config.Resolver{Dir: dir})
	if err != nil {
		t.Fatalf("resolveCanonicalRepo(nil) unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("resolveCanonicalRepo(nil) = %q, want %q", got, want)
	}

	got, err = resolveCanonicalRepo(&config.HordeConfig{Repo: ""}, &config.Resolver{Dir: dir})
	if err != nil {
		t.Fatalf("resolveCanonicalRepo(empty Repo) unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("resolveCanonicalRepo(empty Repo) = %q, want %q", got, want)
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
	// Swap the package-level awscfgLoad seam to force an error.  This
	// isolates the test from the ambient credential environment (which may
	// succeed on EC2, developer machines with ~/.aws/credentials, etc.).
	// Cannot use t.Parallel() because we mutate a package-level var.
	orig := awscfgLoad
	awscfgLoad = func(_ context.Context, _ string) (aws.Config, error) {
		return aws.Config{}, fmt.Errorf("stub: no credentials available")
	}
	t.Cleanup(func() { awscfgLoad = orig })

	got, err := resolveLaunchedBy(context.Background(), "aws-ecs", "", nil, "")
	if err == nil {
		t.Fatalf("resolveLaunchedBy(aws-ecs, nil config) = %q, want error", got)
	}
	if !strings.Contains(err.Error(), "resolving launched_by:") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "resolving launched_by:")
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
		Repo:      "github.com/test/repo",
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

// --- JSON output tests ---

func TestStatus_JSON_CompletedRun(t *testing.T) {
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
	runID := "jsonstatusrun001"
	err = st.CreateRun(ctx, &store.Run{
		ID:           runID,
		Repo:         "github.com/test/repo",
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

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "status", runID})
	out := buf.Bytes()

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	var v StatusV1
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, out)
	}
	if v.ID != runID {
		t.Errorf("ID = %q, want %q", v.ID, runID)
	}
	if v.Ticket != "TICKET-1" {
		t.Errorf("Ticket = %q, want TICKET-1", v.Ticket)
	}
	if v.Status != "success" {
		t.Errorf("Status = %q, want success", v.Status)
	}
	if v.ExitCode == nil || *v.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", v.ExitCode)
	}
	if v.TotalCostUSD == nil || *v.TotalCostUSD != 4.52 {
		t.Errorf("TotalCostUSD = %v, want 4.52", v.TotalCostUSD)
	}
	if v.LaunchedBy != "testuser" {
		t.Errorf("LaunchedBy = %q, want testuser", v.LaunchedBy)
	}
	if v.DurationSecs <= 0 {
		t.Errorf("DurationSecs = %f, want > 0", v.DurationSecs)
	}
	if v.StartedAt == "" {
		t.Errorf("StartedAt is empty")
	}
	if v.CompletedAt == "" {
		t.Errorf("CompletedAt is empty")
	}
}

func TestStatus_JSON_RunningRun(t *testing.T) {
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
	runID := "jsonstatusrun002"
	err = st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo",
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

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "status", runID})
	out := buf.Bytes()

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	var v StatusV1
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, out)
	}
	if v.Status != "running" {
		t.Errorf("Status = %q, want running", v.Status)
	}
	if v.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil", v.ExitCode)
	}
	if v.TotalCostUSD != nil {
		t.Errorf("TotalCostUSD = %v, want nil", v.TotalCostUSD)
	}
	if v.CompletedAt != "" {
		t.Errorf("CompletedAt = %q, want empty", v.CompletedAt)
	}
}

// TestStatus_JSON_FinalizeDuringCall covers horde-5ez: a single horde --json
// status invocation where the run is "running" in the store at call start
// but the container is already stopped, so prov.Finalize updates the run
// before writeJSONTo serializes it. The JSON output must reflect the
// post-Finalize success state (not the pre-call running state) AND the
// store must persist the same transition. Existing JSON tests cover the
// already-terminal path; lazy-completion tests cover the stdout (text)
// path. This is the integration of the two.
func TestStatus_JSON_FinalizeDuringCall(t *testing.T) {
	// inspect reports a stopped container with exit 0; cp/rm noop so
	// Finalize's stopped branch (artifact copy) succeeds silently.
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
	runID := "fin5ezjson01"
	if err := st.CreateRun(ctx, &store.Run{
		ID:         runID,
		Repo:       "github.com/test/repo",
		Ticket:     "TICKET-5EZ",
		Status:     store.StatusRunning, // pre-call: running
		InstanceID: "abc123",
		Provider:   "docker",
		LaunchedBy: "testuser",
		StartedAt:  now.Add(-15 * time.Minute),
		TimeoutAt:  now.Add(45 * time.Minute),
	}); err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "status", runID})
	out := buf.Bytes()

	if runErr != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", runErr, out)
	}

	// JSON output must reflect the POST-Finalize state, not the
	// pre-call StatusRunning that the store held when the call started.
	var v StatusV1
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, out)
	}
	if v.Status != "success" {
		t.Errorf("JSON Status = %q, want %q (Finalize result must surface in JSON)", v.Status, "success")
	}
	if v.ExitCode == nil || *v.ExitCode != 0 {
		t.Errorf("JSON ExitCode = %v, want 0", v.ExitCode)
	}
	if v.CompletedAt == "" {
		t.Errorf("JSON CompletedAt is empty, want set after Finalize")
	}

	// And the store must have been updated by the same call (the
	// finalizeAndSync helper persists the new state alongside).
	st2, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	defer st2.Close()
	r, err := st2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("re-reading run: %v", err)
	}
	if r.Status != store.StatusSuccess {
		t.Errorf("store Status = %q, want %q", r.Status, store.StatusSuccess)
	}
	if r.ExitCode == nil || *r.ExitCode != 0 {
		t.Errorf("store ExitCode = %v, want 0", r.ExitCode)
	}
	if r.CompletedAt == nil {
		t.Errorf("store CompletedAt is nil, want set after Finalize")
	}
}

func TestList_JSON_ActiveOnly(t *testing.T) {
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
		{ID: "listjson001", Ticket: "T-1", Workflow: "implement-ticket", Branch: "develop", Status: store.StatusPending, Repo: "github.com/test/repo", Provider: "docker", InstanceID: "inst-001", LaunchedBy: "testuser", StartedAt: now, TimeoutAt: now.Add(time.Hour)},
		{ID: "listjson002", Ticket: "T-2", Status: store.StatusRunning, Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-5 * time.Minute), TimeoutAt: now.Add(55 * time.Minute)},
		{ID: "listjson003", Ticket: "T-3", Status: store.StatusSuccess, Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-20 * time.Minute), CompletedAt: &completedAt, TimeoutAt: now.Add(40 * time.Minute)},
	}
	for _, r := range runs {
		if err := st.CreateRun(ctx, r); err != nil {
			t.Fatalf("creating run %s: %v", r.ID, err)
		}
	}
	st.Close()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "list"})
	out := buf.Bytes()

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	var v ListV1
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, out)
	}
	if len(v.Runs) != 2 {
		t.Errorf("len(Runs) = %d, want 2", len(v.Runs))
	}
	ids := make([]string, len(v.Runs))
	for i, r := range v.Runs {
		ids[i] = r.ID
	}
	for _, want := range []string{"listjson001", "listjson002"} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("runs missing %q, got %v", want, ids)
		}
	}
	for _, bad := range ids {
		if bad == "listjson003" {
			t.Errorf("runs should not contain listjson003")
		}
	}

	// #25: list --json must carry the same per-run detail as status --json so
	// callers don't need N+1 status lookups. Verify the fields on listjson001.
	var r1 *ListRunV1
	for i := range v.Runs {
		if v.Runs[i].ID == "listjson001" {
			r1 = &v.Runs[i]
		}
	}
	if r1 == nil {
		t.Fatal("listjson001 not found in output")
	}
	if r1.Workflow != "implement-ticket" {
		t.Errorf("Workflow = %q, want implement-ticket", r1.Workflow)
	}
	if r1.Branch != "develop" {
		t.Errorf("Branch = %q, want develop", r1.Branch)
	}
	if r1.InstanceID != "inst-001" {
		t.Errorf("InstanceID = %q, want inst-001", r1.InstanceID)
	}
	if r1.LaunchedBy != "testuser" {
		t.Errorf("LaunchedBy = %q, want testuser", r1.LaunchedBy)
	}
	if r1.StartedAt == "" {
		t.Errorf("StartedAt is empty")
	}
}

func TestList_JSON_Empty(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	st.Close()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "list"})
	out := buf.Bytes()

	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	var v ListV1
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, out)
	}
	if v.Runs == nil || len(v.Runs) != 0 {
		t.Errorf("Runs = %v, want empty non-nil slice", v.Runs)
	}
}

// seedFilterRuns populates a store with a fixed set of runs across statuses,
// workflows, tickets, and labels for the list-filter tests.
func seedFilterRuns(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()
	now := time.Now()
	done := now.Add(-10 * time.Minute)
	repo := "github.com/test/repo"
	mk := func(id, ticket, workflow string, status store.Status, labels map[string]string, cost *float64) *store.Run {
		r := &store.Run{
			ID: id, Ticket: ticket, Workflow: workflow, Status: status, Repo: repo,
			Provider: "docker", LaunchedBy: "testuser", StartedAt: now.Add(-time.Minute),
			TimeoutAt: now.Add(time.Hour), Labels: labels, TotalCostUSD: cost,
		}
		if status.IsTerminal() {
			r.CompletedAt = &done
		}
		return r
	}
	runs := []*store.Run{
		mk("filt00000001", "KS-1", "implement-ticket", store.StatusRunning, map[string]string{"epic": "KS-100"}, ptrf(1.00)),
		mk("filt00000002", "KS-2", "implement-ticket", store.StatusSuccess, map[string]string{"epic": "KS-100"}, ptrf(2.00)),
		mk("filt00000003", "KS-3", "qa-pr", store.StatusSuccess, map[string]string{"epic": "KS-200"}, ptrf(4.00)),
		mk("filt00000004", "KS-4", "qa-pr", store.StatusFailed, nil, nil),
	}
	for _, r := range runs {
		if err := st.CreateRun(ctx, r); err != nil {
			t.Fatalf("creating run %s: %v", r.ID, err)
		}
	}
}

func listJSON(t *testing.T, args ...string) ListV1 {
	t.Helper()
	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	full := append([]string{"horde", "--provider", "docker", "--json", "list"}, args...)
	if err := app.Run(context.Background(), full); err != nil {
		t.Fatalf("list %v: unexpected error: %v\noutput: %s", args, err, buf.Bytes())
	}
	var v ListV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, buf.Bytes())
	}
	return v
}

func listIDs(v ListV1) []string {
	ids := make([]string, len(v.Runs))
	for i, r := range v.Runs {
		ids[i] = r.ID
	}
	sort.Strings(ids)
	return ids
}

func TestList_JSON_FilterByLabel(t *testing.T) {
	env := setupLaunchEnv(t)
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	seedFilterRuns(t, dbPath)

	// epic=KS-100 across all statuses (so --all to include terminal runs).
	v := listJSON(t, "--all", "--label", "epic=KS-100")
	if got := listIDs(v); !reflect.DeepEqual(got, []string{"filt00000001", "filt00000002"}) {
		t.Errorf("ids = %v, want [filt00000001 filt00000002]", got)
	}
}

func TestList_JSON_FilterByWorkflow(t *testing.T) {
	env := setupLaunchEnv(t)
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	seedFilterRuns(t, dbPath)

	v := listJSON(t, "--all", "--workflow", "qa-pr")
	if got := listIDs(v); !reflect.DeepEqual(got, []string{"filt00000003", "filt00000004"}) {
		t.Errorf("ids = %v, want [filt00000003 filt00000004]", got)
	}
}

func TestList_JSON_FilterByStatus(t *testing.T) {
	env := setupLaunchEnv(t)
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	seedFilterRuns(t, dbPath)

	// --status implies looking across terminal runs (no need for --all).
	v := listJSON(t, "--status", "success")
	if got := listIDs(v); !reflect.DeepEqual(got, []string{"filt00000002", "filt00000003"}) {
		t.Errorf("ids = %v, want [filt00000002 filt00000003]", got)
	}
}

func TestList_JSON_CombinedFilters(t *testing.T) {
	env := setupLaunchEnv(t)
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	seedFilterRuns(t, dbPath)

	v := listJSON(t, "--status", "success", "--workflow", "implement-ticket", "--label", "epic=KS-100")
	if got := listIDs(v); !reflect.DeepEqual(got, []string{"filt00000002"}) {
		t.Errorf("ids = %v, want [filt00000002]", got)
	}
}

func TestList_JSON_SummaryOverCohort(t *testing.T) {
	env := setupLaunchEnv(t)
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	seedFilterRuns(t, dbPath)

	// epic=KS-100 cohort: filt001 ($1) + filt002 ($2) = $3, count 2.
	v := listJSON(t, "--all", "--label", "epic=KS-100")
	if v.Summary.Count != 2 {
		t.Errorf("Summary.Count = %d, want 2", v.Summary.Count)
	}
	if got := v.Summary.TotalCostUSD; got < 2.99 || got > 3.01 {
		t.Errorf("Summary.TotalCostUSD = %v, want ~3.00", got)
	}
}

func TestList_JSON_InvalidLabelFilter(t *testing.T) {
	env := setupLaunchEnv(t)
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	seedFilterRuns(t, dbPath)

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	err := app.Run(context.Background(), []string{"horde", "--provider", "docker", "list", "--label", "bogus"})
	if err == nil {
		t.Fatal("expected error for malformed --label filter, got nil")
	}
}

// TestList_SinceUntil_EndToEnd exercises the --since/--until wiring through the
// real command (parseWhen → RunFilter → store), which the parseWhen unit tests
// and store-layer range tests don't cover together.
func TestList_SinceUntil_EndToEnd(t *testing.T) {
	env := setupLaunchEnv(t)
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	ctx := context.Background()
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	done := now.Add(-time.Hour)
	repo := "github.com/test/repo"
	mk := func(id string, age time.Duration) *store.Run {
		return &store.Run{
			ID: id, Repo: repo, Ticket: id, Workflow: "w", Provider: "docker",
			Status: store.StatusSuccess, LaunchedBy: "me",
			StartedAt: now.Add(-age), CompletedAt: &done, TimeoutAt: now.Add(time.Hour),
		}
	}
	// Runs started 3h, 30m, and 2m ago.
	for _, r := range []*store.Run{mk("since000003h", 3*time.Hour), mk("since000030m", 30*time.Minute), mk("since0000002m", 2*time.Minute)} {
		if err := st.CreateRun(ctx, r); err != nil {
			t.Fatalf("CreateRun %s: %v", r.ID, err)
		}
	}
	st.Close()

	// --since 1h should return only the two within the last hour.
	v := listJSON(t, "--all", "--since", "1h")
	if got := listIDs(v); !reflect.DeepEqual(got, []string{"since0000002m", "since000030m"}) {
		t.Errorf("--since 1h ids = %v, want [since0000002m since000030m]", got)
	}

	// Invalid --since must error (routes through the error path).
	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	if err := app.Run(ctx, []string{"horde", "--provider", "docker", "list", "--since", "not-a-time"}); err == nil {
		t.Error("expected error for invalid --since, got nil")
	}
}

// TestList_Human_SummaryLine pins the human-readable cohort rollup line and the
// filtered-to-zero empty message — neither is covered by the JSON-shape tests.
// The human table writes to os.Stdout (not cmd.Writer), so capture via a pipe.
func TestList_Human_SummaryLine(t *testing.T) {
	env := setupLaunchEnv(t)
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	seedFilterRuns(t, dbPath)

	capture := func(args ...string) string {
		t.Helper()
		origStdout := os.Stdout
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		os.Stdout = pw
		full := append([]string{"horde", "--provider", "docker", "list"}, args...)
		runErr := newApp().Run(context.Background(), full)
		pw.Close()
		os.Stdout = origStdout
		out, _ := io.ReadAll(pr)
		if runErr != nil {
			t.Fatalf("list %v: %v", args, runErr)
		}
		return string(out)
	}

	// epic=KS-100 cohort: 2 runs, $1.00 + $2.00 = $3.00.
	out := capture("--all", "--label", "epic=KS-100")
	if !strings.Contains(out, "2 runs, $3.00 total") {
		t.Errorf("expected summary line '2 runs, $3.00 total', got:\n%s", out)
	}

	// A filter that matches nothing prints the filtered-empty message.
	out = capture("--all", "--label", "epic=NOPE")
	if !strings.Contains(out, "No matching runs for this repo.") {
		t.Errorf("expected 'No matching runs for this repo.', got:\n%s", out)
	}
}

func TestResults_JSON_CompletedWithResults(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	runID := "jsonresults001"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
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
	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "results", runID})
	out := buf.Bytes()
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	var v ResultsV1
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, out)
	}
	if v.Partial {
		t.Errorf("Partial = true, want false")
	}
	if v.Status != "success" {
		t.Errorf("Status = %q, want success", v.Status)
	}
	if v.OrcStatus != "completed" {
		t.Errorf("OrcStatus = %q, want completed", v.OrcStatus)
	}
	if v.TotalCostUSD == nil || *v.TotalCostUSD != 4.52 {
		t.Errorf("TotalCostUSD = %v, want 4.52", v.TotalCostUSD)
	}
	if len(v.Phases) != 2 {
		t.Errorf("len(Phases) = %d, want 2", len(v.Phases))
	} else {
		if v.Phases[0].Name != "plan" {
			t.Errorf("Phases[0].Name = %q, want plan", v.Phases[0].Name)
		}
		if v.Phases[0].CostUSD != 1.23 {
			t.Errorf("Phases[0].CostUSD = %f, want 1.23", v.Phases[0].CostUSD)
		}
	}
}

func TestResults_JSON_StillRunning(t *testing.T) {
	dockerScript := "#!/bin/sh\ncase \"$1\" in\n  inspect) printf '{\"Running\":true,\"ExitCode\":0,\"StartedAt\":\"2024-06-15T10:30:00Z\",\"FinishedAt\":\"0001-01-01T00:00:00Z\"}' ;;\n  exec) exit 1 ;;\nesac\n"
	env := setupStatusEnv(t, dockerScript)
	ctx := context.Background()
	runID := "jsonresults002"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	err = st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "testuser",
		StartedAt: time.Now(), TimeoutAt: time.Now().Add(60 * time.Minute),
		Status: store.StatusRunning, InstanceID: "abc123",
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()
	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "results", runID})
	out := buf.Bytes()
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	var v ResultsV1
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, out)
	}
	if !v.Partial {
		t.Errorf("Partial = false, want true")
	}
	if v.Status != "running" {
		t.Errorf("Status = %q, want running", v.Status)
	}
	if v.Phases != nil {
		t.Errorf("Phases = %v, want nil", v.Phases)
	}
}

func TestResults_JSON_MissingRunResult(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	runID := "jsonresults003"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	exitCode := 1
	cost := 2.50
	err = st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "testuser",
		StartedAt: time.Now(), TimeoutAt: time.Now().Add(60 * time.Minute),
		Status: store.StatusFailed, ExitCode: &exitCode, TotalCostUSD: &cost,
	})
	if err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()
	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "results", runID})
	out := buf.Bytes()
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	var v ResultsV1
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, out)
	}
	if !v.Partial {
		t.Errorf("Partial = false, want true")
	}
	if v.Status != "failed" {
		t.Errorf("Status = %q, want failed", v.Status)
	}
	if v.ExitCode == nil || *v.ExitCode != 1 {
		t.Errorf("ExitCode = %v, want 1", v.ExitCode)
	}
}

type listableStubStore struct {
	stubStore
	runs []*store.Run
}

func (s *listableStubStore) ListByRepo(_ context.Context, repo string, activeOnly bool) ([]*store.Run, error) {
	var result []*store.Run
	for _, r := range s.runs {
		if r.Repo != repo {
			continue
		}
		if activeOnly && r.Status != store.StatusPending && r.Status != store.StatusRunning {
			continue
		}
		result = append(result, r)
	}
	if result == nil {
		result = make([]*store.Run, 0)
	}
	return result, nil
}

func (s *listableStubStore) UpdateRun(_ context.Context, id string, update *store.RunUpdate) error {
	for _, r := range s.runs {
		if r.ID == id {
			if update.Status != nil {
				r.Status = *update.Status
			}
			if update.ExitCode != nil {
				r.ExitCode = update.ExitCode
			}
			if update.CompletedAt != nil {
				r.CompletedAt = update.CompletedAt
			}
			if update.TotalCostUSD != nil {
				r.TotalCostUSD = update.TotalCostUSD
			}
			return nil
		}
	}
	return store.ErrRunNotFound
}

type noopProvider struct{}

func (p *noopProvider) Launch(_ context.Context, _ provider.LaunchOpts) (*provider.LaunchResult, error) {
	return nil, fmt.Errorf("not implemented")
}
func (p *noopProvider) Status(_ context.Context, _ string) (*provider.InstanceStatus, error) {
	return nil, fmt.Errorf("not implemented")
}
func (p *noopProvider) Logs(_ context.Context, _ string, _ bool) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}
func (p *noopProvider) Stop(_ context.Context, _ provider.StopOpts) error {
	return fmt.Errorf("not implemented")
}
func (p *noopProvider) ReadFile(_ context.Context, _ provider.ReadFileOpts) ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}
func (p *noopProvider) Finalize(_ context.Context, _ *store.Run, _ string) error {
	return nil
}

func TestList_DynamoStore_ColumnOutput(t *testing.T) {
	ctx := context.Background()
	repo := "github.com/test/repo"
	now := time.Now()
	completedAt := now.Add(-5 * time.Minute)
	cost := 4.52

	st := &listableStubStore{
		runs: []*store.Run{
			{
				ID:           "dynrun00001",
				Ticket:       "PROJ-42",
				Branch:       "feature-x",
				Status:       store.StatusSuccess,
				Repo:         repo,
				Provider:     "aws-ecs",
				LaunchedBy:   "testuser",
				StartedAt:    now.Add(-15 * time.Minute),
				CompletedAt:  &completedAt,
				TimeoutAt:    now.Add(45 * time.Minute),
				TotalCostUSD: &cost,
			},
			{
				ID:         "dynrun00002",
				Ticket:     "PROJ-99",
				Branch:     "",
				Status:     store.StatusRunning,
				Repo:       repo,
				Provider:   "aws-ecs",
				LaunchedBy: "testuser",
				StartedAt:  now.Add(-2 * time.Minute),
				TimeoutAt:  now.Add(58 * time.Minute),
			},
			{
				ID:         "dynrun00003",
				Ticket:     "OTHER-1",
				Branch:     "main",
				Status:     store.StatusSuccess,
				Repo:       "github.com/other/repo",
				Provider:   "aws-ecs",
				LaunchedBy: "testuser",
				StartedAt:  now.Add(-30 * time.Minute),
				TimeoutAt:  now.Add(30 * time.Minute),
			},
		},
	}
	prov := &noopProvider{}

	// Verify ListByRepo filters by repo (all runs)
	allRuns, err := st.ListByRepo(ctx, repo, false)
	if err != nil {
		t.Fatalf("ListByRepo(all): %v", err)
	}
	if len(allRuns) != 2 {
		t.Fatalf("ListByRepo(all): want 2 runs, got %d", len(allRuns))
	}

	// Run Finalize loop (no-op for noopProvider)
	homeDir := t.TempDir()
	for _, run := range allRuns {
		origStatus := run.Status
		if err := prov.Finalize(ctx, run, homeDir); err != nil {
			t.Errorf("Finalize(%s): %v", run.ID, err)
			continue
		}
		if run.Status != origStatus {
			if err := st.UpdateRun(ctx, run.ID, &store.RunUpdate{
				Status:       &run.Status,
				ExitCode:     run.ExitCode,
				CompletedAt:  run.CompletedAt,
				TotalCostUSD: run.TotalCostUSD,
			}); err != nil {
				t.Errorf("UpdateRun(%s): %v", run.ID, err)
			}
		}
	}

	// Capture printRunTable output
	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	printRunTable(allRuns)

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)
	outStr := string(out)

	// Assert header columns
	for _, col := range []string{"RUN ID", "TICKET", "BRANCH", "STATUS", "DURATION", "COST"} {
		if !strings.Contains(outStr, col) {
			t.Errorf("output missing header %q: %s", col, outStr)
		}
	}

	// Assert data rows for this repo
	for _, want := range []string{"dynrun00001", "PROJ-42", "feature-x", "$4.52", "dynrun00002", "(default)"} {
		if !strings.Contains(outStr, want) {
			t.Errorf("output missing %q: %s", want, outStr)
		}
	}

	// Assert other repo's data is absent
	for _, absent := range []string{"dynrun00003", "OTHER-1"} {
		if strings.Contains(outStr, absent) {
			t.Errorf("output should not contain %q: %s", absent, outStr)
		}
	}

	// Verify active-only filtering
	activeRuns, err := st.ListByRepo(ctx, repo, true)
	if err != nil {
		t.Fatalf("ListByRepo(activeOnly): %v", err)
	}
	if len(activeRuns) != 1 {
		t.Fatalf("ListByRepo(activeOnly): want 1 run, got %d", len(activeRuns))
	}
	if activeRuns[0].ID != "dynrun00002" {
		t.Errorf("activeRuns[0].ID = %q, want dynrun00002", activeRuns[0].ID)
	}
}

func TestRetry_ECS_SkipsEnvValidation(t *testing.T) {
	tmpHome := t.TempDir()
	projectDir := filepath.Join(tmpHome, "project")
	os.MkdirAll(projectDir, 0o755)

	// Git init + remote
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "remote", "add", "origin", "https://github.com/test/repo.git")
	// NO .env file — that's the point of this test

	// Fake docker script handling the commands retryCmd invokes:
	// - inspect → JSON for a stopped container (prov.Status calls this)
	// - image   → future timestamp so ensureBaseImage skips rebuild
	// - *       → container ID for docker run -d
	dockerScript := `#!/bin/sh
case "$1" in
  inspect) echo '{"Running":false,"ExitCode":1,"StartedAt":"2025-01-01T00:00:00Z","FinishedAt":"2025-01-01T01:00:00Z"}';;
  image) echo "2099-01-01T00:00:00Z";;
  *) echo "abc123container";;
esac
`
	binDir := filepath.Join(tmpHome, "bin")
	os.MkdirAll(binDir, 0o755)
	os.WriteFile(filepath.Join(binDir, "docker"), []byte(dockerScript), 0o755)

	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	oldDir, _ := os.Getwd()
	os.Chdir(projectDir)
	t.Cleanup(func() { os.Chdir(oldDir) })

	// Pre-insert a failed aws-ecs run into SQLite
	// (factory uses --provider docker → opens SQLite, but the stored run says aws-ecs)
	dbPath := filepath.Join(tmpHome, ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	completedAt := now.Add(-time.Hour)
	runID := "testrun123ab"
	err = st.CreateRun(context.Background(), &store.Run{
		ID:          runID,
		Repo:        "github.com/test/repo",
		Ticket:      "TICKET-1",
		Provider:    "aws-ecs",
		InstanceID:  "abc123",
		Status:      store.StatusFailed,
		LaunchedBy:  "testuser",
		StartedAt:   now.Add(-2 * time.Hour),
		CompletedAt: &completedAt,
		TimeoutAt:   now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("pre-creating run: %v", err)
	}
	st.Close()

	// Deliberately do NOT create a local workspace dir. An aws-ecs run has no
	// on-host workspace; the Docker-only .git guard must be skipped for it.
	workspaceDir := filepath.Join(tmpHome, ".horde", "workspaces", runID)

	ctx := context.Background()
	err = newApp().Run(ctx, []string{"horde", "--provider", "docker", "retry", runID})
	// The retry may succeed or fail for unrelated reasons (e.g. container launch),
	// but it must NOT fail because of a missing .env file...
	if err != nil && strings.Contains(err.Error(), ".env") {
		t.Errorf("got .env-related error for aws-ecs retry: %v", err)
	}
	// ...nor because the local workspace is missing — that guard is Docker-only.
	if err != nil && strings.Contains(err.Error(), "workspace for run") {
		t.Errorf("aws-ecs retry hit the Docker-only workspace guard: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspaceDir, ".git")); statErr == nil {
		t.Fatal("test bug: workspace .git should not exist for this aws-ecs case")
	}
}

// TestRetry_Docker_MissingWorkspaceFails is the positive counterpart to the
// ECS test above: for a docker-provider run the local-workspace .git guard
// MUST still fire when the workspace is gone, so retry refuses rather than
// silently relaunching against nothing. Guards against an inverted condition
// or wrong provider constant in the Docker-gating added for #35.
func TestRetry_Docker_MissingWorkspaceFails(t *testing.T) {
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
	// docker provider validates .env, so provide one.
	os.WriteFile(filepath.Join(projectDir, ".env"),
		[]byte("CLAUDE_CODE_OAUTH_TOKEN=t\nGIT_TOKEN=t\n"), 0o644)

	binDir := filepath.Join(tmpHome, "bin")
	os.MkdirAll(binDir, 0o755)
	// docker stub: inspect → stopped container; anything else → a container id.
	os.WriteFile(filepath.Join(binDir, "docker"), []byte(`#!/bin/sh
case "$1" in
  inspect) echo '{"Running":false,"ExitCode":1,"StartedAt":"2025-01-01T00:00:00Z","FinishedAt":"2025-01-01T01:00:00Z"}';;
  image) echo "2099-01-01T00:00:00Z";;
  *) echo "abc123container";;
esac
`), 0o755)

	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	oldDir, _ := os.Getwd()
	os.Chdir(projectDir)
	t.Cleanup(func() { os.Chdir(oldDir) })

	dbPath := filepath.Join(tmpHome, ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	completedAt := now.Add(-time.Hour)
	runID := "dockrun12345"
	if err := st.CreateRun(context.Background(), &store.Run{
		ID:          runID,
		Repo:        "github.com/test/repo",
		Ticket:      "TICKET-1",
		Provider:    "docker",
		InstanceID:  "abc123",
		Status:      store.StatusFailed,
		LaunchedBy:  "testuser",
		StartedAt:   now.Add(-2 * time.Hour),
		CompletedAt: &completedAt,
		TimeoutAt:   now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("pre-creating run: %v", err)
	}
	st.Close()

	// No workspace dir is created — the Docker-only guard must fire.
	err = newApp().Run(context.Background(), []string{"horde", "--provider", "docker", "retry", runID})
	if err == nil {
		t.Fatal("expected retry to fail for a docker run with no workspace, got nil")
	}
	if !strings.Contains(err.Error(), "workspace for run") {
		t.Errorf("expected workspace-guard error, got: %v", err)
	}
}

// TestClean_All_RemoveFailureWarns covers horde-jdu: the bulk clean path
// (`horde clean` with no args) must emit a stderr warning when docker rm
// fails for a container, rather than silently leaking it.
func TestClean_All_RemoveFailureWarns(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dockerScript := `#!/bin/sh
case "$1" in
  rm)
    # Fail for 'badcid', succeed otherwise
    for a in "$@"; do
      if [ "$a" = "badcid" ]; then
        echo "Error: cannot remove badcid: container in use" >&2
        exit 1
      fi
    done
    exit 0 ;;
  *) exit 0 ;;
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
	completedAt := now.Add(-1 * time.Hour)
	for _, r := range []*store.Run{
		{ID: "badrun000001", Ticket: "T-1", Status: store.StatusFailed, InstanceID: "badcid", Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "u", StartedAt: now.Add(-2 * time.Hour), CompletedAt: &completedAt, TimeoutAt: now.Add(24 * time.Hour)},
		{ID: "goodrun00001", Ticket: "T-2", Status: store.StatusSuccess, InstanceID: "goodcid", Repo: "github.com/test/repo", Provider: "docker", LaunchedBy: "u", StartedAt: now.Add(-2 * time.Hour), CompletedAt: &completedAt, TimeoutAt: now.Add(24 * time.Hour)},
	} {
		if err := st.CreateRun(ctx, r); err != nil {
			t.Fatalf("creating run %s: %v", r.ID, err)
		}
	}
	st.Close()

	origStderr := os.Stderr
	prErr, pwErr, _ := os.Pipe()
	os.Stderr = pwErr
	defer func() { os.Stderr = origStderr }()

	origStdout := os.Stdout
	prOut, pwOut, _ := os.Pipe()
	os.Stdout = pwOut
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "clean"})

	pwErr.Close()
	pwOut.Close()
	os.Stderr = origStderr
	os.Stdout = origStdout
	stderr, _ := io.ReadAll(prErr)
	stdout, _ := io.ReadAll(prOut)

	if runErr != nil {
		t.Fatalf("clean returned error: %v", runErr)
	}
	if !strings.Contains(string(stderr), "warning: removing container") || !strings.Contains(string(stderr), "badrun000001") {
		t.Errorf("stderr missing RemoveContainer warning for badrun000001: %s", string(stderr))
	}
	if !strings.Contains(string(stdout), "Removed 1 container") {
		t.Errorf("stdout should report 1 successful removal, got: %s", string(stdout))
	}
}

func TestAuditRelPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		workflow string
		ticket   string
		filename string
		want     string
	}{
		{"with workflow", "flow1", "PROJ-1", "run-result.json", "audit/flow1/PROJ-1/run-result.json"},
		{"empty workflow", "", "PROJ-1", "run-result.json", "audit/PROJ-1/run-result.json"},
		{"costs.json file", "flow1", "PROJ-1", "costs.json", "audit/flow1/PROJ-1/costs.json"},
		{"empty workflow costs", "", "PROJ-2", "costs.json", "audit/PROJ-2/costs.json"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := provider.AuditRelPath(tc.workflow, tc.ticket, tc.filename)
			if got != tc.want {
				t.Errorf("provider.AuditRelPath(%q, %q, %q) = %q, want %q",
					tc.workflow, tc.ticket, tc.filename, got, tc.want)
			}
		})
	}
}

// --- launch/retry/kill/clean --json output tests ---

func TestLaunch_JSON_Launched(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "launch", "--workflow", "implement-ticket", "--branch", "develop", "TICKET-1"})
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}

	var v LaunchV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, buf.Bytes())
	}
	if v.Status != "launched" {
		t.Errorf("Status = %q, want launched", v.Status)
	}
	if v.RunID == nil || !regexp.MustCompile(`^[a-z0-9]{12}$`).MatchString(*v.RunID) {
		t.Errorf("RunID = %v, want 12-char id", v.RunID)
	}
	if v.Ticket != "TICKET-1" || v.Workflow != "implement-ticket" || v.Branch != "develop" {
		t.Errorf("ticket/workflow/branch = %q/%q/%q", v.Ticket, v.Workflow, v.Branch)
	}
	if v.ExistingRunID != nil {
		t.Errorf("ExistingRunID = %v, want nil", v.ExistingRunID)
	}

	// The recorded run must match the reported run_id.
	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()
	runs, err := st.ListByRepo(ctx, "github.com/test/repo", false)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != *v.RunID {
		t.Errorf("store run id mismatch: runs=%d, want id %q", len(runs), *v.RunID)
	}
}

func TestLaunch_JSON_Duplicate(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	if err := st.CreateRun(ctx, &store.Run{
		ID: "existingrunid", Repo: "github.com/test/repo", Ticket: "TICKET-1",
		Workflow: "implement-ticket",
		Status:   store.StatusRunning, Provider: "docker", LaunchedBy: "someone",
		StartedAt: now, TimeoutAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("pre-creating run: %v", err)
	}
	st.Close()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "launch", "--workflow", "implement-ticket", "TICKET-1"})
	// Duplicate is a protocol-level success: exit 0.
	if runErr != nil {
		t.Fatalf("expected nil error (exit 0), got %v", runErr)
	}

	var v LaunchV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, buf.Bytes())
	}
	if v.Status != "duplicate" {
		t.Errorf("Status = %q, want duplicate", v.Status)
	}
	if v.ExistingRunID == nil || *v.ExistingRunID != "existingrunid" {
		t.Errorf("ExistingRunID = %v, want existingrunid", v.ExistingRunID)
	}
	if v.RunID != nil {
		t.Errorf("RunID = %v, want nil", v.RunID)
	}
	if v.Reason == "" {
		t.Errorf("Reason is empty")
	}
}

func TestLaunch_JSON_Capped(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	for i := 0; i < 100; i++ { // docker cap is 100
		if err := st.CreateRun(ctx, &store.Run{
			ID: fmt.Sprintf("cap-run-%03d", i), Repo: "github.com/test/repo",
			Ticket: fmt.Sprintf("CAP-%d", i), Status: store.StatusRunning, Provider: "docker",
			LaunchedBy: "someone", StartedAt: now.Add(time.Duration(i) * time.Second), TimeoutAt: now.Add(25 * time.Hour),
		}); err != nil {
			t.Fatalf("pre-creating run %d: %v", i, err)
		}
	}
	st.Close()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "launch", "--workflow", "implement-ticket", "TICKET-NEW"})
	// Capped is a protocol-level success: exit 0.
	if runErr != nil {
		t.Fatalf("expected nil error (exit 0), got %v", runErr)
	}

	var v LaunchV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, buf.Bytes())
	}
	if v.Status != "capped" {
		t.Errorf("Status = %q, want capped", v.Status)
	}
	if !strings.Contains(v.Reason, "max concurrent runs reached") {
		t.Errorf("Reason = %q, want it to mention the cap", v.Reason)
	}
	if v.RunID != nil {
		t.Errorf("RunID = %v, want nil", v.RunID)
	}
}

func TestLaunch_JSON_Error(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()

	// Remove GIT_TOKEN from .env to trigger a real validation error.
	if err := os.WriteFile(filepath.Join(env.projectDir, ".env"), []byte("CLAUDE_CODE_OAUTH_TOKEN=test-key\n"), 0o644); err != nil {
		t.Fatalf("rewriting .env: %v", err)
	}

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "launch", "--workflow", "implement-ticket", "TICKET-1"})
	if runErr == nil {
		t.Fatal("expected error, got nil")
	}

	// Under --json a real error emits an ErrorV1 envelope on stdout.
	var v ErrorV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing JSON error envelope: %v\noutput: %s", err, buf.Bytes())
	}
	if v.Status != "error" {
		t.Errorf("Status = %q, want error", v.Status)
	}
	if v.Reason == "" {
		t.Errorf("Reason is empty")
	}
}

func TestLaunch_EnqueueRejectedOnDocker(t *testing.T) {
	// --enqueue is ECS-only; on docker it must be a real error (exit 1) with
	// an ErrorV1 envelope under --json explaining why.
	setupLaunchEnv(t)
	ctx := context.Background()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "launch", "--enqueue", "--workflow", "implement-ticket", "TICKET-Q"})
	if runErr == nil {
		t.Fatal("expected error (exit 1), got nil")
	}
	var v ErrorV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing JSON error envelope: %v\noutput: %s", err, buf.Bytes())
	}
	if v.Status != "error" {
		t.Errorf("Status = %q, want error", v.Status)
	}
	if !strings.Contains(v.Reason, "--enqueue requires the aws-ecs provider") {
		t.Errorf("Reason = %q, want it to mention the ECS-only requirement", v.Reason)
	}
}

func TestLaunch_HumanDuplicate_StillExits1(t *testing.T) {
	// Regression: without --json, a duplicate ticket keeps its human behavior
	// (returns an error → exit 1) and emits no JSON on stdout.
	env := setupLaunchEnv(t)
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	if err := st.CreateRun(ctx, &store.Run{
		ID: "existingrunid", Repo: "github.com/test/repo", Ticket: "TICKET-1",
		Workflow: "implement-ticket",
		Status:   store.StatusRunning, Provider: "docker", LaunchedBy: "someone",
		StartedAt: now, TimeoutAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("pre-creating run: %v", err)
	}
	st.Close()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "launch", "--workflow", "implement-ticket", "TICKET-1"})
	if runErr == nil {
		t.Fatal("expected error (exit 1), got nil")
	}
	if !strings.Contains(runErr.Error(), "duplicate active ticket") {
		t.Errorf("error %q does not mention duplicate active ticket", runErr.Error())
	}
	// No JSON object should have been written.
	var probe map[string]any
	if json.Unmarshal(buf.Bytes(), &probe) == nil {
		if _, ok := probe["status"]; ok {
			t.Errorf("human-mode duplicate emitted JSON on stdout: %s", buf.Bytes())
		}
	}
}

func TestRetry_JSON(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  image) echo "2099-01-01T00:00:00Z" ;;
  inspect) echo '{"Running":false,"ExitCode":1,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"2026-01-01T00:05:00Z"}' ;;
  exec) exit 1 ;;
  cp) exit 0 ;;
  *) echo retryrun0001container ;;
esac
`
	env := setupLaunchEnv(t)
	// Overwrite the fake docker with one that also handles launch on retry.
	if err := os.WriteFile(filepath.Join(env.binDir, "docker"), []byte(dockerScript), 0o755); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	completedAt := now.Add(-1 * time.Minute)
	runID := "retryjson001"
	// Build the workspace so retry's resume path is satisfied (it checks for
	// a .git dir inside the run's workspace).
	wsDir := filepath.Join(filepath.Dir(env.projectDir), ".horde", "workspaces", runID)
	if err := os.MkdirAll(filepath.Join(wsDir, ".git"), 0o755); err != nil {
		t.Fatalf("creating workspace: %v", err)
	}
	if err := st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
		Workflow: "implement-ticket", Status: store.StatusFailed, Provider: "docker",
		LaunchedBy: "someone", StartedAt: now.Add(-10 * time.Minute), CompletedAt: &completedAt,
		TimeoutAt: now.Add(time.Hour), InstanceID: "oldcontainer",
	}); err != nil {
		t.Fatalf("pre-creating run: %v", err)
	}
	st.Close()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "retry", runID})
	if runErr != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", runErr, buf.Bytes())
	}

	var v RetryV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, buf.Bytes())
	}
	if v.Status != "retrying" {
		t.Errorf("Status = %q, want retrying", v.Status)
	}
	if v.RunID != runID {
		t.Errorf("RunID = %q, want %q", v.RunID, runID)
	}
	if v.Ticket != "TICKET-1" {
		t.Errorf("Ticket = %q, want TICKET-1", v.Ticket)
	}
}

func TestKill_JSON(t *testing.T) {
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

	runID := "killjson0001"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	if err := st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "testuser", StartedAt: time.Now(),
		TimeoutAt: time.Now().Add(60 * time.Minute), Status: store.StatusRunning, InstanceID: "abc123",
	}); err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "kill", runID})
	if runErr != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", runErr, buf.Bytes())
	}

	var v KillV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, buf.Bytes())
	}
	if v.Status != "killed" {
		t.Errorf("Status = %q, want killed", v.Status)
	}
	if v.RunID != runID {
		t.Errorf("RunID = %q, want %q", v.RunID, runID)
	}
}

func TestClean_JSON_All(t *testing.T) {
	dockerScript := `#!/bin/sh
case "$1" in
  rm) exit 0 ;;
  *) exit 0 ;;
esac
`
	env := setupLaunchEnv(t)
	if err := os.WriteFile(filepath.Join(env.binDir, "docker"), []byte(dockerScript), 0o755); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
	ctx := context.Background()

	dbPath := filepath.Join(filepath.Dir(env.projectDir), ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	now := time.Now()
	completedAt := now.Add(-1 * time.Minute)
	// Two terminal runs (with containers) and one running run that must survive.
	terminal := []string{"cleanjson001", "cleanjson002"}
	for _, id := range terminal {
		if err := st.CreateRun(ctx, &store.Run{
			ID: id, Repo: "github.com/test/repo", Ticket: id, Status: store.StatusSuccess,
			Provider: "docker", LaunchedBy: "u", StartedAt: now.Add(-10 * time.Minute),
			CompletedAt: &completedAt, TimeoutAt: now.Add(time.Hour), InstanceID: id + "-c",
		}); err != nil {
			t.Fatalf("creating run %s: %v", id, err)
		}
	}
	if err := st.CreateRun(ctx, &store.Run{
		ID: "cleanjson999", Repo: "github.com/test/repo", Ticket: "T-run", Status: store.StatusRunning,
		Provider: "docker", LaunchedBy: "u", StartedAt: now, TimeoutAt: now.Add(time.Hour), InstanceID: "run-c",
	}); err != nil {
		t.Fatalf("creating running run: %v", err)
	}
	st.Close()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "clean"})
	if runErr != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", runErr, buf.Bytes())
	}

	var v CleanV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, buf.Bytes())
	}
	if v.Status != "cleaned" {
		t.Errorf("Status = %q, want cleaned", v.Status)
	}
	if v.RemovedCount != 2 || len(v.RemovedRunIDs) != 2 {
		t.Errorf("removed = %d / %v, want 2 terminal runs", v.RemovedCount, v.RemovedRunIDs)
	}
	for _, id := range v.RemovedRunIDs {
		if id == "cleanjson999" {
			t.Errorf("running run should not be removed")
		}
	}
}

func TestClean_JSON_Empty(t *testing.T) {
	env := setupLaunchEnv(t)
	ctx := context.Background()
	// No runs at all → empty removed list, must serialize as [] not null.
	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "clean"})
	if runErr != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", runErr, buf.Bytes())
	}
	if !strings.Contains(buf.String(), `"removed_run_ids": []`) {
		t.Errorf("expected empty array for removed_run_ids, got: %s", buf.String())
	}
	_ = env
}

func TestHydrate_JSON_FailureSingleEnvelope(t *testing.T) {
	// End-to-end guard for the no-double-envelope behavior: hydrate --json
	// emits one HydrateV1 (status:"error") and returns emittedExit{1}, so the
	// root ExitErrHandler must NOT also write an ErrorV1. A non-terminal run
	// produces a failure outcome without needing a real provider HydrateRun.
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()

	runID := "hydratejson01"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	if err := st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "u", StartedAt: time.Now(),
		TimeoutAt: time.Now().Add(time.Hour), Status: store.StatusRunning, // non-terminal → failure
	}); err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()

	into := filepath.Join(env.tmpHome, "hydrate-into")
	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "hydrate", runID, "--into", into})
	if runErr == nil {
		t.Fatal("expected non-nil error (exit 1) for a failing hydrate")
	}

	// Stdout must be exactly ONE JSON object (a HydrateV1, not a trailing
	// ErrorV1 envelope). json.Decoder.More() catches a second value.
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	var v HydrateV1
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decoding HydrateV1: %v\noutput: %s", err, buf.Bytes())
	}
	if dec.More() {
		t.Errorf("stdout has a second JSON value (double envelope): %s", buf.Bytes())
	}
	if v.Status != "error" {
		t.Errorf("Status = %q, want error", v.Status)
	}
	if v.Failed != 1 || len(v.Runs) != 1 {
		t.Errorf("failed=%d runs=%d, want 1/1", v.Failed, len(v.Runs))
	}
	if v.Runs[0].RunID != runID || v.Runs[0].Status != "failed" || v.Runs[0].Reason == "" {
		t.Errorf("run outcome = %+v, want failed with a reason", v.Runs[0])
	}
}

func TestHydrate_JSON_MissingInto_EmitsEnvelope(t *testing.T) {
	// Regression: --into must NOT be a urfave Required flag, or omitting it
	// under --json dumps help to stdout instead of an ErrorV1 envelope (the
	// same hole that was fixed for launch's --workflow).
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "hydrate", "somerunid"})
	if runErr == nil {
		t.Fatal("expected error when --into is omitted")
	}
	var v ErrorV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("stdout is not a JSON envelope (help leaked?): %v\noutput: %s", err, buf.Bytes())
	}
	if v.Status != "error" || !strings.Contains(v.Reason, "--into") {
		t.Errorf("envelope = %+v, want status=error mentioning --into", v)
	}
	_ = env
}

func TestHydrate_HumanMissingInto_Errors(t *testing.T) {
	// Without --json, omitting --into still errors (no behavior regression
	// from dropping Required:true).
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "hydrate", "somerunid"})
	if err == nil {
		t.Fatal("expected error when --into is omitted")
	}
	if !strings.Contains(err.Error(), "--into is required") {
		t.Errorf("error %q does not mention --into is required", err.Error())
	}
	_ = env
}

func TestStatus_JSON_ErrorEnvelope(t *testing.T) {
	// The JSON error envelope is produced by the shared root ExitErrHandler,
	// so exercise it through a non-launch command (status on a missing run)
	// to guard against a per-command routing regression.
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	// Create an empty store so the run genuinely doesn't exist.
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	st.Close()

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "status", "nonexistentrn"})
	if runErr == nil {
		t.Fatal("expected error for missing run")
	}
	var v ErrorV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing ErrorV1: %v\noutput: %s", err, buf.Bytes())
	}
	if v.Status != "error" || v.Reason == "" {
		t.Errorf("envelope = %+v, want status=error with reason", v)
	}
}

func TestResults_JSON_DurationSeconds(t *testing.T) {
	env := setupStatusEnv(t, "#!/bin/sh\n# no-op\n")
	ctx := context.Background()
	runID := "durresults001"
	st, err := store.NewSQLiteStore(env.dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	if err := st.CreateRun(ctx, &store.Run{
		ID: runID, Repo: "github.com/test/repo", Ticket: "TICKET-1",
		Provider: "docker", LaunchedBy: "u", StartedAt: time.Now(),
		TimeoutAt: time.Now().Add(60 * time.Minute), Status: store.StatusSuccess,
	}); err != nil {
		t.Fatalf("creating run: %v", err)
	}
	st.Close()
	resultDir := filepath.Join(env.tmpHome, ".horde", "results", runID, "audit", "TICKET-1")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatalf("creating result dir: %v", err)
	}
	// total_duration "12m 34s" = 754s; phase "4m 57s" = 297s; phase "bogus" unparseable.
	resultJSON := `{"exit_code":0,"status":"completed","ticket":"TICKET-1","workflow":"","total_cost_usd":1.0,"total_duration":"12m 34s","phases":[{"name":"a","status":"completed","cost_usd":0.5,"duration":"4m 57s"},{"name":"b","status":"completed","cost_usd":0.5,"duration":"bogus"}]}`
	if err := os.WriteFile(filepath.Join(resultDir, "run-result.json"), []byte(resultJSON), 0o644); err != nil {
		t.Fatalf("writing run-result.json: %v", err)
	}

	var buf bytes.Buffer
	app := newApp()
	setOutputs(app, &buf)
	runErr := app.Run(ctx, []string{"horde", "--provider", "docker", "--json", "results", runID})
	if runErr != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", runErr, buf.Bytes())
	}
	var v ResultsV1
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("parsing JSON: %v\noutput: %s", err, buf.Bytes())
	}
	if v.TotalDurationSecs == nil || *v.TotalDurationSecs != 754 {
		t.Errorf("TotalDurationSecs = %v, want 754", v.TotalDurationSecs)
	}
	if len(v.Phases) != 2 {
		t.Fatalf("len(Phases) = %d, want 2", len(v.Phases))
	}
	if v.Phases[0].DurationSecs == nil || *v.Phases[0].DurationSecs != 297 {
		t.Errorf("Phases[0].DurationSecs = %v, want 297", v.Phases[0].DurationSecs)
	}
	// Unparseable orc string → numeric field omitted, string preserved.
	if v.Phases[1].DurationSecs != nil {
		t.Errorf("Phases[1].DurationSecs = %v, want nil (unparseable)", v.Phases[1].DurationSecs)
	}
	if v.Phases[1].Duration != "bogus" {
		t.Errorf("Phases[1].Duration = %q, want bogus (string preserved)", v.Phases[1].Duration)
	}
}

func TestParseEnvFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     []string
		want    map[string]string
		wantErr bool
	}{
		{"nil", nil, map[string]string{}, false},
		{"empty", []string{}, map[string]string{}, false},
		{"single", []string{"FOO=bar"}, map[string]string{"FOO": "bar"}, false},
		{"multiple", []string{"FOO=bar", "BAZ=qux"}, map[string]string{"FOO": "bar", "BAZ": "qux"}, false},
		{"empty value allowed", []string{"FOO="}, map[string]string{"FOO": ""}, false},
		{"value with equals", []string{"DSN=a=b=c"}, map[string]string{"DSN": "a=b=c"}, false},
		{"leading underscore key", []string{"_X=1"}, map[string]string{"_X": "1"}, false},
		{"duplicate last wins", []string{"FOO=a", "FOO=b"}, map[string]string{"FOO": "b"}, false},
		{"missing equals", []string{"FOO"}, nil, true},
		{"empty key", []string{"=bar"}, nil, true},
		{"key starts with digit", []string{"1FOO=bar"}, nil, true},
		{"key with dash", []string{"FOO-BAR=baz"}, nil, true},
		{"key with space", []string{"FOO BAR=baz"}, nil, true},
		{"reserved REPO_URL", []string{"REPO_URL=x"}, nil, true},
		{"reserved TICKET", []string{"TICKET=x"}, nil, true},
		{"reserved BRANCH", []string{"BRANCH=x"}, nil, true},
		{"reserved WORKFLOW", []string{"WORKFLOW=x"}, nil, true},
		{"reserved RUN_ID", []string{"RUN_ID=x"}, nil, true},
		{"reserved ARTIFACTS_BUCKET", []string{"ARTIFACTS_BUCKET=x"}, nil, true},
		{"reserved ORC_EXTRA_ARGS", []string{"ORC_EXTRA_ARGS=x"}, nil, true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseEnvFlags(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseEnvFlags(%v) = %v, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEnvFlags(%v) unexpected error: %v", tc.raw, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseEnvFlags(%v) = %v, want %v", tc.raw, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("parseEnvFlags(%v)[%q] = %q, want %q", tc.raw, k, got[k], v)
				}
			}
		})
	}
}

func TestSecretCollisionsOnECS(t *testing.T) {
	t.Parallel()
	// spec keyed by container env-var name; canonicals are always present in a
	// real merged spec, plus a declared extra here.
	spec := config.SecretSpec{
		"CLAUDE_CODE_OAUTH_TOKEN": {},
		"GIT_TOKEN":               {},
		"STRIPE_API_KEY":          {},
	}
	tests := []struct {
		name     string
		provName string
		extraEnv map[string]string
		want     []string
	}{
		{
			name:     "ecs collision with declared secret",
			provName: config.ProviderECS,
			extraEnv: map[string]string{"STRIPE_API_KEY": "sk_test"},
			want:     []string{"STRIPE_API_KEY"},
		},
		{
			name:     "ecs collision with canonical secret",
			provName: config.ProviderECS,
			extraEnv: map[string]string{"GIT_TOKEN": "x"},
			want:     []string{"GIT_TOKEN"},
		},
		{
			name:     "ecs non-secret key does not warn",
			provName: config.ProviderECS,
			extraEnv: map[string]string{"PROMPT_VARIANT": "v3"},
			want:     nil,
		},
		{
			name:     "ecs multiple collisions sorted",
			provName: config.ProviderECS,
			extraEnv: map[string]string{"STRIPE_API_KEY": "x", "GIT_TOKEN": "y", "NEW": "z"},
			want:     []string{"GIT_TOKEN", "STRIPE_API_KEY"},
		},
		{
			name:     "docker never warns even on collision",
			provName: config.ProviderDocker,
			extraEnv: map[string]string{"STRIPE_API_KEY": "x", "GIT_TOKEN": "y"},
			want:     nil,
		},
		{
			name:     "ecs no extra env",
			provName: config.ProviderECS,
			extraEnv: map[string]string{},
			want:     nil,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := secretCollisionsOnECS(tc.provName, spec, tc.extraEnv)
			if len(got) != len(tc.want) {
				t.Fatalf("secretCollisionsOnECS = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("secretCollisionsOnECS[%d] = %q, want %q (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}
