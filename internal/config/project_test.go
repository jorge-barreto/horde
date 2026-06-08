package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveMounts_RelativeAnchorsToDir(t *testing.T) {
	t.Parallel()
	// Relative host paths anchor to the passed dir — this is the contract the
	// launch/retry mount-anchoring (resolver.EnvFileDir) relies on, so a
	// --config-relocated config's relative mounts resolve against the config's
	// dir, not the process cwd.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := &ProjectConfig{Mounts: []string{".beads:/workspace/.beads"}}

	got := cfg.ResolveMounts(dir)
	want := filepath.Join(dir, ".beads") + ":/workspace/.beads"
	if len(got) != 1 || got[0] != want {
		t.Errorf("ResolveMounts(%q) = %v, want [%q]", dir, got, want)
	}
}

func TestResolveMounts_AbsoluteHostPathUnchanged(t *testing.T) {
	t.Parallel()
	// An absolute host path is used verbatim regardless of the anchor dir.
	abs := t.TempDir()
	cfg := &ProjectConfig{Mounts: []string{abs + ":/workspace/data"}}

	got := cfg.ResolveMounts(t.TempDir())
	want := abs + ":/workspace/data"
	if len(got) != 1 || got[0] != want {
		t.Errorf("ResolveMounts = %v, want [%q]", got, want)
	}
}

func TestResolveMounts_SkipsNonexistentAndMalformed(t *testing.T) {
	t.Parallel()
	cfg := &ProjectConfig{Mounts: []string{
		"does-not-exist:/workspace/x", // host path missing → skipped
		"",                            // empty → skipped
		"noseparator",                 // no colon → skipped
		":/workspace/y",               // empty host → skipped
		"data:",                       // empty container → skipped
	}}
	if got := cfg.ResolveMounts(t.TempDir()); len(got) != 0 {
		t.Errorf("ResolveMounts = %v, want empty (all entries invalid/missing)", got)
	}
}

func TestResolveMounts_RelativeDiffersByAnchor(t *testing.T) {
	t.Parallel()
	// The same relative mount resolves to different host paths under different
	// anchors — proving the anchor (cwd vs --config dir) is load-bearing.
	dirA, dirB := t.TempDir(), t.TempDir()
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(filepath.Join(d, "shared"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	cfg := &ProjectConfig{Mounts: []string{"shared:/workspace/shared"}}

	gotA := cfg.ResolveMounts(dirA)
	gotB := cfg.ResolveMounts(dirB)
	if len(gotA) != 1 || len(gotB) != 1 {
		t.Fatalf("expected one mount each, got A=%v B=%v", gotA, gotB)
	}
	if gotA[0] == gotB[0] {
		t.Errorf("anchor not load-bearing: both resolved to %q", gotA[0])
	}
	if !strings.HasPrefix(gotA[0], dirA) || !strings.HasPrefix(gotB[0], dirB) {
		t.Errorf("mounts not anchored to their dirs: A=%q B=%q", gotA[0], gotB[0])
	}
}
