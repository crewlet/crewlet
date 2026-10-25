package jetstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A SECOND BROKER ON ONE STORE DIRECTORY IS REFUSED, before it reads anything,
// and told who holds it.
//
// nats-server accepts the second one and both write the directory, so without
// this the corruption is silent until a read meets it. In-process here, which
// is the harder half: two brokers in one process are exactly as bad as two
// processes, so a lock that only excluded other processes would wave this
// through.
//
// Mutation: drop the lock from startEmbedded (or take it per process rather
// than per descriptor) and the second start succeeds.
func TestASecondBrokerOnOneStoreDirectoryIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, err := StartServer(t.Context(), Config{StoreDir: dir})
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	t.Cleanup(first.Shutdown)

	second, err := StartServer(t.Context(), Config{StoreDir: dir})
	if err == nil {
		second.Shutdown()
		t.Fatal("a second broker started on a store directory the first is running on")
	}
	if !errors.Is(err, ErrStoreDirHeld) {
		t.Fatalf("the refusal is not ErrStoreDirHeld: %v", err)
	}
	for _, want := range []string{dir, "pid " + strconv.Itoa(os.Getpid())} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, StoreDirLock)); err != nil {
		t.Errorf("no lock file inside the store directory: %v", err)
	}
}

// AND A SHUT-DOWN BROKER LETS THE NEXT ONE IN: an engine restart, or a
// `crewlet` command that reached the fleet's store and exited, must not leave
// the directory locked against the engine that starts after it.
//
// Mutation: drop the release from shutdown and the restart is refused.
func TestAStoppedBrokerReleasesItsStoreDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, err := StartServer(t.Context(), Config{StoreDir: dir})
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	first.Shutdown()

	second, err := StartServer(t.Context(), Config{StoreDir: dir})
	if err != nil {
		t.Fatalf("the directory stayed held after its broker stopped: %v", err)
	}
	second.Shutdown()
}

// A START THAT FAILS LETS THE DIRECTORY GO TOO. Held past a failed start, the
// lock would refuse every retry for as long as the failed process lived — in
// a test binary, every later case on the directory.
//
// The start fails on its caller's context, cancelled before it begins: the
// lock is taken, the server built and started, and the wait for it ends on the
// cancel, which shuts the server down again.
//
// Mutation: drop the release from startEmbedded's failure path and the next
// start on the directory is refused.
func TestAFailedStartReleasesItsStoreDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if srv, err := StartServer(cancelled, Config{StoreDir: dir}); err == nil {
		// The start can win its race with the cancel; then it is an
		// ordinary start, and the case below holds it to its shutdown.
		srv.Shutdown()
	}
	srv, err := StartServer(t.Context(), Config{StoreDir: dir})
	if err != nil {
		t.Fatalf("the directory stayed held after a failed start: %v", err)
	}
	srv.Shutdown()
}

// A STORE DIRECTORY THAT IS NOT ONE IS REFUSED AT THE LOCK, naming the setting
// to fix, before any server is built — the lock is taken inside the directory,
// so a file where the directory goes fails there rather than in nats-server.
//
// Mutation: swallow the lock's failure to prepare the directory and the start
// reaches nats-server, which dies on the store instead of naming the setting.
func TestAStoreDirectoryThatIsAFileIsRefusedNamingTheSetting(t *testing.T) {
	t.Parallel()
	store := filepath.Join(t.TempDir(), "store")
	if err := os.WriteFile(store, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := StartServer(t.Context(), Config{StoreDir: store})
	if err == nil {
		srv.Shutdown()
		t.Fatal("a broker started on a store directory that is a file")
	}
	if !strings.Contains(err.Error(), "stream.store_dir "+store) {
		t.Errorf("the refusal does not name the setting and its value: %v", err)
	}
	if errors.Is(err, ErrStoreDirHeld) {
		t.Errorf("a store directory that is a file is reported as held by another broker: %v", err)
	}
}
