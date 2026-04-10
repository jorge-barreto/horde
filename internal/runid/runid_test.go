package runid

import (
	"regexp"
	"testing"
)

func TestGenerate_Length(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if len(id) != 12 {
		t.Errorf("expected length 12, got %d: %q", len(id), id)
	}
}

func TestGenerate_CharacterSet(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9]{12}$`)
	for i := 0; i < 100; i++ {
		id, err := Generate()
		if err != nil {
			t.Fatalf("Generate() error: %v", err)
		}
		if !re.MatchString(id) {
			t.Errorf("ID %q does not match [a-z0-9]{12}", id)
		}
	}
}

func TestGenerate_Uniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		id, err := Generate()
		if err != nil {
			t.Fatalf("Generate() error on iteration %d: %v", i, err)
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate ID %q on iteration %d", id, i)
		}
		seen[id] = struct{}{}
	}
}
