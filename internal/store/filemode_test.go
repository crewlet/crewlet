//go:build (linux || darwin) && (amd64 || arm64)

package store

import (
	"bytes"
	"context"
	"database/sql/driver"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/logging"
)

// underUmask runs fn with the process umask at mask, and puts the old one back.
//
// NOT PARALLEL, and it cannot be: the umask is the process's, so a case that
// sets it runs while no other case does. It exists because what the driver
// makes on its own depends on it, and a case that took whatever umask the
// machine had would pass under 077 whether or not the store did anything.
func underUmask(t *testing.T, mask int, fn func()) {
	t.Helper()
	old := unix.Umask(mask)
	defer unix.Umask(old)
	fn()
}

// modeOf is a file's permission bits.
func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// THE STORE'S FILES ARE THEIR OWNER'S ALONE, whatever the umask makes of what
// the driver creates, and under a data directory every local user can list.
// The node estate is the audit log, the agents' memory, the config revisions
// and the sealed bootstrap secrets; the replicated one is the company's
// tracker and knowledge base. Under the usual umask of 022 the driver makes
// each database and its -wal 0644 on its own.
func TestTheStoresFilesAreTheOwnersAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	underUmask(t, 0o022, func() {
		db, err := Open(context.Background(), filepath.Join(dir, "node.db"), Options{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		if mode := modeOf(t, filepath.Join(dir, entry.Name())); mode&0o077 != 0 {
			t.Errorf("%s is %#o: every local user can read it", entry.Name(), mode)
		}
	}
	for _, want := range []string{"node.db", "node.db-wal", replicatedFileName, replicatedFileName + "-wal"} {
		if !slices.Contains(names, want) {
			t.Errorf("%s was not made, so its mode was never checked: the directory holds %v", want, names)
		}
	}
}

// lockedBuffer is a buffer the logger may write from any goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A STORE FILE READABLE BEYOND ITS OWNER IS TIGHTENED, AND THE NODE SAYS SO.
// A database the driver made before its opener readied the files is 0644, and
// so is its -wal; nothing but the engine opens either, so the wider mode serves
// nobody the engine knows of and exposes everything in them. The next open
// takes every bit beyond the owner's, and the line names the file and the mode
// it had, so an operator whose own grant went with it learns why.
//
// Not parallel: it reads the node's log, which is the process's.
func TestAStoreFileReadableBeyondItsOwnerIsTightened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")
	db, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	files := []string{path, path + walSuffix}
	for _, file := range files {
		if err := os.Chmod(file, 0o644); err != nil {
			t.Fatalf("widen %s: %v", file, err)
		}
	}

	var lines lockedBuffer
	logging.Configure(slog.LevelWarn, logging.FormatText, &lines)
	defer logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	db, err = Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, file := range files {
		if mode := modeOf(t, file); mode != fileMode {
			t.Errorf("%s is %#o after the store opened it, want %#o", filepath.Base(file), mode, fileMode)
		}
		found := false
		for _, line := range strings.Split(lines.String(), "\n") {
			if strings.Contains(line, "store_file_mode_tightened") && strings.Contains(line, file+" ") &&
				strings.Contains(line, "was=0644") {
				found = true
			}
		}
		if !found {
			t.Errorf("no store_file_mode_tightened line names %s and the 0644 it had; the log holds:\n%s",
				filepath.Base(file), lines.String())
		}
	}
}

// vacuumWitness records, the moment a copy exists, where the driver wrote it
// and what that directory let other users do.
type vacuumWitness struct {
	mu   sync.Mutex
	dirs []string
	mode []os.FileMode
}

func (w *vacuumWitness) wrap(d driver.Driver) driver.Driver { return witnessDriver{inner: d, w: w} }

func (w *vacuumWitness) record(target string) {
	dir := filepath.Dir(target)
	info, err := os.Stat(dir)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dirs = append(w.dirs, dir)
	if err != nil {
		w.mode = append(w.mode, 0o777)
		return
	}
	w.mode = append(w.mode, info.Mode().Perm())
}

type witnessDriver struct {
	inner driver.Driver
	w     *vacuumWitness
}

func (d witnessDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	// The store's own connection, embedded so every interface it answers
	// is still answered; only the statement this watches is intercepted.
	return &witnessConn{beginModeConn: conn.(*beginModeConn), w: d.w}, nil
}

type witnessConn struct {
	*beginModeConn
	w *vacuumWitness
}

func (c *witnessConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	res, err := c.beginModeConn.ExecContext(ctx, q, args)
	if literal, ok := strings.CutPrefix(q, "VACUUM INTO "); ok && err == nil {
		target := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(literal, "'"), "'"), "''", "'")
		c.w.record(target)
	}
	return res, err
}

// A BACKUP IS NEVER REACHABLE BY ANOTHER USER WHILE IT IS WRITTEN. The driver
// makes the copy from the umask, typically 0644, and cannot be handed a file
// made for it in advance, so a copy written straight into the destination's
// directory is readable there for the whole of the copy whenever that
// directory is — and an operator's `mkdir` makes one that is. So the copy is
// written in a stage only its owner can enter, and the stage is gone once the
// copy is in place.
func TestABackupIsNeverReachableByAnotherUserWhileItIsWritten(t *testing.T) {
	t.Parallel()
	witness := &vacuumWitness{}
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "node.db"),
		Options{WrapDriver: witness.wrap})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	loose := filepath.Join(t.TempDir(), "backups")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(loose, "copy.db")
	if _, err := db.Backup(context.Background(), dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	witness.mu.Lock()
	dirs, modes := slices.Clone(witness.dirs), slices.Clone(witness.mode)
	witness.mu.Unlock()
	if len(dirs) != 1 {
		t.Fatalf("saw %d copies written, want the one: %v", len(dirs), dirs)
	}
	if dirs[0] == loose {
		t.Errorf("the copy was written straight into %s, which every local user can enter", loose)
	}
	if modes[0]&0o077 != 0 {
		t.Errorf("the copy was written in %s, which is %#o", dirs[0], modes[0])
	}
	entries, err := os.ReadDir(loose)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "copy.db" {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the destination's directory holds %v, want the copy alone: the stage outlived it", names)
	}
}
