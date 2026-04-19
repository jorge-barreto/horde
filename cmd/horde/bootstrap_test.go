package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupGitRepo initializes a git repo at dir with a configured origin remote.
// Returns dir for convenience.
func setupGitRepo(t *testing.T, dir, remoteURL string) string {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "remote", "add", "origin", remoteURL)
	return dir
}

func runBootstrapInit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	var buf bytes.Buffer
	cmd := newApp()
	cmd.Writer = &buf
	cmd.ErrWriter = &buf
	fullArgs := append([]string{"horde", "bootstrap", "init"}, args...)
	err = cmd.Run(context.Background(), fullArgs)
	return buf.String(), err
}

func TestBootstrapInit_WritesFile(t *testing.T) {
	dir := t.TempDir()
	setupGitRepo(t, dir, "https://github.com/jorge-barreto/horde.git")

	out, err := runBootstrapInit(t, dir)
	if err != nil {
		t.Fatalf("bootstrap init: %v\noutput:\n%s", err, out)
	}

	dest := filepath.Join(dir, ".horde", "cloudformation.yaml")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading %s: %v", dest, err)
	}
	s := string(data)
	if !strings.Contains(s, "horde-jorge-barreto-horde-vpc") {
		t.Errorf("rendered template missing slug-derived VPC tag; got head: %s", s[:min(400, len(s))])
	}
}

func TestBootstrapInit_RefusesExisting(t *testing.T) {
	dir := t.TempDir()
	setupGitRepo(t, dir, "https://github.com/acme/widgets.git")

	if _, err := runBootstrapInit(t, dir); err != nil {
		t.Fatalf("first init: %v", err)
	}

	_, err := runBootstrapInit(t, dir)
	if err == nil {
		t.Fatal("second init: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second init error %q, want substring %q", err.Error(), "already exists")
	}
}

func TestBootstrapInit_RegenerateOverwrites(t *testing.T) {
	dir := t.TempDir()
	setupGitRepo(t, dir, "https://github.com/acme/widgets.git")

	if _, err := runBootstrapInit(t, dir); err != nil {
		t.Fatalf("first init: %v", err)
	}

	dest := filepath.Join(dir, ".horde", "cloudformation.yaml")
	if err := os.WriteFile(dest, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tampering: %v", err)
	}

	if _, err := runBootstrapInit(t, dir, "--regenerate"); err != nil {
		t.Fatalf("regenerate: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading after regenerate: %v", err)
	}
	if string(data) == "tampered" {
		t.Errorf("regenerate did not overwrite the file")
	}
	if !strings.Contains(string(data), "horde-acme-widgets-vpc") {
		t.Errorf("regenerated output does not contain expected slug")
	}
}

func TestBootstrapInit_NoGitRemote(t *testing.T) {
	dir := t.TempDir()
	// Initialize a git repo with NO remote.
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	_, err := runBootstrapInit(t, dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
