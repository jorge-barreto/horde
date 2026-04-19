package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/jorge-barreto/horde/internal/awscfg"
)

// CDK e2e tests verify the @horde/cdk construct against real AWS. The stack
// is brought up once (TestECSCDK_Bringup), exercised any number of times
// (TestECSCDK_Smoke), and torn down explicitly (TestECSCDK_Teardown). State
// flows between phases via /tmp/horde-cdk-e2e-state.json.
//
// Cost note: this stack creates its own NAT Gateway (~$32/mo idle). Always
// run TestECSCDK_Teardown when done.

const (
	cdkE2EStateFile = "/tmp/horde-cdk-e2e-state.json"
	// Fake remote URL whose bootstrap.Slug derives to cdkE2ESlug. Set as the
	// origin of the temp project dirs so `horde push` and `horde launch`
	// resolve to the CDK stack's SSM path.
	cdkE2ERepoURL   = "https://github.com/jorge-barreto/horde-cdke2e.git"
	cdkE2ESlug      = "jorge-barreto-horde-cdke2e"
	cdkE2EStackName = "horde-jorge-barreto-horde-cdke2e"
)

type cdkE2EState struct {
	StackName       string    `json:"stack_name"`
	Slug            string    `json:"slug"`
	SSMPath         string    `json:"ssm_path"`
	ClusterArn      string    `json:"cluster_arn"`
	EcrRepoURI      string    `json:"ecr_repo_uri"`
	EcrRepoName     string    `json:"ecr_repo_name"`
	ArtifactsBucket string    `json:"artifacts_bucket"`
	RunsTable       string    `json:"runs_table"`
	LogGroup        string    `json:"log_group"`
	DeployedAt      time.Time `json:"deployed_at"`
}

func skipUnlessCDKE2E(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("CDK e2e: short mode")
	}
	if os.Getenv("HORDE_E2E_CDK") != "1" {
		t.Skip("CDK e2e: HORDE_E2E_CDK != 1")
	}
}

func cdkRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return root
}

// runCDK shells out to `npx cdk <args>` from cdkDir. Streams output to the
// test logger so deploys are observable in real time.
func runCDK(t *testing.T, cdkDir string, args ...string) error {
	t.Helper()
	full := append([]string{"cdk"}, args...)
	cmd := exec.Command("npx", full...)
	cmd.Dir = cdkDir
	cmd.Env = cdkEnvForCDK()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	t.Logf("running: cd %s && npx %s", cdkDir, strings.Join(full, " "))
	return cmd.Run()
}

func cdkEnvForCDK() []string {
	env := os.Environ()
	if os.Getenv("CDK_DEFAULT_REGION") == "" {
		if r := os.Getenv("AWS_REGION"); r != "" {
			env = append(env, "CDK_DEFAULT_REGION="+r)
		}
	}
	return env
}

// readCDKOutputs parses the JSON file produced by `cdk deploy --outputs-file`.
// The top-level key is the stack name from app.ts (HordeCdkE2E construct ID).
func readCDKOutputs(t *testing.T, path string) cdkE2EState {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading cdk outputs %s: %v", path, err)
	}
	var doc map[string]map[string]string
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse cdk outputs: %v", err)
	}
	// We synthesize one stack via construct id "HordeCdkE2E" but the user-
	// visible stackName overrides it for actual CFN. CDK writes the outputs
	// keyed by stackName when stackName is set explicitly.
	o, ok := doc[cdkE2EStackName]
	if !ok {
		// Fallback: take the only stack in the doc.
		for k, v := range doc {
			t.Logf("outputs file contains stack %q; using it", k)
			o = v
			break
		}
	}
	return cdkE2EState{
		StackName:       o["StackNameOut"],
		Slug:            o["SlugOut"],
		SSMPath:         o["SsmPathOut"],
		ClusterArn:      o["ClusterArnOut"],
		EcrRepoURI:      o["EcrRepoUriOut"],
		EcrRepoName:     o["EcrRepoNameOut"],
		ArtifactsBucket: o["ArtifactsBucketOut"],
		RunsTable:       o["RunsTableOut"],
		LogGroup:        o["LogGroupOut"],
	}
}

func writeCDKState(t *testing.T, s cdkE2EState) {
	t.Helper()
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("marshal cdk state: %v", err)
	}
	if err := os.WriteFile(cdkE2EStateFile, raw, 0o644); err != nil {
		t.Fatalf("writing cdk state: %v", err)
	}
}

func readCDKState(t *testing.T) *cdkE2EState {
	t.Helper()
	s, err := readCDKStateSoft()
	if err != nil {
		t.Fatalf("reading cdk state %s: %v (run TestECSCDK_Bringup first)", cdkE2EStateFile, err)
	}
	return s
}

