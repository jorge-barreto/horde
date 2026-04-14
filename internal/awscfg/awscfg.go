package awscfg

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Load returns an AWS SDK config using the default credential chain.
// If profile is non-empty, the named profile is selected via SharedConfigProfile.
func Load(ctx context.Context, profile string) (aws.Config, error) {
	var opts []func(*config.LoadOptions) error
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("loading AWS config: %w\nhint: %s", err, DiagnoseError(err, profile))
	}
	return cfg, nil
}

// CallerIdentity calls sts:GetCallerIdentity and returns the caller's ARN.
func CallerIdentity(ctx context.Context, cfg aws.Config, profile string) (string, error) {
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("getting caller identity: %w\nhint: %s", err, DiagnoseError(err, profile))
	}
	if out.Arn == nil {
		return "", fmt.Errorf("getting caller identity: ARN not present in response")
	}
	return *out.Arn, nil
}
