package main

import (
	"os"
	"strings"
	"testing"
)

// Guards the release-tag prefix contract: CDK publishes on cdk-v* tags, the
// CLI releases on cli-v* tags. If these drift back to a shared v* prefix, a
// CLI tag would wrongly trigger a CDK publish (and vice versa). See
// docs/superpowers/plans/2026-06-08-release-and-distribution.md.
func TestPublishWorkflowUsesCDKPrefix(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/publish.yml")
	if err != nil {
		t.Fatalf("reading publish.yml: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"cdk-v*"`) {
		t.Errorf("publish.yml must trigger on cdk-v* tags; got:\n%s", s)
	}
	if strings.Contains(s, `- "v*"`) {
		t.Errorf("publish.yml still triggers on the shared v* prefix — CLI tags would fire it")
	}
}

func TestMakefileDescribesCLIPrefix(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("reading Makefile: %v", err)
	}
	if !strings.Contains(string(b), "--match 'cli-v*'") {
		t.Error("Makefile VERSION must describe against cli-v* tags only")
	}
}
