package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestNormalizeVersion(t *testing.T) {
	cases := map[string]string{
		"v0.1.0":           "0.1.0",
		"0.1.0":            "0.1.0",
		"cli-v0.1.0":       "0.1.0",
		"dev":              "",
		"v0.3.0-5-gabc123": "", // git-describe dev string: not a clean release version
	}
	for in, want := range cases {
		if got := normalizeVersion(in); got != want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.1.0", "0.1.1", true},
		{"v0.1.0", "v0.2.0", true}, // prefixes normalized
		{"0.2.0", "0.1.9", false},
		{"0.1.0", "0.1.0", false},
		{"1.0.0", "0.9.9", false},
		{"dev", "0.1.0", false}, // dev/unparseable current: never claim newer
		{"0.1.0", "garbage", false},
	}
	for _, c := range cases {
		if got := isNewer(c.current, c.latest); got != c.want {
			t.Errorf("isNewer(%q,%q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestParseLatestTag(t *testing.T) {
	body := []byte(`{"tag_name":"v0.3.1","name":"horde v0.3.1"}`)
	tag, err := parseLatestTag(body)
	if err != nil {
		t.Fatalf("parseLatestTag: %v", err)
	}
	if tag != "v0.3.1" {
		t.Errorf("tag = %q, want v0.3.1", tag)
	}
}

func TestNewerVersionNoteRespectsCIGuard(t *testing.T) {
	t.Setenv("CI", "1")
	if note := newerVersionNote(context.Background()); note != "" {
		t.Errorf("expected no note under CI guard, got %q", note)
	}
}

// TestVersionSubcommandMatchesFlag pins the `version` subcommand's version line
// to exactly what the --version flag would print. urfave renders the flag as
// "horde version " + the Version field (which is versionString()), so the two
// share one source and can never drift.
func TestVersionSubcommandMatchesFlag(t *testing.T) {
	origV, origC, origD := version, commit, buildDate
	t.Cleanup(func() { version, commit, buildDate = origV, origC, origD })
	// Guard the network note off so we only compare the version line.
	t.Setenv("HORDE_NO_UPDATE_CHECK", "1")

	version, commit, buildDate = "v9.9.9", "deadbee", "2026-01-02T03:04:05Z"

	// The flag renders as "horde version " + Version field.
	flagLine := "horde version " + versionString()

	app := newApp()
	var buf bytes.Buffer
	setOutputs(app, &buf)
	if err := app.Run(context.Background(), []string{"horde", "version"}); err != nil {
		t.Fatalf("running version subcommand: %v", err)
	}
	got := strings.TrimRight(buf.String(), "\n")
	if got != flagLine {
		t.Fatalf("version subcommand line mismatch:\n  subcmd: %q\n  flag:   %q", got, flagLine)
	}
}
