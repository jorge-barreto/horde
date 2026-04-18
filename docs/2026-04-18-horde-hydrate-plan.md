# horde hydrate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `horde hydrate <run-id>... --into <dir>` which materializes `.orc/audit/` and `.orc/artifacts/` trees from completed runs into a local directory so the user can run `orc improve` / `orc doctor` against them.

**Architecture:** New `HydrateRun` method on the `Provider` interface. Docker implementation copies from `~/.horde/results/<run-id>/{audit,artifacts}/` on the local filesystem. ECS implementation lists and downloads objects from `s3://<bucket>/horde-runs/<run-id>/{audit,artifacts}/`. The `cmd/horde/hydrate.go` command is responsible for looking up runs, assembling destination paths with a `<ticket>-<run-id>` segment, skipping when the destination already exists, and aggregating per-run outcomes with a non-zero exit on partial failure.

**Tech Stack:** Go 1.24, `github.com/urfave/cli/v3`, AWS SDK v2 (`aws-sdk-go-v2/service/s3`), standard library `os`, `io`, `io/fs`, `path/filepath`.

**Spec:** `docs/2026-04-17-horde-hydrate-design.md`

---

## File Structure

Files created:

- `cmd/horde/hydrate.go` — command definition, argument parsing, per-run orchestration, exit-code aggregation.
- `cmd/horde/hydrate_test.go` — unit tests for path assembly and aggregation helpers.
- `internal/provider/hydrate_common.go` — shared helpers (`copyLocalTree`).
- `internal/provider/hydrate_common_test.go` — unit tests for `copyLocalTree`.
- `test/integration/hydrate_test.go` — end-to-end integration test against Docker provider.

Files modified:

- `internal/provider/provider.go` — add `HydrateRun` to `Provider` interface.
- `internal/provider/docker.go` — implement `(*DockerProvider).HydrateRun`.
- `internal/provider/docker_test.go` — unit tests for Docker `HydrateRun`.
- `internal/provider/ecs.go` — extend `S3Client` interface with `ListObjectsV2`; implement `(*ECSProvider).HydrateRun`.
- `internal/provider/ecs_test.go` — extend `fakeS3Client` with `ListObjectsV2`; unit tests for ECS `HydrateRun`.
- `cmd/horde/main.go` — register `hydrateCmd()` in `newApp()`.
- `internal/docs/content.go` — new `hydrate` doc topic.
- `SPEC.md` — brief section covering `horde hydrate`.

Guiding decomposition: path-assembly logic lives only in the command layer (one place, easy to test); providers only move bytes from a provider-specific source to two pre-formed destination directories.

---

## Task 1: Add `HydrateRun` to the Provider interface

**Files:**
- Modify: `internal/provider/provider.go`

- [ ] **Step 1: Add method to Provider interface**

In `internal/provider/provider.go`, extend the `Provider` interface and add a doc comment. Replace the existing interface block:

```go
// Provider abstracts container/task lifecycle operations.
type Provider interface {
	Launch(ctx context.Context, opts LaunchOpts) (*LaunchResult, error)
	Status(ctx context.Context, instanceID string) (*InstanceStatus, error)
	Logs(ctx context.Context, instanceID string, follow bool) (io.ReadCloser, error)
	Stop(ctx context.Context, opts StopOpts) error
	ReadFile(ctx context.Context, opts ReadFileOpts) ([]byte, error)
	HydrateRun(ctx context.Context, opts HydrateOpts) error
}

// HydrateOpts contains parameters for copying a run's audit and artifacts
// trees to local destination directories.
//
// The caller is responsible for assembling DestAuditDir and DestArtifactsDir
// (including any <ticket>-<run-id> or workflow-prefix segments). Providers
// only move bytes — they do not interpret run fields.
//
// If the source data does not exist for this run, implementations return
// *FileNotFoundError with Path set to a human-readable description of
// what was missing (e.g. "results for run abc123").
type HydrateOpts struct {
	RunID            string            // run ID (used by ECS to resolve S3 prefix)
	InstanceID       string            // container ID or ECS task ARN
	Metadata         map[string]string // provider-specific metadata from LaunchResult
	DestAuditDir     string            // absolute destination for .orc/audit/<ticket>-<run-id>
	DestArtifactsDir string            // absolute destination for .orc/artifacts/<ticket>-<run-id>
}
```

- [ ] **Step 2: Verify build fails because implementations are missing**

Run: `go build ./...`
Expected: FAIL — `*DockerProvider` and `*ECSProvider` do not implement `Provider` (missing `HydrateRun`).

- [ ] **Step 3: Commit**

```bash
git add internal/provider/provider.go
git commit -m "add HydrateRun to Provider interface"
```

---

## Task 2: Shared helper — `copyLocalTree`

**Files:**
- Create: `internal/provider/hydrate_common.go`
- Create: `internal/provider/hydrate_common_test.go`

This helper is used by the Docker implementation directly, and the ECS implementation streams S3 objects through it via temp files (or directly — see Task 4). Putting it in its own file keeps it unit-testable in isolation.

- [ ] **Step 1: Write failing unit tests**

Create `internal/provider/hydrate_common_test.go`:

```go
package provider

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyLocalTree_Success(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	// src/
	//   a.txt
	//   sub/b.txt
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "out")
	if err := copyLocalTree(src, dst); err != nil {
		t.Fatalf("copyLocalTree returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil || string(got) != "hello" {
		t.Errorf("a.txt: got %q err=%v, want %q", got, err, "hello")
	}
	got, err = os.ReadFile(filepath.Join(dst, "sub", "b.txt"))
	if err != nil || string(got) != "world" {
		t.Errorf("sub/b.txt: got %q err=%v, want %q", got, err, "world")
	}
}

func TestCopyLocalTree_SourceMissing(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "does-not-exist")
	dst := filepath.Join(t.TempDir(), "out")

	err := copyLocalTree(src, dst)
	var nf *FileNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("copyLocalTree: got err=%v, want *FileNotFoundError", err)
	}
}

func TestCopyLocalTree_EmptySource(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")

	if err := copyLocalTree(src, dst); err != nil {
		t.Fatalf("empty source should succeed, got: %v", err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("reading dst: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("dst should be empty, got %d entries", len(entries))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/provider/ -run TestCopyLocalTree -count=1`
Expected: FAIL — `copyLocalTree` undefined.

- [ ] **Step 3: Implement `copyLocalTree`**

