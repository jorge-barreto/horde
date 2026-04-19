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
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dyntypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/jorge-barreto/horde/internal/awscfg"
	"github.com/jorge-barreto/horde/internal/config"
)

// ecsSmokeStatus mirrors the fields from cmd/horde/jsonv1.go's StatusV1 that
// the smoke test cares about. Kept in-sync here so the test file is
// self-contained.
type ecsSmokeStatus struct {
	ID           string   `json:"id"`
	Ticket       string   `json:"ticket"`
	Workflow     string   `json:"workflow,omitempty"`
	Branch       string   `json:"branch"`
	Status       string   `json:"status"`
	InstanceID   string   `json:"instance_id,omitempty"`
	ExitCode     *int     `json:"exit_code,omitempty"`
	DurationSecs float64  `json:"duration_seconds"`
	TotalCostUSD *float64 `json:"total_cost_usd,omitempty"`
	LaunchedBy   string   `json:"launched_by"`
	StartedAt    string   `json:"started_at"`
	CompletedAt  string   `json:"completed_at,omitempty"`
}

const (
	ecsSmokeSSMPath   = "/horde/jorge-barreto-horde/config"
	ecsSmokeRepoURL   = "https://github.com/jorge-barreto/horde.git"
	ecsSmokeWorkflow  = "quick-success"
	ecsSmokePollEvery = 5 * time.Second
	ecsSmokePollMax   = 5 * time.Minute
	ecsSmokeRunMax    = 5 * time.Minute
)

// TestECSSmoke is the ECS happy-path end-to-end test. It launches a 1-phase
// script workflow against the pre-deployed CloudFormation stack, polls to
// terminal, fetches logs, hydrates artifacts, and cleans up the DynamoDB row.
func TestECSSmoke(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping ECS smoke test in short mode")
	}
	if os.Getenv("HORDE_E2E_ECS") != "1" {
		t.Skip("HORDE_E2E_ECS != 1; skipping ECS smoke test")
	}

	// Isolated HOME + project dir. Project dir just needs a git remote so
	// config.RepoURL resolves to the horde repo URL; the worker itself clones
	// using GIT_TOKEN over HTTPS.
	homeDir := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("creating project dir: %v", err)
	}
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("remote", "add", "origin", ecsSmokeRepoURL)
	runGit("config", "user.name", "integration-test")
	runGit("config", "user.email", "test@test.com")

	// ---- Launch ---------------------------------------------------------
	ticket := fmt.Sprintf("TEST-ecs-smoke-%d", time.Now().UnixNano())
	t.Logf("launching ECS run: ticket=%s workflow=%s", ticket, ecsSmokeWorkflow)
	launchStart := time.Now()
	out, err := ecsRunHorde(t, workDir, homeDir,
		"--provider", "aws-ecs",
		"launch",
		"--workflow", ecsSmokeWorkflow,
		"--timeout", ecsSmokeRunMax.String(),
		ticket,
	)
	if err != nil {
		t.Fatalf("horde launch failed: %v\nstdout: %s", err, out)
	}
	runID := lastNonEmptyLine(out)
	if runID == "" {
		t.Fatalf("horde launch returned empty run ID; stdout: %s", out)
	}
	t.Logf("launched runID=%s (launch took %s)", runID, time.Since(launchStart).Truncate(time.Millisecond))

	// ---- Cleanup registration (runs even on later failure) --------------
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Defensive kill for orphaned Fargate tasks. Uses a fresh
		// status check to decide whether action is needed.
		statusOut, statusErr := ecsRunHorde(t, workDir, homeDir,
			"--provider", "aws-ecs", "--json", "status", runID,
		)
		if statusErr == nil {
			var s ecsSmokeStatus
			if jerr := json.Unmarshal([]byte(statusOut), &s); jerr == nil {
				if !isTerminalStatus(s.Status) {
					t.Logf("cleanup: run %s still %q — issuing kill", runID, s.Status)
					if _, kerr := ecsRunHorde(t, workDir, homeDir,
						"--provider", "aws-ecs", "kill", runID,
					); kerr != nil {
						t.Logf("cleanup: horde kill failed: %v", kerr)
					}
				}
			}
		}

		ecsDeleteRunFromDynamoDB(t, cleanCtx, runID)
	})

	// ---- Poll to terminal ----------------------------------------------
	status := ecsWaitTerminal(t, workDir, homeDir, runID, ecsSmokePollMax, ecsSmokePollEvery)
	totalElapsed := time.Since(launchStart).Truncate(time.Second)
	t.Logf("run reached terminal status=%q after %s (from launch)", status.Status, totalElapsed)

	if status.Status != "success" {
		// Log the raw DynamoDB row for debugging.
		if raw := ecsDescribeRunFromDynamoDB(t, runID); raw != "" {
			t.Logf("dynamo row for %s:\n%s", runID, raw)
		}
		t.Fatalf("expected status=success, got %q (exit_code=%v)", status.Status, ptrInt(status.ExitCode))
	}
	if status.ExitCode == nil || *status.ExitCode != 0 {
		if raw := ecsDescribeRunFromDynamoDB(t, runID); raw != "" {
			t.Logf("dynamo row for %s:\n%s", runID, raw)
		}
		t.Fatalf("expected exit_code=0, got %v", ptrInt(status.ExitCode))
	}

	// ---- Fetch logs -----------------------------------------------------
	logsOut, err := ecsRunHorde(t, workDir, homeDir,
		"--provider", "aws-ecs", "logs", runID,
	)
	if err != nil {
		t.Fatalf("horde logs failed: %v\nstdout: %s", err, logsOut)
	}
	if !strings.Contains(logsOut, "quick-success done") {
		t.Errorf("logs missing expected marker %q\nlogs:\n%s", "quick-success done", logsOut)
	}

	// ---- Hydrate --------------------------------------------------------
	hydrateDir := t.TempDir()
	hydrateOut, err := ecsRunHorde(t, workDir, homeDir,
		"--provider", "aws-ecs", "hydrate", "--into", hydrateDir, runID,
	)
	if err != nil {
		t.Fatalf("horde hydrate failed: %v\nstdout: %s", err, hydrateOut)
	}
	runResultPath := filepath.Join(
		hydrateDir, ".orc", "audit", ecsSmokeWorkflow,
		fmt.Sprintf("%s-%s", ticket, runID), "run-result.json",
	)
	data, err := os.ReadFile(runResultPath)
	if err != nil {
		t.Fatalf("reading hydrated run-result.json at %s: %v", runResultPath, err)
	}
	var runResult map[string]interface{}
	if err := json.Unmarshal(data, &runResult); err != nil {
		t.Fatalf("parsing run-result.json: %v\nraw: %s", err, string(data))
	}
	// orc's run-result.json uses "completed" (its own vocabulary) while
	// DynamoDB stores "success" (horde's vocabulary). Accept either.
	if got, _ := runResult["status"].(string); got != "success" && got != "completed" {
		t.Errorf("run-result.json status=%q, want success or completed\nraw: %s", got, string(data))
	}
}

