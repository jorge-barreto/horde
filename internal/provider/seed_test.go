package provider_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jorge-barreto/horde/internal/provider"
)

// seedFixture creates a real git repo at dir with one committed file, then layers
// on an uncommitted edit, an untracked file, and a gitignored file.
func seedFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write("committed.txt", "v1")
	write(".gitignore", "ignored.txt\nbuild/\n")
	run("add", "committed.txt", ".gitignore")
	run("commit", "-q", "-m", "init")
	write("committed.txt", "v2-uncommitted") // tracked + modified
	write("untracked.txt", "new")            // untracked, not ignored
	write("ignored.txt", "secret")           // gitignored
	write("build/out.o", "junk")             // gitignored dir
	return dir
}

func TestSeedWorkspace_IncludesTrackedUncommittedAndUntracked(t *testing.T) {
	src := seedFixture(t)
	dest := t.TempDir()
	if err := provider.SeedWorkspaceFromWorkingTree(context.Background(), src, dest); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Tracked file carries the UNCOMMITTED bytes.
	if b, _ := os.ReadFile(filepath.Join(dest, "committed.txt")); string(b) != "v2-uncommitted" {
		t.Errorf("committed.txt = %q, want uncommitted bytes", b)
	}
	// Untracked-not-ignored file present.
	if _, err := os.Stat(filepath.Join(dest, "untracked.txt")); err != nil {
		t.Errorf("untracked.txt missing: %v", err)
	}
	// .git present (entrypoint skip-clone trigger).
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Errorf(".git missing: %v", err)
	}
}

func TestSeedWorkspace_ExcludesGitignored(t *testing.T) {
	src := seedFixture(t)
	dest := t.TempDir()
	if err := provider.SeedWorkspaceFromWorkingTree(context.Background(), src, dest); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "ignored.txt")); !os.IsNotExist(err) {
		t.Errorf("ignored.txt should be excluded, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "build", "out.o")); !os.IsNotExist(err) {
		t.Errorf("build/out.o should be excluded, stat err = %v", err)
	}
}

func TestSeedWorkspace_DoesNotMutateSource(t *testing.T) {
	src := seedFixture(t)
	dest := t.TempDir()
	if err := provider.SeedWorkspaceFromWorkingTree(context.Background(), src, dest); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Source working file unchanged.
	if b, _ := os.ReadFile(filepath.Join(src, "committed.txt")); string(b) != "v2-uncommitted" {
		t.Errorf("source committed.txt mutated: %q", b)
	}
}

func TestSeedWorkspace_NonGitSourceErrors(t *testing.T) {
	src := t.TempDir() // no git
	dest := t.TempDir()
	if err := provider.SeedWorkspaceFromWorkingTree(context.Background(), src, dest); err == nil {
		t.Fatal("expected error for non-git source, got nil")
	}
}
