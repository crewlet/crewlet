// Package disk is one data node's own chunks, on its own filesystem.
//
// # Every byte is checked against its name, both ways
//
// A chunk is stored under its content hash and is VERIFIED on the way in —
// bytes that do not hash to the name they were sent under are refused — and on
// the way out: a file that no longer hashes to its name is a disk that rotted,
// and it is removed and reported missing rather than served. Repair then
// fetches a good copy from a peer, which is what makes a corrupted chunk a
// replica count that dips rather than bytes somebody downloads. The same check
// runs without a reader asking, a slot at a time over the week
// ([Store.Verify], driven by upkeep's scrub), because a chunk nobody reads is a
// chunk whose rot nobody would otherwise find until its other copies had
// rotted too.
//
// A file whose bytes CANNOT BE READ AT ALL — it opens, and the read fails: the
// latent sector error, the commoner way a disk loses a chunk — is removed in
// the same way ([ErrUnreadable]). Left in place it was worse than rot: it still
// answered [Store.Has], so repair counted it held and never replaced it, a
// good copy could not be written over it because the write's own check read
// it first, and the scrub stopped at it for good. The error is still counted
// toward the store's health; only the file goes.
//
// A bad file is removed only if it is STILL the file that was read: the
// removal takes the directory's lock, which a write's rename takes too, and
// compares the file it finds with the one it read. Otherwise a reader holding
// bad bytes and a writer replacing them with good ones could interleave so
// that the reader removed the good copy the writer had just been told was
// stored.
//
// # Atomic, and durable before it is acknowledged
//
// A write goes to a temporary file in the chunk's own directory, is synced,
// renamed over its final name and the directory synced: a crash leaves either
// the whole chunk or no chunk, never a torn one that a later read would have
// to catch. An acknowledgement a writer counts toward its replicas is one that
// survives this node losing power.
//
// # Laid out by the first byte
//
// A chunk's directory is the first byte of its address, two hex digits
// ([Layout]) — the top eight bits of its slot. Every placement group, at every
// group count a map may have, is a contiguous range of slots, and a map has at
// least eight group bits, so a group lives inside ONE directory — the whole of
// it at eight bits, a filtered part of it at more ([Store.WalkSlots]) — and a
// pass over one group never lists the rest of the disk.
//
// # The store keeps its own layout
//
// A chunk file found in a directory that is not its own — a hand-copied
// restore, a directory arranged by an earlier layout — is moved to where it
// belongs when a walk or [Open] comes across it, and the moves are logged once
// per sweep (`objects_chunk_relocated`). This is the store holding its own
// invariant rather than a reader kept for an old version: every lookup goes
// straight to [Layout], so a misplaced chunk is one no read, no repair and no
// collection can see, and moving it costs a rename where leaving it costs a
// copy fetched again from a peer. A misplaced file whose right place already
// holds the chunk is removed instead — the name is the same bytes, and a copy
// that rotted in the wrong place is not one to move over a good one.
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
// # The store knows whether it can hold chunks
//
// [Store.Health] is what this node tells the fleet about its own disk, and the
// map takes a node out on it. Three facts feed it. A PROBE ([Store.Probe], run
// by the engine every heartbeat) writes, syncs, reads back and removes a small
// file and measures the volume: a probe that fails is a store that is FAILED,
// whatever else it answers. The volume's fullness makes it NEARFULL past
// [NearFullRatio], and FULL past [FullRatio], where a new chunk is refused
// ([ErrFull]) so the writer puts the copy on the next member rather than on a
// disk about to refuse everything. And [FailedAfter] consecutive I/O errors
// from the chunk operations themselves make it failed between probes; the
// next probe that succeeds clears it.
//
// The volume is measured by the filesystem's own statfs unless
// [Options.Space] supplies the reading, and nothing an engine runs supplies
// one: it exists because a test's store sits in a temporary directory on
// whatever disk the machine running it has left, and a suite about moving
// chunks must not pass or fail on that.
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
//
// Beside the chunk directories the root holds the lock (`.lock`), the probe's
// file while a probe runs (`.probe`) and the scrub's cursor (`.scrub`, which
// internal/objstore/upkeep keeps). None is a content address, and nothing that
// is not one is ever read as a chunk, moved or collected.
package disk

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/placement"
)