Create `internal/provider/hydrate_common.go`:

```go
package provider

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// copyLocalTree recursively copies the directory tree rooted at src into dst.
// dst is created if it doesn't exist. Returns *FileNotFoundError if src does
// not exist. An empty src directory is valid and results in an empty dst.
func copyLocalTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return &FileNotFoundError{Path: src, Err: err}
		}
		return fmt.Errorf("stat source: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory: %s", src)
	}

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("creating destination: %w", err)
	}

	return filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("computing relative path: %w", err)
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyRegularFile(path, target, info.Mode().Perm())
	})
}

func copyRegularFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating parent dir for %s: %w", dst, err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copying to %s: %w", dst, err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/provider/ -run TestCopyLocalTree -count=1 -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/provider/hydrate_common.go internal/provider/hydrate_common_test.go
git commit -m "add copyLocalTree helper for hydrate"
```

---

## Task 3: Implement `(*DockerProvider).HydrateRun`

**Files:**
- Modify: `internal/provider/docker.go`
- Modify: `internal/provider/docker_test.go`

- [ ] **Step 1: Write failing unit tests**

Append to `internal/provider/docker_test.go`:

```go
func TestDockerProvider_HydrateRun_Success(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	resultsDir := filepath.Join(home, ".horde", "results", "abc123")
	if err := os.MkdirAll(filepath.Join(resultsDir, "audit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(resultsDir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultsDir, "audit", "run-result.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultsDir, "artifacts", "output.txt"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	destBase := t.TempDir()
	destAudit := filepath.Join(destBase, "audit")
	destArtifacts := filepath.Join(destBase, "artifacts")

	p := NewDockerProvider()
	err := p.HydrateRun(context.Background(), HydrateOpts{
		RunID:            "abc123",
		DestAuditDir:     destAudit,
		DestArtifactsDir: destArtifacts,
	})
	if err != nil {
		t.Fatalf("HydrateRun: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destAudit, "run-result.json"))
	if err != nil || string(got) != `{"ok":true}` {
		t.Errorf("audit: got %q err=%v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(destArtifacts, "output.txt"))
	if err != nil || string(got) != "bytes" {
		t.Errorf("artifacts: got %q err=%v", got, err)
	}
}

func TestDockerProvider_HydrateRun_ResultsMissing(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	dest := t.TempDir()
	p := NewDockerProvider()
	err := p.HydrateRun(context.Background(), HydrateOpts{
		RunID:            "nope",
		DestAuditDir:     filepath.Join(dest, "audit"),
		DestArtifactsDir: filepath.Join(dest, "artifacts"),
	})
	var nf *FileNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want *FileNotFoundError, got %v", err)
	}
}

func TestDockerProvider_HydrateRun_InvalidRunID(t *testing.T) {
	t.Parallel()
	p := NewDockerProvider()
	for _, bad := range []string{"", "../etc", "a/b", "a\\b"} {
		err := p.HydrateRun(context.Background(), HydrateOpts{
			RunID:            bad,
			DestAuditDir:     "/tmp/x/a",
			DestArtifactsDir: "/tmp/x/b",
		})
		if err == nil {
			t.Errorf("run id %q should be rejected", bad)
		}
	}
}

func TestDockerProvider_HydrateRun_AuditOnly(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	resultsDir := filepath.Join(home, ".horde", "results", "abc123")
	// Only audit/ exists — artifacts/ never created.
	if err := os.MkdirAll(filepath.Join(resultsDir, "audit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultsDir, "audit", "r.json"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	destBase := t.TempDir()
	p := NewDockerProvider()
	err := p.HydrateRun(context.Background(), HydrateOpts{
		RunID:            "abc123",
		DestAuditDir:     filepath.Join(destBase, "audit"),
		DestArtifactsDir: filepath.Join(destBase, "artifacts"),
	})
	if err != nil {
		t.Fatalf("missing artifacts subdir should not fail: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destBase, "audit", "r.json")); err != nil {
		t.Errorf("audit file not copied: %v", err)
	}
}
```

Make sure `"errors"`, `"context"`, `"os"`, `"path/filepath"` are already imported in `docker_test.go`; add whatever's missing.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/provider/ -run TestDockerProvider_HydrateRun -count=1`
Expected: FAIL — method `HydrateRun` undefined on `*DockerProvider`.

- [ ] **Step 3: Implement `HydrateRun` on `DockerProvider`**

Append to `internal/provider/docker.go` (below `ReadFile`):

```go
// HydrateRun copies a run's audit and artifacts trees from the local results
// store (~/.horde/results/<run-id>/{audit,artifacts}/) to the caller-supplied
// destination dirs. Returns *FileNotFoundError if the results dir for this
// run does not exist. A missing audit/ or artifacts/ subdirectory individually
// is treated as empty (some runs don't produce both).
func (p *DockerProvider) HydrateRun(ctx context.Context, opts HydrateOpts) error {
	if opts.RunID == "" {
		return fmt.Errorf("hydrating run: run ID is required")
	}
	if strings.ContainsAny(opts.RunID, "/\\") || strings.Contains(opts.RunID, "..") {
		return fmt.Errorf("hydrating run: invalid run ID")
	}
	if opts.DestAuditDir == "" || opts.DestArtifactsDir == "" {
		return fmt.Errorf("hydrating run: destination directories are required")
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("hydrating run: %w", err)
	}
	resultsDir := filepath.Join(homeDir, ".horde", "results", opts.RunID)
	if _, err := os.Stat(resultsDir); err != nil {
		if os.IsNotExist(err) {
			return &FileNotFoundError{Path: resultsDir, Err: err}
		}
		return fmt.Errorf("hydrating run: %w", err)
	}

	srcAudit := filepath.Join(resultsDir, "audit")
	if _, err := os.Stat(srcAudit); err == nil {
		if err := copyLocalTree(srcAudit, opts.DestAuditDir); err != nil {
			return fmt.Errorf("hydrating audit: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat audit source: %w", err)
	}

	srcArtifacts := filepath.Join(resultsDir, "artifacts")
	if _, err := os.Stat(srcArtifacts); err == nil {
		if err := copyLocalTree(srcArtifacts, opts.DestArtifactsDir); err != nil {
			return fmt.Errorf("hydrating artifacts: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat artifacts source: %w", err)
	}

	return nil
}
```

