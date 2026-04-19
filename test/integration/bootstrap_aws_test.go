package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudformation"

	"github.com/jorge-barreto/horde/internal/awscfg"
	"github.com/jorge-barreto/horde/internal/bootstrap"
)

// TestBootstrap_ValidateTemplate calls CloudFormation's ValidateTemplate API
// against the rendered stack template. This is a read-only check that costs
// nothing: ValidateTemplate parses the template and evaluates its structure
// server-side without creating any resources. It catches issues cfn-lint
// doesn't (e.g. CF resource-property validation, parameter shape, intrinsic
// function argument types).
//
// Gated by HORDE_E2E_AWS=1 and skipped under -short because it requires real
// AWS credentials. Uses AWS_PROFILE from the environment (set in .env) and
// no account charges apply.
func TestBootstrap_ValidateTemplate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AWS-backed test in -short mode")
	}
	if os.Getenv("HORDE_E2E_AWS") != "1" {
		t.Skip("set HORDE_E2E_AWS=1 to run (requires AWS credentials; free API call)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	profile := os.Getenv("AWS_PROFILE")
	cfg, err := awscfg.Load(ctx, profile)
	if err != nil {
		t.Fatalf("loading AWS config (profile=%q): %v", profile, err)
	}

	rendered, err := bootstrap.Render("hordetest")
	if err != nil {
		t.Fatalf("rendering template: %v", err)
	}

	client := cloudformation.NewFromConfig(cfg)
	body := string(rendered)
	_, err = client.ValidateTemplate(ctx, &cloudformation.ValidateTemplateInput{
		TemplateBody: &body,
	})
	if err != nil {
		t.Fatalf("ValidateTemplate rejected the rendered template: %v\n\n--- rendered ---\n%s", err, body)
	}
}
