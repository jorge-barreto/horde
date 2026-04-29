package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Guards the --version output contract: "horde version <ver> (<commit>, built <date>)".
// Downstream tooling (release notes, support bug reports) parses this — changing the
// format requires updating consumers in lockstep.
func TestVersionFlag(t *testing.T) {
	origV, origC, origD := version, commit, buildDate
	t.Cleanup(func() { version, commit, buildDate = origV, origC, origD })

	version = "v0.2.0"
	commit = "abc1234"
	buildDate = "2026-04-21T12:00:00Z"

	app := newApp()
	var buf bytes.Buffer
	setOutputs(app, &buf)

	if err := app.Run(context.Background(), []string{"horde", "--version"}); err != nil {
		t.Fatalf("running --version: %v", err)
	}

	got := buf.String()
	want := "horde version v0.2.0 (abc1234, built 2026-04-21T12:00:00Z)"
	if !strings.Contains(got, want) {
		t.Fatalf("--version output:\n  got:  %q\n  want contains: %q", got, want)
	}
}

// Defaults are the unset-ldflags fallback used by `go build ./cmd/horde` without
// make. If these regress, dev builds lose version metadata silently.
func TestVersionDefaults(t *testing.T) {
	if version != "dev" || commit != "none" || buildDate != "unknown" {
		t.Fatalf("defaults drifted: version=%q commit=%q buildDate=%q", version, commit, buildDate)
	}
}
