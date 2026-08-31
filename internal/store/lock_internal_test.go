package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The locked-store message is the ONE diagnostic an operator gets when a
// second process opens the file, and stamp() writes "pid N on HOST since TS".
// A hostname may be 253 bytes, so the fixed 256-byte buffer this replaced
// could drop the host — the single field naming which machine to go and look
// at, on the failure whose whole question is "which machine".
func TestTheLockHolderStampIsReadWhole(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lock")
	host := strings.Repeat("h", 253)
	line := "pid 1234567 on " + host + " since 2026-08-31T00:00:00Z\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got := readHolder(f)
	if !strings.Contains(got, host) {
		t.Errorf("the holder's hostname was cut out of the diagnostic: %q", got)
	}
	if got != strings.TrimSpace(line) {
		t.Errorf("readHolder = %q", got)
	}
}

// An empty sidecar is the window between a peer's lock and its stamp, or a
// release that left the file behind. Both mean "somebody, and we cannot say
// who", which is more useful than an invented pid.
func TestAnEmptyLockStampNamesNobody(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := readHolder(f); got != "another crewlet process" {
		t.Errorf("readHolder on an empty stamp = %q", got)
	}
}

// And the read is bounded: the path is operator-supplied, so reading an
// arbitrary file whole into an error message is how a mistyped store path
// becomes a gigabyte in the heap.
func TestTheLockStampReadIsBounded(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxHolderStamp*3)), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := readHolder(f); len(got) > maxHolderStamp {
		t.Errorf("readHolder returned %d bytes, past its %d bound", len(got), maxHolderStamp)
	}
}