`ctx` is accepted for symmetry with other methods and may be used later for cancellation-aware copies; unused here is fine — Go will not warn.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/provider/ -run TestDockerProvider_HydrateRun -count=1 -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/provider/docker.go internal/provider/docker_test.go
git commit -m "implement DockerProvider.HydrateRun"
```

---

## Task 4: Implement `(*ECSProvider).HydrateRun`

**Files:**
- Modify: `internal/provider/ecs.go`
- Modify: `internal/provider/ecs_test.go`

- [ ] **Step 1: Extend `S3Client` interface and `fakeS3Client`**

In `internal/provider/ecs.go`, extend the `S3Client` interface:

```go
type S3Client interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}
```

In `internal/provider/ecs_test.go`, extend `fakeS3Client` so existing tests still compile. Replace the struct and add the method:

```go
type fakeS3Client struct {
	getObjectInput  *s3.GetObjectInput
	getObjectOutput *s3.GetObjectOutput
	getObjectErr    error

	// ListObjectsV2: keyed by prefix for simple per-prefix responses.
	// Values are the flat list of keys that live under that prefix.
	listKeys    map[string][]string
	listErr     error
	// getObjectByKey overrides getObjectOutput for specific keys during hydrate tests.
	getObjectByKey map[string]string
}

func (f *fakeS3Client) GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.getObjectInput = params
	if f.getObjectErr != nil {
		return nil, f.getObjectErr
	}
	if f.getObjectByKey != nil {
		if body, ok := f.getObjectByKey[aws.ToString(params.Key)]; ok {
			return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(body))}, nil
		}
	}
	if f.getObjectOutput != nil {
		return f.getObjectOutput, nil
	}
	return nil, nil
}

func (f *fakeS3Client) ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	prefix := aws.ToString(params.Prefix)
	keys := f.listKeys[prefix]
	out := &s3.ListObjectsV2Output{}
	for _, k := range keys {
		k := k
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
	}
	return out, nil
}
```

Add any missing imports to the test file: `io`, `strings`, and make sure `s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"` is imported (it already is — verify).

- [ ] **Step 2: Write failing unit tests for ECS HydrateRun**

Append to `internal/provider/ecs_test.go`:

```go
func TestECSProvider_HydrateRun_Success(t *testing.T) {
	t.Parallel()

	fake := &fakeS3Client{
		listKeys: map[string][]string{
			"horde-runs/abc123/audit/":     {"horde-runs/abc123/audit/run-result.json", "horde-runs/abc123/audit/nested/timing.json"},
			"horde-runs/abc123/artifacts/": {"horde-runs/abc123/artifacts/output.txt"},
		},
		getObjectByKey: map[string]string{
			"horde-runs/abc123/audit/run-result.json":   `{"ok":true}`,
			"horde-runs/abc123/audit/nested/timing.json": `{"phase":"plan"}`,
			"horde-runs/abc123/artifacts/output.txt":    "bytes",
		},
	}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())

	destBase := t.TempDir()
	err := p.HydrateRun(context.Background(), HydrateOpts{
		RunID:            "abc123",
		Metadata:         map[string]string{"artifacts_bucket": "my-horde-artifacts"},
		DestAuditDir:     filepath.Join(destBase, "audit"),
		DestArtifactsDir: filepath.Join(destBase, "artifacts"),
	})
	if err != nil {
		t.Fatalf("HydrateRun: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destBase, "audit", "run-result.json"))
	if err != nil || string(got) != `{"ok":true}` {
		t.Errorf("audit: got %q err=%v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(destBase, "audit", "nested", "timing.json"))
	if err != nil || string(got) != `{"phase":"plan"}` {
		t.Errorf("nested audit: got %q err=%v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(destBase, "artifacts", "output.txt"))
	if err != nil || string(got) != "bytes" {
		t.Errorf("artifacts: got %q err=%v", got, err)
	}
}

func TestECSProvider_HydrateRun_NoObjects(t *testing.T) {
	t.Parallel()

	fake := &fakeS3Client{listKeys: map[string][]string{}}
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, fake, testHordeConfig())

	destBase := t.TempDir()
	err := p.HydrateRun(context.Background(), HydrateOpts{
		RunID:            "abc123",
		Metadata:         map[string]string{"artifacts_bucket": "my-horde-artifacts"},
		DestAuditDir:     filepath.Join(destBase, "audit"),
		DestArtifactsDir: filepath.Join(destBase, "artifacts"),
	})
	var nf *FileNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want *FileNotFoundError when both prefixes empty, got: %v", err)
	}
}

func TestECSProvider_HydrateRun_MissingBucket(t *testing.T) {
	t.Parallel()

	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	destBase := t.TempDir()
	err := p.HydrateRun(context.Background(), HydrateOpts{
		RunID:            "abc123",
		Metadata:         nil,
		DestAuditDir:     filepath.Join(destBase, "audit"),
		DestArtifactsDir: filepath.Join(destBase, "artifacts"),
	})
	if err == nil || !strings.Contains(err.Error(), "artifacts_bucket") {
		t.Fatalf("expected bucket-missing error, got: %v", err)
	}
}

func TestECSProvider_HydrateRun_InvalidRunID(t *testing.T) {
	t.Parallel()
	p := NewECSProvider(&fakeECSClient{}, &fakeCloudWatchLogsClient{}, &fakeS3Client{}, testHordeConfig())
	for _, bad := range []string{"", "../etc", "a/b", "a\\b"} {
		err := p.HydrateRun(context.Background(), HydrateOpts{
			RunID:            bad,
			Metadata:         map[string]string{"artifacts_bucket": "b"},
			DestAuditDir:     "/tmp/x/a",
			DestArtifactsDir: "/tmp/x/b",
		})
		if err == nil {
			t.Errorf("run id %q should be rejected", bad)
		}
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/provider/ -run TestECSProvider_HydrateRun -count=1`
Expected: FAIL — method undefined.

- [ ] **Step 4: Implement `HydrateRun` on `ECSProvider`**

Append to `internal/provider/ecs.go` (below `ReadFile`):

