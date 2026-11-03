package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The engine's tool sessions: ONE directory per process, made at boot and
// removed by the teardown, which every phase's harness reaches its seat's tools
// through (internal/toolsession).

// sessionDirs is the session directories under dir — what a boot left behind.
func sessionDirs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "crewlet-mcp-") {
			out = append(out, entry.Name())
		}
	}
	return out
}

// A TMPDIR TOO LONG FOR A SOCKET IS REFUSED AT BOOT, NAMING IT, with nothing
// opened. Every session's socket goes under it, and a socket path past the
// platform's bound fails at every bind — in every seat, at its first phase,
// as an error naming no setting — so the engine refuses to start on it
// instead, before it has a store or a broker to unwind.
//
// Not parallel: it moves TMPDIR for the process, which t.Setenv allows only
// where nothing else is running.
//
// Mutation: make the session directory after the backends, and the store's
// file exists when the boot is refused.
func TestABootOnATMPDIRTooLongForASocketIsRefusedNamingIt(t *testing.T) {
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	long := filepath.Join(t.TempDir(), strings.Repeat("a", 60), strings.Repeat("b", 60))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("TMPDIR", long)

	_, err := New(t.Context(), Options{Bootstrap: &b})
	if err == nil || !strings.Contains(err.Error(), "TMPDIR") {
		t.Fatalf("New on a TMPDIR too long for a socket = %v, want a refusal naming TMPDIR", err)
	}
	if _, statErr := os.Stat(b.Store.Path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the refused boot opened the store (%v): the check must come "+
			"before anything there is to unwind", statErr)
	}
	if left := sessionDirs(t, long); len(left) != 0 {
		t.Errorf("the refused boot left %v behind", left)
	}
}

// A BOOT THAT FAILS BEFORE THE ENGINE EXISTS REMOVES ITS SESSION DIRECTORY.
// The directory is made with nothing else open, and every return between it
// and the engine's own failure path is one the directory has to be removed on
// — or each failed boot leaves one in the temp root.
//
// Mutation: drop the guard that removes it before the handover, and the
// directory is left behind.
func TestABootThatFailsEarlyRemovesItsSessionDirectory(t *testing.T) {
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	// A SHORT temp root of its own: one under t.TempDir() carries the test's
	// name and is itself past the bound a socket's path has.
	tmp, err := os.MkdirTemp("", "crewlet-boot-") //nolint:usetesting // t.TempDir is named for the test, past a socket path's bound
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	// Lent backends with nothing in them, which New refuses before it
	// opens anything — after the session directory is made.
	_, err = New(t.Context(), Options{Bootstrap: &b, Backends: &Backends{}})
	if err == nil || !strings.Contains(err.Error(), "the supplied Backends has no") {
		t.Fatalf("New = %v, want the incomplete backends refused — this case needs "+
			"a boot refused between the session directory and the engine", err)
	}
	if left := sessionDirs(t, tmp); len(left) != 0 {
		t.Errorf("the failed boot left %v behind", left)
	}
}

// THE TEARDOWN REMOVES THE SESSION DIRECTORY, last, on a stop and on a boot
// that failed once the engine held it — and a teardown with none to remove is
// not a fault.
//
// Mutation: leave the directory out of the teardown, and it survives the stop.
func TestTheTeardownRemovesTheSessionDirectory(t *testing.T) {
	t.Parallel()
	e := &Engine{sessions: testSessions(t)}
	dir := e.sessions.Dir()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the session directory is not there to begin with: %v", err)
	}
	e.teardown(context.Background())
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the session directory survived the teardown (%v)", err)
	}
	(&Engine{}).teardown(context.Background())
}