var log = logging.Get("objstore")

// ErrNotFound is a chunk this node does not hold — including one it held and
// found corrupt.
var ErrNotFound = errors.New("objstore/disk: chunk not held here")

// errRotten is a chunk that was held, no longer matched its name, and was
// removed: not held, and a fact about the disk the scrub counts.
var errRotten = fmt.Errorf("%w: it no longer matched its hash and was removed", ErrNotFound)

// ErrUnreadable is a chunk held here whose bytes the disk would not return:
// the file opened, and reading it failed. The file is removed, as a rotten one
// is, and the error then also wraps [ErrNotFound] — the chunk is not held any
// more, which is what a reader and a repair need to hear. Where the removal
// failed too, it does not: the file is still there, and what is wrong is the
// disk rather than one chunk.
var ErrUnreadable = errors.New("objstore/disk: the chunk's bytes could not be read")

// ErrMismatch is bytes that do not hash to the name they were offered under.
var ErrMismatch = errors.New("objstore/disk: the bytes do not match their hash")

// ErrFull is a new chunk refused because the volume is past [FullRatio].
var ErrFull = errors.New("objstore/disk: the object store's volume is full")

// ErrFailed is a chunk refused because the store is failed: its probe did not
// complete, or its operations have failed [FailedAfter] times in a row.
var ErrFailed = errors.New("objstore/disk: the object store is failed")

// Store is a directory of chunks.
type Store struct {
	root string

	// mu guards lock, whose nil is a store that was closed: every method
	// after Close refuses, because a directory this process no longer holds
	// the lock on is one another process may own.
	mu   sync.RWMutex
	lock *os.File

	// dirs serialises, per directory, every decision that has to see a
	// chunk file as it is: a writer touching it or renaming it into place,
	// the collector judging its age, a reader removing it as rotten and a
	// misplaced copy moved onto it. Striped by directory because nothing
	// else about two chunks is shared.
	dirs [dirCount]sync.Mutex

	// probing serialises [Store.Probe]: two at once would write, read and
	// remove one file.
	probing sync.Mutex

	// space measures the volume: [Options.Space], or statfs.
	space func(dir string) (capacity, free uint64, err error)

	// read reads an open chunk file: [Options.Read], or io.ReadAll.
	read func(f *os.File) ([]byte, error)

	health health
}

// dirCount is how many chunk directories there are: one per first byte.
const dirCount = 256

// dirShift is how far a slot shifts to its directory: a slot is two bytes and
// a directory is the first of them.
const dirShift = placement.SlotBits - 8

// staged is the prefix of a write in progress.
const staged = ".incoming-"

// Options is what a store is opened with beyond its directory. The zero value
// is the store an engine runs, and [Open] opens with it.
type Options struct {
	// Space measures the volume dir is on, and every probe asks it — the
	// one [Open] makes included, so a store never answers for a volume
	// other than the one it was given. capacity is the bytes the volume
	// offers this process, those in use plus those it may still write, and
	// free is the bytes it may still write: the store is
	// (capacity - free) / capacity used. A capacity of zero is a volume
	// whose fullness cannot be judged and is never called full; free beyond
	// capacity is not a reading at all, and fails the probe that took it.
	//
	// NIL IS THE FILESYSTEM'S OWN statfs, counted as df counts it, so the
	// blocks a filesystem reserves for root — space this process can never
	// write — are in neither number.
	//
	// A READING IS SUPPLIED WHERE THE VOLUME UNDER THE DIRECTORY IS NOT THE
	// ONE THAT SHOULD DECIDE THE STORE'S HEALTH, which is a test's temporary
	// directory and nothing else: it sits on whatever disk the machine
	// running the test has left, and a store opened there on the real
	// reading is nearfull on a machine 85% used and refuses every new chunk
	// ([ErrFull]) past 95%, so a test of something else would answer for
	// the machine rather than for what it asserts. An engine that supplied
	// one would tell the fleet its disk has room it may not have.
	Space func(dir string) (capacity, free uint64, err error)

	// Read reads the whole of a chunk file the store has opened, and every
	// read of a chunk's bytes goes through it.
	//
	// NIL IS io.ReadAll, the only read an engine runs.
	//
	// SUPPLIED WHERE A TEST NEEDS THE ONE FAILURE A HEALTHY DISK CANNOT BE
	// MADE TO PRODUCE: a regular file that opens and then will not read —
	// the latent sector error the scrub exists to find. Every other failure
	// has a real way in (a directory where a file belongs, a volume
	// reading supplied through Space); this one has none short of a
	// failing device.
	Read func(f *os.File) ([]byte, error)
}