```go
// HydrateRun downloads the run's audit and artifacts trees from
// s3://<artifacts_bucket>/horde-runs/<run-id>/{audit,artifacts}/ into the
// caller-supplied destination directories. Returns *FileNotFoundError if
// no objects are found under either prefix for this run.
func (p *ECSProvider) HydrateRun(ctx context.Context, opts HydrateOpts) error {
	if opts.RunID == "" {
		return fmt.Errorf("hydrating run: run ID is required")
	}
	if strings.ContainsAny(opts.RunID, "/\\") || strings.Contains(opts.RunID, "..") {
		return fmt.Errorf("hydrating run: invalid run ID")
	}
	if opts.DestAuditDir == "" || opts.DestArtifactsDir == "" {
		return fmt.Errorf("hydrating run: destination directories are required")
	}
	bucket := ""
	if opts.Metadata != nil {
		bucket = opts.Metadata["artifacts_bucket"]
	}
	if bucket == "" {
		return fmt.Errorf("hydrating run: artifacts_bucket not found in metadata")
	}

	auditPrefix := "horde-runs/" + opts.RunID + "/audit/"
	artifactsPrefix := "horde-runs/" + opts.RunID + "/artifacts/"

	audit, err := p.downloadS3Prefix(ctx, bucket, auditPrefix, opts.DestAuditDir)
	if err != nil {
		return fmt.Errorf("hydrating audit: %w", err)
	}
	artifacts, err := p.downloadS3Prefix(ctx, bucket, artifactsPrefix, opts.DestArtifactsDir)
	if err != nil {
		return fmt.Errorf("hydrating artifacts: %w", err)
	}
	if audit == 0 && artifacts == 0 {
		return &FileNotFoundError{Path: "s3://" + bucket + "/horde-runs/" + opts.RunID + "/"}
	}
	return nil
}

// downloadS3Prefix lists all objects under prefix and writes each to
// destDir, preserving the path layout under the prefix. Returns the number
// of objects downloaded. A prefix with no objects is not an error here —
// the caller decides how to treat an empty result.
func (p *ECSProvider) downloadS3Prefix(ctx context.Context, bucket, prefix, destDir string) (int, error) {
	var token *string
	count := 0
	for {
		out, err := p.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return count, fmt.Errorf("listing s3 prefix %q: %w", prefix, err)
		}
		for _, obj := range out.Contents {
			key := aws.ToString(obj.Key)
			rel := strings.TrimPrefix(key, prefix)
			if rel == "" || strings.HasSuffix(rel, "/") {
				continue
			}
			if err := p.downloadS3Object(ctx, bucket, key, filepath.Join(destDir, rel)); err != nil {
				return count, err
			}
			count++
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return count, nil
}

func (p *ECSProvider) downloadS3Object(ctx context.Context, bucket, key, destPath string) error {
	out, err := p.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("getting s3://%s/%s: %w", bucket, key, err)
	}
	if out == nil || out.Body == nil {
		return fmt.Errorf("getting s3://%s/%s: nil response", bucket, key)
	}
	defer out.Body.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("creating parent dir for %s: %w", destPath, err)
	}
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", destPath, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, out.Body); err != nil {
		return fmt.Errorf("writing %s: %w", destPath, err)
	}
	return nil
}
```

Make sure `os`, `io`, `filepath`, `strings`, `aws`, and `s3` are already imported in `ecs.go`. If not, add them.

- [ ] **Step 5: Run ECS tests to verify new ones pass and existing ones still pass**

Run: `go test ./internal/provider/ -run TestECSProvider -count=1 -v`
Expected: all PASS (existing + 4 new hydrate tests).

- [ ] **Step 6: Run full provider package**

Run: `go test ./internal/provider/ -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/provider/ecs.go internal/provider/ecs_test.go
git commit -m "implement ECSProvider.HydrateRun"
```

---

## Task 5: Path-assembly helper for hydrate command

**Files:**
- Create: `cmd/horde/hydrate.go` (skeleton — only helper in this task)
- Create: `cmd/horde/hydrate_test.go`

Extracting `hydrateDestPaths` as a pure function lets us unit-test path assembly without mocking a store.

- [ ] **Step 1: Write failing unit tests**

Create `cmd/horde/hydrate_test.go`:

```go
package main

import (
	"path/filepath"
	"testing"
)

func TestHydrateDestPaths_DefaultWorkflow(t *testing.T) {
	t.Parallel()
	auditDir, artDir := hydrateDestPaths("/out", "", "PROJ-123", "abc123")
	wantAudit := filepath.Join("/out", ".orc", "audit", "PROJ-123-abc123")
	wantArt := filepath.Join("/out", ".orc", "artifacts", "PROJ-123-abc123")
	if auditDir != wantAudit {
		t.Errorf("audit dir: got %q want %q", auditDir, wantAudit)
	}
	if artDir != wantArt {
		t.Errorf("artifacts dir: got %q want %q", artDir, wantArt)
	}
}

func TestHydrateDestPaths_NamedWorkflow(t *testing.T) {
	t.Parallel()
	auditDir, artDir := hydrateDestPaths("/out", "review", "PROJ-123", "abc123")
	wantAudit := filepath.Join("/out", ".orc", "audit", "review", "PROJ-123-abc123")
	wantArt := filepath.Join("/out", ".orc", "artifacts", "review", "PROJ-123-abc123")
	if auditDir != wantAudit {
		t.Errorf("audit dir: got %q want %q", auditDir, wantAudit)
	}
	if artDir != wantArt {
		t.Errorf("artifacts dir: got %q want %q", artDir, wantArt)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/horde/ -run TestHydrateDestPaths -count=1`
Expected: FAIL — `hydrateDestPaths` undefined.

- [ ] **Step 3: Create the skeleton and implement the helper**

Create `cmd/horde/hydrate.go`:

```go
package main

import (
	"path/filepath"
)

// hydrateDestPaths returns (auditDir, artifactsDir) for hydrating a run
// into intoDir. Layout matches orc's expected tree with the single twist
// that the leaf ticket segment is "<ticket>-<run-id>" to prevent collisions
// across multiple runs of the same ticket.
func hydrateDestPaths(intoDir, workflow, ticket, runID string) (audit, artifacts string) {
	leaf := ticket + "-" + runID
	if workflow == "" {
		audit = filepath.Join(intoDir, ".orc", "audit", leaf)
		artifacts = filepath.Join(intoDir, ".orc", "artifacts", leaf)
		return
	}
	audit = filepath.Join(intoDir, ".orc", "audit", workflow, leaf)
	artifacts = filepath.Join(intoDir, ".orc", "artifacts", workflow, leaf)
	return
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/horde/ -run TestHydrateDestPaths -count=1 -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/horde/hydrate.go cmd/horde/hydrate_test.go
git commit -m "add hydrateDestPaths helper"
```

---

## Task 6: Hydrate outcome aggregation + per-run orchestration helpers

