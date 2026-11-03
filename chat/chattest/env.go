package chattest

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// EnvKey reads a key for integration tests: first the environment variable envVar, then yamlKey in an env.yaml found
// in the current or a parent directory. It skips the test when neither exists. env.yaml is a local file ignored by git.
func EnvKey(t testing.TB, envVar, yamlKey string) string {
	t.Helper()
	if v := LookupKey(envVar, yamlKey); v != "" {
		return v
	}
	t.Skipf("missing %s (environment variable or %s in env.yaml)", envVar, yamlKey)
	return ""
}

// LookupKey finds a key with the same rules as EnvKey, returning an empty string when not found.
func LookupKey(envVar, yamlKey string) string {
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return v
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if v := readYAMLValue(filepath.Join(dir, "env.yaml"), yamlKey); v != "" {
			return v
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func readYAMLValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		k, v, ok := strings.Cut(scanner.Text(), ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}