// Open is [Options.Open] with the zero options: the store on the volume its
// directory is really on, which is the only store an engine opens.
func Open(root string) (*Store, error) { return Options{}.Open(root) }

// Open prepares a chunk directory, creating it if it does not exist: it clears
// what a crash left staged, moves every chunk it finds out of place to where
// it belongs, and probes the volume once, so [Store.Health] never answers for
// a store nothing has looked at.
func (o Options) Open(root string) (*Store, error) {
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
	space := o.Space
	if space == nil {
		space = statfs
	}
	read := o.Read
	if read == nil {
		read = func(f *os.File) ([]byte, error) { return io.ReadAll(f) }
	}
	s := &Store{root: root, lock: lock, space: space, read: read}
	if err := s.tidy(); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("objstore/disk: tidy %s: %w", root, err)
	}
	s.Probe()
	return s, nil
}

// tidy is what [Open] does to the directory before anything reads it: every
// staged write removed — a crash's debris, since the lock says nobody else is
// writing — and every chunk out of place moved to its own directory.
//
// ONE LISTING OF EVERY DIRECTORY, which removing the staged writes always
// cost, so checking every name against its directory adds nothing to it.
func (s *Store) tidy() error {
	moved := 0
	if err := s.relocateRoot(&moved); err != nil {
		return err
	}
	for dir := range dirCount {
		path := filepath.Join(s.root, dirName(dir))
		entries, err := os.ReadDir(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			if strings.HasPrefix(e.Name(), staged) {
				err := os.Remove(filepath.Join(path, e.Name()))
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("clear what a crash left staged: %w", err)
				}
				continue
			}
			if h := objstore.Hash(e.Name()); h.Valid() && dirOf(h) != dir {
				if err := s.relocate(h, filepath.Join(path, e.Name())); err != nil {
					return err
				}
				moved++
			}
		}
	}
	logRelocated(s.root, moved)
	return nil
}

// relocateRoot moves every chunk lying in the root itself — a flat copy of a
// store — to its directory.
func (s *Store) relocateRoot(moved *int) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		h := objstore.Hash(e.Name())
		if !e.Type().IsRegular() || !h.Valid() {
			continue
		}
		if err := s.relocate(h, filepath.Join(s.root, e.Name())); err != nil {
			return err
		}
		*moved++
	}
	return nil
}

// relocate moves a chunk file found at from to where it belongs.
//
// UNDER THE DESTINATION'S LOCK, which a write's rename takes too, so the check
// that the destination is empty and the rename onto it cannot straddle a write
// landing there. NOT SYNCED: a crash leaves the file in one place or the
// other, never in neither, and the next sweep moves it again — where a sync
// per file would make a large misplaced restore cost an fsync per chunk.
func (s *Store) relocate(h objstore.Hash, from string) error {
	lock := &s.dirs[dirOf(h)]
	lock.Lock()
	defer lock.Unlock()
	to := s.path(h)
	_, err := os.Lstat(to)
	switch {
	case err == nil:
		if rmErr := os.Remove(from); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return fmt.Errorf("remove %s, a second copy of %s out of place: %w", from, h, rmErr)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("look for %s where it belongs: %w", h, err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(to), err)
	}
	if err := os.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("move %s to where it belongs: %w", from, err)
	}
	return nil
}

