package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jorge-barreto/horde/internal/store"
)

// TestExecLocalAllowed_DockerOK verifies the pure guard accepts docker.
func TestExecLocalAllowed_DockerOK(t *testing.T) {
	t.Parallel()
	if err := execLocalAllowed("docker"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestExecLocalAllowed_ECSRejected verifies the pure guard rejects aws-ecs
// and mentions docker in the message.
func TestExecLocalAllowed_ECSRejected(t *testing.T) {
	t.Parallel()
	err := execLocalAllowed("aws-ecs")
	if err == nil {
		t.Fatal("expected error for aws-ecs, got nil")
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Errorf("expected error to mention docker, got: %v", err)
	}
}

// TestExec_RequiresOrcArgs verifies that `horde exec` with no orc args
// after -- returns a usage error mentioning "orc".
func TestExec_RequiresOrcArgs(t *testing.T) {
	setupLaunchEnv(t)
	ctx := context.Background()

	err := newApp().Run(ctx, []string{"horde", "--provider", "docker", "exec"})
	if err == nil || !strings.Contains(err.Error(), "orc") {
		t.Fatalf("expected usage error about missing orc args, got %v", err)
	}
}

// TestExec_LocalNoRemote_SyntheticRepoKey verifies that --local with no git
// remote falls back to "local/<basename-of-cwd>" as the Run.Repo bucket key.
// This exercises the resolveCanonicalRepo error branch in execCmd's Action.
// Cannot be parallel: uses t.Setenv + os.Chdir.
func TestExec_LocalNoRemote_SyntheticRepoKey(t *testing.T) {
	tmpHome := t.TempDir()

	// Create a git repo with NO remote — resolveCanonicalRepo must fail so the
	// synthetic fallback fires.
	projectDir := filepath.Join(tmpHome, "myproject")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("creating project dir: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init")
	// Deliberately do NOT add a remote.

	// Write required .env file.
	envContent := "CLAUDE_CODE_OAUTH_TOKEN=test-key\nGIT_TOKEN=test-token\n"
	if err := os.WriteFile(filepath.Join(projectDir, ".env"), []byte(envContent), 0o644); err != nil {
		t.Fatalf("writing .env: %v", err)
	}

	// Fake docker that records invocations and returns a container ID.
	binDir := filepath.Join(tmpHome, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("creating bin dir: %v", err)
	}
	dockerScript := "#!/bin/sh\ncase \"$1\" in\n  image) echo \"2099-01-01T00:00:00Z\";;\n  *) echo abc123container;;\nesac\n"
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(dockerScript), 0o755); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}

	t.Setenv("HOME", tmpHome)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting working directory: %v", err)
	}
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("changing to project dir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(oldDir) })

	ctx := context.Background()

	// Capture stdout (exec prints the run ID).
	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = origStdout }()

	runErr := newApp().Run(ctx, []string{"horde", "--provider", "docker", "exec", "--local", "--", "eval", "x"})

	pw.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(pr)

	if runErr != nil {
		t.Fatalf("exec --local with no remote: unexpected error: %v", runErr)
	}

	// Verify the stored run's Repo is the synthetic "local/<dirname>" key.
	dbPath := filepath.Join(tmpHome, ".horde", "horde.db")
	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()

	wantRepo := "local/myproject"
	runs, err := st.ListByRepo(ctx, wantRepo, false)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run in repo %q, got %d; stdout=%q", wantRepo, len(runs), strings.TrimSpace(string(out)))
	}
	if runs[0].Repo != wantRepo {
		t.Errorf("Run.Repo = %q, want %q", runs[0].Repo, wantRepo)
	}
}

// TestExecCappedV1_Marshal guards the capped JSON path: status must be
// "capped", run_id must be null, and the reason must appear.
func TestExecCappedV1_Marshal(t *testing.T) {
	t.Parallel()
	v := execCappedV1([]string{"eval", "x"}, false, "max concurrent runs reached (5/5)")
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"status":"capped"`) {
		t.Errorf("missing \"status\":\"capped\" in %s", got)
	}
	if !strings.Contains(got, `"run_id":null`) {
		t.Errorf("missing \"run_id\":null in %s", got)
	}
	if !strings.Contains(got, "max concurrent") {
		t.Errorf("missing reason in %s", got)
	}
}
