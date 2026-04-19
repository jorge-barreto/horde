package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/jorge-barreto/horde/internal/awscfg"
	"github.com/jorge-barreto/horde/internal/bootstrap"
	"github.com/jorge-barreto/horde/internal/config"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// secretReader reads a single secret value. The prompt is a human-readable
// label (e.g. "CLAUDE_CODE_OAUTH_TOKEN") used both when prompting interactively
// and when reporting errors in headless mode.
type secretReader func(prompt string) (string, error)

// defaultSecretReader returns a secretReader that, when stdin is a TTY,
// prompts the user with masked input; otherwise falls back to the named
// environment variable. The secret is never logged or echoed.
func defaultSecretReader(envVar string) secretReader {
	return func(prompt string) (string, error) {
		fd := int(os.Stdin.Fd())
		if term.IsTerminal(fd) {
			fmt.Fprintf(os.Stderr, "%s (input hidden): ", prompt)
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return "", fmt.Errorf("reading %s: %w", prompt, err)
			}
			v := string(b)
			if v == "" {
				return "", fmt.Errorf("reading %s: empty input", prompt)
			}
			return v, nil
		}
		v := os.Getenv(envVar)
		if v == "" {
			return "", fmt.Errorf("stdin is not a TTY and %s is not set", envVar)
		}
		return v, nil
	}
}

func bootstrapDeployCmd() *cli.Command {
	return &cli.Command{
		Name:  "deploy",
		Usage: "Create or update the bootstrap CloudFormation stack",
		Description: `deploy applies .horde/cloudformation.yaml to AWS. If no stack named
horde-<slug> exists, it is created; otherwise it is updated. Prompts for
CLAUDE_CODE_OAUTH_TOKEN and GIT_TOKEN (hidden input) when stdin is a TTY; in
headless / CI contexts reads them from the same-named environment variables.
Polls CloudFormation every 5s and prints each new stack event until the
stack reaches a terminal status.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runBootstrapDeploy(ctx, cmd, defaultSecretReader("CLAUDE_CODE_OAUTH_TOKEN"), defaultSecretReader("GIT_TOKEN"), nil)
		},
	}
}

// cfClientFactory produces a bootstrap.CFClient. Injected for tests.
type cfClientFactory func(ctx context.Context, profile string) (bootstrap.CFClient, error)

func runBootstrapDeploy(ctx context.Context, cmd *cli.Command, readClaude, readGit secretReader, newClient cfClientFactory) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	// 1. Pre-check template file.
	templatePath := filepath.Join(cwd, bootstrapStackFile)
	templateBytes, err := os.ReadFile(templatePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s not found; run 'horde bootstrap init' first", bootstrapStackFile)
		}
		return fmt.Errorf("reading %s: %w", bootstrapStackFile, err)
	}

	// 2. Derive stack name.
	repo, err := config.RepoURL(cwd)
	if err != nil {
		return err
	}
	slug, err := bootstrap.Slug(repo)
	if err != nil {
		return err
	}
	stackName := "horde-" + slug

	// 3. Gather secrets (never echoed).
	claudeToken, err := readClaude("CLAUDE_CODE_OAUTH_TOKEN")
	if err != nil {
		return err
	}
	gitToken, err := readGit("GIT_TOKEN")
	if err != nil {
		return err
	}

	// 4. Load AWS config + CFN client.
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

	// 5-7. Deploy + poll.
	req := bootstrap.DeployRequest{
		StackName:       stackName,
		Slug:            slug,
		TemplateBody:    string(templateBytes),
		ClaudeCodeOauthToken: claudeToken,
		GitToken:        gitToken,
	}
	if err := bootstrap.Deploy(ctx, client, req, cmd.Writer); err != nil {
		return err
	}

	// 8. Next-step guidance.
	fmt.Fprintf(cmd.Writer, "\nStack %s ready.\n", stackName)
	fmt.Fprintf(cmd.Writer, "SSM config: /horde/%s/config\n", slug)
	fmt.Fprintln(cmd.Writer, "Next: horde push")
	return nil
}
