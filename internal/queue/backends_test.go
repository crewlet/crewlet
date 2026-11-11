package queue_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// backendSources is every non-test Go file of each backend this contract
// certifies, keyed "backend/file", for a guard that refuses a backend spelling
// a line the contract owns.
//
// A backend whose files stopped being found would pass such a guard by reading
// nothing at all, so a backend with no source file fails the test here.
func backendSources(t *testing.T) map[string][]byte {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locating this package: %v", err)
	}
	out := map[string][]byte{}
	for _, backend := range []string{"memory", "jetstream"} {
		entries, err := os.ReadDir(filepath.Join(dir, backend))
		if err != nil {
			t.Fatalf("reading backend %s: %v", backend, err)
		}
		var scanned int
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
				strings.HasSuffix(name, "_test.go") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, backend, name))
			if err != nil {
				t.Fatalf("reading %s/%s: %v", backend, name, err)
			}
			out[backend+"/"+name] = raw
			scanned++
		}
		if scanned == 0 {
			t.Fatalf("scanned no source files for backend %s", backend)
		}
	}
	return out
}
