package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	if args[len(args)-1] != DockerImage {
		t.Errorf("last arg = %q, want %q", args[len(args)-1], DockerImage)
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

func TestDockerProvider_Launch_WithOrcArgs(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	containerID := strings.Repeat("c", 64)
	dir := t.TempDir()
	script := fmt.Sprintf("printf '%%s\\n' \"$@\" > %s\necho '%s'\n", argsFile, containerID)
	writeFakeDocker(t, dir, script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, err := NewDockerProvider().Launch(context.Background(), LaunchOpts{
		Repo: "r", Ticket: "T", Branch: "b", Workflow: "w", RunID: "id",
		OrcArgs: []string{"--resume", "--cost-limit", "5.00"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args file: %v", err)
	}
	joined := strings.Join(strings.Split(strings.TrimSpace(string(raw)), "\n"), " ")
	want := "-e ORC_EXTRA_ARGS=--resume --cost-limit 5.00"
	if !strings.Contains(joined, want) {
		t.Errorf("args missing %q; got: %s", want, joined)
	}
}

func TestDockerProvider_Launch_NoOrcArgs(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	dir := t.TempDir()
	script := fmt.Sprintf("printf '%%s\\n' \"$@\" > %s\necho '%s'\n", argsFile, strings.Repeat("d", 64))
	writeFakeDocker(t, dir, script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, err := NewDockerProvider().Launch(context.Background(), LaunchOpts{
		Repo: "r", Ticket: "T", Branch: "b", Workflow: "w", RunID: "id",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, _ := os.ReadFile(argsFile)
	if strings.Contains(string(raw), "ORC_EXTRA_ARGS") {
		t.Error("expected ORC_EXTRA_ARGS to be absent when OrcArgs is empty")
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

func TestDockerProvider_Status_BadStartedAt(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "inspect" ]; then
  echo '{"Running":true,"ExitCode":0,"StartedAt":"not-a-timestamp","FinishedAt":"0001-01-01T00:00:00Z"}'
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, err := NewDockerProvider().Status(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "parsing container start time") {
		t.Errorf("expected 'parsing container start time' in error, got: %v", err)
	}
}

func TestDockerProvider_Status_BadFinishedAt(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "inspect" ]; then
  echo '{"Running":false,"ExitCode":1,"StartedAt":"2024-06-15T10:30:00Z","FinishedAt":"not-a-timestamp"}'
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, err := NewDockerProvider().Status(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "parsing container finish time") {
		t.Errorf("expected 'parsing container finish time' in error, got: %v", err)
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
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected stderr content 'connection refused' in error, got: %v", err)
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

func TestDockerProvider_CopyFromContainer_Success(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "cp" ]; then
  exit 0
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	destDir := filepath.Join(t.TempDir(), "dest")
	p := NewDockerProvider()
	err := p.CopyFromContainer(context.Background(), "abc123", "/workspace/.orc/audit/.", destDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(destDir); err != nil {
		t.Errorf("expected destDir to exist: %v", err)
	}
}

func TestDockerProvider_CopyFromContainer_VerifyArgs(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	dir := t.TempDir()
	script := fmt.Sprintf(`
if [ "$1" = "cp" ]; then
  printf '%%s\n' "$@" > %s
  exit 0
fi
`, argsFile)
	writeFakeDocker(t, dir, script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	hostPath := filepath.Join(t.TempDir(), "dest")
	p := NewDockerProvider()
	err := p.CopyFromContainer(context.Background(), "cid", "/workspace/.orc/audit/.", hostPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args file: %v", err)
	}
	args := strings.TrimSpace(string(raw))
	for _, want := range []string{"cp", "cid:/workspace/.orc/audit/.", hostPath} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q; got: %s", want, args)
		}
	}
}

func TestDockerProvider_CopyFromContainer_Failure(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "cp" ]; then
  echo "no such directory" >&2
  exit 1
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.CopyFromContainer(context.Background(), "abc123", "/workspace/.orc/audit/.", t.TempDir())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "copying from container") {
		t.Errorf("expected 'copying from container' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no such directory") {
		t.Errorf("expected stderr content 'no such directory' in error, got: %v", err)
	}
}

func TestDockerProvider_RemoveContainer_Success(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "rm" ]; then
  exit 0
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.RemoveContainer(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDockerProvider_RemoveContainer_VerifyArgs(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	dir := t.TempDir()
	script := fmt.Sprintf(`
if [ "$1" = "rm" ]; then
  printf '%%s\n' "$@" > %s
  exit 0
fi
`, argsFile)
	writeFakeDocker(t, dir, script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.RemoveContainer(context.Background(), "container-xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args file: %v", err)
	}
	args := strings.TrimSpace(string(raw))
	for _, want := range []string{"rm", "container-xyz"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q; got: %s", want, args)
		}
	}
}

func TestDockerProvider_RemoveContainer_Failure(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "rm" ]; then
  echo "No such container" >&2
  exit 1
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.RemoveContainer(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "removing container") {
		t.Errorf("expected 'removing container' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "No such container") {
		t.Errorf("expected stderr content 'No such container' in error, got: %v", err)
	}
}

func TestDockerProvider_Logs_NoFollow_Success(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "logs" ]; then
  echo "log line 1"
  echo "log line 2"
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	rc, err := NewDockerProvider().Logs(context.Background(), "abc123", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading logs: %v", err)
	}
	if string(got) != "log line 1\nlog line 2\n" {
		t.Errorf("content = %q, want %q", string(got), "log line 1\nlog line 2\n")
	}
}

func TestDockerProvider_Logs_NoFollow_ContainerNotFound(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "logs" ]; then
  echo "Error: No such container: abc123" >&2
  exit 1
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, err := NewDockerProvider().Logs(context.Background(), "abc123", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "container not found") {
		t.Errorf("expected 'container not found' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "abc123") {
		t.Errorf("expected 'abc123' in error, got: %v", err)
	}
}

func TestDockerProvider_Logs_NoFollow_DockerError(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "logs" ]; then
  echo "Error response from daemon: connection refused" >&2
  exit 1
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, err := NewDockerProvider().Logs(context.Background(), "abc123", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "reading container logs") {
		t.Errorf("expected 'reading container logs' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected 'connection refused' in error, got: %v", err)
	}
}

func TestDockerProvider_Logs_NoFollow_VerifyArgs(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	dir := t.TempDir()
	script := fmt.Sprintf(`
if [ "$1" = "logs" ]; then
  printf '%%s\n' "$@" > %s
fi
`, argsFile)
	writeFakeDocker(t, dir, script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, _ = NewDockerProvider().Logs(context.Background(), "container-id", false)

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading args file: %v", err)
	}
	args := strings.TrimSpace(string(raw))
	if !strings.Contains(args, "logs") {
		t.Errorf("expected 'logs' subcommand in args, got: %s", args)
	}
	if !strings.Contains(args, "container-id") {
		t.Errorf("expected 'container-id' in args, got: %s", args)
	}
	if strings.Contains(args, "--follow") {
		t.Errorf("expected no --follow flag in args, got: %s", args)
	}
}

func TestDockerProvider_Logs_DockerNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir — no docker binary

	_, err := NewDockerProvider().Logs(context.Background(), "abc123", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("expected exec.ErrNotFound in chain, got: %v", err)
	}
	if !strings.Contains(err.Error(), "reading container logs") {
		t.Errorf("expected 'reading container logs' in error, got: %v", err)
	}
}

func TestDockerProvider_Logs_Follow_Success(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "inspect" ]; then
  echo 'abc123full'
elif [ "$1" = "logs" ]; then
  echo "follow line 1"
  echo "follow line 2"
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	rc, err := NewDockerProvider().Logs(context.Background(), "abc123", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading logs: %v", err)
	}
	if !strings.Contains(string(got), "follow line 1") {
		t.Errorf("expected 'follow line 1' in output, got: %q", string(got))
	}
	if !strings.Contains(string(got), "follow line 2") {
		t.Errorf("expected 'follow line 2' in output, got: %q", string(got))
	}
}

func TestDockerProvider_Logs_Follow_ContainerNotFound(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "inspect" ]; then
  echo "Error: No such container: abc123" >&2
  exit 1
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, err := NewDockerProvider().Logs(context.Background(), "abc123", true)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "container not found") {
		t.Errorf("expected 'container not found' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "abc123") {
		t.Errorf("expected 'abc123' in error, got: %v", err)
	}
}

func TestDockerProvider_Logs_Follow_Close(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "inspect" ]; then
  echo 'abc123full'
elif [ "$1" = "logs" ]; then
  while true; do
    echo "streaming line"
    sleep 0.1
  done
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rc, err := NewDockerProvider().Logs(ctx, "abc123", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	buf := make([]byte, 5)
	_, err = rc.Read(buf)
	if err != nil {
		t.Fatalf("reading from log stream: %v", err)
	}

	if err := rc.Close(); err != nil {
		t.Errorf("Close() returned error: %v", err)
	}
}

func TestDockerProvider_Logs_Follow_Close_ReapsProcess(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "inspect" ]; then
  echo 'abc123full'
elif [ "$1" = "logs" ]; then
  while true; do
    echo "streaming line"
    sleep 0.1
  done
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rc, err := NewDockerProvider().Logs(ctx, "abc123", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	buf := make([]byte, 5)
	_, err = rc.Read(buf)
	if err != nil {
		t.Fatalf("reading from log stream: %v", err)
	}

	if err := rc.Close(); err != nil {
		t.Fatalf("Close() returned error: %v", err)
	}

	lrc := rc.(*logReadCloser)
	if lrc.cmd.ProcessState == nil {
		t.Error("expected ProcessState to be non-nil after Close() (process not reaped)")
	}
}

func TestDockerProvider_Logs_Follow_VerifyArgs(t *testing.T) {
	inspectArgsFile := filepath.Join(t.TempDir(), "inspect-args.txt")
	logsArgsFile := filepath.Join(t.TempDir(), "logs-args.txt")
	dir := t.TempDir()
	script := fmt.Sprintf(`
if [ "$1" = "inspect" ]; then
  printf '%%s\n' "$@" > %s
  echo 'abc123full'
elif [ "$1" = "logs" ]; then
  printf '%%s\n' "$@" > %s
fi
`, inspectArgsFile, logsArgsFile)
	writeFakeDocker(t, dir, script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	rc, err := NewDockerProvider().Logs(context.Background(), "container-id", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Read all output to wait for the logs script to finish writing the args file.
	io.ReadAll(rc) //nolint
	rc.Close()

	raw, err := os.ReadFile(logsArgsFile)
	if err != nil {
		t.Fatalf("reading logs args file: %v", err)
	}
	args := strings.TrimSpace(string(raw))
	for _, want := range []string{"logs", "--follow", "container-id"} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in logs args, got: %s", want, args)
		}
	}
}

func TestDockerProvider_Stop_Success(t *testing.T) {
	resultsDir := filepath.Join(t.TempDir(), "results")
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "stop" ]; then
  exit 0
elif [ "$1" = "cp" ]; then
  exit 0
elif [ "$1" = "rm" ]; then
  exit 0
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.Stop(context.Background(), StopOpts{InstanceID: "abc123", ResultsDir: resultsDir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDockerProvider_Stop_ContainerNotFound(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "stop" ]; then
  echo "Error: No such container: abc123" >&2
  exit 1
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.Stop(context.Background(), StopOpts{InstanceID: "abc123", ResultsDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "stopping container") {
		t.Errorf("expected 'killing container' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "container not found") {
		t.Errorf("expected 'container not found' in error, got: %v", err)
	}
}

func TestDockerProvider_Stop_AlreadyStopped(t *testing.T) {
	resultsDir := filepath.Join(t.TempDir(), "results")
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "stop" ]; then
  exit 0
elif [ "$1" = "cp" ]; then
  exit 0
elif [ "$1" = "rm" ]; then
  exit 0
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.Stop(context.Background(), StopOpts{InstanceID: "abc123", ResultsDir: resultsDir})
	if err != nil {
		t.Fatalf("expected no error for already-stopped container, got: %v", err)
	}
}

func TestDockerProvider_Stop_StopError(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "stop" ]; then
  echo "Error response from daemon: cannot stop container" >&2
  exit 1
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.Stop(context.Background(), StopOpts{InstanceID: "abc123", ResultsDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "stopping container") {
		t.Errorf("expected 'stopping container' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot stop container") {
		t.Errorf("expected stderr content in error, got: %v", err)
	}
}

func TestDockerProvider_Stop_CopyFailure_OK(t *testing.T) {
	resultsDir := filepath.Join(t.TempDir(), "results")
	dir := t.TempDir()
	writeFakeDocker(t, dir, `
if [ "$1" = "stop" ]; then
  exit 0
elif [ "$1" = "cp" ]; then
  echo "no such directory" >&2
  exit 1
elif [ "$1" = "rm" ]; then
  exit 0
fi
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.Stop(context.Background(), StopOpts{InstanceID: "abc123", ResultsDir: resultsDir})
	if err != nil {
		t.Fatalf("expected no error when copy fails (best-effort), got: %v", err)
	}
}

func TestDockerProvider_Stop_VerifyStopArgs(t *testing.T) {
	stopArgsFile := filepath.Join(t.TempDir(), "stop-args.txt")
	dir := t.TempDir()
	script := fmt.Sprintf(`
if [ "$1" = "stop" ]; then
  printf '%%s\n' "$@" > %s
  exit 0
elif [ "$1" = "cp" ]; then
  exit 0
elif [ "$1" = "rm" ]; then
  exit 0
fi
`, stopArgsFile)
	writeFakeDocker(t, dir, script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.Stop(context.Background(), StopOpts{InstanceID: "container-xyz", ResultsDir: t.TempDir()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, err := os.ReadFile(stopArgsFile)
	if err != nil {
		t.Fatalf("reading stop args file: %v", err)
	}
	args := strings.TrimSpace(string(raw))
	for _, want := range []string{"stop", "container-xyz"} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in stop args, got: %s", want, args)
		}
	}
}

func TestDockerProvider_Stop_CopyPaths(t *testing.T) {
	cpArgsFile := filepath.Join(t.TempDir(), "cp-args.txt")
	resultsDir := filepath.Join(t.TempDir(), "results")
	dir := t.TempDir()
	script := fmt.Sprintf(`
if [ "$1" = "stop" ]; then
  exit 0
elif [ "$1" = "cp" ]; then
  printf '%%s\n' "$@" >> %s
  exit 0
elif [ "$1" = "rm" ]; then
  exit 0
fi
`, cpArgsFile)
	writeFakeDocker(t, dir, script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.Stop(context.Background(), StopOpts{InstanceID: "cid", ResultsDir: resultsDir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, err := os.ReadFile(cpArgsFile)
	if err != nil {
		t.Fatalf("reading cp args file: %v", err)
	}
	args := string(raw)

	wantAuditSrc := "cid:/workspace/.orc/audit/."
	wantArtifactsSrc := "cid:/workspace/.orc/artifacts/."
	wantAuditDest := filepath.Join(resultsDir, "audit")
	wantArtifactsDest := filepath.Join(resultsDir, "artifacts")

	for _, want := range []string{wantAuditSrc, wantArtifactsSrc, wantAuditDest, wantArtifactsDest} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in cp args, got: %s", want, args)
		}
	}
}

func TestDockerProvider_Stop_EmptyResultsDir(t *testing.T) {
	cpArgsFile := filepath.Join(t.TempDir(), "cp-args.txt")
	dir := t.TempDir()
	script := fmt.Sprintf(`
if [ "$1" = "stop" ]; then
  exit 0
elif [ "$1" = "cp" ]; then
  printf '%%s\n' "$@" >> %s
  exit 0
elif [ "$1" = "rm" ]; then
  exit 0
fi
`, cpArgsFile)
	writeFakeDocker(t, dir, script)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	p := NewDockerProvider()
	err := p.Stop(context.Background(), StopOpts{InstanceID: "abc123", ResultsDir: ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(cpArgsFile); err == nil {
		t.Error("expected no docker cp calls when ResultsDir is empty, but cp-args.txt was created")
	}
}

func TestDockerProvider_ReadFile_Success(t *testing.T) {
	tmpdir := t.TempDir()
	dir := filepath.Join(tmpdir, ".horde", "results", "run-001", "audit", "PROJ-123")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("creating dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run-result.json"), []byte(`{"ok":true}`), 0644); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	t.Setenv("HOME", tmpdir)

	p := NewDockerProvider()
	data, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "run-001", Path: ".orc/audit/PROJ-123/run-result.json"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("expected %q, got %q", `{"ok":true}`, string(data))
	}
}

func TestDockerProvider_ReadFile_FileNotFound(t *testing.T) {
	tmpdir := t.TempDir()
	dir := filepath.Join(tmpdir, ".horde", "results", "run-001")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("creating dir: %v", err)
	}
	t.Setenv("HOME", tmpdir)

	p := NewDockerProvider()
	_, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "run-001", Path: ".orc/audit/missing.json"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var notFound *FileNotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("ReadFile() error type = %T, want *FileNotFoundError", err)
	}
	if notFound.Path != ".orc/audit/missing.json" {
		t.Errorf("FileNotFoundError.Path = %q, want %q", notFound.Path, ".orc/audit/missing.json")
	}
}

func TestDockerProvider_ReadFile_PathTraversal(t *testing.T) {
	tmpdir := t.TempDir()
	t.Setenv("HOME", tmpdir)

	p := NewDockerProvider()
	_, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "run-001", Path: ".orc/audit/../../etc/passwd"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "path escapes results directory") {
		t.Errorf("expected error to contain %q, got: %v", "path escapes results directory", err)
	}
}

func TestDockerProvider_ReadFile_EmptyRunID(t *testing.T) {
	p := NewDockerProvider()
	_, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "", Path: ".orc/audit/foo.json"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "run ID is required") {
		t.Errorf("expected error to contain %q, got: %v", "run ID is required", err)
	}
}

func TestDockerProvider_ReadFile_EmptyPath(t *testing.T) {
	p := NewDockerProvider()
	_, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "run-001", Path: ""})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("expected error to contain %q, got: %v", "path is required", err)
	}
}

func TestDockerProvider_ReadFile_InvalidPrefix(t *testing.T) {
	p := NewDockerProvider()
	_, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "run-001", Path: "some/other/file.txt"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "path must start with") {
		t.Errorf("expected error to contain %q, got: %v", "path must start with", err)
	}
}

func TestDockerProvider_ReadFile_ArtifactsPath(t *testing.T) {
	tmpdir := t.TempDir()
	dir := filepath.Join(tmpdir, ".horde", "results", "run-002", "artifacts")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("creating dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "output.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	t.Setenv("HOME", tmpdir)

	p := NewDockerProvider()
	data, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "run-002", Path: ".orc/artifacts/output.txt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("expected %q, got %q", "hello", string(data))
	}
}

func TestDockerProvider_ReadFile_RunIDTraversal(t *testing.T) {
	p := NewDockerProvider()
	_, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "../../etc", Path: ".orc/passwd"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid run ID") {
		t.Errorf("expected error to contain %q, got: %v", "invalid run ID", err)
	}
}

func TestDockerProvider_ReadFile_BareOrcPrefix(t *testing.T) {
	p := NewDockerProvider()
	_, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "run-001", Path: ".orc/"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "path must include a filename") {
		t.Errorf("expected error to contain %q, got: %v", "path must include a filename", err)
	}
}

func TestDockerProvider_ReadFile_ReadError(t *testing.T) {
	tmpdir := t.TempDir()
	// Create target path as a directory — os.ReadFile on a dir returns EISDIR, not IsNotExist
	dir := filepath.Join(tmpdir, ".horde", "results", "run-001", "audit")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("creating dir: %v", err)
	}
	t.Setenv("HOME", tmpdir)

	p := NewDockerProvider()
	_, err := p.ReadFile(context.Background(), ReadFileOpts{RunID: "run-001", Path: ".orc/audit"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "reading file:") {
		t.Errorf("expected error to contain %q, got: %v", "reading file:", err)
	}
	var notFound *FileNotFoundError
	if errors.As(err, &notFound) {
		t.Errorf("ReadFile() error = %T, should NOT be *FileNotFoundError", err)
	}
}