**Files:**
- Modify: `cmd/horde/hydrate.go`
- Modify: `cmd/horde/hydrate_test.go`

These helpers encode the partial-failure semantics from the spec. They stay close to `hydrateDestPaths` so the command body in Task 7 is short.

- [ ] **Step 1: Write failing unit tests**

Append to `cmd/horde/hydrate_test.go`:

```go
import (
	"bytes"
	"errors"

	"github.com/jorge-barreto/horde/internal/store"
)

func TestHydrateSummary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		outcomes []hydrateOutcome
		want     string
	}{
		{
			name: "all hydrated",
			outcomes: []hydrateOutcome{
				{RunID: "a", Status: hydrateStatusHydrated},
				{RunID: "b", Status: hydrateStatusHydrated},
			},
			want: "hydrated: 2, skipped: 0, failed: 0",
		},
		{
			name: "mixed",
			outcomes: []hydrateOutcome{
				{RunID: "a", Status: hydrateStatusHydrated},
				{RunID: "b", Status: hydrateStatusSkipped},
				{RunID: "c", Status: hydrateStatusFailed, Err: errors.New("boom")},
			},
			want: "hydrated: 1, skipped: 1, failed: 1",
		},
	}
	for _, tc := range cases {
		got := hydrateSummary(tc.outcomes)
		if got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestHydrateHasFailure(t *testing.T) {
	t.Parallel()
	if hydrateHasFailure([]hydrateOutcome{{Status: hydrateStatusHydrated}, {Status: hydrateStatusSkipped}}) {
		t.Error("no failures should report false")
	}
	if !hydrateHasFailure([]hydrateOutcome{{Status: hydrateStatusHydrated}, {Status: hydrateStatusFailed}}) {
		t.Error("one failure should report true")
	}
}

func TestHydrateWriteFailures(t *testing.T) {
	t.Parallel()
	outcomes := []hydrateOutcome{
		{RunID: "a", Status: hydrateStatusHydrated},
		{RunID: "b", Status: hydrateStatusFailed, Err: errors.New("not found")},
	}
	var buf bytes.Buffer
	hydrateWriteFailures(&buf, outcomes)
	got := buf.String()
	if !bytes.Contains([]byte(got), []byte("b")) || !bytes.Contains([]byte(got), []byte("not found")) {
		t.Errorf("failure output missing run id or error: %q", got)
	}
	if bytes.Contains([]byte(got), []byte("a")) {
		t.Errorf("successful run id should not appear in failures: %q", got)
	}
}

func TestRunNotTerminalCheck(t *testing.T) {
	t.Parallel()
	if !isTerminalStatus(store.StatusSuccess) {
		t.Error("success should be terminal")
	}
	if !isTerminalStatus(store.StatusFailed) {
		t.Error("failed should be terminal")
	}
	if !isTerminalStatus(store.StatusKilled) {
		t.Error("killed should be terminal")
	}
	if isTerminalStatus(store.StatusRunning) {
		t.Error("running should not be terminal")
	}
	if isTerminalStatus(store.StatusPending) {
		t.Error("pending should not be terminal")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/horde/ -run 'TestHydrate(Summary|HasFailure|WriteFailures)|TestRunNotTerminalCheck' -count=1`
Expected: FAIL — symbols undefined.

- [ ] **Step 3: Add helpers to `cmd/horde/hydrate.go`**

Append to `cmd/horde/hydrate.go`:

```go
import (
	"fmt"
	"io"

	"github.com/jorge-barreto/horde/internal/store"
)

type hydrateStatus string

const (
	hydrateStatusHydrated hydrateStatus = "hydrated"
	hydrateStatusSkipped  hydrateStatus = "skipped"
	hydrateStatusFailed   hydrateStatus = "failed"
)

type hydrateOutcome struct {
	RunID  string
	Status hydrateStatus
	Err    error
}

func hydrateSummary(outs []hydrateOutcome) string {
	var h, s, f int
	for _, o := range outs {
		switch o.Status {
		case hydrateStatusHydrated:
			h++
		case hydrateStatusSkipped:
			s++
		case hydrateStatusFailed:
			f++
		}
	}
	return fmt.Sprintf("hydrated: %d, skipped: %d, failed: %d", h, s, f)
}

func hydrateHasFailure(outs []hydrateOutcome) bool {
	for _, o := range outs {
		if o.Status == hydrateStatusFailed {
			return true
		}
	}
	return false
}

func hydrateWriteFailures(w io.Writer, outs []hydrateOutcome) {
	for _, o := range outs {
		if o.Status != hydrateStatusFailed {
			continue
		}
		fmt.Fprintf(w, "  %s: %v\n", o.RunID, o.Err)
	}
}

func isTerminalStatus(s store.Status) bool {
	switch s {
	case store.StatusSuccess, store.StatusFailed, store.StatusKilled:
		return true
	default:
		return false
	}
}
```

Note: Go supports multiple `import` blocks, but since `hydrate.go` already has one with `path/filepath`, merge the new imports into it. Final import block:

```go
import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/jorge-barreto/horde/internal/store"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/horde/ -run 'TestHydrate(Summary|HasFailure|WriteFailures|DestPaths)|TestRunNotTerminalCheck' -count=1 -v`
Expected: PASS (all).

- [ ] **Step 5: Commit**

```bash
git add cmd/horde/hydrate.go cmd/horde/hydrate_test.go
git commit -m "add hydrate outcome aggregation helpers"
```

---

## Task 7: Wire up `horde hydrate` command

**Files:**
- Modify: `cmd/horde/hydrate.go`
- Modify: `cmd/horde/main.go`

- [ ] **Step 1: Append the command constructor**

Append to `cmd/horde/hydrate.go`:

```go
import (
	"context"
	"errors"
	"os"

	"github.com/jorge-barreto/horde/internal/provider"
	"github.com/urfave/cli/v3"
)

func hydrateCmd() *cli.Command {
	return hydrateCmdWith(defaultFactoryDeps())
}

func hydrateCmdWith(deps factoryDeps) *cli.Command {
	return &cli.Command{
		Name:      "hydrate",
		Usage:     "Copy run artifacts to a local directory for orc improve/doctor",
		ArgsUsage: "<run-id> [<run-id>...] --into <dir>",
		Description: `Materializes .orc/audit/ and .orc/artifacts/ from one or more
