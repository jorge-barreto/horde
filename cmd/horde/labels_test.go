package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseLabels_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want map[string]string
	}{
		{"empty", nil, nil},
		{"single", []string{"epic=KS-100"}, map[string]string{"epic": "KS-100"}},
		{"multiple", []string{"epic=KS-100", "variant=v3"}, map[string]string{"epic": "KS-100", "variant": "v3"}},
		{"value with equals", []string{"note=a=b=c"}, map[string]string{"note": "a=b=c"}},
		{"dotted and dashed key", []string{"prompt.version=v3", "dispatched-by=orchestrator"}, map[string]string{"prompt.version": "v3", "dispatched-by": "orchestrator"}},
		{"value with spaces", []string{"desc=hello world"}, map[string]string{"desc": "hello world"}},
		{"underscore key", []string{"run_tick=12"}, map[string]string{"run_tick": "12"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLabels(tc.in)
			if err != nil {
				t.Fatalf("parseLabels(%v): unexpected error %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseLabels(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseLabels_Invalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		in        []string
		errSubstr string
	}{
		{"no equals", []string{"epic"}, "must be key=value"},
		{"empty key", []string{"=v3"}, "empty key"},
		{"empty value", []string{"epic="}, "empty value"},
		{"key with space", []string{"my key=v"}, "invalid label key"},
		{"key with slash", []string{"a/b=v"}, "invalid label key"},
		{"duplicate key", []string{"epic=KS-100", "epic=KS-200"}, "duplicate label key"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseLabels(tc.in)
			if err == nil {
				t.Fatalf("parseLabels(%v): expected error containing %q, got nil", tc.in, tc.errSubstr)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("parseLabels(%v): error %q does not contain %q", tc.in, err.Error(), tc.errSubstr)
			}
		})
	}
}

func TestParseLabels_KeyLengthBound(t *testing.T) {
	t.Parallel()
	longKey := strings.Repeat("k", 65) + "=v"
	if _, err := parseLabels([]string{longKey}); err == nil {
		t.Error("expected error for over-long key (>64), got nil")
	}
	okKey := strings.Repeat("k", 64) + "=v"
	if _, err := parseLabels([]string{okKey}); err != nil {
		t.Errorf("64-char key should be valid, got %v", err)
	}
}

func TestParseLabels_ValueLengthBound(t *testing.T) {
	t.Parallel()
	longVal := "k=" + strings.Repeat("v", 257)
	if _, err := parseLabels([]string{longVal}); err == nil {
		t.Error("expected error for over-long value (>256), got nil")
	}
	okVal := "k=" + strings.Repeat("v", 256)
	if _, err := parseLabels([]string{okVal}); err != nil {
		t.Errorf("256-char value should be valid, got %v", err)
	}
}