func readCDKStateSoft() (*cdkE2EState, error) {
	raw, err := os.ReadFile(cdkE2EStateFile)
	if err != nil {
		return nil, err
	}
	var s cdkE2EState
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// hordePushForCDK builds the worker image (via `make docker-build`) and
// pushes it to the CDK stack's ECR repo by invoking `horde push` from a
// temp project dir whose git remote derives to cdkE2ESlug.
func hordePushForCDK(t *testing.T, repoRoot string) error {
	t.Helper()

	// Build the worker image (idempotent; cached layers make repeat runs fast).
	if err := runMake(t, repoRoot, "docker-build"); err != nil {
		return fmt.Errorf("make docker-build: %w", err)
	}

	projDir := t.TempDir()
	runGit := func(args ...string) error {
		c := exec.Command("git", args...)
		c.Dir = projDir
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v\n%s", args, err, out)
		}
		return nil
	}
	if err := runGit("init"); err != nil {
		return err
	}
	if err := runGit("remote", "add", "origin", cdkE2ERepoURL); err != nil {
		return err
	}
	if err := runGit("config", "user.name", "integration-test"); err != nil {
		return err
	}
	if err := runGit("config", "user.email", "test@test.com"); err != nil {
		return err
	}

	// Dummy .env so client-side validation passes; the worker reads real
	// secrets from Secrets Manager via the deployed task definition.
	envContent := "CLAUDE_CODE_OAUTH_TOKEN=test-token\nGIT_TOKEN=test-token\n"
	if err := os.WriteFile(filepath.Join(projDir, ".env"), []byte(envContent), 0o644); err != nil {
		return err
	}

	cmd := exec.Command(hordeBin, "push")
	cmd.Dir = projDir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	t.Logf("running: cd %s && horde push", projDir)
	return cmd.Run()
}

func runMake(t *testing.T, dir, target string) error {
	t.Helper()
	cmd := exec.Command("make", target)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	t.Logf("running: cd %s && make %s", dir, target)
	return cmd.Run()
}

// emptyECRRepo deletes every image in the named repo. Idempotent.
func emptyECRRepo(ctx context.Context, c *ecr.Client, name string) error {
	for {
		out, err := c.ListImages(ctx, &ecr.ListImagesInput{
			RepositoryName: aws.String(name),
		})
		if err != nil {
			var notFound *ecrtypes.RepositoryNotFoundException
			if errors.As(err, &notFound) {
				return nil
			}
			return fmt.Errorf("ListImages(%s): %w", name, err)
		}
		if len(out.ImageIds) == 0 {
			return nil
		}
		_, err = c.BatchDeleteImage(ctx, &ecr.BatchDeleteImageInput{
			RepositoryName: aws.String(name),
			ImageIds:       out.ImageIds,
		})
		if err != nil {
			return fmt.Errorf("BatchDeleteImage(%s): %w", name, err)
		}
		if out.NextToken == nil {
			return nil
		}
	}
}

