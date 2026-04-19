package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jorge-barreto/horde/internal/bootstrap"
	"github.com/urfave/cli/v3"
)

// TestRunBootstrapDestroy_ConfirmationMismatch drives runBootstrapDestroy with
// a stub confirmReader that returns the wrong string; the function should
// abort before calling any CF client method.
func TestRunBootstrapDestroy_ConfirmationMismatch(t *testing.T) {
	// Build a *cli.Command matching what the urfave runtime would pass, with
	// just enough state (no flags set => --force is false, no --profile).
	cmd := &cli.Command{Name: "destroy"}
	var buf bytes.Buffer
	cmd.Writer = &buf

	// A stub that should never be invoked because confirmation fails first.
	// (initProviderAndStoreWith will fail because there's no valid AWS setup,
	// but refuseIfActiveRuns is tolerant of that: it warns and returns nil.)
	neverCalled := func(ctx context.Context, profile string) (bootstrap.CFClient, error) {
		t.Fatal("CFClient factory should not be called when confirmation fails")
		return nil, nil
	}

	// Set cwd to a temp dir with a git remote so slug derivation succeeds.
	dir := t.TempDir()
	setupGitRepo(t, dir, "https://github.com/acme/widgets.git")
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	stdin := strings.NewReader("wrong-name\n")
	err = runBootstrapDestroy(context.Background(), cmd, stringsReader{stdin}, neverCalled)
	if err == nil {
		t.Fatal("expected error on confirmation mismatch; got nil")
	}
	if !strings.Contains(err.Error(), "confirmation did not match") {
		t.Errorf("error %q should mention confirmation mismatch", err.Error())
	}
}

// stringsReader adapts *strings.Reader to the confirmReader interface
// (ReadString). bufio.Reader implements it; strings.Reader does not.
type stringsReader struct {
	*strings.Reader
}

func (s stringsReader) ReadString(delim byte) (string, error) {
	var out []byte
	for {
		b, err := s.ReadByte()
		if err != nil {
			return string(out), err
		}
		out = append(out, b)
		if b == delim {
			return string(out), nil
		}
	}
}
