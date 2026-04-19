package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/jorge-barreto/horde/internal/awscfg"
	"github.com/jorge-barreto/horde/internal/bootstrap"
	"github.com/jorge-barreto/horde/internal/config"
	"github.com/urfave/cli/v3"
)

// localWorkerImage is the local Docker image tag that `horde launch` builds
// and that `horde push` tags and pushes to ECR.
const localWorkerImage = "horde-worker:latest"

// dockerRunner is the minimal shell-out interface used by `horde push`. It is
// mocked in tests so pushes can be validated without requiring a real docker
// daemon.
type dockerRunner interface {
	// run executes `docker <args...>` and returns combined stdout/stderr.
	run(ctx context.Context, args ...string) ([]byte, error)
	// runStdin executes `docker <args...>` with stdin fed from the given string.
	// Returns combined stdout/stderr.
	runStdin(ctx context.Context, stdin string, args ...string) ([]byte, error)
}

// execDockerRunner is the default dockerRunner; it shells out to the `docker`
// binary on PATH.
type execDockerRunner struct{}

func (execDockerRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	return cmd.CombinedOutput()
}

func (execDockerRunner) runStdin(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.CombinedOutput()
}

// ecrAuthClient is the subset of the ECR API used by `horde push`.
type ecrAuthClient interface {
	GetAuthorizationToken(ctx context.Context, params *ecr.GetAuthorizationTokenInput, optFns ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
}

// pushDeps collects the injectable dependencies of `horde push` so tests can
// substitute fakes for AWS, SSM, ECR, and docker.
type pushDeps struct {
	awsLoad   func(ctx context.Context, profile string) (aws.Config, error)
	ssmClient func(aws.Config) config.SSMClient
	ecrClient func(aws.Config) ecrAuthClient
	docker    dockerRunner
	slug      func(remoteURL string) (string, error)
}

func defaultPushDeps() pushDeps {
	return pushDeps{
		awsLoad: awscfg.Load,
		ssmClient: func(cfg aws.Config) config.SSMClient {
			return ssm.NewFromConfig(cfg)
		},
		ecrClient: func(cfg aws.Config) ecrAuthClient {
			return ecr.NewFromConfig(cfg)
		},
		docker: execDockerRunner{},
		slug:   bootstrap.Slug,
	}
}

func pushCmd() *cli.Command { return pushCmdWith(defaultPushDeps()) }

func pushCmdWith(deps pushDeps) *cli.Command {
	return &cli.Command{
		Name:  "push",
		Usage: "Tag and push the local worker image to the project's ECR repository",
		Description: `push tags the local horde-worker:latest image with the project's ECR
repository URI (discovered via SSM at /horde/<slug>/config) and pushes
it to ECR. Requires that 'horde bootstrap deploy' has already created
the ECR repository, and that horde-worker:latest has been built locally
(via 'horde launch' or 'make docker-build').

Authenticates to ECR by calling GetAuthorizationToken via the AWS SDK
and piping the decoded password to 'docker login --password-stdin' —
no dependency on the AWS CLI.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runPush(ctx, cmd, deps)
		},
	}
}

func runPush(ctx context.Context, cmd *cli.Command, deps pushDeps) error {
	// 1. Derive project slug from git remote for SSM path.
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	repo, err := config.RepoURL(cwd)
	if err != nil {
		return err
	}
	slug, err := deps.slug(repo)
	if err != nil {
		return err
	}
	ssmPath := fmt.Sprintf("/horde/%s/config", slug)

	// 2. Verify the local worker image exists before touching AWS.
	if _, err := deps.docker.run(ctx, "image", "inspect", localWorkerImage); err != nil {
		return fmt.Errorf("local image %s not found: run 'horde launch' first to build the worker image, or run 'make docker-build'", localWorkerImage)
	}

	// 3. Load AWS config and discover the ECR repo URI from SSM.
	awsCfg, err := deps.awsLoad(ctx, cmd.String("profile"))
	if err != nil {
		return err
	}
	cfg, err := config.LoadFromSSM(ctx, deps.ssmClient(awsCfg), ssmPath)
	if err != nil {
		var notFound *config.NotFoundError
		if errors.As(err, &notFound) {
			return fmt.Errorf("ssm parameter %q not found: run 'horde bootstrap deploy' to create the project's ECR repository and SSM config", ssmPath)
		}
		return fmt.Errorf("reading horde config: %s", config.Diagnostic(err))
	}
	if cfg.EcrRepoURI == "" {
		return fmt.Errorf("ssm parameter %q is missing ecr_repo_uri; re-run 'horde bootstrap deploy' to regenerate the config", ssmPath)
	}

	// 4. Authenticate to ECR via the SDK and `docker login --password-stdin`.
	if err := ecrLogin(ctx, deps.docker, deps.ecrClient(awsCfg)); err != nil {
		return err
	}

	// 5. Tag the local image with the ECR URI and push.
	target := cfg.EcrRepoURI + ":latest"
	if out, err := deps.docker.run(ctx, "tag", localWorkerImage, target); err != nil {
		return fmt.Errorf("docker tag %s %s: %w\n%s", localWorkerImage, target, err, out)
	}
	pushOut, err := deps.docker.run(ctx, "push", target)
	if err != nil {
		return fmt.Errorf("docker push %s: %w\n%s", target, err, pushOut)
	}

	digest := parsePushDigest(pushOut)
	w := cmd.Root().Writer
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintf(w, "Pushed %s\n", target)
	if digest != "" {
		fmt.Fprintf(w, "Digest: %s\n", digest)
	}
	return nil
}

// ecrLogin calls ecr:GetAuthorizationToken and pipes the decoded password
// into `docker login --password-stdin <proxy-endpoint>`. The token is an
// opaque "AWS:<password>" pair; we extract the password and discard it as
// soon as docker consumes it.
func ecrLogin(ctx context.Context, docker dockerRunner, client ecrAuthClient) error {
	out, err := client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return fmt.Errorf("ecr GetAuthorizationToken: %w", err)
	}
	if len(out.AuthorizationData) == 0 {
		return fmt.Errorf("ecr GetAuthorizationToken: empty authorization data")
	}
	auth := out.AuthorizationData[0]
	if auth.AuthorizationToken == nil || auth.ProxyEndpoint == nil {
		return fmt.Errorf("ecr GetAuthorizationToken: missing token or proxy endpoint")
	}
	decoded, err := base64.StdEncoding.DecodeString(*auth.AuthorizationToken)
	if err != nil {
		return fmt.Errorf("decoding ecr authorization token: %w", err)
	}
	user, pass, ok := splitAuth(decoded)
	if !ok {
		return fmt.Errorf("ecr authorization token malformed: expected user:password")
	}
	if loginOut, err := docker.runStdin(ctx, pass, "login", "--username", user, "--password-stdin", *auth.ProxyEndpoint); err != nil {
		return fmt.Errorf("docker login %s: %w\n%s", *auth.ProxyEndpoint, err, loginOut)
	}
	return nil
}

// splitAuth splits a decoded ECR authorization blob ("AWS:<password>") into
// its user and password components. Returns ok=false on malformed input.
func splitAuth(decoded []byte) (user, password string, ok bool) {
	i := bytes.IndexByte(decoded, ':')
	if i < 0 {
		return "", "", false
	}
	return string(decoded[:i]), string(decoded[i+1:]), true
}

// pushDigestRE matches the "digest: sha256:<hex>" portion of a `docker push`
// status line. docker prints lines like:
//
//	latest: digest: sha256:abc123... size: 1234
//
// so we grab the first such hash.
var pushDigestRE = regexp.MustCompile(`sha256:[0-9a-f]{16,64}`)

// parsePushDigest extracts the sha256 digest from `docker push` output.
// Returns "" if no digest is found (e.g. docker push emits nothing useful
// because it was short-circuited).
func parsePushDigest(out []byte) string {
	if m := pushDigestRE.Find(out); m != nil {
		return string(m)
	}
	return ""
}

