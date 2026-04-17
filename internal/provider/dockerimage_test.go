package provider

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestSyncWorkerFiles_CreatesFiles(t *testing.T) {
	embedded := fstest.MapFS{
		"Dockerfile":    {Data: []byte("FROM debian:bookworm-slim\n")},
		"entrypoint.sh": {Data: []byte("#!/bin/bash\necho hi\n")},
	}

	dst := filepath.Join(t.TempDir(), "workerfiles")
	if err := syncWorkerFiles(embedded, dst); err != nil {
		t.Fatalf("syncWorkerFiles: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "Dockerfile"))
	if err != nil {
		t.Fatalf("reading Dockerfile: %v", err)
	}
	if string(data) != "FROM debian:bookworm-slim\n" {
		t.Errorf("Dockerfile = %q, want %q", data, "FROM debian:bookworm-slim\n")
	}

	data, err = os.ReadFile(filepath.Join(dst, "entrypoint.sh"))
	if err != nil {
		t.Fatalf("reading entrypoint.sh: %v", err)
	}
	if string(data) != "#!/bin/bash\necho hi\n" {
		t.Errorf("entrypoint.sh = %q, want %q", data, "#!/bin/bash\necho hi\n")
	}
}

func TestSyncWorkerFiles_PreservesMtimeOnSameContent(t *testing.T) {
	embedded := fstest.MapFS{
		"Dockerfile": {Data: []byte("FROM debian\n")},
	}

	dst := filepath.Join(t.TempDir(), "workerfiles")
	if err := syncWorkerFiles(embedded, dst); err != nil {
		t.Fatal(err)
	}

	info1, _ := os.Stat(filepath.Join(dst, "Dockerfile"))
	mtime1 := info1.ModTime()

	time.Sleep(50 * time.Millisecond)

	if err := syncWorkerFiles(embedded, dst); err != nil {
		t.Fatal(err)
	}

	info2, _ := os.Stat(filepath.Join(dst, "Dockerfile"))
	mtime2 := info2.ModTime()

	if !mtime1.Equal(mtime2) {
		t.Errorf("mtime changed on identical sync: %v → %v", mtime1, mtime2)
	}
}

func TestSyncWorkerFiles_UpdatesMtimeOnContentChange(t *testing.T) {
	v1 := fstest.MapFS{"Dockerfile": {Data: []byte("v1\n")}}
	v2 := fstest.MapFS{"Dockerfile": {Data: []byte("v2\n")}}

	dst := filepath.Join(t.TempDir(), "workerfiles")
	syncWorkerFiles(v1, dst)
	info1, _ := os.Stat(filepath.Join(dst, "Dockerfile"))
	mtime1 := info1.ModTime()

	time.Sleep(50 * time.Millisecond)
	syncWorkerFiles(v2, dst)
	info2, _ := os.Stat(filepath.Join(dst, "Dockerfile"))
	mtime2 := info2.ModTime()

	if !mtime2.After(mtime1) {
		t.Errorf("mtime did not advance on content change: %v → %v", mtime1, mtime2)
	}

	data, _ := os.ReadFile(filepath.Join(dst, "Dockerfile"))
	if string(data) != "v2\n" {
		t.Errorf("content = %q, want %q", data, "v2\n")
	}
}

// fakeDockerScript models `docker image inspect` / `docker build` / `docker tag`
// against a per-image state dir. The state dir contains one file per known
// image tag (with "/" and ":" replaced by "_"); the file's contents are the
// image's horde.built_at label value.
//
//   - `image inspect <img> --format ...` → exits 1 if the state file is missing,
//     else echoes the file's contents.
//   - `build --label horde.built_at=<ts> -t <tag> <dir>` → writes <ts> to the
//     tag's state file.
//   - `tag <src> <dst>` → copies src's state file to dst's.
const fakeDockerScript = `
state_dir="$FAKE_DOCKER_STATE_DIR"
image_state() {
  echo "$state_dir/$(echo "$1" | tr '/:' '__')"
}
case "$1" in
  image)
    # image inspect <tag> --format '...'
    tag="$3"
    f=$(image_state "$tag")
    if [ -f "$f" ]; then
      cat "$f"
      exit 0
    fi
    echo "Error: No such image: $tag" >&2
    exit 1
    ;;
  build)
    # find --label and -t values in args
    label_val=""
    tag=""
    shift
    while [ $# -gt 0 ]; do
      case "$1" in
        --label)
          label_val="${2#horde.built_at=}"
          shift 2
          ;;
        -t)
          tag="$2"
          shift 2
          ;;
        *) shift ;;
      esac
    done
    if [ -n "$tag" ]; then
      printf '%s' "$label_val" > "$(image_state "$tag")"
    fi
    exit 0
    ;;
  tag)
    src="$2"; dst="$3"
    sf=$(image_state "$src")
    df=$(image_state "$dst")
    if [ -f "$sf" ]; then
      cp "$sf" "$df"
    fi
    exit 0
    ;;
esac
exit 0
`

// fakeDockerBuildFails is the same script as fakeDockerScript but fails builds.
const fakeDockerBuildFails = `
state_dir="$FAKE_DOCKER_STATE_DIR"
image_state() {
  echo "$state_dir/$(echo "$1" | tr '/:' '__')"
}
case "$1" in
  image)
    tag="$3"
    f=$(image_state "$tag")
    if [ -f "$f" ]; then cat "$f"; exit 0; fi
    exit 1
    ;;
  build)
    echo "build failed" >&2
    exit 1
    ;;
esac
exit 0
`

// setupFakeDocker installs a fake docker on PATH and points it at a state dir.
// seedLabels pre-populates image labels (image tag → label value).
func setupFakeDocker(t *testing.T, script string, seedLabels map[string]string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := t.TempDir()
	writeFakeDocker(t, dir, script)
	for tag, ts := range seedLabels {
		name := strings.NewReplacer("/", "_", ":", "_").Replace(tag)
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte(ts), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", tag, err)
		}
	}
	t.Setenv("FAKE_DOCKER_STATE_DIR", stateDir)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
}

// readImageLabel returns the label value the fake docker has stored for tag.
func readImageLabel(t *testing.T, tag string) string {
	t.Helper()
	name := strings.NewReplacer("/", "_", ":", "_").Replace(tag)
	data, err := os.ReadFile(filepath.Join(os.Getenv("FAKE_DOCKER_STATE_DIR"), name))
	if err != nil {
		t.Fatalf("reading label for %s: %v", tag, err)
	}
	return string(data)
}

func TestEnsureImage_MissingBase(t *testing.T) {
	setupFakeDocker(t, fakeDockerScript, nil)

	embedded := fstest.MapFS{
		"Dockerfile":    {Data: []byte("FROM debian\n")},
		"entrypoint.sh": {Data: []byte("#!/bin/bash\n")},
	}

	p := NewDockerProvider()
	var buf bytes.Buffer
	if err := p.EnsureImage(context.Background(), embedded, t.TempDir(), &buf); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	if !strings.Contains(buf.String(), "not found") {
		t.Errorf("output %q does not contain 'not found'", buf.String())
	}
	// Build must have stamped a label.
	if readImageLabel(t, baseImage) == "" {
		t.Errorf("base image has no built_at label after build")
	}
}

func TestEnsureImage_UpToDate(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	setupFakeDocker(t, fakeDockerScript, map[string]string{baseImage: future})

	embedded := fstest.MapFS{
		"Dockerfile": {Data: []byte("FROM debian\n")},
	}

	p := NewDockerProvider()
	var buf bytes.Buffer
	if err := p.EnsureImage(context.Background(), embedded, t.TempDir(), &buf); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	if strings.Contains(buf.String(), "Building") || strings.Contains(buf.String(), "Rebuilding") {
		t.Errorf("unexpected build output for up-to-date image: %q", buf.String())
	}
}

func TestEnsureImage_StaleBase(t *testing.T) {
	ancient := "2000-01-01T00:00:00Z"
	setupFakeDocker(t, fakeDockerScript, map[string]string{baseImage: ancient})

	embedded := fstest.MapFS{
		"Dockerfile": {Data: []byte("FROM debian\n")},
	}

	p := NewDockerProvider()
	var buf bytes.Buffer
	if err := p.EnsureImage(context.Background(), embedded, t.TempDir(), &buf); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	if !strings.Contains(buf.String(), "outdated") {
		t.Errorf("output %q does not contain 'outdated'", buf.String())
	}
	// After rebuild, label should no longer be ancient.
	if got := readImageLabel(t, baseImage); got == ancient {
		t.Errorf("built_at label was not refreshed after rebuild (still %s)", got)
	}
}

// TestEnsureImage_CacheHitDoesNotThrash is the regression guard for the bug
// where cache-hit docker builds left .Created frozen, causing every launch to
// re-enter the rebuild path forever. Under label-based freshness, a successful
// build always refreshes the label, so the next call is a no-op.
func TestEnsureImage_CacheHitDoesNotThrash(t *testing.T) {
	setupFakeDocker(t, fakeDockerScript, nil)

	embedded := fstest.MapFS{
		"Dockerfile": {Data: []byte("FROM debian\n")},
	}

	p := NewDockerProvider()

	// First call: image missing → build.
	var buf1 bytes.Buffer
	if err := p.EnsureImage(context.Background(), embedded, t.TempDir(), &buf1); err != nil {
		t.Fatalf("first EnsureImage: %v", err)
	}
	if !strings.Contains(buf1.String(), "Building") && !strings.Contains(buf1.String(), "Rebuilding") {
		t.Fatalf("expected initial build, got: %q", buf1.String())
	}

	// Second call with identical embedded files: must NOT rebuild.
	var buf2 bytes.Buffer
	if err := p.EnsureImage(context.Background(), embedded, t.TempDir(), &buf2); err != nil {
		t.Fatalf("second EnsureImage: %v", err)
	}
	if strings.Contains(buf2.String(), "Building") || strings.Contains(buf2.String(), "Rebuilding") {
		t.Errorf("unexpected rebuild on second call: %q", buf2.String())
	}
}

// TestEnsureImage_UnlabeledImageTriggersRebuild covers pre-label images in the
// wild: once a horde version that predates this change has built the image,
// the new horde must rebuild to stamp the label.
func TestEnsureImage_UnlabeledImageTriggersRebuild(t *testing.T) {
	// Seed the image as existing but with an empty label (pre-label behavior).
	setupFakeDocker(t, fakeDockerScript, map[string]string{baseImage: ""})

	embedded := fstest.MapFS{
		"Dockerfile": {Data: []byte("FROM debian\n")},
	}

	p := NewDockerProvider()
	var buf bytes.Buffer
	if err := p.EnsureImage(context.Background(), embedded, t.TempDir(), &buf); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	if !strings.Contains(buf.String(), "not found or unlabeled") {
		t.Errorf("expected unlabeled rebuild message, got: %q", buf.String())
	}
	if readImageLabel(t, baseImage) == "" {
		t.Errorf("label was not written after rebuild")
	}
}

func TestEnsureImage_BuildFailure(t *testing.T) {
	setupFakeDocker(t, fakeDockerBuildFails, nil)

	embedded := fstest.MapFS{
		"Dockerfile": {Data: []byte("FROM debian\n")},
	}

	p := NewDockerProvider()
	var buf bytes.Buffer
	err := p.EnsureImage(context.Background(), embedded, t.TempDir(), &buf)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "building image") {
		t.Errorf("error %q does not contain 'building image'", err.Error())
	}
}

func TestEnsureImage_WithProjectDockerfile(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	setupFakeDocker(t, fakeDockerScript, map[string]string{
		baseImage:    future,
		DockerImage: future,
	})

	embedded := fstest.MapFS{
		"Dockerfile": {Data: []byte("FROM debian\n")},
	}

	// Create a project with worker/Dockerfile. Its mtime will be "now",
	// but the project image label is already in the future, so no rebuild.
	projectDir := t.TempDir()
	workerDir := filepath.Join(projectDir, "worker")
	os.MkdirAll(workerDir, 0o755)
	os.WriteFile(filepath.Join(workerDir, "Dockerfile"), []byte("FROM horde-worker-base:latest\nRUN apt-get install -y golang\n"), 0o644)

	p := NewDockerProvider()
	var buf bytes.Buffer
	if err := p.EnsureImage(context.Background(), embedded, projectDir, &buf); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	if strings.Contains(buf.String(), "Project image") && strings.Contains(buf.String(), "Building") {
		t.Errorf("unexpected project rebuild: %q", buf.String())
	}
}