completed runs into --into <dir>. Each run is written under a
"<ticket>-<run-id>" leaf segment to avoid collisions across runs of
the same ticket:

  <dir>/.orc/audit/<ticket>-<run-id>/...
  <dir>/.orc/artifacts/<ticket>-<run-id>/...

For runs that used a named workflow, the workflow name is inserted
before the leaf, matching orc's named-workflow layout.

Runs whose destination subdirectory already exists are skipped. To
re-hydrate, delete the subdirectory and re-run.

Exit 0 if all run-ids were hydrated or skipped. Exit non-zero if any
run-id failed (missing, still running, transport error, etc.); the
successful runs are still materialized.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "into",
				Usage:    "Destination directory (will be created)",
				Required: true,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			into := cmd.String("into")
			runIDs := cmd.Args().Slice()
			if len(runIDs) == 0 {
				return fmt.Errorf("missing required argument: one or more <run-id>")
			}
			if err := os.MkdirAll(into, 0o755); err != nil {
				return fmt.Errorf("creating --into directory: %w", err)
			}

			profile := cmd.String("profile")
			provFlag := cmd.String("provider")

			outcomes := make([]hydrateOutcome, 0, len(runIDs))
			for _, runID := range runIDs {
				outcomes = append(outcomes, hydrateOne(ctx, deps, provFlag, profile, runID, into))
			}

			if failures := filterFailures(outcomes); len(failures) > 0 {
				fmt.Fprintln(os.Stderr, "failures:")
				hydrateWriteFailures(os.Stderr, outcomes)
			}
			fmt.Println(hydrateSummary(outcomes))

			if hydrateHasFailure(outcomes) {
				return cli.Exit("", 1)
			}
			return nil
		},
	}
}

func filterFailures(outs []hydrateOutcome) []hydrateOutcome {
	var out []hydrateOutcome
	for _, o := range outs {
		if o.Status == hydrateStatusFailed {
			out = append(out, o)
		}
	}
	return out
}

func hydrateOne(ctx context.Context, deps factoryDeps, provFlag, profile, runID, into string) hydrateOutcome {
	prov, _, run, cleanup, err := initFromRunIDWith(ctx, provFlag, profile, runID, deps)
	if err != nil {
		return hydrateOutcome{RunID: runID, Status: hydrateStatusFailed, Err: err}
	}
	defer cleanup()

	if !isTerminalStatus(run.Status) {
		return hydrateOutcome{RunID: runID, Status: hydrateStatusFailed,
			Err: fmt.Errorf("run is not terminal (status: %s)", run.Status)}
	}

	auditDir, artifactsDir := hydrateDestPaths(into, run.Workflow, run.Ticket, run.ID)

	auditExists := dirExists(auditDir)
	artifactsExists := dirExists(artifactsDir)
	if auditExists && artifactsExists {
		return hydrateOutcome{RunID: runID, Status: hydrateStatusSkipped}
	}

	if err := prov.HydrateRun(ctx, provider.HydrateOpts{
		RunID:            run.ID,
		InstanceID:       run.InstanceID,
		Metadata:         run.Metadata,
		DestAuditDir:     auditDir,
		DestArtifactsDir: artifactsDir,
	}); err != nil {
		var nf *provider.FileNotFoundError
		if errors.As(err, &nf) {
			return hydrateOutcome{RunID: runID, Status: hydrateStatusFailed,
				Err: fmt.Errorf("artifacts not available: %w", err)}
		}
		return hydrateOutcome{RunID: runID, Status: hydrateStatusFailed, Err: err}
	}
	return hydrateOutcome{RunID: runID, Status: hydrateStatusHydrated}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
```

Merge imports so the final `import` block at the top of `hydrate.go` is a single block:

```go
import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jorge-barreto/horde/internal/provider"
	"github.com/jorge-barreto/horde/internal/store"
	"github.com/urfave/cli/v3"
)
```

- [ ] **Step 2: Register command in `main.go`**

In `cmd/horde/main.go`, inside `newApp()`'s `Commands` slice, add `hydrateCmd()` — placing it next to `resultsCmd()`:

```go
Commands: []*cli.Command{
    launchCmd(),
    retryCmd(),
    statusCmd(),
    logsCmd(),
    killCmd(),
    resultsCmd(),
    hydrateCmd(),
    listCmd(),
    cleanCmd(),
    shellCmd(),
    docsCmd(),
},
```

- [ ] **Step 3: Build**

Run: `go build ./cmd/horde/`
Expected: build succeeds.

- [ ] **Step 4: Smoke test — help text renders**

Run: `./horde hydrate --help`
Expected: output containing "Copy run artifacts to a local directory" and `--into` flag. If the binary isn't in cwd, build with `go build -o /tmp/horde ./cmd/horde/` and invoke that.

- [ ] **Step 5: Smoke test — missing args**

Run: `./horde hydrate --into /tmp/x`
Expected: error `missing required argument: one or more <run-id>`, non-zero exit.

- [ ] **Step 6: Run all unit tests in affected packages**

Run: `go test -short ./... -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/horde/hydrate.go cmd/horde/main.go
git commit -m "add horde hydrate command"
```

---

## Task 8: Integration test — Docker provider end-to-end

**Files:**
- Create: `test/integration/hydrate_test.go`

This test uses the existing integration harness to: launch a short run, wait for it to stop, invoke `horde hydrate` against a fresh temp `--into` dir, and assert the expected tree layout + file content. It also covers the idempotent-skip case and the partial-failure case with a mix of valid + invalid run-ids.

- [ ] **Step 1: Inspect the harness to confirm helpers available**

Read `test/integration/harness_test.go` for `Launch`, `WaitForOrc`, `runHordeFull`. No changes needed; the test will shell out to the horde binary like the others.

Run: `head -100 test/integration/lifecycle_test.go` to see the canonical pattern of launch → wait → assert files.

- [ ] **Step 2: Write the integration test**

Create `test/integration/hydrate_test.go`:

```go
package integration

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHydrate_DockerSuccess launches a short run, waits for it to complete,
// then hydrates the results into a temp dir and verifies the expected tree.
func TestHydrate_DockerSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	h := newHarness(t)

	runID := h.Launch("HYD-1", "", 5*time.Minute)
	h.WaitForOrc(runID, 5*time.Minute)

	into := t.TempDir()
	out, err := h.runHorde("--provider", "docker", "hydrate", runID, "--into", into)
	if err != nil {
		t.Fatalf("hydrate failed: %v\nstdout: %s", err, out)
	}
	if !strings.Contains(out, "hydrated: 1") {
		t.Errorf("summary missing from output: %q", out)
	}

	leaf := "HYD-1-" + runID
	auditDir := filepath.Join(into, ".orc", "audit", leaf)
	artifactsDir := filepath.Join(into, ".orc", "artifacts", leaf)

	if _, err := os.Stat(auditDir); err != nil {
		t.Errorf("audit dir not created: %v", err)
	}
	// run-result.json is written by orc on completion; expect it to exist.
	if _, err := os.Stat(filepath.Join(auditDir, "run-result.json")); err != nil {
		t.Logf("run-result.json missing (orc may not have produced it): %v", err)
	}
	if _, err := os.Stat(artifactsDir); err != nil {
		// artifacts/ may legitimately not exist for some workflows — log, don't fail.
		t.Logf("artifacts dir not created: %v", err)
	}
}

