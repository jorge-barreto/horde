package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jorge-barreto/horde/internal/config"
)

var hordeBin string // set by TestMain

// dockerAvailable caches the result of exec.LookPath("docker"). Docker-backed
// tests should skip when false; ECS-backed tests do not require it.
var dockerAvailable bool

func TestMain(m *testing.M) {
	// Load .env from the repo root so tests inherit AWS_PROFILE, HORDE_E2E_*
	// flags, and any other variables declared there. Real environment wins
	// (same policy as cmd/horde/main.go).
	if repoRoot, err := filepath.Abs(filepath.Join("..", "..")); err == nil {
		if err := config.ApplyDotEnvToProcess(repoRoot); err != nil {
			fmt.Fprintf(os.Stderr, "warning: loading .env: %v\n", err)
		}
	}

	// Docker is a soft dependency — record availability so Docker-backed
	// tests can skip individually. ECS tests do not need Docker.
	_, err := exec.LookPath("docker")
	dockerAvailable = err == nil
	if !dockerAvailable {
		fmt.Fprintln(os.Stderr, "note: docker not found in PATH; Docker-backed tests will skip")
	}

	// Build horde binary
	tmp, err := os.MkdirTemp("", "horde-integration-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	hordeBin = filepath.Join(tmp, "horde")
	if runtime.GOOS == "windows" {
		hordeBin += ".exe"
	}

	// Resolve repo root (test/integration/ -> repo root)
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving repo root: %v\n", err)
		os.Exit(1)
	}

	cmd := exec.Command("go", "build", "-o", hordeBin, "./cmd/horde/")
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "building horde binary: %v\n", err)
		os.Exit(1)
	}

	// If HORDE_E2E_ECS is set, ensure the bootstrap stack is deployed and
	// the worker image is pushed before running any ECS-backed tests. This
	// avoids each ECS test needing its own deploy/push dance and lets a
	// whole suite of ECS tests share one stack (by default, with
	// HORDE_E2E_ECS_KEEP=1, the stack survives between runs for fast
	// iteration — destroy it explicitly with `horde bootstrap destroy`).
	if os.Getenv("HORDE_E2E_ECS") == "1" {
		if err := ensureECSStack(repoRoot); err != nil {
			fmt.Fprintf(os.Stderr, "ECS stack setup failed: %v\n", err)
			os.Exit(1)
		}
	}

	code := m.Run()

	// Only destroy the stack if the user explicitly opted in by UNsetting
	// HORDE_E2E_ECS_KEEP. Default behavior is to keep the stack alive.
	if os.Getenv("HORDE_E2E_ECS") == "1" && os.Getenv("HORDE_E2E_ECS_KEEP") != "1" {
		fmt.Fprintln(os.Stderr, "HORDE_E2E_ECS_KEEP unset — destroying stack")
		destroyECSStack(repoRoot)
	}

	os.Exit(code)
}

// ensureECSStack deploys the bootstrap CloudFormation stack if it isn't
// already up, and pushes the worker image to ECR. Idempotent — if the stack
// already exists and the image is current, this is fast.
func ensureECSStack(repoRoot string) error {
	// Regenerate the on-disk template so it matches the current source.
	if out, err := runHordeTool(repoRoot, "bootstrap", "init", "--regenerate"); err != nil {
		return fmt.Errorf("bootstrap init: %w\n%s", err, out)
	}
	// Deploy/update. On a live stack with no template changes this is
	// effectively a no-op (UpdateStack returns "No updates are to be
	// performed" which horde treats as success).
	if out, err := runHordeTool(repoRoot, "bootstrap", "deploy"); err != nil {
		return fmt.Errorf("bootstrap deploy: %w\n%s", err, out)
	}
	// Push the image. Cheap when ECR already has the same digest.
	if out, err := runHordeTool(repoRoot, "push"); err != nil {
		return fmt.Errorf("push: %w\n%s", err, out)
	}
	return nil
}

// destroyECSStack tears down the bootstrap stack. Errors are printed but
// not returned — test results should not be gated on teardown.
func destroyECSStack(repoRoot string) {
	if out, err := runHordeTool(repoRoot, "bootstrap", "destroy", "--force"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: bootstrap destroy failed: %v\n%s\n", err, out)
	}
}

// runHordeTool runs the freshly-built horde binary with cwd=repoRoot,
// inheriting the current environment so AWS credentials and .env values
// (already merged in by ApplyDotEnvToProcess above) are available.
func runHordeTool(repoRoot string, args ...string) (string, error) {
	cmd := exec.Command(hordeBin, args...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	return string(out), err
}
