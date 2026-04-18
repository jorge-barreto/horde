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
