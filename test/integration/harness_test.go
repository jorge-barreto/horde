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

	os.Exit(m.Run())
}
