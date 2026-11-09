package execstate_test

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
)

var update = flag.Bool("update", false, "write testdata/state-v<Version>.json from Encode's answer")

// EVERY VERSION A BUILD WROTE DECODES OR IS SETTLED: the current version reads
// its committed blob back, every lower one is retired, a higher one only waits.
// A bump keeps the old golden, which this then holds to retired.
func TestEveryStateVersionAReleaseWroteDecodesOrSettles(t *testing.T) {
	current := filepath.Join("testdata", fmt.Sprintf("state-v%d.json", execstate.Version))
	if *update {
		blob, err := execstate.Encode(parked())
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if err := os.WriteFile(current, append(blob, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", current, err)
		}
	}
	golden, err := os.ReadFile(current)
	if err != nil {
		t.Fatalf("no golden blob for version %d (run this test with -update once): %v",
			execstate.Version, err)
	}
	if _, ok, err := execstate.Decode(golden); err != nil || !ok {
		t.Fatalf("Decode(%s) = %v, %v; a blob this version wrote must read back", current, ok, err)
	}

	for v := 0; v < execstate.Version; v++ {
		retired(t, fmt.Sprintf("version %d", v), withVersion(t, golden, v), v)
	}
	olders, err := filepath.Glob(filepath.Join("testdata", "state-v*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range olders {
		if path == current {
			continue
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var v int
		if _, err := fmt.Sscanf(filepath.Base(path), "state-v%d.json", &v); err != nil {
			t.Fatalf("%s names no version: %v", path, err)
		}
		retired(t, path, blob, v)
	}

	_, ok, err := execstate.Decode(withVersion(t, golden, execstate.Version+1))
	if ok || !errors.Is(err, execstate.ErrUnknownVersion) || errors.Is(err, execstate.ErrRetired) {
		t.Fatalf("a newer version = %v, %v; want it refused and waited for, never retired", ok, err)
	}
}

// retired asserts blob is refused as the retired version v.
func retired(t *testing.T, name string, blob []byte, v int) {
	t.Helper()
	_, ok, err := execstate.Decode(blob)
	var version *execstate.VersionError
	if ok || !errors.Is(err, execstate.ErrRetired) || !errors.As(err, &version) || version.Found != v {
		t.Fatalf("%s = %v, %v; want it refused as retired version %d", name, ok, err, v)
	}
}
