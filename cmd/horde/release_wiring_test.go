package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowTriggers captures a GitHub Actions workflow's push-tag trigger.
//
// The top-level `on:` key is tricky: in YAML 1.1 a bare `on` is a boolean, but
// yaml.v3 decodes a bare `on` used as a mapping key as the string "on", so the
// struct tag `yaml:"on"` matches and the nested tags list decodes cleanly. This
// lets us assert on the SEMANTIC trigger (the decoded glob list) rather than the
// raw bytes, so a YAML reformat — single quotes, no quotes, or a flow sequence —
// passes while a change to the actual trigger globs fails.
type workflowTriggers struct {
	On struct {
		Push struct {
			Tags     []string `yaml:"tags"`
			Branches []string `yaml:"branches"`
			Paths    []string `yaml:"paths"`
		} `yaml:"push"`
	} `yaml:"on"`
}

func readWorkflowTags(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var w workflowTriggers
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatalf("unmarshalling %s: %v", path, err)
	}
	return w.On.Push.Tags
}

// readWorkflowPush decodes a workflow's full push trigger (tags, branches,
// paths) so tests can assert path-filtered branch triggers, not just tag globs.
func readWorkflowPush(t *testing.T, path string) (tags, branches, paths []string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var w workflowTriggers
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatalf("unmarshalling %s: %v", path, err)
	}
	return w.On.Push.Tags, w.On.Push.Branches, w.On.Push.Paths
}

// Guards the release-tag prefix contract: CDK publishes on cdk-v* tags, the
// CLI releases on plain v* tags. The v* glob anchors at the start and so never
// matches cdk-v*, so a CLI tag won't trigger a CDK publish (and vice versa).
// See docs/superpowers/plans/2026-06-08-release-and-distribution.md.
func TestPublishWorkflowUsesCDKPrefix(t *testing.T) {
	tags := readWorkflowTags(t, "../../.github/workflows/publish.yml")
	if len(tags) != 1 || tags[0] != "cdk-v*" {
		t.Errorf("publish.yml must trigger on exactly [cdk-v*]; got %#v", tags)
	}
	for _, tag := range tags {
		if tag == "v*" {
			t.Errorf("publish.yml still triggers on the shared v* prefix — CLI tags would fire it")
		}
	}
}

// The CLI version is sourced from the CLI_VERSION file (not git tags), and the
// Makefile stamps it as a v-prefixed string so the CLI version stays distinct
// from the cdk-v* prefix. Guards both halves of that contract.
func TestMakefileReadsCLIVersionFile(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("reading Makefile: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "cat CLI_VERSION") {
		t.Error("Makefile VERSION must read the CLI_VERSION file")
	}
	// Whitespace-insensitive: the VERSION assignment must stamp a v-prefixed
	// string built from CLI_VERSION_BASE (keeps the CLI version distinct from
	// the cdk-v* prefix). Matches `VERSION := v$(CLI_VERSION_BASE)...` with any
	// run of spaces around `:=`.
	if !regexp.MustCompile(`VERSION\s*:=\s*v\$\(CLI_VERSION_BASE\)`).MatchString(src) {
		t.Error("Makefile must stamp a v-prefixed version from CLI_VERSION (keeps CLI distinct from cdk-v*)")
	}
}

// The CLI_VERSION file is the source of truth tag-on-cli-bump.yml turns into a
// v<version> tag; it must contain a bare semver with no leading v.
func TestCLIVersionFileIsBareSemver(t *testing.T) {
	b, err := os.ReadFile("../../CLI_VERSION")
	if err != nil {
		t.Fatalf("reading CLI_VERSION: %v", err)
	}
	v := strings.TrimSpace(string(b))
	if strings.HasPrefix(v, "v") {
		t.Errorf("CLI_VERSION must be bare semver with no leading 'v'; got %q", v)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v) {
		t.Errorf("CLI_VERSION must be MAJOR.MINOR.PATCH; got %q", v)
	}
}

// tag-on-cli-bump.yml is the CLI analog of tag-on-bump.yml: it must watch the
// CLI_VERSION file on main so a bump auto-pushes the v<version> release tag.
func TestTagOnCLIBumpWatchesVersionFile(t *testing.T) {
	_, branches, paths := readWorkflowPush(t, "../../.github/workflows/tag-on-cli-bump.yml")
	if len(branches) != 1 || branches[0] != "main" {
		t.Errorf("tag-on-cli-bump.yml must trigger on push to [main]; got branches=%#v", branches)
	}
	foundPath := false
	for _, p := range paths {
		if p == "CLI_VERSION" {
			foundPath = true
		}
	}
	if !foundPath {
		t.Errorf("tag-on-cli-bump.yml must filter on the CLI_VERSION path; got paths=%#v", paths)
	}
}

// Guards that the CLI release workflow triggers on plain v* tags (the prefix
// the CLI now releases under — cdk keeps cdk-v*).
func TestReleaseCLIWorkflowUsesVPrefix(t *testing.T) {
	tags := readWorkflowTags(t, "../../.github/workflows/release-cli.yml")
	if len(tags) != 1 || tags[0] != "v*" {
		t.Errorf("release-cli.yml must trigger on exactly [v*]; got %#v", tags)
	}
}
