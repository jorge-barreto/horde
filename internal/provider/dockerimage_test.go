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

func TestEnsureImage_MissingImage(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `case "$1" in
  image) exit 1;;
  build) exit 0;;
esac
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())

	embedded := fstest.MapFS{
		"Dockerfile":    {Data: []byte("FROM debian\n")},
		"entrypoint.sh": {Data: []byte("#!/bin/bash\n")},
	}

	p := NewDockerProvider()
	var buf bytes.Buffer
	if err := p.EnsureImage(context.Background(), embedded, &buf); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	if !strings.Contains(buf.String(), "not found") {
		t.Errorf("output %q does not contain 'not found'", buf.String())
	}
}

func TestEnsureImage_UpToDate(t *testing.T) {
	dir := t.TempDir()
	// Return a far-future timestamp so image is newer than any file
	writeFakeDocker(t, dir, `case "$1" in
  image) echo "2099-01-01T00:00:00Z";;
esac
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())

	embedded := fstest.MapFS{
		"Dockerfile": {Data: []byte("FROM debian\n")},
	}

	p := NewDockerProvider()
	var buf bytes.Buffer
	if err := p.EnsureImage(context.Background(), embedded, &buf); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	// Should be silent — no rebuild message
	if strings.Contains(buf.String(), "Building") || strings.Contains(buf.String(), "Rebuilding") {
		t.Errorf("unexpected build output for up-to-date image: %q", buf.String())
	}
}

func TestEnsureImage_StaleImage(t *testing.T) {
	dir := t.TempDir()
	// Return a timestamp in the past
	writeFakeDocker(t, dir, `case "$1" in
  image) echo "2000-01-01T00:00:00Z";;
  build) exit 0;;
esac
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())

	embedded := fstest.MapFS{
		"Dockerfile": {Data: []byte("FROM debian\n")},
	}

	p := NewDockerProvider()
	var buf bytes.Buffer
	if err := p.EnsureImage(context.Background(), embedded, &buf); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	if !strings.Contains(buf.String(), "outdated") {
		t.Errorf("output %q does not contain 'outdated'", buf.String())
	}
}

func TestEnsureImage_BuildFailure(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, dir, `case "$1" in
  image) exit 1;;
  build) echo "build failed" >&2; exit 1;;
esac
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())

	embedded := fstest.MapFS{
		"Dockerfile": {Data: []byte("FROM debian\n")},
	}

	p := NewDockerProvider()
	var buf bytes.Buffer
	err := p.EnsureImage(context.Background(), embedded, &buf)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "building worker image") {
		t.Errorf("error %q does not contain 'building worker image'", err.Error())
	}
}
