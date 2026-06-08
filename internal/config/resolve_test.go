package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolver_CanonicalRepo_OverrideWins(t *testing.T) {
	t.Parallel()
	// Dir points at a git repo with a DIFFERENT remote; the override must win
	// and the canonicalization must still apply to the override.
	dir := initGitRepo(t, "https://github.com/other/repo.git")
	r := &Resolver{RepoOverride: "https://github.com/Org/Override.git", Dir: dir}
	got, err := r.CanonicalRepo()
	if err != nil {
		t.Fatalf("CanonicalRepo() error: %v", err)
	}
	if want := "github.com/org/override"; got != want {
		t.Errorf("CanonicalRepo() = %q, want %q (override should win and canonicalize)", got, want)
	}
}

func TestResolver_CanonicalRepo_OverrideNoFilesystem(t *testing.T) {
	t.Parallel()
	// The #29 acceptance test: empty dir, no git, only the override set.
	// Must resolve without ever shelling out to git or erroring "not a git repo".
	dir := t.TempDir() // no git repo here
	r := &Resolver{RepoOverride: "github.com/org/prepdesk", Dir: dir}
	got, err := r.CanonicalRepo()
	if err != nil {
		t.Fatalf("CanonicalRepo() from empty dir with override errored: %v", err)
	}
	if want := "github.com/org/prepdesk"; got != want {
		t.Errorf("CanonicalRepo() = %q, want %q", got, want)
	}
}

func TestResolver_CanonicalRepo_DiscoveryFallback(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t, "git@github.com:Org/Repo.git")
	r := &Resolver{Dir: dir}
	got, err := r.CanonicalRepo()
	if err != nil {
		t.Fatalf("CanonicalRepo() error: %v", err)
	}
	if want := "github.com/org/repo"; got != want {
		t.Errorf("CanonicalRepo() = %q, want %q (discovery should canonicalize)", got, want)
	}
}

func TestResolver_CanonicalRepo_NoOverrideNoDir(t *testing.T) {
	t.Parallel()
	r := &Resolver{} // no override, no dir
	_, err := r.CanonicalRepo()
	if err == nil {
		t.Fatal("CanonicalRepo() expected error, got nil")
	}
	for _, want := range []string{"--repo", "HORDE_REPO_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err.Error(), want)
		}
	}
}

func TestResolver_CanonicalRepo_NonGitDirNoOverride(t *testing.T) {
	t.Parallel()
	r := &Resolver{Dir: t.TempDir()} // non-git dir, no override
	_, err := r.CanonicalRepo()
	if err == nil {
		t.Fatal("CanonicalRepo() expected error for non-git dir, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error %q should mention 'not a git repository'", err.Error())
	}
}

func TestResolver_ProjectConfig_OverrideDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, ".horde", "config.yaml"), "mounts:\n  - .beads:/workspace/.beads\n")
	r := &Resolver{ConfigOverride: dir}
	cfg, err := r.ProjectConfig()
	if err != nil {
		t.Fatalf("ProjectConfig() error: %v", err)
	}
	if len(cfg.Mounts) != 1 || cfg.Mounts[0] != ".beads:/workspace/.beads" {
		t.Errorf("ProjectConfig() mounts = %v, want one .beads entry", cfg.Mounts)
	}
}

func TestResolver_ProjectConfig_OverrideFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	writeConfig(t, path, "mounts:\n  - .cache:/workspace/.cache\n")
	r := &Resolver{ConfigOverride: path}
	cfg, err := r.ProjectConfig()
	if err != nil {
		t.Fatalf("ProjectConfig() error: %v", err)
	}
	if len(cfg.Mounts) != 1 || cfg.Mounts[0] != ".cache:/workspace/.cache" {
		t.Errorf("ProjectConfig() mounts = %v, want one .cache entry", cfg.Mounts)
	}
}

func TestResolver_ProjectConfig_MissingIsEmpty(t *testing.T) {
	t.Parallel()
	r := &Resolver{Dir: t.TempDir()} // no .horde/config.yaml
	cfg, err := r.ProjectConfig()
	if err != nil {
		t.Fatalf("ProjectConfig() error: %v", err)
	}
	if len(cfg.Mounts) != 0 || len(cfg.Secrets) != 0 {
		t.Errorf("ProjectConfig() = %+v, want empty", cfg)
	}
}

func TestResolver_ProjectConfig_NoDirNoOverride(t *testing.T) {
	t.Parallel()
	r := &Resolver{} // nothing set
	cfg, err := r.ProjectConfig()
	if err != nil {
		t.Fatalf("ProjectConfig() error: %v", err)
	}
	if cfg == nil || len(cfg.Mounts) != 0 {
		t.Errorf("ProjectConfig() = %+v, want non-nil empty", cfg)
	}
}

func TestResolver_EnvFileDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		resolver Resolver
		want     string
	}{
		{"discovery dir", Resolver{Dir: "/work/proj"}, "/work/proj"},
		{"config override dir", Resolver{ConfigOverride: "/cfg/proj", Dir: "/work/proj"}, "/cfg/proj"},
		{"config override yaml file", Resolver{ConfigOverride: "/cfg/proj/custom.yaml", Dir: "/work/proj"}, "/cfg/proj"},
		{"config override yml file", Resolver{ConfigOverride: "/cfg/proj/custom.yml"}, "/cfg/proj"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.resolver.EnvFileDir(); got != tc.want {
				t.Errorf("EnvFileDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

// initGitRepo creates a git repo in a temp dir with the given origin remote
// and returns the dir.
func initGitRepo(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("command %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init")
	run("git", "remote", "add", "origin", remote)
	return dir
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
}
