package main

import (
	"os/exec"
	"testing"

	"github.com/jorge-barreto/horde/internal/config"
)

func TestResolveSSMPath_SSMOverrideWins(t *testing.T) {
	t.Parallel()
	// --ssm-path / HORDE_SSM_PATH is used verbatim, ahead of every other input.
	r := &config.Resolver{
		SSMOverride:  "/custom/path/config",
		RepoOverride: "github.com/org/repo",
		Dir:          "/some/dir",
	}
	if got := resolveSSMPath(r); got != "/custom/path/config" {
		t.Errorf("resolveSSMPath = %q, want verbatim override %q", got, "/custom/path/config")
	}
}

func TestResolveSSMPath_RepoOverrideDerivesSlug(t *testing.T) {
	t.Parallel()
	// With only a --repo override and no git, the path is derived
	// deterministically from the override's slug — no filesystem access.
	r := &config.Resolver{RepoOverride: "github.com/Org/Prepdesk.git", Dir: t.TempDir()}
	if got := resolveSSMPath(r); got != "/horde/org-prepdesk/config" {
		t.Errorf("resolveSSMPath = %q, want %q", got, "/horde/org-prepdesk/config")
	}
}

func TestResolveSSMPath_DiscoversFromGitRemote(t *testing.T) {
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
	run("git", "remote", "add", "origin", "https://github.com/example/myproj.git")

	r := &config.Resolver{Dir: dir}
	if got := resolveSSMPath(r); got != "/horde/example-myproj/config" {
		t.Errorf("resolveSSMPath = %q, want %q", got, "/horde/example-myproj/config")
	}
}

func TestResolveSSMPath_NonGitDirFallsBackToDefault(t *testing.T) {
	t.Parallel()
	// No overrides, no git: the discovery failure is swallowed to the legacy
	// default so docker-only users with no AWS/git keep working.
	r := &config.Resolver{Dir: t.TempDir()}
	if got := resolveSSMPath(r); got != config.DefaultSSMPath {
		t.Errorf("resolveSSMPath = %q, want default %q", got, config.DefaultSSMPath)
	}
}

func TestResolveSSMPath_NilResolverFallsBackToDefault(t *testing.T) {
	t.Parallel()
	if got := resolveSSMPath(nil); got != config.DefaultSSMPath {
		t.Errorf("resolveSSMPath(nil) = %q, want default %q", got, config.DefaultSSMPath)
	}
}
