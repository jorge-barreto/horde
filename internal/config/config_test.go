package config

import (
	"strings"
	"testing"
)

func TestNormalizeRepoURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https with .git", "https://github.com/org/repo.git", "github.com/org/repo.git"},
		{"https without .git", "https://github.com/org/repo", "github.com/org/repo"},
		{"http", "http://github.com/org/repo.git", "github.com/org/repo.git"},
		{"ssh scp-style with .git", "git@github.com:org/repo.git", "github.com/org/repo.git"},
		{"ssh scp-style without .git", "git@github.com:org/repo", "github.com/org/repo"},
		{"ssh url-style", "ssh://git@github.com/org/repo.git", "github.com/org/repo.git"},
		{"git protocol", "git://github.com/org/repo.git", "github.com/org/repo.git"},
		{"https with trailing whitespace", "https://github.com/org/repo.git\n", "github.com/org/repo.git"},
		{"non-github host", "git@gitlab.com:org/repo.git", "gitlab.com/org/repo.git"},
		{"deep path", "https://github.com/org/sub/repo.git", "github.com/org/sub/repo.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRepoURL(tc.in)
			if err != nil {
				t.Fatalf("NormalizeRepoURL(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeRepoURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeRepoURL_Errors(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"empty string", "", "empty remote URL"},
		{"whitespace only", "  ", "empty remote URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeRepoURL(tc.in)
			if err == nil {
				t.Fatalf("NormalizeRepoURL(%q) expected error, got nil", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("NormalizeRepoURL(%q) error = %q, want it to contain %q", tc.in, err.Error(), tc.wantErr)
			}
		})
	}
}
