package store

import (
	"fmt"
	"os"
	"strings"
)

// The store's files are their owner's alone.
//
// What they hold is the reason: the node estate is this node's audit log, its
// agents' memory, the company's config revisions and the sealed bootstrap half
// of the secret store, and the replicated estate is the company's tracker and
// knowledge base. No other process opens either — the lock beside each
// (lock.go) refuses every other crewlet process, and a copy is taken from
// inside this one ([DB.Backup]) — so a mode wider than the owner's grants
// nothing the engine uses, and grants every local user everything in them.
//
// THE DRIVER DOES NOT DO THIS ON ITS OWN. It creates a database and its -wal
// from the process umask, which under the usual 022 is 0644, and it does not
// give the -wal it makes the mode of the database beside it: measured at the
// pinned driver, a database made 0600 grew a 0644 -wal. So every open readies
// both files BEFORE the driver sees the path ([ownerOnly]).

// fileMode is the mode of every database file the store makes: the owner's to
// read and write, and nobody else's.
const fileMode os.FileMode = 0o600

// beyondOwner is every permission bit that reaches somebody other than the
// file's owner: its group's and everybody's.
const beyondOwner os.FileMode = 0o077

// walSuffix names the sidecar a database in WAL mode keeps committed pages in
// until they are checkpointed into the database itself.
const walSuffix = "-wal"

// ownerOnly readies a database's files for the driver: the database and its
// -wal, each created [fileMode] when absent, and each stripped of every bit
// [beyondOwner] names when present with one.
//
// CREATED EMPTY, which the driver takes for exactly what it would have made
// itself: measured at the pinned driver, an empty database file with an empty
// -wal beside it opens as a new database, and an empty -wal beside a database
// with rows in it opens as a WAL with no frames.
//
// AN EXISTING FILE WITH A WIDER MODE IS TIGHTENED rather than warned about, and
// the node logs `store_file_mode_tightened` naming the file and the mode it
// had. No process but this one reads these files, so the wider mode serves
// nothing the engine does; a grant an operator meant for something else fails
// loudly once it is gone — that reader is refused, naming the path — while a
// mode left open fails silently for as long as nobody reads a warning about
// it. Only the bits beyond the owner's go: the owner's own are left as they
// are.
//
// A file this process cannot tighten — one another user owns — is logged
// (`store_file_mode_not_tightened`) and left for the driver to open, which it
// does or refuses on its own terms: a store that works with a file too open
// is a warning to act on, not a node to take down.
func ownerOnly(path string) error {
	if path == "" || strings.HasPrefix(path, ":memory:") {
		// An in-memory database has no file for anybody to read.
		return nil
	}
	for _, file := range []string{path, path + walSuffix} {
		if err := ownerOnlyFile(file); err != nil {
			return err
		}
	}
	return nil
}

// ownerOnlyFile is [ownerOnly] for one file.
//
// THROUGH ONE DESCRIPTOR: the create, the mode it reports and the tightening
// all name the file this call opened, so a path that changed underneath it
// between a stat and a chmod cannot have the wrong file tightened.
func ownerOnlyFile(file string) error {
	f, err := os.OpenFile(file, os.O_RDONLY|os.O_CREATE, fileMode)
	if err != nil {
		return fmt.Errorf("store: ready %s for the driver: %w", file, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("store: ready %s for the driver: %w", file, err)
	}
	was := info.Mode().Perm()
	if was&beyondOwner == 0 {
		return nil
	}
	now := was &^ beyondOwner
	if err := f.Chmod(now); err != nil {
		log.Warn("store_file_mode_not_tightened",
			"path", file, "mode", fmt.Sprintf("%#o", was), "error", err.Error(),
			"detail", "this file is readable beyond its owner and this process could not "+
				"change that: run the engine as the file's owner, or `chmod go-rwx` it")
		return nil
	}
	log.Warn("store_file_mode_tightened",
		"path", file, "was", fmt.Sprintf("%#o", was), "now", fmt.Sprintf("%#o", now),
		"detail", "no process but this engine opens the store's files, so every permission "+
			"beyond the owner's was taken from this one")
	return nil
}
