package provider

import (
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
