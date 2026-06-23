package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SeedWorkspaceFromWorkingTree populates destDir with the git working tree at
// srcDir — every tracked file (with its current, possibly-uncommitted bytes)
// and every untracked file not matched by .gitignore, plus the full .git
// directory. The .git copy makes the worker entrypoint's "/workspace/.git
// present → skip clone" branch fire, so the run executes against exactly the
// local state. srcDir is never modified. Docker-only (the local-source mode
// for `horde exec --local`).
func SeedWorkspaceFromWorkingTree(ctx context.Context, srcDir, destDir string) error {
	// Enumerate tracked + untracked-not-ignored paths via git so .gitignore is
	// honored exactly. -z gives NUL-separated paths (safe for odd filenames).
	cmd := exec.CommandContext(ctx, "git", "-C", srcDir,
		"ls-files", "--cached", "--others", "--exclude-standard", "-z")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("listing working tree (is %s a git repo?): %w", srcDir, err)
	}
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" {
			continue
		}
		src := filepath.Join(srcDir, rel)
		info, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("seeding %s: %w", rel, err)
		}
		dst := filepath.Join(destDir, rel)
		if err := copyRegularFile(src, dst, info.Mode().Perm()); err != nil {
			return fmt.Errorf("seeding %s: %w", rel, err)
		}
	}
	// Copy the whole .git so history/branch state is present in the workspace.
	if err := copyLocalTree(filepath.Join(srcDir, ".git"), filepath.Join(destDir, ".git")); err != nil {
		return fmt.Errorf("seeding .git: %w", err)
	}
	return nil
}