// emptyS3Bucket deletes every object (and version) from the bucket. Idempotent.
func emptyS3Bucket(ctx context.Context, c *s3.Client, bucket string) error {
	for {
		out, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
			Bucket: aws.String(bucket),
		})
		if err != nil {
			var nsk *s3types.NoSuchBucket
			if errors.As(err, &nsk) {
				return nil
			}
			return fmt.Errorf("ListObjectVersions(%s): %w", bucket, err)
		}
		var ids []s3types.ObjectIdentifier
		for _, v := range out.Versions {
			ids = append(ids, s3types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range out.DeleteMarkers {
			ids = append(ids, s3types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		if len(ids) == 0 {
			// Fall back to non-versioned listing for buckets without versioning.
			lo, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
			if err != nil {
				return nil
			}
			for _, o := range lo.Contents {
				ids = append(ids, s3types.ObjectIdentifier{Key: o.Key})
			}
			if len(ids) == 0 {
				return nil
			}
		}
		_, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("DeleteObjects(%s): %w", bucket, err)
		}
		if out.NextKeyMarker == nil && out.NextVersionIdMarker == nil {
			return nil
		}
	}
}

// TestECSCDK_Bringup deploys cdk/e2e and pushes a worker image to the
// freshly-created ECR repo. Writes /tmp/horde-cdk-e2e-state.json. Idempotent.
func TestECSCDK_Bringup(t *testing.T) {
	skipUnlessCDKE2E(t)
	t.Logf("NOTE: deploying CDK e2e stack. Includes its own NAT Gateway (~$32/mo idle). Tear down with TestECSCDK_Teardown.")

	repoRoot := cdkRepoRoot(t)
	cdkDir := filepath.Join(repoRoot, "cdk")

	outputsDir := t.TempDir()
	outputsFile := filepath.Join(outputsDir, "outputs.json")
	deployStart := time.Now()
	if err := runCDK(t, cdkDir,
		"deploy", "--require-approval", "never", "--ci",
		"--outputs-file", outputsFile,
	); err != nil {
		t.Fatalf("cdk deploy: %v", err)
	}
	t.Logf("cdk deploy took %s", time.Since(deployStart).Truncate(time.Second))

	state := readCDKOutputs(t, outputsFile)
	state.DeployedAt = time.Now().UTC()
	if state.SSMPath == "" || state.EcrRepoName == "" {
		t.Fatalf("cdk outputs missing required fields: %+v", state)
	}

	// Push the worker image. On failure, persist state anyway so teardown
	// can clean up the deployed stack.
	if err := hordePushForCDK(t, repoRoot); err != nil {
		writeCDKState(t, state)
		t.Fatalf("horde push: %v (stack is deployed; run TestECSCDK_Teardown to clean up)", err)
	}

	writeCDKState(t, state)
	t.Logf("bring-up complete: slug=%s, ssm=%s, ecr=%s", state.Slug, state.SSMPath, state.EcrRepoURI)
}

// TestECSCDK_Smoke runs one quick-success workflow through the CDK stack
// end-to-end: launch -> wait for terminal -> assert success. If this passes,
// the construct works.
func TestECSCDK_Smoke(t *testing.T) {
	skipUnlessCDKE2E(t)
	state := readCDKState(t)
	t.Logf("smoke test against slug=%s, cluster=%s", state.Slug, state.ClusterArn)

	// Reuse the standard ECS harness, parameterized on the CDK fake remote.
	h := newECSHarnessForRepo(t, cdkE2ERepoURL)

	ticket := uniqueTicket("cdke2e-smoke")
	t.Logf("launching: ticket=%s, workflow=quick-success", ticket)
	runID := h.Launch(ticket, "quick-success", 5*time.Minute)
	t.Logf("runID=%s", runID)

	// Poll DynamoDB row to terminal status. Status-sync Lambda is the path
	// under test — if it doesn't fire, this times out.
	deadline := time.Now().Add(8 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		s := h.driver.StoreStatus(runID)
		if s != last {
			t.Logf("status: %q -> %q", last, s)
			last = s
		}
		switch s {
		case "success":
			ec := h.driver.StoreExitCode(runID)
			if ec == nil || *ec != 0 {
				t.Fatalf("status=success but exit_code=%v", ec)
			}
			t.Logf("smoke passed: status=success, exit_code=0")
			return
		case "failed", "killed", "timed_out":
			t.Fatalf("run reached non-success terminal: %q", s)
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("run %s never reached terminal status (last=%q)", runID, last)
}

// TestECSCDK_Teardown empties the ECR repo + S3 artifacts bucket, then runs
// `cdk destroy --force`. Removes /tmp/horde-cdk-e2e-state.json on success.
// Tolerates missing state file by destroying by stack name.
func TestECSCDK_Teardown(t *testing.T) {
	skipUnlessCDKE2E(t)

	state, err := readCDKStateSoft()
	if err != nil {
		t.Logf("no state file at %s: %v — destroying by stack name", cdkE2EStateFile, err)
		state = &cdkE2EState{StackName: cdkE2EStackName}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	awsCfg, aerr := awscfg.Load(ctx, os.Getenv("AWS_PROFILE"))
	if aerr != nil {
		t.Fatalf("loading AWS config: %v", aerr)
	}

	if state.EcrRepoName != "" {
		if err := emptyECRRepo(ctx, ecr.NewFromConfig(awsCfg), state.EcrRepoName); err != nil {
			t.Logf("emptying ECR %s: %v (continuing; cdk destroy may still succeed)", state.EcrRepoName, err)
		} else {
			t.Logf("emptied ECR repo %s", state.EcrRepoName)
		}
	}
	if state.ArtifactsBucket != "" {
		if err := emptyS3Bucket(ctx, s3.NewFromConfig(awsCfg), state.ArtifactsBucket); err != nil {
			t.Logf("emptying S3 %s: %v", state.ArtifactsBucket, err)
		} else {
			t.Logf("emptied S3 bucket %s", state.ArtifactsBucket)
		}
	}

	cdkDir := filepath.Join(cdkRepoRoot(t), "cdk")
	if err := runCDK(t, cdkDir, "destroy", "--force"); err != nil {
		t.Fatalf("cdk destroy: %v", err)
	}

	if err := os.Remove(cdkE2EStateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Logf("removing state file: %v", err)
	}
	t.Logf("teardown complete")
}
