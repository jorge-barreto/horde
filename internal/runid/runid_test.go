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

func TestGenerate_Uniformity(t *testing.T) {
	counts := make(map[byte]int)
	n := 50000
	for i := 0; i < n; i++ {
		id, err := Generate()
		if err != nil {
			t.Fatalf("Generate() error on iteration %d: %v", i, err)
		}
		for j := 0; j < len(id); j++ {
			counts[id[j]]++
		}
	}

	totalChars := float64(n * idLength)             // 50000 * 12 = 600000
	expected := totalChars / float64(len(alphabet)) // 600000 / 36 = 16666.67
	var chiSq float64
	for i := 0; i < len(alphabet); i++ {
		observed := float64(counts[alphabet[i]])
		diff := observed - expected
		chiSq += (diff * diff) / expected
	}

	// Chi-squared critical value for 35 degrees of freedom at p=0.001 is ~66.6.
	// Threshold of 80 gives extra margin against flakiness.
	if chiSq > 80 {
		t.Errorf("chi-squared statistic %.2f exceeds threshold 80 (df=35, p<0.001); distribution is not uniform", chiSq)
	}
}