// logRelocated says, once per sweep, that chunks were out of place.
func logRelocated(root string, moved int) {
	if moved > 0 {
		log.Info("objects_chunk_relocated", "dir", root, "chunks", moved,
			"detail", "chunk files were outside their own directory and were moved "+
				"to where every read looks for them")
	}
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

// dirOf is a chunk's directory: the first byte of its address.
func dirOf(h objstore.Hash) int { return h.Slot() >> dirShift }

// dirName is a directory's name: two lowercase hex digits.
func dirName(dir int) string { return fmt.Sprintf("%02x", dir) }

// Layout is where a chunk lives relative to a chunk directory: its first
// byte's directory, two hex digits, then its name.
//
// EXPORTED FOR THE BACKUP, which writes chunks in this same shape so that
// restoring them is copying a directory into store.objects.dir — a second
// description of the layout would be a restore that puts every chunk where
// no node looks.
func Layout(h objstore.Hash) string {
	return filepath.Join(dirName(dirOf(h)), string(h))
}

// Put stores a chunk after checking it hashes to h.
//
// A chunk already held is not written again — content-addressed bytes under
// one name are the same bytes — but it is READ BACK first, because a copy that
// rotted or will not read is not one to count toward a writer's replicas — it
// is written over with the caller's checked bytes instead — and it is
// touched, so the collector's grace starts again (see the package doc).
//
// A FAILED store refuses every chunk ([ErrFailed]) and a FULL one every chunk
// it does not already hold ([ErrFull]) — one it holds costs no space, and
// counting it is honest — so the writer takes the copy to the next member.
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
	if why := s.failure(); why != "" {
		return fmt.Errorf("%w: %s", ErrFailed, why)
	}
	final := s.path(h)
	held, err := s.refresh(h, final)
	if err != nil {
		return s.fault(err)
	}
	if held {
		s.succeeded()
		return nil
	}
	if now := s.Health(); now.State == HealthFull {
		return fmt.Errorf("%w: %s", ErrFull, now.Detail)
	}
	if err := s.write(h, final, data); err != nil {
		return s.fault(err)
	}
	s.succeeded()
	return nil
}

// write stages a chunk beside its final name, syncs it, renames it into place
// and syncs the directory.
func (s *Store) write(h objstore.Hash, final string, data []byte) error {
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o750); err != nil {
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
	// UNDER THE DIRECTORY'S LOCK, and only the rename: a reader removing a
	// rotten copy and a misplaced copy being moved in both decide on the
	// file they find under it, so neither can act on a file this rename is
	// replacing. The sync after it needs no lock, and holding one through
	// it would make every write to the directory wait for every other's.
	lock := &s.dirs[dirOf(h)]
	lock.Lock()
	placed := os.Rename(tmp.Name(), final)
	lock.Unlock()
	if placed != nil {
		return fmt.Errorf("objstore/disk: place %s: %w", h, placed)
	}
	cleanup = false
	return syncDir(dir)
}

// refresh touches a held, intact chunk and reports whether there was one.
//
// A COPY THAT WILL NOT READ IS NOT HELD, exactly as a rotten one is not: the
// caller's checked bytes are renamed over it. Answering the read's error
// instead — as this once did — refused every good copy a writer or a repair
// offered, so a chunk whose sector went bad was a copy short on this node for
// ever. It is logged rather than counted toward the store's health: the Put
// goes on to write, and whether the disk is working is what that write says.
func (s *Store) refresh(h objstore.Hash, path string) (bool, error) {
	lock := &s.dirs[dirOf(h)]
	lock.Lock()
	defer lock.Unlock()
	existing, err := s.readFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		log.Warn("objects_chunk_unreadable", "dir", s.root, "chunk", string(h),
			"error", err.Error(), "detail", "the copy held here could not be read; "+
				"the good bytes being stored are written over it")
		return false, nil
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

// readFile reads a whole chunk file through the store's read.
func (s *Store) readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return s.read(f)
}