// TestHydrate_Idempotent hydrates the same run twice into the same dir and
// verifies the second run reports skipped.
func TestHydrate_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	h := newHarness(t)

	runID := h.Launch("HYD-2", "", 5*time.Minute)
	h.WaitForOrc(runID, 5*time.Minute)

	into := t.TempDir()
	if _, err := h.runHorde("--provider", "docker", "hydrate", runID, "--into", into); err != nil {
		t.Fatalf("first hydrate: %v", err)
	}
	out, err := h.runHorde("--provider", "docker", "hydrate", runID, "--into", into)
	if err != nil {
		t.Fatalf("second hydrate: %v\nstdout: %s", err, out)
	}
	if !strings.Contains(out, "skipped: 1") {
		t.Errorf("expected skipped: 1 on second hydrate, got: %q", out)
	}
}

// TestHydrate_PartialFailure hydrates a valid run-id plus a bogus one.
// Expect exit code 1, the valid run materialized, and summary reflecting
// hydrated: 1, failed: 1.
func TestHydrate_PartialFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	h := newHarness(t)

	runID := h.Launch("HYD-3", "", 5*time.Minute)
	h.WaitForOrc(runID, 5*time.Minute)

	into := t.TempDir()
	_, err := h.runHorde("--provider", "docker", "hydrate", runID, "does-not-exist", "--into", into)

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected non-zero exit, got err=%v", err)
	}
	if exitErr.ExitCode() == 0 {
		t.Errorf("expected non-zero exit code, got 0")
	}

	leaf := "HYD-3-" + runID
	if _, err := os.Stat(filepath.Join(into, ".orc", "audit", leaf)); err != nil {
		t.Errorf("valid run's audit dir should still be materialized: %v", err)
	}
}

// TestHydrate_RunStillRunning expects a non-terminal run to be rejected.
func TestHydrate_RunStillRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	h := newHarness(t)

	// Launch but do not wait — run is still pending/running.
	runID := h.Launch("HYD-4", "", 5*time.Minute)

	into := t.TempDir()
	_, err := h.runHorde("--provider", "docker", "hydrate", runID, "--into", into)

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected non-zero exit for non-terminal run, got err=%v", err)
	}
	if exitErr.ExitCode() == 0 {
		t.Errorf("expected non-zero exit code, got 0")
	}

	// Clean up the running container so t.Cleanup doesn't hang.
	h.WaitForOrc(runID, 5*time.Minute)
}
```

Notes:
- `h.runHorde` already returns `(string, error)` where `error` is an `*exec.ExitError` on non-zero exit — `errors.As` against `*exec.ExitError` works.
- The `Launch` harness helper uses a default workflow (none) and default timeout; the ticket name is the only distinguisher per test.
- `run-result.json` assertion is soft-logged rather than hard-failed because the integration-test entrypoint (`test/integration/test-entrypoint.sh`) may or may not produce a full run-result.json depending on what the stubbed orc writes — consult that file and tighten the assertion if it does produce one consistently.

- [ ] **Step 3: Check what the test entrypoint actually writes**

Run: `cat test/integration/test-entrypoint.sh`

If the entrypoint writes a deterministic file under `.orc/audit/<ticket>/` (e.g. an `orc-log.txt` or similar), update the assertion in `TestHydrate_DockerSuccess` to check for that file instead of / in addition to `run-result.json`. Example: if the entrypoint writes `.orc/audit/<ticket>/done.txt`, change the post-hydrate assertion to:

```go
if _, err := os.Stat(filepath.Join(auditDir, "done.txt")); err != nil {
    t.Errorf("audit file missing: %v", err)
}
```

This keeps the test strict on at least one file we know is produced.

- [ ] **Step 4: Run the integration test**

Run: `go test -v -count=1 -timeout 10m ./test/integration/ -run TestHydrate`
Expected: all 4 tests PASS (~5-10 minutes total since each launches a real container).

- [ ] **Step 5: If any test fails, diagnose and fix**

Common failure modes and what to do:

| Symptom | Likely cause | Fix |
|---|---|---|
| `audit dir not created` — stat ENOENT | Entrypoint didn't produce `.orc/audit/` for this ticket | Check `test-entrypoint.sh`; may need to weaken assertion or strengthen entrypoint |
| Second hydrate reports `hydrated: 1` not `skipped: 1` | `dirExists` check too strict (e.g. expects both dirs present) | Review Task 7 skip logic — it requires BOTH audit and artifacts dirs. If integration runs don't produce artifacts/, relax to "audit exists" |
| Partial-failure test exits 0 | Failure path not propagating non-zero exit | Verify `cli.Exit("", 1)` in action |

Specifically on the skip logic: if integration runs don't populate `.orc/artifacts/`, the skip check in `hydrateOne` that requires both dirs never triggers. Two options:

1. **Weaken the check** in `hydrateOne`: skip if `auditDir` exists (sufficient to prove this run was hydrated before).
2. **Keep the strict check** and adjust the idempotency test to force-create a marker in the artifacts dir.

Prefer option 1 — it reflects real usage (some workflows produce only audit data) and is the user-friendly behavior. If you choose option 1, update `hydrateOne`:

```go
// Skip if we've already hydrated this run (at least the audit dir exists).
if dirExists(auditDir) {
    return hydrateOutcome{RunID: runID, Status: hydrateStatusSkipped}
}
```

Re-run tests after any change.

- [ ] **Step 6: Commit**

```bash
git add test/integration/hydrate_test.go
# include hydrate.go if the skip-logic fix from Step 5 was applied
git commit -m "add hydrate integration tests"
```

---

## Task 9: Documentation — embedded docs topic and SPEC.md entry

**Files:**
- Modify: `internal/docs/content.go`
- Modify: `SPEC.md`

- [ ] **Step 1: Add the `hydrate` topic to the docs registry**

In `internal/docs/content.go`, add a new topic entry inside the `topics` slice (after `retry` or alphabetically):

```go
{
    Name:    "hydrate",
    Title:   "Hydrating Run Results",
    Summary: "Copy run artifacts locally for orc improve / orc doctor",
    Content: topicHydrate,
},
```

Then append the content constant at the bottom of the file:

```go
const topicHydrate = `Hydrating Run Results
=====================

` + "`horde hydrate`" + ` copies the .orc/audit/ and .orc/artifacts/ trees from
one or more completed runs into a local directory so you can run
` + "`orc improve`" + `, ` + "`orc doctor`" + `, or any other orc tool that operates on
a local .orc/ folder.

Synopsis

    horde hydrate <run-id> [<run-id>...] --into <dir>

Layout

Hydrated data is placed under a per-run leaf directory so multiple runs
never collide:

    <dir>/.orc/audit/<ticket>-<run-id>/...
    <dir>/.orc/artifacts/<ticket>-<run-id>/...

For runs that used a named workflow, the workflow name is inserted before
the leaf:

    <dir>/.orc/audit/<workflow>/<ticket>-<run-id>/...

Examples

Single run:

    horde hydrate abc123def456 --into /tmp/inspect
    cd /tmp/inspect
    orc improve

Weekly batch (e.g. a cron job):

    horde list --all --json \
      | jq -r '.[].id' \
      | xargs horde hydrate --into /tmp/weekly

Semantics

- Each run-id is processed independently. A failure on one does not abort
  the others.
- Runs whose destination subdirectory already exists are skipped. To
  re-hydrate, delete the subdirectory.
- Runs that are not in a terminal state (pending/running) are reported as
  failures and skipped.
- Exit 0 if all run-ids were hydrated or skipped. Exit non-zero if any
  run-id failed.

Providers

- Docker provider: copies from ~/.horde/results/<run-id>/.
- ECS provider: downloads from s3://<artifacts-bucket>/horde-runs/<run-id>/.
`
```

- [ ] **Step 2: Verify docs test still passes**

Run: `go test ./internal/docs/ -count=1`
Expected: PASS.

- [ ] **Step 3: Smoke test the topic**

Run: `go build -o /tmp/horde ./cmd/horde && /tmp/horde docs hydrate`
Expected: the topic content prints; no "unknown topic" error.

- [ ] **Step 4: Add SPEC.md section**

In `SPEC.md`, find the section describing `horde results` (or the commands list) and add a short companion section. Use the existing section style (header level and tone) — don't refactor surrounding structure.

Example inserted paragraph (adapt heading level to context):

```markdown
### horde hydrate

