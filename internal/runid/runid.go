package runid

import (
	"crypto/rand"
	"fmt"
)

const (
	alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	idLength = 12
)

// Generate returns a 12-character lowercase alphanumeric run ID
// using crypto/rand. Safe for URLs, filenames, shell arguments,
// and database keys.
func Generate() (string, error) {
	b := make([]byte, idLength)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating run ID: %w", err)
	}
	for i := range b {
		b[i] = alphabet[b[i]%byte(len(alphabet))]
	}
	return string(b), nil
}
