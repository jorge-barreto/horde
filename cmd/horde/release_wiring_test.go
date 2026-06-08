package main

import (
	"os"
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
			Tags []string `yaml:"tags"`
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

func TestMakefileDescribesCLIPrefix(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("reading Makefile: %v", err)
	}
	if !strings.Contains(string(b), "--match 'v*'") {
		t.Error("Makefile VERSION must describe against v* tags only")
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
