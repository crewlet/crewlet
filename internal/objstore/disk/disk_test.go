package disk

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/placement"
)

// gauge is a volume a test sets: a percentage used of a hundred gibibytes, or
// an error in place of a reading. Whole gibibytes, so 90 used of 100 judges
// to exactly 90%.
type gauge struct {
	mu   sync.Mutex
	used uint64
	err  error
}

// at is a volume percent used.
func at(percent uint64) *gauge { return &gauge{used: percent} }

// set makes the volume percent used from the next probe on.
func (g *gauge) set(percent uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.used, g.err = percent, nil
}

// fail makes the next probe's measurement fail with err.
func (g *gauge) fail(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.err = err
}

// space is the reading, as [Options.Space] takes one.
func (g *gauge) space(string) (capacity, free uint64, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return 0, 0, g.err
	}
	return 100 << 30, (100 - g.used) << 30, nil
}

// halfFull is the options of a store whose health must not depend on the
// machine running the test: a temporary directory sits on whatever disk that
// is, and past 95% used a store there refuses every new chunk.
func halfFull() Options { return Options{Space: at(50).space} }

// open is a store on a volume measured as half full, whatever the machine
// running the test has left: a health a test asserts must not depend on it.
func open(t *testing.T) *Store { return openOn(t, at(50)) }

