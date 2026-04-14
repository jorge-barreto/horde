package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// EnsureImage checks that the worker Docker image exists and is up to date.
// If the image is missing or older than the embedded worker files, it rebuilds
// automatically. Build progress is streamed to out.
func (p *DockerProvider) EnsureImage(ctx context.Context, workerFiles fs.FS, out io.Writer) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}
	workerDir := filepath.Join(homeDir, ".horde", "workerfiles")

	if err := syncWorkerFiles(workerFiles, workerDir); err != nil {
		return fmt.Errorf("syncing worker files: %w", err)
	}

	imageTime, exists, err := inspectImageTime(ctx)
	if err != nil {
		return err
	}

	if !exists {
		fmt.Fprintf(out, "Worker image %s not found. Building...\n", dockerImage)
		return buildImage(ctx, workerDir, out)
	}

	srcTime, err := newestFileMtime(workerDir)
	if err != nil {
		return fmt.Errorf("checking worker file timestamps: %w", err)
	}

	if srcTime.After(imageTime) {
		fmt.Fprintf(out, "Worker image outdated (image: %s, sources: %s). Rebuilding...\n",
			imageTime.Format("2006-01-02 15:04:05"), srcTime.Format("2006-01-02 15:04:05"))
		return buildImage(ctx, workerDir, out)
	}

	return nil
}

// syncWorkerFiles writes embedded files to dst, skipping files whose content
// already matches. This preserves mtimes for unchanged files so the staleness
// check only triggers when content actually changes.
func syncWorkerFiles(embedded fs.FS, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	entries, err := fs.ReadDir(embedded, ".")
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		data, err := fs.ReadFile(embedded, name)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", name, err)
		}

		dstPath := filepath.Join(dst, name)
		existing, err := os.ReadFile(dstPath)
		if err == nil && bytes.Equal(existing, data) {
			continue
		}

		if err := os.WriteFile(dstPath, data, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
	}

	return nil
}

func inspectImageTime(ctx context.Context) (time.Time, bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", dockerImage, "--format", "{{.Created}}")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("running docker: %w", err)
	}

	ts := strings.TrimSpace(string(out))
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("parsing image timestamp %q: %w", ts, err)
		}
	}
	return t, true, nil
}

func newestFileMtime(dir string) (time.Time, error) {
	var newest time.Time
	entries, err := os.ReadDir(dir)
	if err != nil {
		return newest, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return newest, err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest, nil
}

func buildImage(ctx context.Context, contextDir string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", dockerImage, contextDir)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building worker image: %w", err)
	}
	return nil
}
