package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/jorge-barreto/horde/internal/config"
	"github.com/urfave/cli/v3"
)

// dockerCall records a single invocation of the fake docker runner.
type dockerCall struct {
	args  []string
	stdin string
}

// fakeDocker is a programmable dockerRunner used by push tests. Callers set
// `respond` to return per-invocation output/error; if unset, every call
// succeeds with nil output.
type fakeDocker struct {
	calls   []dockerCall
	respond func(call dockerCall) ([]byte, error)
}

func (f *fakeDocker) run(_ context.Context, args ...string) ([]byte, error) {
	call := dockerCall{args: append([]string(nil), args...)}
	f.calls = append(f.calls, call)
	if f.respond != nil {
		return f.respond(call)
	}
	return nil, nil
}

func (f *fakeDocker) runStdin(_ context.Context, stdin string, args ...string) ([]byte, error) {
	call := dockerCall{args: append([]string(nil), args...), stdin: stdin}
	f.calls = append(f.calls, call)
	if f.respond != nil {
		return f.respond(call)
	}
	return nil, nil
}

// fakePushSSM returns a canned HordeConfig payload or a canned error.
type fakePushSSM struct {
	cfg *config.HordeConfig
	err error
}

func (f *fakePushSSM) GetParameter(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	data, err := json.Marshal(f.cfg)
	if err != nil {
		return nil, err
	}
	val := string(data)
	return &ssm.GetParameterOutput{
		Parameter: &ssmtypes.Parameter{Value: aws.String(val)},
	}, nil
}

// fakeECR returns a canned GetAuthorizationToken response.
type fakeECR struct {
	token    string // decoded form "AWS:password"
	endpoint string
	err      error
}

func (f *fakeECR) GetAuthorizationToken(_ context.Context, _ *ecr.GetAuthorizationTokenInput, _ ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(f.token))
	return &ecr.GetAuthorizationTokenOutput{
		AuthorizationData: []ecrtypes.AuthorizationData{{
			AuthorizationToken: aws.String(encoded),
			ProxyEndpoint:      aws.String(f.endpoint),
		}},
	}, nil
}

// healthyHordeConfig returns a HordeConfig suitable for push tests.
func healthyHordeConfig() *config.HordeConfig {
	return &config.HordeConfig{
		ClusterARN:            "arn:aws:ecs:us-east-1:123456789012:cluster/horde",
		TaskDefinitionARN:     "arn:aws:ecs:us-east-1:123456789012:task-definition/horde-worker:1",
		Subnets:               []string{"subnet-abc"},
		SecurityGroup:         "sg-123",
		LogGroup:              "/ecs/horde-worker",
		LogStreamPrefix:       "ecs",
		ArtifactsBucket:       "bucket",
		RunsTable:             "horde-runs",
		EcrRepoURI:            "123456789012.dkr.ecr.us-east-1.amazonaws.com/horde-acme-widgets",
		MaxConcurrent:         1,
		DefaultTimeoutMinutes: 1440,
	}
}

// runPushInDir sets cwd to dir, runs `horde push` with the injected deps,
// and returns combined stdout/stderr + error.
func runPushInDir(t *testing.T, dir string, deps pushDeps) (string, error) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	var buf bytes.Buffer
	app := &cli.Command{
		Name: "horde",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "profile"},
		},
		Commands: []*cli.Command{pushCmdWith(deps)},
	}
	app.Writer = &buf
	app.ErrWriter = &buf
	err = app.Run(context.Background(), []string{"horde", "push"})
	return buf.String(), err
}

// TestPush_MissingLocalImage verifies push aborts before tagging/pushing if
// `docker image inspect horde-worker:latest` returns non-zero.
func TestPush_MissingLocalImage(t *testing.T) {
	dir := t.TempDir()
	setupGitRepo(t, dir, "https://github.com/acme/widgets.git")

	docker := &fakeDocker{
		respond: func(c dockerCall) ([]byte, error) {
			if len(c.args) >= 2 && c.args[0] == "image" && c.args[1] == "inspect" {
				return []byte("Error: No such image"), errors.New("exit 1")
			}
			return nil, nil
		},
	}
	deps := pushDeps{
		awsLoad: func(ctx context.Context, profile string) (aws.Config, error) {
			t.Fatal("awsLoad should not be called when local image is missing")
			return aws.Config{}, nil
		},
		ssmClient: func(aws.Config) config.SSMClient {
			t.Fatal("ssmClient should not be called when local image is missing")
			return nil
		},
		ecrClient: func(aws.Config) ecrAuthClient {
			t.Fatal("ecrClient should not be called when local image is missing")
			return nil
		},
		docker: docker,
		slug:   func(s string) (string, error) { return "acme-widgets", nil },
	}

	_, err := runPushInDir(t, dir, deps)
	if err == nil {
		t.Fatal("expected error when local image missing; got nil")
	}
	if !strings.Contains(err.Error(), "run 'horde launch' first") {
		t.Errorf("error %q missing guidance", err.Error())
	}
	// Only the inspect call should have happened.
	for _, c := range docker.calls {
		if len(c.args) > 0 && (c.args[0] == "tag" || c.args[0] == "push" || c.args[0] == "login") {
			t.Errorf("unexpected docker %s call after missing image: %v", c.args[0], c.args)
		}
	}
}

