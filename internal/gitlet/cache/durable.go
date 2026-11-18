package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// Every cache file but CACHEDIR.TAG (writeTag) is staged in tmp/ and made
// durable there, then placed: renamed over its destination, each directory a
// step placed into synced once before anything names its files (package doc,
// "Every write is a rename, and a step is synced once"). A downloaded pack's
// three files sync together with every other pack its round is placing
// (install.go's placePack), so their syncs share journal commits.
//
// Not hostbox.ReplaceFile: it stages beside its destination, so a crash
// leaves a temporary file in a repository's directory, and it keeps no order
// across files, on which the cache's correctness rests. tmp/ and repos/ share
// the cache's root, one filesystem, so every rename is atomic.

// stage writes a new file in tmp/ through fill, sets its mode, syncs and
// closes it, and returns its path; on failure the file is removed.
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

// place renames a staged file over dst, removing it if the rename fails. The
// rename is NOT yet durable: the caller syncs each directory its step placed
// into once ([syncDir]) before anything names the files. EXDEV cannot happen
// inside one root, so it is returned as any other error.
func (c *Cache) place(staged, dst string) error {
	if err := os.Rename(staged, dst); err != nil {
		return removeStaged(staged, fmt.Errorf("cache: renaming %s over %s: %w", staged, dst, err))
	}
	return nil
}

// durableDir is syncDir for a swap, a state's write and a code index build,
// reported first to the suite's knobs.syncing so a case can count the syncs.
func (c *Cache) durableDir(dir string) error {
	if c.knobs != nil && c.knobs.syncing != nil {
		c.knobs.syncing(dir)
	}
	return syncDir(dir)
}

// syncDir makes every rename into dir and every file created in it so far
// durable.
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

// removeStaged removes a staged file given up on before its rename and
// returns err, joined with the removal's failure so a file left in tmp/ is
// never unmentioned.
func removeStaged(staged string, err error) error {
	if rerr := os.Remove(staged); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("cache: removing %s: %w", staged, rerr))
	}
	return err
}
