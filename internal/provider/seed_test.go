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

// TestSeedWorkspace_SkipsNestedGitRepo verifies that an untracked nested git
// repository (e.g. a vendored sub-repo or a cloned examples/ directory) does
// not cause SeedWorkspaceFromWorkingTree to fail. git ls-files emits the
// nested repo's directory entry (e.g. "nested/") as a single path when
// --others is used; prior to the fix, os.Stat returned a directory FileInfo
// and copyRegularFile failed with "copy_file_range: is a directory". The fix
// uses os.Lstat and skips any non-regular entry.
func TestSeedWorkspace_SkipsNestedGitRepo(t *testing.T) {
	src := seedFixture(t)

	// Create an untracked nested git repo inside the fixture.
	nestedDir := filepath.Join(src, "nested")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	nestedFile := filepath.Join(nestedDir, "file.txt")
	if err := os.WriteFile(nestedFile, []byte("nested content"), 0o644); err != nil {
		t.Fatal(err)
	}
	initCmd := exec.Command("git", "init", "-q")
	initCmd.Dir = nestedDir
	initCmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init nested: %v\n%s", err, out)
	}

	dest := t.TempDir()

	// Must not error — the nested repo gitlink must be skipped gracefully.
	if err := provider.SeedWorkspaceFromWorkingTree(context.Background(), src, dest); err != nil {
		t.Fatalf("seed with nested git repo: %v", err)
	}

	// The nested/ directory should not have been copied as a file (or at all).
	if _, err := os.Stat(filepath.Join(dest, "nested")); !os.IsNotExist(err) {
		t.Errorf("nested/ should not be present in dest; stat err = %v", err)
	}

	// Regular files from the outer repo must still be present.
	if b, _ := os.ReadFile(filepath.Join(dest, "committed.txt")); string(b) != "v2-uncommitted" {
		t.Errorf("committed.txt = %q, want uncommitted bytes", b)
	}
	if _, err := os.Stat(filepath.Join(dest, "untracked.txt")); err != nil {
		t.Errorf("untracked.txt missing: %v", err)
	}
}
