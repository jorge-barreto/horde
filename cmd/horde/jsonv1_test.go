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
		{"2m 30s", 150, true},
		{"  9m 34s  ", 574, true}, // surrounding whitespace tolerated
		{"", 0, false},
		{"bogus", 0, false},
		{"12 minutes", 0, false},
		// Known wart of the space-collapsing stopgap: malformed "2 30s"
		// collapses to "230s" and parses as 230s. We don't guard against it
		// here because this whole parser goes away once orc emits a numeric
		// duration (jorge-barreto/orc#5); real orc output never has bare
		// number-number spacing, so it isn't reachable in practice
		// (jorge-barreto/orc#3 tracks removing this parser entirely).
		{"2 30s", 230, true},
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
