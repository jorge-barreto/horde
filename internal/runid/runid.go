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
	id := make([]byte, idLength)
	buf := make([]byte, idLength+10) // over-provision to reduce rand.Read calls
	filled := 0
	for filled < idLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generating run ID: %w", err)
		}
		for _, b := range buf {
			if b >= 252 { // 252 = 36 * 7, largest multiple of 36 fitting in a byte
				continue
			}
			id[filled] = alphabet[b%36]
			filled++
			if filled == idLength {
				break
			}
		}
	}
	return string(id), nil
}