// Get reads a chunk and checks it still hashes to its name. A chunk that does
// not, or whose bytes will not read, is REMOVED and reported not held, so
// repair replaces it from a peer.
func (s *Store) Get(h objstore.Hash) ([]byte, error) {
	done, err := s.hold()
	if err != nil {
		return nil, err
	}
	defer done()
	if !h.Valid() {
		return nil, fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	return s.load(h)
}

// Verify reads a chunk and checks it still hashes to its name: true when it
// does; false and no error when it did not and was removed, as [Store.Get]
// removes it; [ErrUnreadable] when its bytes would not read — wrapping
// [ErrNotFound] too once it was removed; [ErrNotFound] alone when it was not
// held at all. It is the scrub's read, and what a member does before telling
// a collector its copy is good.
func (s *Store) Verify(h objstore.Hash) (bool, error) {
	done, err := s.hold()
	if err != nil {
		return false, err
	}
	defer done()
	if !h.Valid() {
		return false, fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	_, err = s.load(h)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errRotten):
		return false, nil
	}
	return false, err
}

// load reads a chunk and verifies it, removing it if it rotted or its bytes
// would not read.
func (s *Store) load(h objstore.Hash) ([]byte, error) {
	f, err := os.Open(s.path(h))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.succeeded()
		return nil, fmt.Errorf("%w: %s", ErrNotFound, h)
	case err != nil:
		// NOT OPENED IS NOT UNREADABLE, and nothing is removed: a name
		// that will not open is as often every name — a directory whose
		// permissions or ownership changed under a restore — and removing
		// on it would empty a working store of every chunk a reader asked
		// for. The fault is counted, and a store that keeps answering it
		// is failed.
		return nil, s.fault(fmt.Errorf("objstore/disk: open %s: %w", h, err))
	}
	defer func() { _ = f.Close() }()
	read, err := f.Stat()
	if err != nil {
		return nil, s.fault(fmt.Errorf("objstore/disk: stat %s: %w", h, err))
	}
	if !read.Mode().IsRegular() {
		// NOTHING THIS STORE WROTE: a directory or a device under a
		// chunk's name was put there by hand, and is neither read as a
		// chunk nor removed as one.
		return nil, s.fault(fmt.Errorf("objstore/disk: %s is not a file", h))
	}
	data, err := s.read(f)
	if err != nil {
		return nil, s.unreadable(h, read, err)
	}
	if objstore.HashOf(data) == h {
		s.succeeded()
		return data, nil
	}
	if err := s.removeBad(h, read, "it rotted"); err != nil {
		return nil, s.fault(err)
	}
	s.succeeded()
	return nil, fmt.Errorf("%w: %s", errRotten, h)
}

// unreadable answers a chunk file that opened and then would not read: the
// error counted toward the store's health, and the file removed so the chunk
// is not held — what repair replaces, and what a good copy may be written
// over.
//
// REMOVED, NOT RETRIED: an EIO reaching this process has already been retried
// by the kernel's block layer, so a second read here asks the same bad sector
// again. And ONE FAULT for the whole operation, as for any other: a removal
// that fails too is the same failing disk, not a second failed operation.
func (s *Store) unreadable(h objstore.Hash, read fs.FileInfo, readErr error) error {
	readErr = s.fault(fmt.Errorf("objstore/disk: read %s: %w", h, readErr))
	if err := s.removeBad(h, read, "its bytes could not be read"); err != nil {
		return fmt.Errorf("%w: %w; and it could not be removed: %w", ErrUnreadable, readErr, err)
	}
	return fmt.Errorf("%w: %w; it was removed: %w", ErrUnreadable, readErr, ErrNotFound)
}

// removeBad removes a chunk found bad — rotten, or unreadable — if the file
// under its name is still the one that was read. A write that replaced it
// since put good bytes there, and told a writer they were stored.
func (s *Store) removeBad(h objstore.Hash, read fs.FileInfo, why string) error {
	lock := &s.dirs[dirOf(h)]
	lock.Lock()
	defer lock.Unlock()
	path := s.path(h)
	now, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("objstore/disk: stat %s: %w", h, err)
	case !os.SameFile(read, now):
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("objstore/disk: remove %s, because %s: %w", h, why, err)
	}
	return nil
}

