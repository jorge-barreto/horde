package main

import "testing"

func TestNormalizeVersion(t *testing.T) {
	cases := map[string]string{
		"v0.1.0":           "0.1.0",
		"0.1.0":            "0.1.0",
		"cli-v0.1.0":       "0.1.0",
		"dev":              "",
		"v0.3.0-5-gabc123": "", // git-describe dev string: not a clean release version
	}
	for in, want := range cases {
		if got := normalizeVersion(in); got != want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.1.0", "0.1.1", true},
		{"v0.1.0", "v0.2.0", true}, // prefixes normalized
		{"0.2.0", "0.1.9", false},
		{"0.1.0", "0.1.0", false},
		{"1.0.0", "0.9.9", false},
		{"dev", "0.1.0", false}, // dev/unparseable current: never claim newer
		{"0.1.0", "garbage", false},
	}
	for _, c := range cases {
		if got := isNewer(c.current, c.latest); got != c.want {
			t.Errorf("isNewer(%q,%q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestParseLatestTag(t *testing.T) {
	body := []byte(`{"tag_name":"v0.3.1","name":"horde v0.3.1"}`)
	tag, err := parseLatestTag(body)
	if err != nil {
		t.Fatalf("parseLatestTag: %v", err)
	}
	if tag != "v0.3.1" {
		t.Errorf("tag = %q, want v0.3.1", tag)
	}
}
