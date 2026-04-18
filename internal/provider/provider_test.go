package provider

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidateRunID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		id      string
		wantErr string // substring; "" means no error
	}{
		{"valid", "abc123XYZ789", ""},
		{"empty", "", "required"},
		{"forward slash", "abc/def", "invalid"},
		{"backslash", "abc\\def", "invalid"},
		{"parent dir", "..", "invalid"},
		{"embedded parent", "abc..def", "invalid"},
		{"leading dotdot-slash", "../etc/passwd", "invalid"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateRunID(tc.id)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateRunID(%q) = %v, want nil", tc.id, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateRunID(%q) = nil, want error containing %q", tc.id, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ValidateRunID(%q) = %q, want it to contain %q", tc.id, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestFileNotFoundError_Error(t *testing.T) {
	t.Parallel()
	e := &FileNotFoundError{Path: ".orc/audit/run-result.json"}
	want := "file not found: .orc/audit/run-result.json"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestFileNotFoundError_UnwrapNil(t *testing.T) {
	t.Parallel()
	e := &FileNotFoundError{Path: ".orc/foo"}
	if got := e.Unwrap(); got != nil {
		t.Errorf("Unwrap() = %v, want nil", got)
	}
}

func TestFileNotFoundError_UnwrapWrapped(t *testing.T) {
	t.Parallel()
	inner := fmt.Errorf("s3: NoSuchKey")
	e := &FileNotFoundError{Path: ".orc/foo", Err: inner}
	got := e.Unwrap()
	if got == nil {
		t.Fatal("Unwrap() = nil, want non-nil wrapped error")
	}
	if got.Error() != inner.Error() {
		t.Errorf("Unwrap() = %q, want %q", got.Error(), inner.Error())
	}
}

func TestFileNotFoundError_ErrorsIs(t *testing.T) {
	// Ensure errors.As lifts the sentinel through a wrapping layer,
	// which is how horde's CLI code detects "no such file" across
	// providers.
	t.Parallel()
	inner := fmt.Errorf("sentinel")
	e := &FileNotFoundError{Path: ".orc/foo", Err: inner}
	wrapped := fmt.Errorf("reading file: %w", e)
	var fnf *FileNotFoundError
	if !errors.As(wrapped, &fnf) {
		t.Fatal("errors.As did not extract *FileNotFoundError from wrapped error")
	}
	if fnf.Path != ".orc/foo" {
		t.Errorf("extracted Path = %q, want %q", fnf.Path, ".orc/foo")
	}
	if !strings.Contains(fnf.Error(), ".orc/foo") {
		t.Errorf("Error() = %q, want it to contain path", fnf.Error())
	}
}
