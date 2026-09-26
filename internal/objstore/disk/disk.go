// Package disk is one data node's own chunks, on its own filesystem.
//
// # Every byte is checked against its name, both ways
//
// A chunk is stored under its content hash and is VERIFIED on the way in —
// bytes that do not hash to the name they were sent under are refused — and on
// the way out: a file that no longer hashes to its name is a disk that rotted,
// and it is removed and reported missing rather than served. Repair then
// fetches a good copy from a peer, which is what makes a corrupted chunk a
// replica count that dips rather than bytes somebody downloads.
//
// # Atomic, and durable before it is acknowledged
//
// A write goes to a temporary file in the chunk's own directory, is synced,
// renamed over its final name and the directory synced: a crash leaves either
// the whole chunk or no chunk, never a torn one that a later read would have
// to catch. An acknowledgement a writer counts toward its replicas is one that
// survives this node losing power.
//
// # Grouped by placement group
//
// A chunk's directory is its placement group, two hex digits, so a repair or
// a map change that moves one group walks one directory rather than every
// chunk the node holds.
//
// # A write restarts the collector's grace
//
// A chunk that is already held is not written again, but it IS touched: its
// modification time is when the collector's grace for an unreferenced chunk
// starts, and a chunk uploaded again for a record that has not landed yet —
// the same bytes a deleted file once held — must get that record's whole
// grace, not whatever was left of the old one. The touch and the collector's
// age check take the same lock, so a chunk a writer has just been told is
// stored is never one the collector judged by its old age a moment later.
//
// # One process owns the directory
//
// [Open] takes an exclusive advisory lock on a file inside it and holds it
// until [Store.Close], for the store's own reason: the kernel releases it
// however the holder exits, so there is no stale state to reap. It is what
// makes a staged write found at Open a crash's debris rather than a peer's
// write in flight — and what refuses a second engine pointed at the same
// directory, which would otherwise delete the first one's chunks as garbage
// it does not recognise.
package disk

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/placement"
)

// ErrNotFound is a chunk this node does not hold — including one it held and
// found corrupt.
var ErrNotFound = errors.New("objstore/disk: chunk not held here")

// ErrMismatch is bytes that do not hash to the name they were offered under.
var ErrMismatch = errors.New("objstore/disk: the bytes do not match their hash")

// Store is a directory of chunks.
type Store struct {
	root string

	// mu guards lock, whose nil is a store that was closed: every method
	// after Close refuses, because a directory this process no longer holds
	// the lock on is one another process may own.
	mu   sync.RWMutex
	lock *os.File

	// groups serialises, per placement group, the two decisions that read
	// a held chunk's age: a writer touching it and the collector judging
	// it. Striped by group because nothing else about two chunks is shared.
	groups [placement.PGCount]sync.Mutex
}

// staged is the prefix of a write in progress.
const staged = ".incoming-"

// Open prepares a chunk directory, creating it if it does not exist, and
// clears what a crash left staged.
func Open(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("objstore/disk: no directory")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("objstore/disk: create %s: %w", root, err)
	}
	lock, err := os.OpenFile(filepath.Join(root, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("objstore/disk: open the lock in %s: %w", root, err)
	}
	if lockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
		_ = lock.Close()
		if errors.Is(lockErr, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s — another engine keeps its chunks there; "+
				"give each node its own store.objects.dir", ErrLocked, root)
		}
		return nil, fmt.Errorf("objstore/disk: lock %s: %w", root, lockErr)
	}
	s := &Store{root: root, lock: lock}
	debris, err := filepath.Glob(filepath.Join(root, "*", staged+"*"))
	if err == nil {
		for _, path := range debris {
			if err = os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				break
			}
			err = nil
		}
	}
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("objstore/disk: clear what a crash left staged in %s: %w", root, err)
	}
	return s, nil
}

// lockName is the file the directory's lock is taken on.
const lockName = ".lock"

// ErrLocked is a chunk directory another process holds.
var ErrLocked = errors.New("objstore/disk: the chunk directory is in use")

// ErrClosed is a store used after [Store.Close].
var ErrClosed = errors.New("objstore/disk: the chunk directory was released")

// Close releases the directory, waiting for every operation in flight. The
// chunks stay where they are.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close() // closing the descriptor releases the lock
	s.lock = nil
	return err
}

// hold keeps the directory open for one operation, refusing once it is
// released. The returned function ends the hold.
func (s *Store) hold() (func(), error) {
	s.mu.RLock()
	if s.lock == nil {
		s.mu.RUnlock()
		return nil, ErrClosed
	}
	return s.mu.RUnlock, nil
}

// Root is the directory this store keeps its chunks in.
func (s *Store) Root() string { return s.root }

// path is where a chunk lives.
func (s *Store) path(h objstore.Hash) string {
	return filepath.Join(s.root, Layout(h))
}

// Layout is where a chunk lives relative to a chunk directory: its group's
// directory, two hex digits, then its name.
//
// EXPORTED FOR THE BACKUP, which writes chunks in this same shape so that
// restoring them is copying a directory into store.objects.dir — a second
// description of the layout would be a restore that puts every chunk where
// no node looks.
func Layout(h objstore.Hash) string {
	return filepath.Join(fmt.Sprintf("%02x", h.PG()), string(h))
}