// openOn is a store on the volume g reads, from its first probe on.
func openOn(t *testing.T, g *gauge) *Store {
	t.Helper()
	s, err := Options{Space: g.space}.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A SECOND OPENER OF ONE DIRECTORY IS REFUSED BY NAME, and the directory is
// free again once the first closes it: two engines sharing a chunk directory
// would each collect the other's chunks as garbage it does not recognise.
func TestADirectoryHasOneOwner(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second Open = %v, want ErrLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(root)
	if err != nil {
		t.Fatalf("Open after the owner closed = %v", err)
	}
	_ = again.Close()
}

// A CHUNK READS BACK AS IT WAS WRITTEN, under the name its bytes hash to.
func TestAChunkReadsBackUnderItsHash(t *testing.T) {
	t.Parallel()
	s := open(t)
	data := []byte("the rollback runbook")
	h := objstore.HashOf(data)
	if err := s.Put(h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(h)
	if err != nil || string(got) != string(data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if !s.Has(h) {
		t.Fatal("Has is false for a stored chunk")
	}
	// AND AGAIN IS NOTHING: the same name is the same bytes.
	if err := s.Put(h, data); err != nil {
		t.Fatalf("a second Put of the same chunk: %v", err)
	}
}

// BYTES THAT DO NOT MATCH THEIR NAME ARE REFUSED, so a peer's corrupted copy
// never becomes this node's.
func TestBytesUnderTheWrongNameAreRefused(t *testing.T) {
	t.Parallel()
	s := open(t)
	err := s.Put(objstore.HashOf([]byte("one thing")), []byte("another"))
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("Put = %v, want ErrMismatch", err)
	}
}

// A CHUNK THAT ROTTED ON DISK IS NOT SERVED: it is removed and reported not
// held, which is what sends repair to fetch a good copy from a peer.
func TestARottenChunkIsRemovedRatherThanServed(t *testing.T) {
	t.Parallel()
	s := open(t)
	data := []byte("bytes the disk will flip")
	h := objstore.HashOf(data)
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(h), []byte("flipped"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of a rotten chunk = %v, want ErrNotFound", err)
	}
	if s.Has(h) {
		t.Fatal("the rotten chunk is still there")
	}
}

// A WALK SEES EVERY CHUNK, ONCE, AND STEPS OVER A WRITE IN PROGRESS — which
// is somebody's Put, so removing it would fail that Put at its rename.
func TestAWalkSeesEveryChunkAndLeavesAStagedWriteAlone(t *testing.T) {
	t.Parallel()
	s := open(t)
	var want []objstore.Hash
	for _, body := range []string{"a", "b", "c"} {
		h := objstore.HashOf([]byte(body))
		if err := s.Put(h, []byte(body)); err != nil {
			t.Fatal(err)
		}
		want = append(want, h)
	}
	inFlight := filepath.Join(filepath.Dir(s.path(want[0])), staged+"in-flight")
	if err := os.WriteFile(inFlight, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []objstore.Hash
	if err := s.Walk(func(h Held) error {
		got = append(got, h.Hash)
		if h.Written.IsZero() || h.Size != 1 {
			t.Errorf("chunk %s walked as %+v", h.Hash, h)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("walked %v, want %v", got, want)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Fatalf("the walk removed a write in progress: %v", err)
	}
}

// WHAT A CRASH LEFT STAGED IS CLEARED WHEN THE DIRECTORY IS OPENED, which is
// the one moment nothing can be writing it.
func TestOpeningClearsWhatACrashLeftStaged(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, err := halfFull().Open(root)
	if err != nil {
		t.Fatal(err)
	}
	h := objstore.HashOf([]byte("kept"))
	if err := s.Put(h, []byte("kept")); err != nil {
		t.Fatal(err)
	}
	debris := filepath.Join(filepath.Dir(s.path(h)), staged+"crashed")
	if err := os.WriteFile(debris, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := halfFull().Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := os.Stat(debris); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a staged write left by a crash survived Open")
	}
	if !reopened.Has(h) {
		t.Fatal("Open removed a chunk")
	}
}

// A WALK OF A SLOT RANGE SEES THE CHUNKS WHOSE SLOTS ARE IN IT, in slot order,
// at every group count a map may have: a whole directory at the fewest group
// bits, part of one at more, and a range across directories — which is what a
// pass over one group, or over a run of them, reads.
func TestASlotWalkSeesExactlyItsRange(t *testing.T) {
	t.Parallel()
	s := open(t)
	var all []objstore.Hash
	for i := range 400 {
		body := []byte{byte(i), byte(i >> 8), 's'}
		h := objstore.HashOf(body)
		if err := s.Put(h, body); err != nil {
			t.Fatal(err)
		}
		all = append(all, h)
	}
	walk := func(lo, hi int) []objstore.Hash {
		t.Helper()
		var got []objstore.Hash
		if err := s.WalkSlots(lo, hi, func(h Held) error {
			got = append(got, h.Hash)
			return nil
		}); err != nil {
			t.Fatalf("WalkSlots(%d, %d): %v", lo, hi, err)
		}
		return got
	}
	for _, bits := range []int{placement.MinPGBits, 10, placement.MaxPGBits} {
		m := placement.Map{PGBits: bits}
		for _, pg := range []int{0, 1, m.Groups() / 2, m.Groups() - 1} {
			lo, hi := m.SlotRange(pg)
			var want []objstore.Hash
			for _, h := range all {
				if h.Slot() >= lo && h.Slot() < hi {
					want = append(want, h)
				}
			}
			slices.Sort(want)
			if got := walk(lo, hi); !slices.Equal(got, want) {
				t.Fatalf("group %d at %d bits walked %v, want %v", pg, bits, got, want)
			}
		}
	}
	// ACROSS DIRECTORIES, still in slot order.
	got := walk(0x10ff, 0x3301)
	var want []objstore.Hash
	for _, h := range all {
		if h.Slot() >= 0x10ff && h.Slot() < 0x3301 {
			want = append(want, h)
		}
	}
	slices.Sort(want)
	if !slices.Equal(got, want) || len(want) == 0 {
		t.Fatalf("a range across directories walked %d chunks, want the %d in it", len(got), len(want))
	}
	// THE WHOLE STORE IN SLOT ORDER, and nothing for an empty range.
	var everything []objstore.Hash
	if err := s.Walk(func(h Held) error {
		everything = append(everything, h.Hash)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.IsSorted(everything) || len(everything) != len(all) {
		t.Fatalf("Walk visited %d chunks, sorted %v; want all %d in order",
			len(everything), slices.IsSorted(everything), len(all))
	}
	if got := walk(5, 5); len(got) != 0 {
		t.Fatalf("an empty range walked %v", got)
	}
	for _, bad := range [][2]int{{-1, 3}, {0, placement.Slots + 1}, {9, 8}} {
		if err := s.WalkSlots(bad[0], bad[1], func(Held) error { return nil }); err == nil {
			t.Errorf("WalkSlots(%d, %d) was accepted", bad[0], bad[1])
		}
	}
}

// misplace puts a chunk's bytes at a path that is not its own.
func misplace(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// wrongDir is a directory of the store that is not h's own.
func wrongDir(root string, h objstore.Hash) string {
	return filepath.Join(root, fmt.Sprintf("%02x", (dirOf(h)+1)%dirCount))
}

// A CHUNK OUT OF PLACE IS MOVED TO WHERE EVERY READ LOOKS, by Open — whether it
// sits in another chunk's directory or loose in the root — and a misplaced copy
// of a chunk already held where it belongs is removed rather than moved over
// it. Left where it was, no read, repair or collection would ever see it.
func TestOpenMovesChunksOutOfPlace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inDir := []byte("in another chunk's directory")
	loose := []byte("loose in the root")
	both := []byte("held twice")
	misplace(t, filepath.Join(wrongDir(root, objstore.HashOf(inDir)), string(objstore.HashOf(inDir))), inDir)
	misplace(t, filepath.Join(root, string(objstore.HashOf(loose))), loose)
	misplace(t, filepath.Join(root, Layout(objstore.HashOf(both))), both)
	stray := filepath.Join(wrongDir(root, objstore.HashOf(both)), string(objstore.HashOf(both)))
	misplace(t, stray, []byte("a rotten second copy"))
	// NOT A CHUNK: left exactly where it is.
	notes := filepath.Join(root, "notes.txt")
	misplace(t, notes, []byte("an operator's"))

	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, data := range [][]byte{inDir, loose, both} {
		got, err := s.Get(objstore.HashOf(data))
		if err != nil || string(got) != string(data) {
			t.Fatalf("Get of %q after Open = %q, %v", data, got, err)
		}
	}
	if _, err := os.Stat(stray); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the misplaced second copy was left behind: %v", err)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Fatalf("a file that is not a chunk was touched: %v", err)
	}
}

// A WALK MOVES WHAT IT FINDS OUT OF PLACE TOO — a copy that arrived while the
// store was open — and visits it where it belongs rather than where it was.
func TestAWalkMovesChunksOutOfPlace(t *testing.T) {
	t.Parallel()
	s := open(t)
	data := []byte("copied in by hand")
	h := objstore.HashOf(data)
	from := filepath.Join(wrongDir(s.root, h), string(h))
	misplace(t, from, data)
	dir := (dirOf(h) + 1) % dirCount
	if err := s.WalkSlots(dir<<dirShift, (dir+1)<<dirShift, func(held Held) error {
		t.Fatalf("the walk of another directory visited %s", held.Hash)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(from); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the walk left the chunk out of place: %v", err)
	}
	var seen []objstore.Hash
	if err := s.WalkSlots(h.Slot(), h.Slot()+1, func(held Held) error {
		seen = append(seen, held.Hash)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []objstore.Hash{h}) {
		t.Fatalf("the walk of its own slot saw %v, want the moved chunk", seen)
	}
}

// VERIFY SAYS WHICH OF THREE THINGS IS TRUE — intact, rotten and now removed,
// or never held — because the scrub counts the second and must not count the
// third, and a member vouching for a copy must not vouch for either.
func TestVerifyTellsIntactRottenAndAbsentApart(t *testing.T) {
	t.Parallel()
	s := open(t)
	good := []byte("still good")
	if err := s.Put(objstore.HashOf(good), good); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Verify(objstore.HashOf(good)); !ok || err != nil {
		t.Fatalf("Verify of an intact chunk = %v, %v", ok, err)
	}
	bad := []byte("about to rot")
	h := objstore.HashOf(bad)
	if err := s.Put(h, bad); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(h), []byte("rot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Verify(h); ok || err != nil {
		t.Fatalf("Verify of a rotten chunk = %v, %v; want false and no error", ok, err)
	}
	if s.Has(h) {
		t.Fatal("the rotten chunk was not removed")
	}
	if ok, err := s.Verify(h); ok || !errors.Is(err, ErrNotFound) {
		t.Fatalf("Verify of a chunk not held = %v, %v; want ErrNotFound", ok, err)
	}
}

// badSectors is a disk whose reads of the named chunks fail with EIO — the
// latent sector error a healthy disk in a temporary directory cannot be made
// to return — each for as many reads as it is set to.
type badSectors struct {
	mu   sync.Mutex
	left map[string]int
}

func (b *badSectors) fail(h objstore.Hash, reads int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.left == nil {
		b.left = map[string]int{}
	}
	b.left[string(h)] = reads
}

// read is [Options.Read].
func (b *badSectors) read(f *os.File) ([]byte, error) {
	b.mu.Lock()
	name := filepath.Base(f.Name())
	bad := b.left[name] > 0
	if bad {
		b.left[name]--
	}
	b.mu.Unlock()
	if bad {
		return nil, &os.PathError{Op: "read", Path: f.Name(), Err: syscall.EIO}
	}
	return io.ReadAll(f)
}

// openBad is a store on a half-full volume whose reads b decides.
func openBad(t *testing.T, b *badSectors) *Store {
	t.Helper()
	s, err := Options{Space: at(50).space, Read: b.read}.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A CHUNK WHOSE BYTES WILL NOT READ IS A BAD COPY, as a rotten one is: removed,
// so it is not held — repair fetches a good one, and Verify says it is not
// intact rather than answering an error the scrub stopped on for good — with
// the error still counted toward the store's health. Left in place it answered
// Has, so repair counted it held and never replaced it.
func TestAChunkThatWillNotReadIsRemoved(t *testing.T) {
	t.Parallel()
	for name, read := range map[string]func(*Store, objstore.Hash) error{
		"verify": func(s *Store, h objstore.Hash) error {
			ok, err := s.Verify(h)
			if ok {
				return errors.New("verified")
			}
			return err
		},
		"get": func(s *Store, h objstore.Hash) error {
			_, err := s.Get(h)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bad := &badSectors{}
			s := openBad(t, bad)
			data := []byte("a sector under this went bad")
			h := objstore.HashOf(data)
			if err := s.Put(h, data); err != nil {
				t.Fatal(err)
			}
			bad.fail(h, 1)
			err := read(s, h)
			if !errors.Is(err, ErrUnreadable) || !errors.Is(err, ErrNotFound) ||
				!errors.Is(err, syscall.EIO) {
				t.Fatalf("reading it = %v, want ErrUnreadable, removed, naming the EIO", err)
			}
			if s.Has(h) {
				t.Fatal("the unreadable copy is still held")
			}
			if got := s.health.errors; got != 1 {
				t.Fatalf("%d errors counted toward the store's health, want 1", got)
			}
		})
	}

	// THE ERROR STILL COUNTS: unreadable chunks in a row fail the store.
	bad := &badSectors{}
	s := openBad(t, bad)
	var chunks []objstore.Hash
	for i := range FailedAfter {
		data := []byte{byte(i), 's'}
		if err := s.Put(objstore.HashOf(data), data); err != nil {
			t.Fatal(err)
		}
		bad.fail(objstore.HashOf(data), 1)
		chunks = append(chunks, objstore.HashOf(data))
	}
	for _, h := range chunks {
		_, _ = s.Get(h)
	}
	if got := s.Health(); got.State != HealthFailed {
		t.Fatalf("after %d unreadable chunks in a row = %+v, want failed", FailedAfter, got)
	}
}

// A GOOD COPY IS WRITTEN OVER ONE THAT WILL NOT READ: the write's own check of
// what is there reads it first, and answering that read's error refused every
// good copy a writer or a repair offered — the chunk a copy short on this node
// for ever.
func TestAPutReplacesACopyThatWillNotRead(t *testing.T) {
	t.Parallel()
	bad := &badSectors{}
	s := openBad(t, bad)
	data := []byte("the good bytes")
	h := objstore.HashOf(data)
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(s.path(h))
	if err != nil {
		t.Fatal(err)
	}
	bad.fail(h, 1)
	if err := s.Put(h, data); err != nil {
		t.Fatalf("a Put over a copy that will not read = %v", err)
	}
	after, err := os.Stat(s.path(h))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("the unreadable file was kept rather than written over")
	}
	if got, err := s.Get(h); err != nil || string(got) != string(data) {
		t.Fatalf("Get after the repairing Put = %q, %v", got, err)
	}
}

// A ROTTEN COPY IS REMOVED ONLY IF IT IS STILL THE FILE THAT WAS READ: a write
// that replaced it in between put good bytes there and told a writer they were
// stored, and a reader acting on what it read a moment before would delete
// them.
func TestARottenCopyReplacedSinceItWasReadIsKept(t *testing.T) {
	t.Parallel()
	s := open(t)
	data := []byte("the good bytes a writer stored")
	h := objstore.HashOf(data)
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(h), []byte("rot"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := os.Stat(s.path(h))
	if err != nil {
		t.Fatal(err)
	}
	// A writer replaces the rotten file between the read and the removal.
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	if err := s.removeBad(h, read, "it rotted"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(h); err != nil || string(got) != string(data) {
		t.Fatalf("the replaced copy after a stale removal = %q, %v", got, err)
	}
	// And the file that WAS read is removed.
	if err := os.WriteFile(s.path(h), []byte("rot again"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err = os.Stat(s.path(h))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.removeBad(h, read, "it rotted"); err != nil {
		t.Fatal(err)
	}
	if s.Has(h) {
		t.Fatal("the rotten file that was read survived its removal")
	}
}

// A STORE IS HEALTHY WHEN IT OPENS, NEARFULL PAST 85%, AND FULL PAST 95% — and
// a full store refuses a NEW chunk by name, so the writer takes it to the next
// member, while one it already holds is still counted: it costs no space.
func TestFullnessIsMeasuredAndAFullStoreTakesNothingNew(t *testing.T) {
	t.Parallel()
	// OPEN MEASURES THE REAL VOLUME before anything asks — whatever it is:
	// something in use or something free, and the probe did not fail.
	measured, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer measured.Close()
	if got := measured.Health(); !got.State.Valid() || got.State == HealthFailed ||
		(got.UsedPercent <= 0 && got.FreeBytes == 0) {
		t.Fatalf("a store just opened = %+v, want its volume measured", got)
	}

	vol := at(50)
	s := openOn(t, vol)
	if got := s.Health(); got.State != HealthOK || got.UsedPercent != 50 {
		t.Fatalf("a store half full = %+v, want ok", got)
	}
	held := []byte("held before it filled")
	if err := s.Put(objstore.HashOf(held), held); err != nil {
		t.Fatal(err)
	}

	vol.set(90)
	if got := s.Probe(); got.State != HealthNearFull || got.UsedPercent != 90 || got.Detail == "" {
		t.Fatalf("at 90%% used = %+v, want nearfull", got)
	}
	nearly := []byte("accepted while nearly full")
	if err := s.Put(objstore.HashOf(nearly), nearly); err != nil {
		t.Fatalf("Put while nearfull = %v", err)
	}

	vol.set(96)
	if got := s.Probe(); got.State != HealthFull {
		t.Fatalf("at 96%% used = %+v, want full", got)
	}
	fresh := []byte("nowhere to put it")
	if err := s.Put(objstore.HashOf(fresh), fresh); !errors.Is(err, ErrFull) {
		t.Fatalf("Put of a new chunk while full = %v, want ErrFull", err)
	}
	if err := s.Put(objstore.HashOf(held), held); err != nil {
		t.Fatalf("Put of a chunk already held while full = %v, want it counted", err)
	}

	vol.set(50)
	if got := s.Probe(); got.State != HealthOK {
		t.Fatalf("with room again = %+v, want ok", got)
	}
	if err := s.Put(objstore.HashOf(fresh), fresh); err != nil {
		t.Fatalf("Put after space was freed = %v", err)
	}
}

// A STORE OPENED ON A FULL VOLUME IS FULL FROM ITS FIRST ANSWER, and one opened
// on a healthy volume is healthy, whatever the disk under the directory has
// left: the reading a store is given is the one Open's own probe takes, so a
// store never answers — not even before its first heartbeat — for a volume it
// was not given.
func TestTheSuppliedReadingIsTheOneOpenMeasures(t *testing.T) {
	t.Parallel()
	full := openOn(t, at(97))
	if got := full.Health(); got.State != HealthFull || got.UsedPercent != 97 {
		t.Fatalf("a store opened on a volume 97%% used = %+v, want full", got)
	}
	fresh := []byte("refused before any heartbeat")
	if err := full.Put(objstore.HashOf(fresh), fresh); !errors.Is(err, ErrFull) {
		t.Fatalf("Put on a store opened full = %v, want ErrFull", err)
	}
	roomy := openOn(t, at(10))
	if got := roomy.Health(); got.State != HealthOK || got.UsedPercent != 10 ||
		got.FreeBytes != 90<<30 {
		t.Fatalf("a store opened on a volume 10%% used = %+v, want ok with 90 GiB free", got)
	}
}

// A READING IS JUDGED AS df JUDGES ONE: used over capacity, a volume that
// reports no capacity is never full, and a reading with more free than its
// capacity is not a reading at all — the probe that took it fails rather than
// guessing a fullness nobody measured.
func TestAReadingIsJudgedByWhatItSays(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		capacity, free uint64
		state          HealthState
		used           float64
	}{
		"half used":           {capacity: 1000, free: 500, state: HealthOK, used: 50},
		"nearfull at 85%":     {capacity: 1000, free: 150, state: HealthNearFull, used: 85},
		"full at 95%":         {capacity: 1000, free: 50, state: HealthFull, used: 95},
		"every byte used":     {capacity: 1000, free: 0, state: HealthFull, used: 100},
		"no capacity at all":  {capacity: 0, free: 0, state: HealthOK, used: 0},
		"more free than held": {capacity: 1000, free: 1001, state: HealthFailed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, err := Options{Space: func(string) (uint64, uint64, error) {
				return c.capacity, c.free, nil
			}}.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			got := s.Health()
			if got.State != c.state || (c.state != HealthFailed && got.UsedPercent != c.used) {
				t.Fatalf("capacity %d, free %d = %+v, want %s at %.0f%%",
					c.capacity, c.free, got, c.state, c.used)
			}
		})
	}
}

// breakRead makes a chunk's path unreadable: a directory where its file
// belongs, which opens and then fails the read.
func breakRead(t *testing.T, s *Store, h objstore.Hash) {
	t.Helper()
	if err := os.MkdirAll(s.path(h), 0o750); err != nil {
		t.Fatal(err)
	}
}

// A STORE WHOSE OPERATIONS FAIL THREE TIMES IN A ROW IS FAILED, and takes no
// chunk until a probe — which exercises every step a write takes — succeeds.
// Fewer in a row, or a success between them, is not a failed disk.
func TestConsecutiveErrorsFailTheStoreUntilAProbeSucceeds(t *testing.T) {
	t.Parallel()
	s := open(t)
	good := []byte("readable")
	if err := s.Put(objstore.HashOf(good), good); err != nil {
		t.Fatal(err)
	}
	broken := objstore.HashOf([]byte("unreadable"))
	breakRead(t, s, broken)
	if s.Has(broken) {
		t.Fatal("a directory under a chunk's name is held as a chunk")
	}
	for range FailedAfter - 1 {
		if _, err := s.Get(broken); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("Get of an unreadable chunk = %v, want an I/O error", err)
		}
	}
	if _, err := s.Get(objstore.HashOf(good)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(broken); err == nil {
		t.Fatal("the unreadable chunk read")
	}
	if got := s.Health(); got.State != HealthOK {
		t.Fatalf("after errors broken by a success = %+v, want ok", got)
	}

	for range FailedAfter {
		_, _ = s.Get(broken)
	}
	got := s.Health()
	if got.State != HealthFailed || !strings.Contains(got.Detail, string(broken)) {
		t.Fatalf("after %d errors in a row = %+v, want failed naming the last", FailedAfter, got)
	}
	fresh := []byte("refused while failed")
	if err := s.Put(objstore.HashOf(fresh), fresh); !errors.Is(err, ErrFailed) {
		t.Fatalf("Put while failed = %v, want ErrFailed", err)
	}
	// A SUCCESSFUL OPERATION DOES NOT CLEAR IT; a probe does.
	if _, err := s.Get(objstore.HashOf(good)); err != nil {
		t.Fatal(err)
	}
	if got := s.Health(); got.State != HealthFailed {
		t.Fatalf("after a read succeeded = %+v, want still failed", got)
	}
	if got := s.Probe(); got.State != HealthOK {
		t.Fatalf("after a probe succeeded = %+v, want ok", got)
	}
	if err := s.Put(objstore.HashOf(fresh), fresh); err != nil {
		t.Fatalf("Put after the probe = %v", err)
	}
}

// A PROBE THAT CANNOT WRITE ITS FILE FAILS THE STORE, and one that can clears
// it; a probe of a store that was closed answers failed.
func TestAFailedProbeFailsTheStore(t *testing.T) {
	t.Parallel()
	vol := at(50)
	s := openOn(t, vol)
	blocker := filepath.Join(s.root, probeName)
	if err := os.Mkdir(blocker, 0o750); err != nil {
		t.Fatal(err)
	}
	if got := s.Probe(); got.State != HealthFailed || !strings.Contains(got.Detail, "probe") {
		t.Fatalf("a probe that cannot write = %+v, want failed naming the probe", got)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if got := s.Probe(); got.State != HealthOK {
		t.Fatalf("a probe that can write again = %+v, want ok", got)
	}
	if _, err := os.Stat(blocker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the probe left its file behind")
	}
	vol.fail(errors.New("statfs refused"))
	if got := s.Probe(); got.State != HealthFailed || !strings.Contains(got.Detail, "statfs refused") ||
		!strings.Contains(got.Detail, s.root) {
		t.Fatalf("a probe that cannot measure the volume = %+v, want failed naming the "+
			"volume and why", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.Probe(); got.State != HealthFailed {
		t.Fatalf("a probe of a closed store = %+v, want failed", got)
	}
}

// Every health state this build writes is one it knows, and nothing else is.
func TestHealthStatesAreValid(t *testing.T) {
	t.Parallel()
	for _, s := range []HealthState{HealthOK, HealthNearFull, HealthFull, HealthFailed} {
		if !s.Valid() {
			t.Errorf("%q is not valid", s)
		}
	}
	for _, s := range []HealthState{"", "degraded", "OK"} {
		if s.Valid() {
			t.Errorf("%q is valid", s)
		}
	}
}

// A WRITE OF A CHUNK ALREADY HELD RESTARTS ITS GRACE, and the collector
// judges by that: bytes uploaded again for a record that has not landed must
// not be collected on the age of the copy a deleted file left behind.
func TestAWriteRestartsTheCollectorsGrace(t *testing.T) {
	t.Parallel()
	s := open(t)
	data := []byte("uploaded twice")
	h := objstore.HashOf(data)
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(s.path(h), old, old); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)

	// Written again: the chunk is young, so the collector leaves it.
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Collect(h, cutoff)
	if err != nil || removed {
		t.Fatalf("Collect after a fresh write = %v, %v; want kept", removed, err)
	}

	// Aged past the cutoff with no write since: collected.
	if err := os.Chtimes(s.path(h), old, old); err != nil {
		t.Fatal(err)
	}
	removed, err = s.Collect(h, cutoff)
	if err != nil || !removed {
		t.Fatalf("Collect of an aged chunk = %v, %v; want removed", removed, err)
	}
	if s.Has(h) {
		t.Fatal("a collected chunk is still held")
	}
	// And collecting what is gone is nothing.
	if removed, err := s.Collect(h, cutoff); err != nil || removed {
		t.Fatalf("Collect of a gone chunk = %v, %v", removed, err)
	}
}

// A ROTTEN COPY IS WRITTEN OVER, not counted: a Put that found bytes under
// the name that no longer match it replaces them with the caller's, which
// were checked.
func TestAPutReplacesARottenCopy(t *testing.T) {
	t.Parallel()
	s := open(t)
	data := []byte("the good bytes")
	h := objstore.HashOf(data)
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(h), []byte("rot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(h)
	if err != nil || string(got) != string(data) {
		t.Fatalf("Get after a repairing Put = %q, %v", got, err)
	}
}

// A DELETE OF WHAT IS ALREADY GONE IS NOT AN ERROR — two passes racing on one
// chunk agree about where it ended up.
func TestDeletingAGoneChunkIsNothing(t *testing.T) {
	t.Parallel()
	s := open(t)
	h := objstore.HashOf([]byte("x"))
	if err := s.Delete(h); err != nil {
		t.Fatalf("Delete of an absent chunk: %v", err)
	}
}
