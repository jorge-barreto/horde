package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dyntypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/jorge-barreto/horde/internal/awscfg"
	"github.com/jorge-barreto/horde/internal/bootstrap"
	"github.com/jorge-barreto/horde/internal/config"
)

// ecsHarnessRepoURL is the repo whose remote we set on the temp project dir,
// so that `horde` can derive a slug and resolve the deployed stack.
const ecsHarnessRepoURL = "https://github.com/jorge-barreto/horde.git"

// ecsDriver implements instanceDriver against the deployed ECS stack.
// Instance state lives in ECS (Fargate tasks) and DynamoDB (run rows).
type ecsDriver struct {
	t           *testing.T
	ctx         context.Context
	dynamo      *dynamodb.Client
	ecs         *ecs.Client
	cwLogs      *cloudwatchlogs.Client
	cluster     string // cluster ARN or name
	runsTable   string
	logGroup    string
	logPrefix   string // LogStreamPrefix (typically "ecs")
	runsToClean []string
}

// InstanceID queries DynamoDB for the ECS task ARN recorded for a run.
// Returns empty string if the row or attribute is missing.
func (d *ecsDriver) InstanceID(runID string) string {
	d.t.Helper()
	out, err := d.dynamo.GetItem(d.ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.runsTable),
		Key: map[string]dyntypes.AttributeValue{
			"id": &dyntypes.AttributeValueMemberS{Value: runID},
		},
	})
	if err != nil {
		d.t.Fatalf("ecsDriver.InstanceID: dynamo GetItem(%s, id=%s): %v", d.runsTable, runID, err)
	}
	if out.Item == nil {
		return ""
	}
	v, ok := out.Item["instance_id"].(*dyntypes.AttributeValueMemberS)
	if !ok {
		return ""
	}
	return v.Value
}

// InstanceRunning returns true if the Fargate task is not yet STOPPED.
// Returns false when the task has been garbage-collected or cannot be found.
func (d *ecsDriver) InstanceRunning(instanceID string) bool {
	d.t.Helper()
	if instanceID == "" {
		return false
	}
	out, err := d.ecs.DescribeTasks(d.ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(d.cluster),
		Tasks:   []string{instanceID},
	})
	if err != nil {
		d.t.Fatalf("ecsDriver.InstanceRunning: DescribeTasks(%s): %v", instanceID, err)
	}
	if len(out.Tasks) == 0 {
		// Task not found — either GC'd or never existed.
		return false
	}
	last := aws.ToString(out.Tasks[0].LastStatus)
	return last != "" && last != "STOPPED"
}

// FetchContainerLogs reads all CloudWatch log events for the task.
// The stream name follows the ECS awslogs-stream-prefix convention:
//
//	<prefix>/horde-worker/<task-id>
func (d *ecsDriver) FetchContainerLogs(instanceID string) (string, error) {
	d.t.Helper()
	if instanceID == "" {
		return "", fmt.Errorf("ecsDriver.FetchContainerLogs: empty instance ID")
	}
	taskID := instanceID
	if i := strings.LastIndex(taskID, "/"); i >= 0 {
		taskID = taskID[i+1:]
	}
	if taskID == "" {
		return "", fmt.Errorf("ecsDriver.FetchContainerLogs: empty task ID from instance %q", instanceID)
	}
	prefix := d.logPrefix
	if prefix == "" {
		prefix = "ecs"
	}
	streamName := prefix + "/horde-worker/" + taskID

	var lines []string
	var nextToken *string
	for {
		input := &cloudwatchlogs.GetLogEventsInput{
			LogGroupName:  aws.String(d.logGroup),
			LogStreamName: aws.String(streamName),
			StartFromHead: aws.Bool(true),
		}
		if nextToken != nil {
			input.NextToken = nextToken
		}
		out, err := d.cwLogs.GetLogEvents(d.ctx, input)
		if err != nil {
			var rnf *cwltypes.ResourceNotFoundException
			if errors.As(err, &rnf) {
				return "", fmt.Errorf("log stream %q not found in group %q: %w", streamName, d.logGroup, err)
			}
			return "", fmt.Errorf("GetLogEvents(%s/%s): %w", d.logGroup, streamName, err)
		}
		for _, ev := range out.Events {
			lines = append(lines, aws.ToString(ev.Message))
		}
		// CloudWatch pagination: stop when NextForwardToken no longer advances.
		if out.NextForwardToken == nil || (nextToken != nil && *out.NextForwardToken == *nextToken) {
			break
		}
		nextToken = out.NextForwardToken
	}
	return strings.Join(lines, "\n"), nil
}

// StoreStatus reads the "status" attribute from the DynamoDB run row.
// Returns "" if the row doesn't exist.
func (d *ecsDriver) StoreStatus(runID string) string {
	d.t.Helper()
	out, err := d.dynamo.GetItem(d.ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.runsTable),
		Key: map[string]dyntypes.AttributeValue{
			"id": &dyntypes.AttributeValueMemberS{Value: runID},
		},
	})
	if err != nil {
		d.t.Fatalf("ecsDriver.StoreStatus: dynamo GetItem(%s, id=%s): %v", d.runsTable, runID, err)
	}
	if out.Item == nil {
		return ""
	}
	v, ok := out.Item["status"].(*dyntypes.AttributeValueMemberS)
	if !ok {
		return ""
	}
	return v.Value
}

