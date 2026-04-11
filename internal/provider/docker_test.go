package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFakeDocker(t *testing.T, dir, script string) {
	t.Helper()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("writing fake docker: %v", err)
	}
}

func TestDockerProvider_Launch_Success(t *testing.T) {
	containerID := "abc123def456" + strings.Repeat("0", 52) // 64-char hex
	dir := t.TempDir()
	writeFakeDocker(t, dir, fmt.Sprintf("echo '%s'\n", containerID))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	result, err := p.Launch(context.Background(), LaunchOpts{
		Repo: "https://github.com/org/repo", Ticket: "T-1",
		Branch: "main", Workflow: "build", RunID: "run-001",
		EnvFile: "/tmp/test.env",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.InstanceID != containerID {
		t.Errorf("InstanceID = %q, want %q", result.InstanceID, containerID)
	}
	if result.Metadata != nil {
		t.Errorf("Metadata = %v, want nil", result.Metadata)
	}
}

func TestDockerProvider_Launch_VerifyArgs(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	containerID := strings.Repeat("a", 64)
	dir := t.TempDir()
	script := fmt.Sprintf("printf '%%s\\n' \"$@\" > %s\necho '%s'\n", argsFile, containerID)
	writeFakeDocker(t, dir, script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	opts := LaunchOpts{
		Repo: "https://github.com/org/repo", Ticket: "T-42",
		Branch: "feat", Workflow: "test", RunID: "run-99",
		EnvFile: "/secrets/.env",
	}
	_, err := NewDockerProvider().Launch(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args file: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")

	if args[0] != "run" {
		t.Errorf("args[0] = %q, want %q", args[0], "run")
	}
	if args[1] != "-d" {
		t.Errorf("args[1] = %q, want %q", args[1], "-d")
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-e REPO_URL=https://github.com/org/repo",
		"-e TICKET=T-42",
		"-e BRANCH=feat",
		"-e WORKFLOW=test",
		"-e RUN_ID=run-99",
		"--env-file /secrets/.env",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; got: %v", want, args)
		}
	}
	if args[len(args)-1] != dockerImage {
		t.Errorf("last arg = %q, want %q", args[len(args)-1], dockerImage)
	}
}

func TestDockerProvider_Launch_NoEnvFile(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	dir := t.TempDir()
	script := fmt.Sprintf("printf '%%s\\n' \"$@\" > %s\necho '%s'\n", argsFile, strings.Repeat("b", 64))
	writeFakeDocker(t, dir, script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, err := NewDockerProvider().Launch(context.Background(), LaunchOpts{
		Repo: "r", Ticket: "T", Branch: "b", Workflow: "w", RunID: "id",
		EnvFile: "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, _ := os.ReadFile(argsFile)
	if strings.Contains(string(raw), "--env-file") {
		t.Error("expected --env-file to be absent when EnvFile is empty")
	}
}

func TestDockerProvider_Launch_DockerNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir — no docker binary

	_, err := NewDockerProvider().Launch(context.Background(), LaunchOpts{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("expected exec.ErrNotFound in chain, got: %v", err)
	}
	if !strings.Contains(err.Error(), "launching docker container") {
		t.Errorf("expected 'launching docker container' in error, got: %v", err)
	}
}

func TestDockerProvider_Launch_DockerError(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, "echo 'Error response from daemon: pull access denied' >&2\nexit 1\n")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, err := NewDockerProvider().Launch(context.Background(), LaunchOpts{
		Repo: "r", Ticket: "T", Branch: "b", Workflow: "w", RunID: "id",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "launching docker container") {
		t.Errorf("expected 'launching docker container' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "pull access denied") {
		t.Errorf("expected stderr content in error, got: %v", err)
	}
}

func TestDockerProvider_Launch_EmptyOutput(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, "echo ''\n")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, err := NewDockerProvider().Launch(context.Background(), LaunchOpts{
		Repo: "r", Ticket: "T", Branch: "b", Workflow: "w", RunID: "id",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "empty container ID") {
		t.Errorf("expected 'empty container ID' in error, got: %v", err)
	}
}

func TestDockerProvider_Status_Running(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "inspect" ]; then
  echo '{"Running":true,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"0001-01-01T00:00:00Z"}'
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	status, err := p.Status(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.State != "running" {
		t.Errorf("State = %q, want %q", status.State, "running")
	}
	if status.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil", status.ExitCode)
	}
	if status.FinishedAt != nil {
		t.Errorf("FinishedAt = %v, want nil", status.FinishedAt)
	}
	wantStarted, _ := time.Parse(time.RFC3339Nano, "2024-06-15T10:30:00Z")
	if !status.StartedAt.Equal(wantStarted) {
		t.Errorf("StartedAt = %v, want %v", status.StartedAt, wantStarted)
	}
}

func TestDockerProvider_Status_Stopped(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "inspect" ]; then
  echo '{"Running":false,"ExitCode":1,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"2024-06-15T11:30:00Z"}'
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	status, err := p.Status(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.State != "stopped" {
		t.Errorf("State = %q, want %q", status.State, "stopped")
	}
	if status.ExitCode == nil {
		t.Fatal("ExitCode = nil, want non-nil")
	}
	if *status.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", *status.ExitCode)
	}
	wantStarted, _ := time.Parse(time.RFC3339Nano, "2024-06-15T10:30:00Z")
	if !status.StartedAt.Equal(wantStarted) {
		t.Errorf("StartedAt = %v, want %v", status.StartedAt, wantStarted)
	}
	if status.FinishedAt == nil {
		t.Fatal("FinishedAt = nil, want non-nil")
	}
	wantFinished, _ := time.Parse(time.RFC3339Nano, "2024-06-15T11:30:00Z")
	if !status.FinishedAt.Equal(wantFinished) {
		t.Errorf("FinishedAt = %v, want %v", status.FinishedAt, wantFinished)
	}
}

func TestDockerProvider_Status_StoppedExitZero(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "inspect" ]; then
  echo '{"Running":false,"ExitCode":0,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"2024-06-15T11:30:00Z"}'
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	status, err := p.Status(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.State != "stopped" {
		t.Errorf("State = %q, want %q", status.State, "stopped")
	}
	if status.ExitCode == nil {
		t.Fatal("ExitCode = nil, want non-nil")
	}
	if *status.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", *status.ExitCode)
	}
}

func TestDockerProvider_Status_Nonexistent(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "inspect" ]; then
  echo "Error: No such container: abc123" >&2
  exit 1
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	status, err := p.Status(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.State != "unknown" {
		t.Errorf("State = %q, want %q", status.State, "unknown")
	}
	if status.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil", status.ExitCode)
	}
}

func TestDockerProvider_Status_DockerError(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "inspect" ]; then
  echo "Error response from daemon: connection refused" >&2
  exit 1
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	_, err := p.Status(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "inspecting container") {
		t.Errorf("expected 'inspecting container' in error, got: %v", err)
	}
}

func TestDockerProvider_Status_DockerNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir — no docker binary

	p := NewDockerProvider()
	_, err := p.Status(context.Background(), "abc123")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("expected exec.ErrNotFound, got: %v", err)
	}
}

func TestDockerProvider_InterfaceCompliance(t *testing.T) {
	t.Parallel()
	var p Provider = NewDockerProvider()
	if p == nil {
		t.Error("NewDockerProvider() returned nil")
	}
}
