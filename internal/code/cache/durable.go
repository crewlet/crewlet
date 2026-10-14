package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// THE WAY A CACHE FILE IS WRITTEN: STAGED in tmp/ and made durable there,
// then PLACED — renamed over its destination; and before any file that NAMES
// what a step placed is itself placed, the directory each of the step's files
// landed in is SYNCED, once for the whole step. The rule is "what a file
// names is durable before that file is renamed in": a pack's three files
// before the refs that need its objects, the refs before the state that
// claims their position. So a step that places many files in one directory —
// a round installing a thousand packs into objects/pack — pays ONE directory
// sync, not one a file: each sync is a journal commit, and paid a rename it
// made every rename about nine times dearer (the package doc's Measured
// section). A downloaded pack's three files are synced AT ONCE, beside every
// other pack its round is placing meanwhile (install.go's placePack), so their
// syncs share journal commits rather than each paying one. A reader — a git
// process that already opened the old file — keeps its descriptor; a new one
// opens the whole new file or the whole old one, never a half.
//
// The one file written otherwise is CACHEDIR.TAG, in place and first, before
// tmp/ exists (writeTag says why).
//
// Not hostbox.ReplaceFile, deliberately: that stages BESIDE its destination,
// so a crash would leave a temporary file inside a repository's directory,
// and it leaves no room for the order: the cache's correctness is an ORDER
// across files — what a file claims must already be on disk — and a rename
// that has not reached the disk keeps no order. tmp/ and repos/ are both
// under the cache's root, on one filesystem, so every rename is atomic.

// stage writes a new file in tmp/ through fill, sets its mode, makes it
// durable and closes it, and returns its path. A file stage made and could
// not finish is removed.
func (c *Cache) stage(mode os.FileMode, fill func(f *os.File) error) (string, error) {
	f, err := os.CreateTemp(filepath.Join(c.dir, tmpDir), "stage-*")
	if err != nil {
		return "", fmt.Errorf("cache: staging a file: %w", err)
	}
	staged := f.Name()
	err = fill(f)
	if err == nil && mode != hostbox.FileMode {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(staged)
		return "", fmt.Errorf("cache: staging %s: %w", staged, err)
	}
	return staged, nil
}

// place renames a staged file over dst. The rename is NOT yet durable: the
// caller places every file of the step, then syncs each directory they
// landed in once ([syncDir]) before anything names them. The staged file is
// removed when the rename fails. A rename across filesystems (EXDEV) cannot
// happen inside one root and is returned as it is, naming both paths.
func (c *Cache) place(staged, dst string) error {
	if err := os.Rename(staged, dst); err != nil {
		return removeStaged(staged, fmt.Errorf("cache: renaming %s over %s: %w", staged, dst, err))
	}
	return nil
}

// syncDir makes dir's entries durable: every rename into it, every file
// created in it, so far.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("cache: opening %s to sync it: %w", dir, err)
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("cache: syncing %s: %w", dir, err)
	}
	return nil
}

// removeStaged removes a file a writer staged and then gave up on before its
// rename, and answers err — joined with the removal's own failure, if any,
// so a file left behind in tmp/ is never left unmentioned.
func removeStaged(staged string, err error) error {
	if rerr := os.Remove(staged); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("cache: removing %s: %w", staged, rerr))
	}
	return err
}
