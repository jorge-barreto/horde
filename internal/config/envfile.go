package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateEnvFile checks that dir/.env exists and contains the two
// canonical secrets (CLAUDE_CODE_OAUTH_TOKEN, GIT_TOKEN). Equivalent to
// ValidateEnvFileFor(dir, DefaultSecrets()).
//
// Prefer ValidateEnvFileFor at call sites that have a merged SecretSpec —
// it enforces every declared docker source, not only the two canonicals.
func ValidateEnvFile(dir string) (string, error) {
	return ValidateEnvFileFor(dir, DefaultSecrets())
}

// ValidateEnvFileFor checks that dir/.env exists and contains every host
// env-var name referenced by the spec's docker (Env) sources. Returns the
// absolute path to the .env file on success. The error names every
// missing key in one pass so callers don't have to fix .env iteratively.
func ValidateEnvFileFor(dir string, spec SecretSpec) (string, error) {
	envPath := filepath.Join(dir, ".env")

	f, err := os.Open(envPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("opening .env file: %w", err)
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

	var missing []string
	for _, key := range spec.EnvKeys() {
		if !keys[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("validating .env file: missing required key(s): %s", strings.Join(missing, ", "))
	}

	return envPath, nil
}

// LoadDotEnv parses dir/.env and returns its key/value pairs. A missing file
// is NOT an error — returns an empty map. Lines that are blank or start with
// '#' are skipped. Values may be optionally surrounded by double or single
// quotes, which are stripped. No variable expansion is performed.
func LoadDotEnv(dir string) (map[string]string, error) {
	envPath := filepath.Join(dir, ".env")
	f, err := os.Open(envPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("opening %s: %w", envPath, err)
	}
	defer f.Close()

	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		out[key] = val
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", envPath, err)
	}
	return out, nil
}

// ApplyDotEnvToProcess loads dir/.env and exports each pair into the current
// process environment unless the variable is already set (real env wins).
// Missing .env is a silent no-op; other read errors propagate.
func ApplyDotEnvToProcess(dir string) error {
	pairs, err := LoadDotEnv(dir)
	if err != nil {
		return err
	}
	for k, v := range pairs {
		if _, ok := os.LookupEnv(k); ok {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("setting %s: %w", k, err)
		}
	}
	return nil
}
