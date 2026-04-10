package config

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRepoURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https with .git", "https://github.com/org/repo.git", "github.com/org/repo.git"},
		{"https without .git", "https://github.com/org/repo", "github.com/org/repo"},
		{"http", "http://github.com/org/repo.git", "github.com/org/repo.git"},
		{"ssh scp-style with .git", "git@github.com:org/repo.git", "github.com/org/repo.git"},
		{"ssh scp-style without .git", "git@github.com:org/repo", "github.com/org/repo"},
		{"ssh url-style", "ssh://git@github.com/org/repo.git", "github.com/org/repo.git"},
		{"git protocol", "git://github.com/org/repo.git", "github.com/org/repo.git"},
		{"https with trailing whitespace", "https://github.com/org/repo.git\n", "github.com/org/repo.git"},
		{"non-github host", "git@gitlab.com:org/repo.git", "gitlab.com/org/repo.git"},
		{"deep path", "https://github.com/org/sub/repo.git", "github.com/org/sub/repo.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRepoURL(tc.in)
			if err != nil {
				t.Fatalf("NormalizeRepoURL(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeRepoURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeRepoURL_Errors(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"empty string", "", "empty remote URL"},
		{"whitespace only", "  ", "empty remote URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeRepoURL(tc.in)
			if err == nil {
				t.Fatalf("NormalizeRepoURL(%q) expected error, got nil", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("NormalizeRepoURL(%q) error = %q, want it to contain %q", tc.in, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestNormalizeRepoURL_PreservesParseError(t *testing.T) {
	// "http://host/%zz" contains a scheme so it enters the url.Parse branch.
	// %zz is an invalid percent-encoding, causing url.Parse to return *url.Error.
	_, err := NormalizeRepoURL("http://host/%zz")
	if err == nil {
		t.Fatal("NormalizeRepoURL with invalid URL expected error, got nil")
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Errorf("expected error chain to contain *url.Error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot normalize remote URL") {
		t.Errorf("expected error message to contain wrapper context, got: %v", err)
	}
}

func TestRepoURL_Success(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "remote", "add", "origin", "https://github.com/test/repo.git")

	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	got, err := RepoURL()
	if err != nil {
		t.Fatalf("RepoURL() error: %v", err)
	}
	want := "github.com/test/repo.git"
	if got != want {
		t.Errorf("RepoURL() = %q, want %q", got, want)
	}
}

func TestRepoURL_NotGitRepo(t *testing.T) {
	dir := t.TempDir()

	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	_, err = RepoURL()
	if err == nil {
		t.Fatal("RepoURL() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("RepoURL() error = %q, want it to contain %q", err.Error(), "not a git repository")
	}
}

func TestRepoURL_NoOriginRemote(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	_, err = RepoURL()
	if err == nil {
		t.Fatal("RepoURL() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no origin remote") {
		t.Errorf("RepoURL() error = %q, want it to contain %q", err.Error(), "no origin remote")
	}
}

func TestRepoURL_PreservesExitError(t *testing.T) {
	dir := t.TempDir()

	// Create a fake git that outputs a non-standard message to stderr and exits 1.
	// This exercises the fallthrough path at config.go:69 (not "not a git repository"
	// or "No such remote"), confirming *exec.ExitError is preserved in the chain.
	fakeGit := filepath.Join(dir, "git")
	err := os.WriteFile(fakeGit, []byte("#!/bin/sh\necho 'something unexpected' >&2\nexit 1\n"), 0o755)
	if err != nil {
		t.Fatalf("writing fake git: %v", err)
	}

	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	_, err = RepoURL()
	if err == nil {
		t.Fatal("RepoURL() expected error, got nil")
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("expected error chain to contain *exec.ExitError, got: %v", err)
	}
	if !strings.Contains(err.Error(), "resolving git remote") {
		t.Errorf("expected error message to contain wrapper context, got: %v", err)
	}
	if !strings.Contains(err.Error(), "something unexpected") {
		t.Errorf("expected error message to contain stderr text, got: %v", err)
	}
}

func TestLaunchedBy_Configured(t *testing.T) {
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

	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	got := LaunchedBy()
	if got != "Test User" {
		t.Errorf("LaunchedBy() = %q, want %q", got, "Test User")
	}
}

func TestLaunchedBy_Unconfigured(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	// Prevent global/system git config from leaking user.name into the test
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	got := LaunchedBy()
	if got != "unknown" {
		t.Errorf("LaunchedBy() = %q, want %q", got, "unknown")
	}
}
