package main

import "testing"

func TestParseOrcDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   float64
		wantOK bool
	}{
		{"12m 34s", 754, true},
		{"4m 57s", 297, true},
		{"1h 2m 3s", 3723, true},
		{"45s", 45, true},
		{"  9m 34s  ", 574, true}, // surrounding whitespace tolerated
		{"", 0, false},
		{"bogus", 0, false},
		{"12 minutes", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseOrcDuration(tc.in)
		if ok != tc.wantOK {
			t.Errorf("parseOrcDuration(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseOrcDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
