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

const baseImage = "horde-worker-base:latest"

// EnsureImage ensures the worker image is ready. It manages two layers:
//
//  1. Base image (horde-worker-base:latest) — built from embedded files (orc, claude, bd, entrypoint).
//  2. Project image (horde-worker:latest) — if projectDir/worker/Dockerfile exists, built from that
//     (typically FROM horde-worker-base:latest + project tools). Otherwise the base is tagged directly.
func (p *DockerProvider) EnsureImage(ctx context.Context, workerFiles fs.FS, projectDir string, out io.Writer) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}
	workerDir := filepath.Join(homeDir, ".horde", "workerfiles")

	if err := syncWorkerFiles(workerFiles, workerDir); err != nil {
		return fmt.Errorf("syncing worker files: %w", err)
	}

	// --- Base image ---
	if err := ensureBaseImage(ctx, workerDir, out); err != nil {
		return err
	}

	// --- Project image ---
	projectDockerfile := filepath.Join(projectDir, "worker", "Dockerfile")
	if _, err := os.Stat(projectDockerfile); err == nil {
		return ensureProjectImage(ctx, p.Image, filepath.Join(projectDir, "worker"), out)
	}

	// No project Dockerfile — tag base as the run image
	return tagImage(ctx, baseImage, p.Image)
}

func ensureBaseImage(ctx context.Context, workerDir string, out io.Writer) error {
	imageTime, exists, err := inspectImageTimeOf(ctx, baseImage)
	if err != nil {
		return err
	}

	if !exists {
		fmt.Fprintf(out, "Base image %s not found. Building...\n", baseImage)
		return buildImageAs(ctx, baseImage, workerDir, out)
	}

	srcTime, err := newestFileMtime(workerDir)
	if err != nil {
		return fmt.Errorf("checking worker file timestamps: %w", err)
	}

	if srcTime.After(imageTime) {
		fmt.Fprintf(out, "Base image outdated. Rebuilding...\n")
		return buildImageAs(ctx, baseImage, workerDir, out)
	}

	return nil
}

func ensureProjectImage(ctx context.Context, image, projectWorkerDir string, out io.Writer) error {
	projectTime, projectExists, err := inspectImageTimeOf(ctx, image)
	if err != nil {
		return err
	}

	// Rebuild if project image is missing
	if !projectExists {
		fmt.Fprintf(out, "Project image %s not found. Building...\n", image)
		return buildImageAs(ctx, image, projectWorkerDir, out)
	}

	// Rebuild if base is newer than project image
	baseTime, baseExists, err := inspectImageTimeOf(ctx, baseImage)
	if err != nil {
		return err
	}
	if baseExists && baseTime.After(projectTime) {
		fmt.Fprintf(out, "Project image outdated (base rebuilt). Rebuilding...\n")
		return buildImageAs(ctx, image, projectWorkerDir, out)
	}

	// Rebuild if project sources are newer than project image
	srcTime, err := newestFileMtime(projectWorkerDir)
	if err != nil {
		return fmt.Errorf("checking project worker file timestamps: %w", err)
	}
	if srcTime.After(projectTime) {
		fmt.Fprintf(out, "Project image outdated (worker/ changed). Rebuilding...\n")
		return buildImageAs(ctx, image, projectWorkerDir, out)
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

func inspectImageTimeOf(ctx context.Context, image string) (time.Time, bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", image, "--format", "{{.Created}}")
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

func buildImageAs(ctx context.Context, tag, contextDir string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", tag, contextDir)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building image %s: %w", tag, err)
	}
	return nil
}

func tagImage(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "docker", "tag", src, dst)
	if _, err := cmd.Output(); err != nil {
		return fmt.Errorf("tagging %s as %s: %w", src, dst, err)
	}
	return nil
}