// Has reports whether this node holds a chunk, without reading it: a FILE
// under its name, as a walk counts one — a directory put there by hand is no
// chunk to a walk, and was a chunk repair counted held and never fetched.
func (s *Store) Has(h objstore.Hash) bool {
	done, err := s.hold()
	if err != nil {
		return false
	}
	defer done()
	if !h.Valid() {
		return false
	}
	info, err := os.Stat(s.path(h))
	return err == nil && info.Mode().IsRegular()
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
	lock := &s.dirs[dirOf(h)]
	lock.Lock()
	defer lock.Unlock()
	path := s.path(h)
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.succeeded()
		return false, nil
	case err != nil:
		return false, s.fault(fmt.Errorf("objstore/disk: stat %s: %w", h, err))
	}
	if !info.ModTime().Before(before) {
		s.succeeded()
		return false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, s.fault(fmt.Errorf("objstore/disk: delete %s: %w", h, err))
	}
	s.succeeded()
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
		return s.fault(fmt.Errorf("objstore/disk: delete %s: %w", h, err))
	}
	s.succeeded()
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

// Walk visits every chunk this node holds, in slot order.
//
// visit MUST NOT CALL THE STORE: the walk holds the directory open throughout,
// and a nested call would wait behind a [Store.Close] that is waiting for the
// walk. Gather what the walk finds and act on it after.
//
// A chunk found out of place is moved to where it belongs (see the package
// doc) and not visited where it was found: a walk visits each chunk at most
// once, so one moved into a directory it has already passed is visited by the
// next walk instead.
func (s *Store) Walk(visit func(Held) error) error {
	done, err := s.hold()
	if err != nil {
		return err
	}
	defer done()
	moved := 0
	defer func() { logRelocated(s.root, moved) }()
	if err := s.relocateRoot(&moved); err != nil {
		return fmt.Errorf("objstore/disk: walk %s: %w", s.root, err)
	}
	for dir := range dirCount {
		if err := s.walkDir(dir, 0, placement.Slots, visit, &moved); err != nil {
			return err
		}
	}
	return nil
}

// WalkSlots visits every chunk this node holds whose slot is in [lo, hi), in
// slot order — the chunks of one placement group, or of a run of them. It
// lists only the directories the range touches, and visit is bound by the
// same rule as [Store.Walk]'s.
func (s *Store) WalkSlots(lo, hi int, visit func(Held) error) error {
	if lo < 0 || hi > placement.Slots || lo > hi {
		return fmt.Errorf("objstore/disk: the slots [%d, %d) are not a range of 0..%d",
			lo, hi, placement.Slots)
	}
	done, err := s.hold()
	if err != nil {
		return err
	}
	defer done()
	moved := 0
	defer func() { logRelocated(s.root, moved) }()
	for dir := lo >> dirShift; dir < dirCount && dir<<dirShift < hi; dir++ {
		if err := s.walkDir(dir, lo, hi, visit, &moved); err != nil {
			return err
		}
	}
	return nil
}

// walkDir visits the chunks of one directory whose slots are in [lo, hi),
// moving any that are out of place. A write in progress is stepped over: it is
// somebody's Put, not a chunk yet.
func (s *Store) walkDir(dir, lo, hi int, visit func(Held) error, moved *int) error {
	path := filepath.Join(s.root, dirName(dir))
	entries, err := os.ReadDir(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("objstore/disk: list %s: %w", path, err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), staged) {
			continue
		}
		h := objstore.Hash(e.Name())
		if !h.Valid() {
			continue
		}
		if dirOf(h) != dir {
			if err := s.relocate(h, filepath.Join(path, e.Name())); err != nil {
				return fmt.Errorf("objstore/disk: %w", err)
			}
			*moved++
			continue
		}
		if slot := h.Slot(); slot < lo || slot >= hi {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("objstore/disk: stat %s: %w", h, err)
		}
		if err := visit(Held{Hash: h, Size: info.Size(), Written: info.ModTime().UTC()}); err != nil {
			return err
		}
	}
	return nil
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