// ecsRunHorde runs the horde binary with the given args, cwd=workDir, and an
// environment scrubbed of HOME/HORDE_DOCKER_IMAGE (HOME is overridden to
// homeDir; HORDE_DOCKER_IMAGE is not needed for ECS).
func ecsRunHorde(t *testing.T, workDir, homeDir string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, hordeBin, args...)
	cmd.Dir = workDir
	cmd.Env = ecsTestEnv(homeDir)
	out, err := cmd.Output()
	s := strings.TrimSpace(string(out))
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return s, fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
	}
	return s, err
}

func ecsTestEnv(homeDir string) []string {
	// Preserve the real user's AWS config location before we override HOME,
	// so the subprocess can still resolve AWS_PROFILE against ~/.aws/config.
	realHome, _ := os.UserHomeDir()
	env := os.Environ()
	filtered := env[:0]
	hasConfigFile := false
	hasCredsFile := false
	for _, e := range env {
		switch {
		case strings.HasPrefix(e, "HOME="):
			continue
		case strings.HasPrefix(e, "HORDE_DOCKER_IMAGE="):
			continue
		case strings.HasPrefix(e, "AWS_CONFIG_FILE="):
			hasConfigFile = true
		case strings.HasPrefix(e, "AWS_SHARED_CREDENTIALS_FILE="):
			hasCredsFile = true
		}
		filtered = append(filtered, e)
	}
	filtered = append(filtered, "HOME="+homeDir)
	if realHome != "" {
		if !hasConfigFile {
			filtered = append(filtered, "AWS_CONFIG_FILE="+filepath.Join(realHome, ".aws", "config"))
		}
		if !hasCredsFile {
			filtered = append(filtered, "AWS_SHARED_CREDENTIALS_FILE="+filepath.Join(realHome, ".aws", "credentials"))
		}
		// Symlink the real user's SSO cache into the temp HOME so the AWS
		// SDK's SSO credential provider can find it. The SDK resolves the
		// cache path relative to $HOME, not AWS_CONFIG_FILE.
		fakeSSO := filepath.Join(homeDir, ".aws", "sso")
		if _, err := os.Stat(fakeSSO); os.IsNotExist(err) {
			_ = os.MkdirAll(filepath.Join(homeDir, ".aws"), 0o755)
			_ = os.Symlink(filepath.Join(realHome, ".aws", "sso"), fakeSSO)
		}
	}
	return filtered
}

