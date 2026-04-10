package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateEnvFile checks that dir/.env exists and contains ANTHROPIC_API_KEY and GIT_TOKEN.
// Returns the absolute path to the .env file on success.
func ValidateEnvFile(dir string) (string, error) {
	envPath := filepath.Join(dir, ".env")

	f, err := os.Open(envPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf(".env file not found: %s", envPath)
		}
		return "", fmt.Errorf("reading .env file: %w", err)
	}
	defer f.Close()

	keys := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx > 0 {
			key := strings.TrimSpace(line[:idx])
			keys[key] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading .env file: %w", err)
	}

	required := []string{"ANTHROPIC_API_KEY", "GIT_TOKEN"}
	for _, key := range required {
		if !keys[key] {
			return "", fmt.Errorf(".env file missing required key: %s", key)
		}
	}

	return envPath, nil
}
