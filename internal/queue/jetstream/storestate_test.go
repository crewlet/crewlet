package jetstream

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
)

// A STORE DIRECTORY HOLDS A BROKER'S STATE ONCE A MEMBER HAS STARTED ON IT,
// and not before — the one fact a stopped node's command line reads to tell a
// node whose own secret rows are the company's whole store (its broker never
// ran) from one whose rows are not.
//
// The started member is the case that matters: it pins the directory this
// looks for to the one nats-server actually writes, on a member that has
// stored nothing, so a server that kept its state anywhere else turns this
// red rather than reading every node as new. And a store directory that
// cannot be looked in is an error, never "never ran": a regular file where
// the directory should be gives ENOTDIR, which no privilege bypasses.
//
// Mutation: answer (false, nil) for every stat error, and the file case goes
// red; look for any other name, and the started member does.
func TestAStoreDirHoldsStateOnceAMemberHasStarted(t *testing.T) {
	t.Parallel()
	never := func(t *testing.T, dir string) {
		t.Helper()
		ran, err := StoreHasRun(dir)
		if err != nil || ran {
			t.Errorf("StoreHasRun(%s) = %v, %v, want a broker that never ran", dir, ran, err)
		}
	}
	t.Run("an empty directory", func(t *testing.T) {
		t.Parallel()
		never(t, t.TempDir())
	})
	t.Run("a directory that does not exist", func(t *testing.T) {
		t.Parallel()
		never(t, filepath.Join(t.TempDir(), "stream"))
	})
	t.Run("a directory holding the broker's state", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, server.JetStreamStoreDir), 0o700); err != nil {
			t.Fatal(err)
		}
		if ran, err := StoreHasRun(dir); err != nil || !ran {
			t.Errorf("StoreHasRun = %v, %v, want a broker that has run", ran, err)
		}
	})
	t.Run("a store directory that is a file", func(t *testing.T) {
		t.Parallel()
		file := filepath.Join(t.TempDir(), "stream")
		if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		ran, err := StoreHasRun(file)
		if err == nil {
			t.Fatalf("StoreHasRun on a file = %v with no error, so a path nothing "+
				"can look in reads as a broker that never ran", ran)
		}
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("the error does not carry the stat's own: %v", err)
		}
	})
	t.Run("no store directory", func(t *testing.T) {
		t.Parallel()
		if ran, err := StoreHasRun(""); err == nil {
			t.Errorf("StoreHasRun(\"\") = %v with no error", ran)
		}
	})
	t.Run("a member that started and stopped", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		never(t, dir)
		srv, err := StartServer(t.Context(), Config{StoreDir: dir})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		srv.Shutdown()
		if ran, err := StoreHasRun(dir); err != nil || !ran {
			t.Errorf("StoreHasRun after a member ran on it = %v, %v, want a broker that has run",
				ran, err)
		}
	})
}