// ecsWaitTerminal polls horde --json status until the run reaches a terminal
// status or the timeout expires. Returns the terminal status object.
func ecsWaitTerminal(t *testing.T, workDir, homeDir, runID string, timeout, pollInterval time.Duration) ecsSmokeStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastStatus string
	for time.Now().Before(deadline) {
		out, err := ecsRunHorde(t, workDir, homeDir,
			"--provider", "aws-ecs", "--json", "status", runID,
		)
		if err != nil {
			t.Logf("status poll error (will retry): %v", err)
			time.Sleep(pollInterval)
			continue
		}
		var s ecsSmokeStatus
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			t.Logf("status poll: invalid JSON (will retry): %v\nraw: %s", err, out)
			time.Sleep(pollInterval)
			continue
		}
		if s.Status != lastStatus {
			t.Logf("status transition: %q -> %q", lastStatus, s.Status)
			lastStatus = s.Status
		}
		if isTerminalStatus(s.Status) {
			return s
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("run %s did not reach terminal status within %s (last status: %q)", runID, timeout, lastStatus)
	return ecsSmokeStatus{}
}

func isTerminalStatus(s string) bool {
	switch s {
	case "success", "failed", "killed", "timed_out":
		return true
	}
	return false
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

func ptrInt(p *int) interface{} {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// ecsDeleteRunFromDynamoDB removes the run row from the shared DynamoDB table.
// Best-effort: logs on failure rather than failing the cleanup.
func ecsDeleteRunFromDynamoDB(t *testing.T, ctx context.Context, runID string) {
	t.Helper()
	tableName, awsCfg, ok := ecsLoadHordeConfig(t, ctx)
	if !ok {
		return
	}
	ddb := dynamodb.NewFromConfig(awsCfg)
	_, err := ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dyntypes.AttributeValue{
			"id": &dyntypes.AttributeValueMemberS{Value: runID},
		},
	})
	if err != nil {
		t.Logf("cleanup: DeleteItem(%s, id=%s) failed: %v", tableName, runID, err)
		return
	}
	t.Logf("cleanup: deleted dynamo row %s from %s", runID, tableName)
}

// ecsDescribeRunFromDynamoDB returns a pretty-printed JSON dump of the run's
// DynamoDB item, for debugging on failure. Returns "" if anything goes wrong.
func ecsDescribeRunFromDynamoDB(t *testing.T, runID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tableName, awsCfg, ok := ecsLoadHordeConfig(t, ctx)
	if !ok {
		return ""
	}
	ddb := dynamodb.NewFromConfig(awsCfg)
	out, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dyntypes.AttributeValue{
			"id": &dyntypes.AttributeValueMemberS{Value: runID},
		},
	})
	if err != nil || out.Item == nil {
		return ""
	}
	// Project to a map[string]interface{} for readability.
	flat := make(map[string]interface{}, len(out.Item))
	for k, v := range out.Item {
		switch tv := v.(type) {
		case *dyntypes.AttributeValueMemberS:
			flat[k] = tv.Value
		case *dyntypes.AttributeValueMemberN:
			flat[k] = tv.Value
		case *dyntypes.AttributeValueMemberBOOL:
			flat[k] = tv.Value
		case *dyntypes.AttributeValueMemberNULL:
			flat[k] = nil
		default:
			flat[k] = fmt.Sprintf("%T", v)
		}
	}
	b, _ := json.MarshalIndent(flat, "", "  ")
	return string(b)
}

// ecsLoadHordeConfig loads the deployed stack's SSM config. Returns (table,
// awsConfig, ok). On any error it logs and returns ok=false.
func ecsLoadHordeConfig(t *testing.T, ctx context.Context) (string, aws.Config, bool) {
	t.Helper()
	awsCfg, err := awscfg.Load(ctx, os.Getenv("AWS_PROFILE"))
	if err != nil {
		t.Logf("loading AWS config: %v", err)
		return "", aws.Config{}, false
	}
	ssmClient := ssm.NewFromConfig(awsCfg)
	hc, err := config.LoadFromSSM(ctx, ssmClient, ecsSmokeSSMPath)
	if err != nil {
		t.Logf("loading SSM config from %s: %v", ecsSmokeSSMPath, err)
		return "", aws.Config{}, false
	}
	if hc.RunsTable == "" {
		t.Logf("SSM config has empty runs_table")
		return "", aws.Config{}, false
	}
	return hc.RunsTable, awsCfg, true
}