// TestPush_EcrAuth verifies that the ECR authorization token is decoded and
// piped to `docker login --password-stdin <proxy>`.
func TestPush_EcrAuth(t *testing.T) {
	dir := t.TempDir()
	setupGitRepo(t, dir, "https://github.com/acme/widgets.git")

	const password = "s3cret-token-value"
	const proxy = "https://123456789012.dkr.ecr.us-east-1.amazonaws.com"

	docker := &fakeDocker{
		respond: func(c dockerCall) ([]byte, error) {
			if len(c.args) > 0 && c.args[0] == "push" {
				return []byte("latest: digest: sha256:deadbeefcafef00d size: 1234"), nil
			}
			return nil, nil
		},
	}
	ssmStub := &fakePushSSM{cfg: healthyHordeConfig()}
	ecrStub := &fakeECR{token: "AWS:" + password, endpoint: proxy}

	deps := pushDeps{
		awsLoad:   func(ctx context.Context, profile string) (aws.Config, error) { return aws.Config{}, nil },
		ssmClient: func(aws.Config) config.SSMClient { return ssmStub },
		ecrClient: func(aws.Config) ecrAuthClient { return ecrStub },
		docker:    docker,
		slug:      func(s string) (string, error) { return "acme-widgets", nil },
	}

	out, err := runPushInDir(t, dir, deps)
	if err != nil {
		t.Fatalf("runPush: %v\n%s", err, out)
	}

	// Locate the `docker login` call and assert its shape.
	var loginCall *dockerCall
	for i := range docker.calls {
		c := docker.calls[i]
		if len(c.args) > 0 && c.args[0] == "login" {
			loginCall = &docker.calls[i]
			break
		}
	}
	if loginCall == nil {
		t.Fatalf("no docker login call; calls: %+v", docker.calls)
	}
	if loginCall.stdin != password {
		t.Errorf("docker login stdin = %q, want %q", loginCall.stdin, password)
	}
	wantArgs := []string{"login", "--username", "AWS", "--password-stdin", proxy}
	if fmt.Sprintf("%v", loginCall.args) != fmt.Sprintf("%v", wantArgs) {
		t.Errorf("docker login args = %v, want %v", loginCall.args, wantArgs)
	}
}

// TestPush_TagAndPush runs the whole happy path and asserts that the output
// contains the parsed digest.
func TestPush_TagAndPush(t *testing.T) {
	dir := t.TempDir()
	setupGitRepo(t, dir, "https://github.com/acme/widgets.git")

	cfg := healthyHordeConfig()
	target := cfg.EcrRepoURI + ":latest"

	docker := &fakeDocker{
		respond: func(c dockerCall) ([]byte, error) {
			if len(c.args) > 0 && c.args[0] == "push" {
				return []byte("The push refers to repository [...]\nlatest: digest: sha256:abc123def4567890 size: 1234\n"), nil
			}
			return nil, nil
		},
	}
	ssmStub := &fakePushSSM{cfg: cfg}
	ecrStub := &fakeECR{token: "AWS:pw", endpoint: "https://x.dkr.ecr.us-east-1.amazonaws.com"}

	deps := pushDeps{
		awsLoad:   func(ctx context.Context, profile string) (aws.Config, error) { return aws.Config{}, nil },
		ssmClient: func(aws.Config) config.SSMClient { return ssmStub },
		ecrClient: func(aws.Config) ecrAuthClient { return ecrStub },
		docker:    docker,
		slug:      func(s string) (string, error) { return "acme-widgets", nil },
	}

	out, err := runPushInDir(t, dir, deps)
	if err != nil {
		t.Fatalf("runPush: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sha256:abc123def4567890") {
		t.Errorf("output missing digest; got: %q", out)
	}
	if !strings.Contains(out, target) {
		t.Errorf("output missing push target %q; got: %q", target, out)
	}

	// Assert tag and push were invoked with the right target.
	var sawTag, sawPush bool
	for _, c := range docker.calls {
		if len(c.args) >= 3 && c.args[0] == "tag" && c.args[1] == localWorkerImage && c.args[2] == target {
			sawTag = true
		}
		if len(c.args) >= 2 && c.args[0] == "push" && c.args[1] == target {
			sawPush = true
		}
	}
	if !sawTag {
		t.Errorf("no docker tag call targeting %q; calls: %+v", target, docker.calls)
	}
	if !sawPush {
		t.Errorf("no docker push call targeting %q; calls: %+v", target, docker.calls)
	}
}

// TestPush_SSMMissing verifies that a ParameterNotFound from SSM produces an
// error that guides the user to run `horde bootstrap deploy`.
func TestPush_SSMMissing(t *testing.T) {
	dir := t.TempDir()
	setupGitRepo(t, dir, "https://github.com/acme/widgets.git")

	docker := &fakeDocker{
		respond: func(c dockerCall) ([]byte, error) {
			// image inspect succeeds; all other calls succeed too (none expected).
			return nil, nil
		},
	}
	ssmStub := &fakePushSSM{err: &ssmtypes.ParameterNotFound{Message: aws.String("not found")}}
	ecrStub := &fakeECR{err: errors.New("should not be called")}

	deps := pushDeps{
		awsLoad:   func(ctx context.Context, profile string) (aws.Config, error) { return aws.Config{}, nil },
		ssmClient: func(aws.Config) config.SSMClient { return ssmStub },
		ecrClient: func(aws.Config) ecrAuthClient { return ecrStub },
		docker:    docker,
		slug:      func(s string) (string, error) { return "acme-widgets", nil },
	}

	_, err := runPushInDir(t, dir, deps)
	if err == nil {
		t.Fatal("expected error when SSM parameter is missing")
	}
	if !strings.Contains(err.Error(), "horde bootstrap deploy") {
		t.Errorf("error %q missing guidance substring %q", err.Error(), "horde bootstrap deploy")
	}
	// Make sure we didn't call login/tag/push after SSM failed.
	for _, c := range docker.calls {
		if len(c.args) > 0 && (c.args[0] == "login" || c.args[0] == "tag" || c.args[0] == "push") {
			t.Errorf("unexpected docker %s call after SSM error", c.args[0])
		}
	}
}
