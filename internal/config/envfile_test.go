package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateEnvFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
	}{
		{"both keys present", "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-xxx\nGIT_TOKEN=ghp_xxx\n"},
		{"empty values", "CLAUDE_CODE_OAUTH_TOKEN=\nGIT_TOKEN=\n"},
		{"extra keys ignored", "CLAUDE_CODE_OAUTH_TOKEN=sk\nGIT_TOKEN=ghp\nEXTRA_KEY=foo\n"},
		{"comments and blank lines", "# comment\n\nCLAUDE_CODE_OAUTH_TOKEN=sk\n\nGIT_TOKEN=ghp\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
	cases := []struct {
		name    string
		write   bool
		content string
		wantErr string
	}{
		{"missing file", false, "", "opening .env file"},
		{"missing CLAUDE_CODE_OAUTH_TOKEN", true, "GIT_TOKEN=ghp_xxx\n", "missing required key(s): CLAUDE_CODE_OAUTH_TOKEN"},
		{"missing GIT_TOKEN", true, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-xxx\n", "missing required key(s): GIT_TOKEN"},
		{"empty file", true, "", "missing required key(s): CLAUDE_CODE_OAUTH_TOKEN, GIT_TOKEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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

func TestValidateEnvFileFor_SpecDriven(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := "CLAUDE_CODE_OAUTH_TOKEN=sk\nGIT_TOKEN=ghp\nREVIEW_GIT_TOKEN=rev\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	spec := MergeSecrets(SecretSpec{
		"REVIEW_GIT_TOKEN": {Env: "REVIEW_GIT_TOKEN", AWSSecret: "horde/review"},
	})
	if _, err := ValidateEnvFileFor(dir, spec); err != nil {
		t.Fatalf("ValidateEnvFileFor() error: %v", err)
	}
}

func TestValidateEnvFileFor_NamesUnknownKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := "CLAUDE_CODE_OAUTH_TOKEN=sk\nGIT_TOKEN=ghp\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	spec := MergeSecrets(SecretSpec{
		"STRIPE_API_KEY": {Env: "STRIPE_API_KEY", AWSSecret: "p/stripe"},
	})
	_, err := ValidateEnvFileFor(dir, spec)
	if err == nil {
		t.Fatal("expected error for missing STRIPE_API_KEY")
	}
	if !strings.Contains(err.Error(), "STRIPE_API_KEY") {
		t.Errorf("error should name the missing key: %v", err)
	}
}

func TestValidateEnvFile_PreservesNotExistError(t *testing.T) {
	t.Parallel()
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
