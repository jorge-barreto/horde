package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultSecrets_HasCanonicals(t *testing.T) {
	d := DefaultSecrets()
	if _, ok := d[SecretClaudeCodeOauthToken]; !ok {
		t.Errorf("DefaultSecrets missing %s", SecretClaudeCodeOauthToken)
	}
	if _, ok := d[SecretGitToken]; !ok {
		t.Errorf("DefaultSecrets missing %s", SecretGitToken)
	}
	if d[SecretGitToken].Env != SecretGitToken {
		t.Errorf("default GIT_TOKEN env source = %q, want %q", d[SecretGitToken].Env, SecretGitToken)
	}
	if d[SecretGitToken].AWSSecret == "" {
		t.Errorf("default GIT_TOKEN aws-secret source is empty")
	}
}

func TestMergeSecrets_OverrideReplacesEntireEntry(t *testing.T) {
	overrides := SecretSpec{
		SecretGitToken: {Env: "MY_GIT_TOKEN"}, // no aws-secret
		"REVIEW_GIT_TOKEN": {
			Env:       "REVIEW_GIT_TOKEN",
			AWSSecret: "horde/review-git-token",
		},
	}
	merged := MergeSecrets(overrides)

	got := merged[SecretGitToken]
	if got.Env != "MY_GIT_TOKEN" {
		t.Errorf("override GIT_TOKEN env = %q, want %q", got.Env, "MY_GIT_TOKEN")
	}
	if got.AWSSecret != "" {
		t.Errorf("override GIT_TOKEN aws-secret should be empty, got %q", got.AWSSecret)
	}
	if _, ok := merged["REVIEW_GIT_TOKEN"]; !ok {
		t.Errorf("merged spec missing REVIEW_GIT_TOKEN")
	}
	// Canonical not overridden stays at default.
	if merged[SecretClaudeCodeOauthToken].Env != SecretClaudeCodeOauthToken {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN was unexpectedly altered")
	}
}

func TestMergeSecrets_NoOverridesKeepsDefaults(t *testing.T) {
	merged := MergeSecrets(nil)
	if len(merged) != 2 {
		t.Errorf("expected 2 default entries, got %d", len(merged))
	}
}

func TestValidateForProvider_DockerRequiresEnv(t *testing.T) {
	spec := SecretSpec{
		"FOO": {AWSSecret: "horde/foo"},
	}
	err := spec.ValidateForProvider(ProviderDocker)
	if err == nil {
		t.Fatal("expected error for missing docker source")
	}
	if !strings.Contains(err.Error(), "FOO") || !strings.Contains(err.Error(), "docker") {
		t.Errorf("error should name the unsourced secret and provider, got: %v", err)
	}
}

func TestValidateForProvider_ECSRequiresAWSSecret(t *testing.T) {
	spec := SecretSpec{
		"BAR": {Env: "BAR"},
	}
	err := spec.ValidateForProvider(ProviderECS)
	if err == nil {
		t.Fatal("expected error for missing aws-secret source")
	}
	if !strings.Contains(err.Error(), "BAR") || !strings.Contains(err.Error(), "aws-ecs") {
		t.Errorf("error should name the unsourced secret and provider, got: %v", err)
	}
}

func TestValidateForProvider_HappyPath(t *testing.T) {
	spec := MergeSecrets(SecretSpec{
		"EXTRA": {Env: "EXTRA", AWSSecret: "horde/extra"},
	})
	if err := spec.ValidateForProvider(ProviderDocker); err != nil {
		t.Errorf("docker validation: %v", err)
	}
	if err := spec.ValidateForProvider(ProviderECS); err != nil {
		t.Errorf("ecs validation: %v", err)
	}
}

func TestValidateForProvider_MultipleMissingListed(t *testing.T) {
	spec := SecretSpec{
		"A": {AWSSecret: "x"},
		"B": {AWSSecret: "y"},
	}
	err := spec.ValidateForProvider(ProviderDocker)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "A") || !strings.Contains(msg, "B") {
		t.Errorf("error should list both missing secrets, got: %v", err)
	}
}

func TestEnvKeys_Sorted(t *testing.T) {
	spec := SecretSpec{
		"Z": {Env: "ZED"},
		"A": {Env: "ALPHA"},
		"M": {AWSSecret: "only-aws"},
	}
	keys := spec.EnvKeys()
	want := []string{"ALPHA", "ZED"}
	if len(keys) != len(want) {
		t.Fatalf("EnvKeys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("EnvKeys[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

func TestExtraAWSSecretNames_ExcludesCanonicals(t *testing.T) {
	spec := MergeSecrets(SecretSpec{
		"STRIPE_KEY": {AWSSecret: "prepdesk/stripe"},
	})
	extras := spec.ExtraAWSSecretNames()
	if len(extras) != 1 {
		t.Fatalf("expected 1 extra, got %d: %+v", len(extras), extras)
	}
	if extras[0].EnvName != "STRIPE_KEY" || extras[0].SecretName != "prepdesk/stripe" {
		t.Errorf("unexpected extra: %+v", extras[0])
	}
}

func TestSecretSpec_YAMLRoundtrip(t *testing.T) {
	src := []byte(`
GIT_TOKEN:
  env: GIT_TOKEN
  aws-secret: horde/git-token
REVIEW_GIT_TOKEN:
  env: REVIEW_GIT_TOKEN
  aws-secret: horde/review-git-token
`)
	var spec SecretSpec
	if err := yaml.Unmarshal(src, &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := spec["GIT_TOKEN"].Env; got != "GIT_TOKEN" {
		t.Errorf("GIT_TOKEN.env = %q", got)
	}
	if got := spec["REVIEW_GIT_TOKEN"].AWSSecret; got != "horde/review-git-token" {
		t.Errorf("REVIEW_GIT_TOKEN.aws-secret = %q", got)
	}
}
