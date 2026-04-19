package bootstrap

import (
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https with .git", "https://github.com/jorge-barreto/horde.git", "jorge-barreto-horde"},
		{"ssh scp-style", "git@github.com:jorge-barreto/horde.git", "jorge-barreto-horde"},
		{"https no .git", "https://github.com/org/repo", "org-repo"},
		{"ssh url-style", "ssh://git@github.com/org/repo.git", "org-repo"},
		{"gitlab subgroup", "https://gitlab.com/group/subgroup/repo.git", "group-subgroup-repo"},
		{"uppercase", "https://github.com/Org/Repo.git", "org-repo"},
		{"underscores and dots", "https://github.com/my_org/repo.v2.git", "my-org-repo-v2"},
		{"unicode", "https://github.com/café/repo.git", "caf-repo"},
		{"repeated separators", "https://github.com/a--b/__c.git", "a-b-c"},
		{"leading/trailing junk", "https://github.com/-org-/-repo-.git", "org-repo"},
		// CDK e2e fixture: cdk/e2e/app.ts hardcodes this slug; if Slug ever
		// changes its hashing/normalization, the e2e test (and `horde push`
		// against the cdke2e stack) will break — this case prevents that.
		{"cdk e2e fixture", "https://github.com/jorge-barreto/horde-cdke2e.git", "jorge-barreto-horde-cdke2e"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Slug(tc.in)
			if err != nil {
				t.Fatalf("Slug(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSlug_TruncationBoundary(t *testing.T) {
	t.Parallel()
	in := "https://github.com/org/" + strings.Repeat("a", 200) + ".git"
	got, err := Slug(in)
	if err != nil {
		t.Fatalf("Slug(%q) error: %v", in, err)
	}
	if len(got) != MaxSlugLen {
		t.Errorf("Slug(long) length = %d, want %d; got=%q", len(got), MaxSlugLen, got)
	}
	if strings.HasPrefix(got, "-") {
		t.Errorf("Slug(long) has leading '-': %q", got)
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("Slug(long) has trailing '-': %q", got)
	}
}

func TestSlug_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"empty", "", "deriving project slug"},
		{"malformed", "justahostname", "deriving project slug"},
		{"host only", "https://github.com/", "deriving project slug"},
		{"sanitizes to empty", "https://github.com/---/---.git", "sanitizes to empty string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Slug(tc.in)
			if err == nil {
				t.Fatalf("Slug(%q) expected error, got nil", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Slug(%q) error = %q, want it to contain %q", tc.in, err.Error(), tc.wantErr)
			}
		})
	}
}