// Put stores a chunk after checking it hashes to h.
//
// A chunk already held is not written again — content-addressed bytes under
// one name are the same bytes — but it is READ BACK first, because a copy that
// rotted is not one to count toward a writer's replicas, and it is touched,
// so the collector's grace starts again (see the package doc).
func (s *Store) Put(h objstore.Hash, data []byte) error {
	done, err := s.hold()
	if err != nil {
		return err
	}
	defer done()
	if !h.Valid() {
		return fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	if objstore.HashOf(data) != h {
		return fmt.Errorf("%w: %s", ErrMismatch, h)
	}
	final := s.path(h)
	held, err := s.refresh(h, final)
	if err != nil || held {
		return err
	}
	dir := filepath.Dir(final)
	if err = os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("objstore/disk: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, staged+"*")
	if err != nil {
		return fmt.Errorf("objstore/disk: stage %s: %w", h, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("objstore/disk: write %s: %w", h, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("objstore/disk: sync %s: %w", h, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("objstore/disk: close %s: %w", h, err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return fmt.Errorf("objstore/disk: place %s: %w", h, err)
	}
	cleanup = false
	return syncDir(dir)
}

// refresh touches a held, intact chunk and reports whether there was one.
func (s *Store) refresh(h objstore.Hash, path string) (bool, error) {
	lock := &s.groups[h.PG()]
	lock.Lock()
	defer lock.Unlock()
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("objstore/disk: read %s: %w", h, err)
	}
	if objstore.HashOf(existing) != h {
		// ROTTEN: written over by the caller, whose bytes were checked.
		return false, nil
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		return false, fmt.Errorf("objstore/disk: touch %s: %w", h, err)
	}
	return true, nil
}

// Get reads a chunk and checks it still hashes to its name. A chunk that does
// not is REMOVED and reported not held, so repair replaces it from a peer.
func (s *Store) Get(h objstore.Hash) ([]byte, error) {
	done, err := s.hold()
	if err != nil {
		return nil, err
	}
	defer done()
	if !h.Valid() {
		return nil, fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	data, err := os.ReadFile(s.path(h))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: %s", ErrNotFound, h)
	case err != nil:
		return nil, fmt.Errorf("objstore/disk: read %s: %w", h, err)
	}
	if objstore.HashOf(data) != h {
		_ = os.Remove(s.path(h))
		return nil, fmt.Errorf("%w: %s no longer matches its hash and was removed", ErrNotFound, h)
	}
	return data, nil
}

// Has reports whether this node holds a chunk, without reading it.
func (s *Store) Has(h objstore.Hash) bool {
	done, err := s.hold()
	if err != nil {
		return false
	}
	defer done()
	if !h.Valid() {
		return false
	}
	_, err = os.Stat(s.path(h))
	return err == nil
}

// Collect removes a chunk only if nothing has written it since before, and
// reports whether it did. It is the collector's delete for a chunk nothing
// references: judged by its age under the lock a writer's touch takes, so a
// chunk a writer was just told is stored survives.
func (s *Store) Collect(h objstore.Hash, before time.Time) (bool, error) {
	done, err := s.hold()
	if err != nil {
		return false, err
	}
	defer done()
	if !h.Valid() {
		return false, fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	lock := &s.groups[h.PG()]
	lock.Lock()
	defer lock.Unlock()
	path := s.path(h)
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("objstore/disk: stat %s: %w", h, err)
	}
	if !info.ModTime().Before(before) {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("objstore/disk: delete %s: %w", h, err)
	}
	return true, nil
}

// Delete removes a chunk whatever its age — the collector's delete for a copy
// this node holds beyond its placement, once the members that place it have
// said they hold it. One already gone is not an error.
func (s *Store) Delete(h objstore.Hash) error {
	done, err := s.hold()
	if err != nil {
		return err
	}
	defer done()
	if !h.Valid() {
		return fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	if err := os.Remove(s.path(h)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("objstore/disk: delete %s: %w", h, err)
	}
	return nil
}

// Held is one chunk as a walk finds it.
type Held struct {
	Hash objstore.Hash
	Size int64

	// Written is when the chunk arrived on this node — the instant the
	// collector's grace is measured from, so a chunk written for a record
	// that has not landed yet is not collected before it can.
	Written time.Time
}

// Walk visits every chunk this node holds.
//
// visit MUST NOT CALL THE STORE: the walk holds the directory open throughout,
// and a nested call would wait behind a [Store.Close] that is waiting for the
// walk. Gather what the walk finds and act on it after.
func (s *Store) Walk(visit func(Held) error) error {
	return s.walk(s.root, visit)
}

// WalkGroup visits every chunk this node holds in one placement group.
func (s *Store) WalkGroup(pg int, visit func(Held) error) error {
	return s.walk(filepath.Join(s.root, fmt.Sprintf("%02x", pg)), visit)
}

// walk visits the chunks under one directory. A write in progress is stepped
// over: it is somebody's Put, not a chunk yet.
func (s *Store) walk(dir string, visit func(Held) error) error {
	done, err := s.hold()
	if err != nil {
		return err
	}
	defer done()
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, staged) {
			return nil
		}
		h := objstore.Hash(name)
		if !h.Valid() {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return visit(Held{Hash: h, Size: info.Size(), Written: info.ModTime().UTC()})
	})
}

// syncDir makes a rename durable: until the directory is synced, a crash can
// forget that the chunk was ever placed.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("objstore/disk: open %s: %w", dir, err)
	}
	// A DIRECTORY OPENED TO BE SYNCED holds nothing a failed close could
	// lose; the sync is the answer, and a close error after it is noise.
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("objstore/disk: sync %s: %w", dir, err)
	}
	return nil
}
