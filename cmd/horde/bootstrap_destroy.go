package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	smithy "github.com/aws/smithy-go"
	"github.com/jorge-barreto/horde/internal/awscfg"
	"github.com/jorge-barreto/horde/internal/bootstrap"
	"github.com/jorge-barreto/horde/internal/config"
	"github.com/urfave/cli/v3"
)

func bootstrapDestroyCmd() *cli.Command {
	return &cli.Command{
		Name:  "destroy",
		Usage: "Delete the bootstrap CloudFormation stack",
		Description: `destroy deletes the horde-<slug> CloudFormation stack and all resources
it created (VPC, ECS cluster, DynamoDB, S3, ECR, Secrets Manager, IAM,
Lambda, SSM parameter, etc.).

Refuses if there are pending or running runs recorded in DynamoDB.
Requires the user to type the stack name to confirm.

Use --force to skip the interactive confirmation (for CI/scripts).`,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "force",
				Usage: "Skip the confirmation prompt",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runBootstrapDestroy(ctx, cmd, bufio.NewReader(os.Stdin), nil)
		},
	}
}

// confirmReader reads a single line of user input.
type confirmReader interface {
	ReadString(delim byte) (string, error)
}

func runBootstrapDestroy(ctx context.Context, cmd *cli.Command, stdin confirmReader, newClient cfClientFactory) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	repo, err := config.RepoURL(cwd)
	if err != nil {
		return err
	}
	slug, err := bootstrap.Slug(repo)
	if err != nil {
		return err
	}
	stackName := "horde-" + slug

	// 1. Check for active runs. Best-effort: if we can't reach the store,
	//    warn and proceed (the stack may already be partially torn down).
	if err := refuseIfActiveRuns(ctx, cmd); err != nil {
		return err
	}

	// 2. Confirmation prompt (before loading AWS config, so a mistyped
	//    confirmation doesn't waste an STS round-trip).
	if !cmd.Bool("force") {
		fmt.Fprintf(cmd.Writer, "This will destroy all horde infrastructure for %s.\n", slug)
		fmt.Fprintf(cmd.Writer, "Type the stack name (%s) to confirm: ", stackName)
		line, err := stdin.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading confirmation: %w", err)
		}
		typed := strings.TrimSpace(line)
		if typed != stackName {
			return fmt.Errorf("confirmation did not match stack name; aborting")
		}
	}

	// 3. Load AWS config.
	awsCfg, err := awscfg.Load(ctx, cmd.String("profile"))
	if err != nil {
		return err
	}

	// 4. Empty the artifacts S3 bucket. CloudFormation can't delete an
	//    S3 bucket that still has objects in it, and tests / real runs
	//    populate it with artifacts. Best-effort: if we can't read SSM
	//    config (stack already partially gone) or the bucket doesn't
	//    exist, warn and proceed — DeleteStack will succeed anyway for
	//    a missing bucket and surface a clear error for a non-empty one.
	emptyArtifactsBucket(ctx, cmd, awsCfg, slug)

	// 5. CFN client + delete.
	if newClient == nil {
		newClient = func(_ context.Context, _ string) (bootstrap.CFClient, error) {
			return cloudformation.NewFromConfig(awsCfg), nil
		}
	}
	client, err := newClient(ctx, cmd.String("profile"))
	if err != nil {
		return err
	}
	return bootstrap.Destroy(ctx, client, stackName, 0, cmd.Writer)
}

// emptyArtifactsBucket deletes every object in the stack's artifacts bucket
// so the subsequent CloudFormation DeleteStack doesn't fail on a non-empty
// S3 bucket. All errors are warnings, not fatal: if SSM is unreachable
// (stack already partially destroyed) or the bucket is already gone, let
// DeleteStack drive the final outcome.
func emptyArtifactsBucket(ctx context.Context, cmd *cli.Command, awsCfg aws.Config, slug string) {
	ssmClient := ssm.NewFromConfig(awsCfg)
	hordeCfg, err := config.LoadFromSSM(ctx, ssmClient, "/horde/"+slug+"/config")
	if err != nil {
		fmt.Fprintf(cmd.Writer, "warning: could not read stack SSM config to find artifacts bucket (%v); proceeding\n", err)
		return
	}
	bucket := hordeCfg.ArtifactsBucket
	if bucket == "" {
		return
	}
	s3Client := s3.NewFromConfig(awsCfg)

	// Paginate list+delete in batches of 1000.
	var continuationToken *string
	total := 0
	for {
		out, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket" {
				return
			}
			fmt.Fprintf(cmd.Writer, "warning: listing objects in %s failed (%v); proceeding\n", bucket, err)
			return
		}
		if len(out.Contents) == 0 {
			break
		}
		ids := make([]s3types.ObjectIdentifier, 0, len(out.Contents))
		for _, obj := range out.Contents {
			ids = append(ids, s3types.ObjectIdentifier{Key: obj.Key})
		}
		if _, err := s3Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		}); err != nil {
			fmt.Fprintf(cmd.Writer, "warning: deleting objects in %s failed (%v); proceeding\n", bucket, err)
			return
		}
		total += len(ids)
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		continuationToken = out.NextContinuationToken
	}
	if total > 0 {
		fmt.Fprintf(cmd.Writer, "Emptied %d object(s) from artifacts bucket %s.\n", total, bucket)
	}
}

// refuseIfActiveRuns queries the DynamoDB store for pending or running runs
// and errors if any exist. A store-level error (e.g. table missing, auth
// problem) is treated as "nothing to check" — warn and proceed, since the
// stack may already be partially torn down.
func refuseIfActiveRuns(ctx context.Context, cmd *cli.Command) error {
	_, st, _, _, _, cleanup, err := initProviderAndStoreWith(ctx, "aws-ecs", cmd.String("profile"), defaultFactoryDeps())
	if err != nil {
		fmt.Fprintf(cmd.Writer, "warning: could not reach DynamoDB store to check for active runs (%v); proceeding with destroy\n", err)
		return nil
	}
	defer cleanup()

	active, err := st.ListActive(ctx)
	if err != nil {
		fmt.Fprintf(cmd.Writer, "warning: could not list active runs (%v); proceeding with destroy\n", err)
		return nil
	}
	if len(active) > 0 {
		return fmt.Errorf("%d active run(s) in DynamoDB; kill them before destroying the stack", len(active))
	}
	return nil
}
