package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jorge-barreto/horde/internal/bootstrap"
	"github.com/jorge-barreto/horde/internal/config"
	"github.com/urfave/cli/v3"
)

// bootstrapStackFile is the path (relative to the project root) where the
// rendered CloudFormation template is written by `horde bootstrap init`.
const bootstrapStackFile = ".horde/cloudformation.yaml"

func bootstrapCmd() *cli.Command {
	return &cli.Command{
		Name:  "bootstrap",
		Usage: "Provision AWS infrastructure for running horde on ECS",
		Description: `bootstrap generates, deploys, and destroys a self-contained CloudFormation
stack that provisions the AWS resources horde needs to run workflows on
ECS Fargate: VPC, ECS cluster, DynamoDB, S3, ECR, Secrets Manager, and
an EventBridge-triggered Lambda that keeps run status in sync.

Run subcommands in order:

  horde bootstrap init       # generates .horde/cloudformation.yaml from the git remote
  horde bootstrap deploy     # creates/updates the CloudFormation stack (~15 min first time)
  horde push                 # pushes the worker image to the ECR repo
  horde launch --provider aws-ecs ...

'deploy' prompts for CLAUDE_CODE_OAUTH_TOKEN and GIT_TOKEN with hidden input; in
headless contexts (CI) it reads the same-named environment variables. It
polls CloudFormation, streaming each new stack event until CREATE_COMPLETE
or UPDATE_COMPLETE. 'destroy' tears everything down once no active runs
remain.`,
		Commands: []*cli.Command{
			bootstrapInitCmd(),
			bootstrapDeployCmd(),
			bootstrapDestroyCmd(),
		},
	}
}

func bootstrapInitCmd() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "Generate .horde/cloudformation.yaml from the project's git remote",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "regenerate",
				Usage: "Overwrite an existing .horde/cloudformation.yaml",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
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

			dest := filepath.Join(cwd, bootstrapStackFile)
			if !cmd.Bool("regenerate") {
				if _, err := os.Stat(dest); err == nil {
					return fmt.Errorf("%s already exists; pass --regenerate to overwrite", bootstrapStackFile)
				} else if !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("stat %s: %w", dest, err)
				}
			}

			rendered, err := bootstrap.Render(slug)
			if err != nil {
				return err
			}

			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("creating .horde dir: %w", err)
			}
			if err := os.WriteFile(dest, rendered, 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", bootstrapStackFile, err)
			}

			fmt.Fprintf(cmd.Writer, "wrote %s (slug: %s)\n", bootstrapStackFile, slug)
			fmt.Fprintln(cmd.Writer)
			fmt.Fprintln(cmd.Writer, "The template will create, under CloudFormation stack horde-"+slug+":")
			fmt.Fprintln(cmd.Writer, "  - VPC with 2 public + 2 private subnets, IGW, NAT gateway")
			fmt.Fprintln(cmd.Writer, "  - ECS Fargate cluster + task definition")
			fmt.Fprintln(cmd.Writer, "  - ECR repository for the worker image")
			fmt.Fprintln(cmd.Writer, "  - DynamoDB table (horde-runs-"+slug+") with 4 GSIs")
			fmt.Fprintln(cmd.Writer, "  - S3 artifacts bucket (horde-artifacts-"+slug+"-<account>)")
			fmt.Fprintln(cmd.Writer, "  - Secrets Manager secrets for CLAUDE_CODE_OAUTH_TOKEN and GIT_TOKEN")
			fmt.Fprintln(cmd.Writer, "  - IAM task, execution, and CLI-user roles/policies")
			fmt.Fprintln(cmd.Writer, "  - EventBridge rule + inline Lambda to sync run status")
			fmt.Fprintln(cmd.Writer, "  - CloudWatch log group and SSM config parameter")
			fmt.Fprintln(cmd.Writer)
			fmt.Fprintln(cmd.Writer, "Next: horde bootstrap deploy")
			_ = ctx
			return nil
		},
	}
}