// StoreExitCode reads the "exit_code" Number attribute from the run row.
// Returns nil if the attribute is missing or the row doesn't exist.
func (d *ecsDriver) StoreExitCode(runID string) *int {
	d.t.Helper()
	out, err := d.dynamo.GetItem(d.ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.runsTable),
		Key: map[string]dyntypes.AttributeValue{
			"id": &dyntypes.AttributeValueMemberS{Value: runID},
		},
	})
	if err != nil {
		d.t.Fatalf("ecsDriver.StoreExitCode: dynamo GetItem(%s, id=%s): %v", d.runsTable, runID, err)
	}
	if out.Item == nil {
		return nil
	}
	v, ok := out.Item["exit_code"].(*dyntypes.AttributeValueMemberN)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(v.Value)
	if err != nil {
		d.t.Fatalf("ecsDriver.StoreExitCode: parsing %q: %v", v.Value, err)
	}
	return &n
}

// TearDown removes any DynamoDB run rows the harness tracked during the test.
// Best-effort: logs on failure rather than failing the cleanup.
func (d *ecsDriver) TearDown() {
	for _, runID := range d.runsToClean {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := d.dynamo.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: aws.String(d.runsTable),
			Key: map[string]dyntypes.AttributeValue{
				"id": &dyntypes.AttributeValueMemberS{Value: runID},
			},
		})
		cancel()
		if err != nil {
			d.t.Logf("ecsDriver.TearDown: DeleteItem(%s, id=%s): %v", d.runsTable, runID, err)
			continue
		}
		d.t.Logf("ecsDriver.TearDown: deleted dynamo row %s from %s", runID, d.runsTable)
	}
}

// newECSHarness builds an ECS-backed harness against the deployed stack for
// the horde repo. It skips unless -short is off and HORDE_E2E_ECS=1.
func newECSHarness(t *testing.T) *harness {
	t.Helper()
	if testing.Short() {
		t.Skip("ECS integration: short mode")
	}
	if os.Getenv("HORDE_E2E_ECS") != "1" {
		t.Skip("ECS integration: HORDE_E2E_ECS != 1")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}

	homeDir, err := os.MkdirTemp("", "horde-integ-ecs-*")
	if err != nil {
		t.Fatalf("creating temp home: %v", err)
	}
	// Symlink AWS SSO cache / copy AWS config so the horde subprocess can
	// resolve AWS_PROFILE from the real user's home — `env()` will ensure
	// AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE point at the real home.
	t.Cleanup(func() { os.RemoveAll(homeDir) })

	workDir := filepath.Join(homeDir, "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("creating project dir: %v", err)
	}

	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = workDir
		if out, gerr := cmd.CombinedOutput(); gerr != nil {
			t.Fatalf("git %v failed: %v\n%s", args, gerr, out)
		}
	}
	runGit("init")
	runGit("remote", "add", "origin", ecsHarnessRepoURL)
	runGit("config", "user.name", "integration-test")
	runGit("config", "user.email", "test@test.com")

	// Dummy .env so any local horde validation passes. The real Fargate task
	// reads its secrets from Secrets Manager via the deployed stack; these
	// values are never transmitted.
	envContent := "CLAUDE_CODE_OAUTH_TOKEN=test-token\nGIT_TOKEN=test-token\n"
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte(envContent), 0o644); err != nil {
		t.Fatalf("writing .env: %v", err)
	}

	// Load SSM config to discover cluster / runs table / log group. Derive
	// the SSM path from the remote URL via bootstrap.Slug so we don't hard-
	// code the project identity.
	ctx := context.Background()
	awsCfg, err := awscfg.Load(ctx, os.Getenv("AWS_PROFILE"))
	if err != nil {
		t.Fatalf("loading AWS config (profile=%q): %v", os.Getenv("AWS_PROFILE"), err)
	}
	slug, err := bootstrap.Slug(ecsHarnessRepoURL)
	if err != nil {
		t.Fatalf("deriving slug: %v", err)
	}
	ssmPath := "/horde/" + slug + "/config"
	ssmClient := ssm.NewFromConfig(awsCfg)
	hc, err := config.LoadFromSSM(ctx, ssmClient, ssmPath)
	if err != nil {
		t.Fatalf("loading SSM config from %s: %v", ssmPath, err)
	}

	driver := &ecsDriver{
		t:         t,
		ctx:       ctx,
		dynamo:    dynamodb.NewFromConfig(awsCfg),
		ecs:       ecs.NewFromConfig(awsCfg),
		cwLogs:    cloudwatchlogs.NewFromConfig(awsCfg),
		cluster:   hc.ClusterARN,
		runsTable: hc.RunsTable,
		logGroup:  hc.LogGroup,
		logPrefix: hc.LogStreamPrefix,
	}

	h := &harness{
		t:             t,
		homeDir:       homeDir,
		workDir:       workDir,
		repoRoot:      repoRoot,
		driver:        driver,
		hordeProvider: "aws-ecs",
	}
	t.Cleanup(driver.TearDown)
	return h
}

// uniqueTicket returns a ticket ID unique within this test binary invocation.
// Format: TEST-<name>-<unix-nano>. Lets individual ECS tests run concurrently
// without ticket collisions, and makes a failing test's run trivially
// identifiable in AWS consoles.
func uniqueTicket(name string) string {
	return fmt.Sprintf("TEST-%s-%d", name, time.Now().UnixNano())
}

// waitForECSTerminal polls StoreStatus every 5 seconds up to timeout. Fails
// the test via t.Fatalf if the run never reaches success/failed/killed. On
// success logs each status transition so failure mode is trivially visible.
func waitForECSTerminal(t *testing.T, h *harness, runID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		got := h.driver.StoreStatus(runID)
		if got != last {
			t.Logf("status transition: %q -> %q", last, got)
			last = got
		}
		switch got {
		case "success", "failed", "killed", "timed_out":
			t.Logf("run %s reached terminal status=%q", runID, got)
			return got
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("run %s did not reach terminal status within %s (last=%q)", runID, timeout, last)
	return ""
}
