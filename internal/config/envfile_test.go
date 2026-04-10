package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateEnvFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"both keys present", "ANTHROPIC_API_KEY=sk-ant-xxx\nGIT_TOKEN=ghp_xxx\n"},
		{"empty values", "ANTHROPIC_API_KEY=\nGIT_TOKEN=\n"},
		{"extra keys ignored", "ANTHROPIC_API_KEY=sk\nGIT_TOKEN=ghp\nEXTRA_KEY=foo\n"},
		{"comments and blank lines", "# comment\n\nANTHROPIC_API_KEY=sk\n\nGIT_TOKEN=ghp\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(tc.content), 0644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			got, err := ValidateEnvFile(dir)
			if err != nil {
				t.Fatalf("ValidateEnvFile() error: %v", err)
			}
			want := filepath.Join(dir, ".env")
			if got != want {
				t.Errorf("ValidateEnvFile() = %q, want %q", got, want)
			}
		})
	}
}

func TestValidateEnvFile_Errors(t *testing.T) {
	cases := []struct {
		name    string
		write   bool
		content string
		wantErr string
	}{
		{"missing file", false, "", "opening .env file"},
		{"missing ANTHROPIC_API_KEY", true, "GIT_TOKEN=ghp_xxx\n", "validating .env file: missing required key ANTHROPIC_API_KEY"},
		{"missing GIT_TOKEN", true, "ANTHROPIC_API_KEY=sk-ant-xxx\n", "validating .env file: missing required key GIT_TOKEN"},
		{"empty file", true, "", "validating .env file: missing required key ANTHROPIC_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.write {
				if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(tc.content), 0644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}
			_, err := ValidateEnvFile(dir)
			if err == nil {
				t.Fatalf("ValidateEnvFile() expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ValidateEnvFile() error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateEnvFile_PreservesNotExistError(t *testing.T) {
	dir := t.TempDir()
	// dir exists but contains no .env file
	_, err := ValidateEnvFile(dir)
	if err == nil {
		t.Fatal("ValidateEnvFile() expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error chain to contain os.ErrNotExist, got: %v", err)
	}
	if !strings.Contains(err.Error(), "opening .env file") {
		t.Errorf("expected error message to contain wrapper context, got: %v", err)
	}
}
