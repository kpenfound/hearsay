// Package configtest writes a configuration directory for a test.
package configtest

import (
	"os"
	"path/filepath"
	"testing"
)

// Write puts files, keyed by slash-separated path, in a new temporary directory
// and returns the directory.
func Write(t testing.TB, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	WriteTo(t, root, files)
	return root
}

// WriteTo puts files, keyed by slash-separated path, under root.
func WriteTo(t testing.TB, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", full, err)
		}
	}
}
