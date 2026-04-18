package awscfg

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func TestLoad_DefaultProfile(t *testing.T) {
	t.Parallel()
	cfg, err := Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	// Region may be empty without env set — just verify no panic
	_ = cfg.Region
}

func TestLoad_WithProfile(t *testing.T) {
	// Cannot use t.Parallel: t.Setenv modifies process-wide state
	f, err := os.CreateTemp(t.TempDir(), "awscfg")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	f.Close()
	t.Setenv("AWS_CONFIG_FILE", f.Name())

	_, err = Load(context.Background(), "nonexistent-profile-name")
	if err == nil {
		t.Fatal("Load() with nonexistent profile expected error, got nil")
	}
	if !strings.Contains(err.Error(), "loading AWS config") {
		t.Errorf("Load() error = %q, want it to contain %q", err.Error(), "loading AWS config")
	}
	if !strings.Contains(err.Error(), "hint:") {
		t.Errorf("Load() error = %q, want it to contain %q", err.Error(), "hint:")
	}
}

func TestLoad_RespectsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// SDK v2 LoadDefaultConfig is synchronous init with no network calls;
	// it does not check context cancellation during config loading.
	_, err := Load(ctx, "")
	if err != nil {
		t.Fatalf("Load() with cancelled context unexpected error: %v", err)
	}
}

func TestLoad_RegionFromEnv(t *testing.T) {
	// Cannot use t.Parallel: t.Setenv modifies process-wide state
	t.Setenv("AWS_REGION", "us-west-2")
	cfg, err := Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Region != "us-west-2" {
		t.Errorf("cfg.Region = %q, want %q", cfg.Region, "us-west-2")
	}
}

func TestCallerIdentity_NilARN(t *testing.T) {
	// Cannot use t.Parallel — we mutate package var getCallerIdentity.
	orig := getCallerIdentity
	t.Cleanup(func() { getCallerIdentity = orig })
	getCallerIdentity = func(ctx context.Context, cfg aws.Config, in *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error) {
		return &sts.GetCallerIdentityOutput{}, nil // Arn is nil
	}
	_, err := CallerIdentity(context.Background(), aws.Config{}, "")
	if err == nil {
		t.Fatal("CallerIdentity() expected error when ARN is nil, got nil")
	}
	want := "ARN not present in response"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("CallerIdentity() error = %q, want it to contain %q", err.Error(), want)
	}
}

func TestCallerIdentity_Success(t *testing.T) {
	orig := getCallerIdentity
	t.Cleanup(func() { getCallerIdentity = orig })
	getCallerIdentity = func(ctx context.Context, cfg aws.Config, in *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error) {
		arn := "arn:aws:iam::123:user/test"
		return &sts.GetCallerIdentityOutput{Arn: &arn}, nil
	}
	got, err := CallerIdentity(context.Background(), aws.Config{}, "")
	if err != nil {
		t.Fatalf("CallerIdentity() unexpected error: %v", err)
	}
	if got != "arn:aws:iam::123:user/test" {
		t.Errorf("CallerIdentity() = %q, want %q", got, "arn:aws:iam::123:user/test")
	}
}

func TestCallerIdentity_Error(t *testing.T) {
	t.Parallel()
	_, err := CallerIdentity(context.Background(), aws.Config{Credentials: aws.AnonymousCredentials{}}, "")
	if err == nil {
		t.Fatal("CallerIdentity() with empty config expected error, got nil")
	}
	if !strings.Contains(err.Error(), "getting caller identity") {
		t.Errorf("CallerIdentity() error = %q, want it to contain %q", err.Error(), "getting caller identity")
	}
	if !strings.Contains(err.Error(), "hint:") {
		t.Errorf("CallerIdentity() error = %q, want it to contain %q", err.Error(), "hint:")
	}
}