Copies `.orc/audit/` and `.orc/artifacts/` from one or more completed runs
into a user-specified local directory so the user can invoke
`orc improve` / `orc doctor` against the results. Runs are placed under
`.orc/audit/<ticket>-<run-id>/` and `.orc/artifacts/<ticket>-<run-id>/`
(with an optional workflow prefix segment). Idempotent per run — existing
subdirs are skipped. Best-effort multi-run: exits non-zero if any run-id
fails, while still materializing the runs that succeeded. Docker provider
copies from `~/.horde/results/`; ECS provider downloads from the S3
artifacts prefix.

See `horde docs hydrate` for user-facing documentation.
```

- [ ] **Step 5: Run full short test suite**

Run: `go test -short ./... -count=1`
Expected: PASS.

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/docs/content.go SPEC.md
git commit -m "document horde hydrate"
```

---

## Final verification

- [ ] **Step 1: Full unit test run**

Run: `go test -short ./... -count=1`
Expected: PASS across all packages.

- [ ] **Step 2: Full integration test run**

Run: `go test -v -count=1 -timeout 10m ./test/integration/`
Expected: all existing + new hydrate integration tests PASS.

- [ ] **Step 3: Build and sanity-check binary**

Run:
```bash
go build -o /tmp/horde ./cmd/horde
/tmp/horde hydrate --help
/tmp/horde docs hydrate
```
Expected: both commands produce documented output.

- [ ] **Step 4: Vet + format clean**

Run: `go vet ./... && gofmt -l .`
Expected: no output (both clean).

---

## Self-Review Notes

**Spec coverage check:**
- "Command shape `horde hydrate <run-id>... --into <dir>`" → Task 7.
- "Layout `<ticket>-<run-id>` with optional workflow prefix" → Task 5.
- "Idempotency: skip if target subdir exists" → Task 7 + adjusted in Task 8 Step 5 if needed.
- "Best-effort, non-zero exit on partial failure" → Task 6 helpers + Task 7 exit logic + Task 8 test.
- "Failure cases: not-found, non-terminal, missing artifacts" → Task 7 (non-terminal, errors.As FileNotFoundError).
- "Docker provider: local fs copy" → Tasks 2–3.
- "ECS provider: S3 download" → Task 4.
- "Provider method `HydrateRun`, caller assembles destination paths" → Task 1.
- "Documentation updates (docs topic + SPEC.md)" → Task 9.
- "No change to ORC_CONTRACT_EXPECTATIONS.md" → honored (not in files list).

**Type consistency check:**
- `HydrateOpts` fields used consistently: `RunID`, `InstanceID`, `Metadata`, `DestAuditDir`, `DestArtifactsDir` across Tasks 1, 3, 4, 7.
- `hydrateOutcome`, `hydrateStatus` naming consistent across Tasks 6 and 7.
- `hydrateDestPaths` return order `(audit, artifacts)` consistent between Task 5 test and Task 7 use.
- `isTerminalStatus` defined in Task 6, used in Task 7 — signatures match.
- `copyLocalTree(src, dst string) error` used identically in Task 2 (defined) and Task 3 (callers).

**Placeholder scan:**
- No "TBD", "TODO", "implement later" in any step.
- Every code step shows the full code snippet.
- Every test step lists the expected outcome.
- Task 8 Step 5 has a conditional fix path that's fully specified — it's contingent on what Step 4 finds, not a placeholder.

---

## Execution Handoff

Plan complete and saved to `docs/2026-04-18-horde-hydrate-plan.md`.

Two execution options:

1. **Subagent-Driven (recommended)** — fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach would you like?
