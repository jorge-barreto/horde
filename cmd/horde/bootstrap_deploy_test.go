package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// runBootstrapDeploy runs the CLI's `horde bootstrap deploy` command in dir.
func runBootstrapDeployCLI(t *testing.T, dir string) (string, error) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	var buf bytes.Buffer
	cmd := newApp()
	cmd.Writer = &buf
	cmd.ErrWriter = &buf
	err = cmd.Run(context.Background(), []string{"horde", "bootstrap", "deploy"})
	return buf.String(), err
}

func TestBootstrapDeploy_MissingTemplate(t *testing.T) {
	dir := t.TempDir()
	setupGitRepo(t, dir, "https://github.com/acme/widgets.git")

	_, err := runBootstrapDeployCLI(t, dir)
	if err == nil {
		t.Fatal("expected error when .horde/cloudformation.yaml is missing")
	}
	if !strings.Contains(err.Error(), "run 'horde bootstrap init' first") {
		t.Errorf("error %q missing guidance substring %q", err.Error(), "run 'horde bootstrap init' first")
	}
}
