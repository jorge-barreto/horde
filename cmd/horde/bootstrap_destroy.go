package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
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

	// 3. Load AWS config + CFN client.
	if newClient == nil {
		newClient = func(ctx context.Context, profile string) (bootstrap.CFClient, error) {
			awsCfg, err := awscfg.Load(ctx, profile)
			if err != nil {
				return nil, err
			}
			return cloudformation.NewFromConfig(awsCfg), nil
		}
	}
	client, err := newClient(ctx, cmd.String("profile"))
	if err != nil {
		return err
	}

	// 4. Delete stack.
	return bootstrap.Destroy(ctx, client, stackName, 0, cmd.Writer)
}

// refuseIfActiveRuns queries the DynamoDB store for pending or running runs
// and errors if any exist. A store-level error (e.g. table missing, auth
// problem) is treated as "nothing to check" — warn and proceed, since the
// stack may already be partially torn down.
func refuseIfActiveRuns(ctx context.Context, cmd *cli.Command) error {
	_, st, _, _, cleanup, err := initProviderAndStoreWith(ctx, "aws-ecs", cmd.String("profile"), defaultFactoryDeps())
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
