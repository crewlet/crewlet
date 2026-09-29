package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A node's replicated estate is divided into PARTITIONS, and each partition
// the node holds is a file of its own, opened and closed while the node runs
// as partitions are joined and left. This file is that half of the store: the
// name a partition's file is given, the handle the node keeps on it, and how
// the node's connections and page cache are shared out across however many of
// them it holds.
//
// # Why one file per partition
//
// The unit a node holds, snapshots, adopts, joins and leaves is a partition,
// and each of those is a copy, an install or a delete of ONE file. With every
// partition in one file, handing a single partition to another node would be
// a copy of everything followed by a delete of most of it — and with no
// in-place VACUUM the deleted pages would ride along in every artefact. It is
// the argument that split the node's own estate from the replicated one,
// applied again one level down.
//
// No transaction spans two of these files and no read joins across them, for
// the reason [Estate] gives about the node's two: a transaction is one file.
//
// # Layout 0 is one partition, and its file is where it always was
//
// Layout 0 — the estate before it was divided — is one partition, estate.000,
// and its file is the one [ReplicatedPath] has always named. That is what lets
// every change to how partitions are opened land while a fleet runs layout 0,
// with no file moving underneath it.

// PartitionFile names one partition's file to this package: which layout it
// belongs to, which partition it is, and how many logs it carries.
//
// PLAIN VALUES, and deliberately not internal/statelog's vocabulary: statelog
// imports this package, so it cannot be imported back. statelog builds one of
// these from a layout ([statelog.Layout.File]), and this type checks what it
// relies on rather than trusting the conversion.
type PartitionFile struct {
	// Layout is the number of the layout the partition belongs to. It is in
	// every partitioned layout's file names, so a repartition's files never
	// share a name with the files of the layout they replace.
	Layout int

	// Name is the partition's name — `tracker.007`, or layout 0's
	// `estate.000` — in the grammar internal/queue/topics owns.
	Name string

	// Logs is how many logs the partition carries. Each log's apply loop
	// PINS one writer on the file for the life of the loop, so the file's
	// pool is sized for exactly that many — see [DB.Writer].
	Logs int
}

// layoutZeroPartition is layout 0's one partition: the whole estate, as it
// was before it was divided.
var layoutZeroPartition = topics.PartitionName("estate", 0)

// Validate refuses a file this package cannot name or size, saying what to
// change.
//
// LAYOUT 0 IS EXACTLY estate.000, and estate.000 is layout 0's alone: its
// file is the one [ReplicatedPath] names, so a layout-0 file of any other
// name would be a second partition at the same path, and a partitioned
// layout's estate.000 would be two histories under one name.
func (f PartitionFile) Validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("store: the partition file %q of layout %d: %s",
			f.Name, f.Layout, fmt.Sprintf(format, args...))
	}
	if f.Layout < 0 {
		return fail("the layout number is negative; layouts count from 0")
	}
	if _, _, ok := topics.ParsePartitionName(f.Name); !ok {
		return fail("the name is not a partition's — a space and a three-digit index, like tracker.007")
	}
	if (f.Layout == 0) != (f.Name == layoutZeroPartition) {
		return fail("layout 0 is exactly one partition, %s, and no other layout has it",
			layoutZeroPartition)
	}
	if f.Logs < 1 {
		return fail("it carries %d logs; a partition carries at least one, and each "+
			"pins a writer on the file", f.Logs)
	}
	return nil
}

// partitionPath is where a node whose own file is nodePath keeps f, with
// store.replicated_path configured as configured.
//
// LAYOUT 0's FILE IS [ReplicatedPath] — the configured path when one is set,
// under whatever name it gives — so no layout-0 node's file ever moves. Every
// partitioned layout's files are BESIDE IT, as `l<layout>-<name>.db`
// (`l1-tracker.007.db`): an operator who moved the replicated estate to a
// faster volume moved the estate, and a divided estate is still the estate.
// An in-memory node's partitions are in memory too, each its own anonymous
// database, exactly as two files are two files.
//
// THE LAYOUT IS IN THE NAME for the reason it is in every stream name: a
// repartition creates a disjoint set of files, and one name never means two
// histories.
//
// NOT EXPORTED, and no path is derived from a directory alone: layout 0's
// file is wherever the node's configuration says, so an answer computed
// without that configuration is right only while the configured name happens
// to be the default — which is the bug `crewlet migrate` had while it derived
// the path itself. [DB.PartitionPath] is the one answer, asked of the node.
func partitionPath(nodePath, configured string, f PartitionFile) string {
	zero := ReplicatedPath(nodePath, configured)
	if zero == "" || strings.HasPrefix(zero, ":memory:") || f.Layout == 0 {
		return zero
	}
	return filepath.Join(filepath.Dir(zero), fmt.Sprintf("l%d-%s.db", f.Layout, f.Name))
}

// Pool and page-cache sizing for partition files.
//
// A node holding every partition of a small fleet's layout opens 81 files at
// a tracker count of 64 and 321 at 256. The pool and the per-connection page
// cache were sized when the replicated estate was ONE file, so each file taking
// that one file's share would multiply both by the partition count. These are
// the numbers that divide them instead; BenchmarkANodeOpensEveryPartition is
// the measurement they were checked against, and its result is recorded
// there.
const (
	// PartitionReadConns is the fewest readers a partition file's pool
	// holds, beside the writers its apply loops pin.
	//
	// TWO, so a long scan — a search reading its candidates, a snapshot
	// being taken — never holds the only connection a seat's point read
	// could use. More are given when the node holds few enough files that
	// the node's own read concurrency divides into more than two each (see
	// [divide]): a node holding one partition, which is every node under
	// layout 0, gives it the whole of it, as before the estate was divided.
	PartitionReadConns = 2

	// partitionCacheKiB is the page cache the node's partition files share,
	// per connection: 32 MiB, which is what each connection on the one
	// replicated file had before the estate was divided. A node holding one
	// partition therefore sizes it exactly as before, and a node holding many
	// DIVIDES the same memory rather than multiplying it — at 321 files and
	// four connections each, 32 MiB apiece would be 40 GiB of cache a node
	// could fill.
	partitionCacheKiB = nodeCacheKiB

	// partitionCacheFloorKiB is the least any open partition file's
	// connections cache, and it is not a choice: it is the database engine's
	// own minimum. Turso keeps at least 200 pages a connection and RAISES any
	// smaller `cache_size` to that without a word — measured: -797 KiB reads
	// back as 200 pages, -800 as itself — and every file here has the 4 KiB
	// page, so 800 KiB. A floor below it is a division the engine does not
	// honour: the node would believe it had given a file less while each of
	// its connections kept 800, and the ceiling the division exists to hold
	// would be off by the difference on every file.
	// TestThePartitionCacheFloorIsTheEnginesMinimum holds this to the
	// engine, so a driver that moves its minimum fails there first.
	//
	// Past 40 open files the floors alone exceed [partitionCacheKiB] and
	// every file gets the floor — which the default layout's 81 and 321
	// files both are, so on a node holding every partition the budget is
	// the engine's minimum times the connections rather than 32 MiB, about
	// 1 GiB at 321 files. BenchmarkANodeOpensEveryPartition measured it
	// against the node footprint the partition count was chosen under.
	partitionCacheFloorKiB = 800
)

// allotment is one open partition file's share of the node: how many readers
// its pool holds beside its pinned writers, and how large a page cache each of
// its connections keeps.
type allotment struct {
	readers  int
	cacheKiB int64
}

// divide shares the node's read concurrency and partition page cache across
// its open partition files, whose sizes in bytes are given by name.
//
// READERS are the node's read concurrency spread over its files, never fewer
// than [PartitionReadConns] each: every file is read from, and a file with one
// reader serves its seats one read at a time behind whatever scan is running.
//
// THE CACHE — [partitionCacheKiB] — is shared in proportion to each file's
// size, above a floor every file keeps: the floors are set aside first and the
// rest is divided by size, so the whole is the budget whenever the floors
// leave any, and a node whose floors alone exceed it gives every file the
// floor. By size, because what the per-connection cache holds is a file's
// B-tree interiors, and those grow with the file. Integer arithmetic
// throughout, so the same sizes always divide the same way.
func divide(sizes map[string]int64, readers int) map[string]allotment {
	out := make(map[string]allotment, len(sizes))
	n := len(sizes)
	if n == 0 {
		return out
	}
	each := max(PartitionReadConns, (readers+n-1)/n)
	free := int64(partitionCacheKiB - n*partitionCacheFloorKiB)
	var total int64
	for _, size := range sizes {
		total += max(size, 0)
	}
	for name, size := range sizes {
		share := int64(partitionCacheFloorKiB)
		switch {
		case free <= 0:
		case total == 0:
			// NOTHING WRITTEN ANYWHERE YET: every file is as large as
			// every other, so each takes an even share.
			share += free / int64(n)
		default:
			share += free * max(size, 0) / total
		}
		out[name] = allotment{readers: each, cacheKiB: share}
	}
	return out
}

// partitionSet is the partitions a node handle holds open.
type partitionSet struct {
	// mu serialises every change to the set — an open, a close, a drop — and
	// the division that follows each, so two changes cannot each divide the
	// node across a set that lacks the other's file.
	mu sync.Mutex

	// open is every open partition handle by name. SWAPPED, NEVER MUTATED,
	// and read without mu: [DB.PartitionDB] is on every read a domain makes,
	// and a partition is opened or closed a handful of times in a node's
	// life.
	open atomic.Pointer[map[string]*DB]

	// layout is the layout every open partition belongs to, meaningful while
	// any is open. ONE LAYOUT PER NODE: a partition is keyed by its name
	// alone, and `tracker@tracker.007` of layout 1 and of a repartitioned
	// layout 2 are the same name over two different histories.
	layout int
}

// held is the open set, never nil.
func (s *partitionSet) held() map[string]*DB {
	if m := s.open.Load(); m != nil {
		return *m
	}
	return nil
}

// ErrNotANode is a partition asked of a handle that is not a node's.
//
// A partition is held by the NODE: its handle is what every open, close and
// lookup goes through, so a caller holding a partition's handle — or a copy
// opened with [OpenEstate] — cannot reach a sibling through it.
var ErrNotANode = errors.New("store: partitions are held by a node's own handle, and this is not one")

// node refuses a handle that is not a node's own, naming the gesture.
//
// IT DOES NOT JUDGE WHETHER THE NODE IS STILL OPEN, beyond a handle that was
// never opened: that is [DB.stillOpen]'s, asked under the set's lock by every
// gesture that changes the set.
func (d *DB) node(gesture string) error {
	switch {
	case d == nil || d.sql == nil:
		return fmt.Errorf("%w: %s: %w", ErrNotANode, gesture, ErrNoEstate)
	case d.parts == nil:
		return fmt.Errorf("%w: %s on the %s handle at %s", ErrNotANode, gesture, d.estate, d.path)
	}
	return nil
}

// stillOpen refuses a gesture on a node that has been closed. A gesture that
// changes the set asks it holding d.parts.mu.
//
// UNDER THE LOCK, AND ONLY THERE, because that is what orders it against
// [DB.Close]: Close marks the node closed BEFORE it takes the same lock to
// empty the set, so a gesture that finds the node open here holds the lock
// until its change is in the set — which Close then takes down with the rest
// — and one that comes after finds it closed. Judged before the lock, an open
// that passed the check, waited for the lock while Close emptied the set, and
// then took it, would open a file into a node nothing will close again: its
// pool, its descriptors and this process's claim on it would outlive the node
// for the life of the process, and another process would be refused the file.
func (d *DB) stillOpen(gesture string) error {
	if d.closed.Load() {
		return fmt.Errorf("store: %s: the node is closed: %w", gesture, ErrNoEstate)
	}
	return nil
}

// PartitionPath is where THIS node keeps f's file: layout 0's is
// [ReplicatedPath] for this node's own file and its store.replicated_path,
// and every partitioned layout's are beside it — see [partitionPath]. A
// partition that is not open is still somewhere, and an operator reading the
// path of one a failed join left closed needs exactly that answer.
func (d *DB) PartitionPath(f PartitionFile) string {
	if d == nil {
		return ""
	}
	return partitionPath(d.path, d.opened.ReplicatedPath, f)
}

// OpenPartition opens one partition's file — creating it, and migrating it to
// this binary's schema, when it is absent or behind — under this process's
// exclusive lock, and holds it on this node until [DB.ClosePartition],
// [DB.DropPartition] or [DB.Close].
//
// IDEMPOTENT for the file already open under that name, which answers the
// handle it is open as: a join that retries after a partial failure opens
// what is already open. The same name as a DIFFERENT file — another layout,
// another log count — is refused, and so is any partition of a layout other
// than the one this node's open partitions belong to.
//
// Every open re-divides the node's readers and page cache across the files it
// then holds ([divide]); the new file is opened with its share rather than
// resized after.
func (d *DB) OpenPartition(ctx context.Context, f PartitionFile) (*DB, error) {
	if err := d.node("open a partition"); err != nil {
		return nil, err
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	path := d.PartitionPath(f)
	// REFUSED BEFORE THE LOCK, for the reason [OpenNode] refused it before
	// anything: two exclusive claims on one path do not collide inside this
	// process, so one file for both would carry two migration sequences and
	// every applier would write beside the audit log.
	if path == d.path && path != "" && !strings.HasPrefix(path, ":memory:") {
		return nil, fmt.Errorf("%w: %s", ErrOneFile, path)
	}

	d.parts.mu.Lock()
	defer d.parts.mu.Unlock()
	if err := d.stillOpen("open a partition"); err != nil {
		return nil, err
	}
	open := d.parts.held()
	if held := open[f.Name]; held != nil {
		if held.file != f {
			return nil, fmt.Errorf("store: the partition %s is open on this node as %+v, "+
				"not %+v — close it before opening it as another file", f.Name, held.file, f)
		}
		return held, nil
	}
	if len(open) > 0 && d.parts.layout != f.Layout {
		return nil, fmt.Errorf("store: this node holds partitions of layout %d, and %s "+
			"is layout %d's — a node's partitions are one layout's, because a "+
			"partition is keyed by its name and two layouts name two histories alike",
			d.parts.layout, f.Name, f.Layout)
	}

	shares := d.divide(open, f.Name, path)
	db, err := openEstate(ctx, EstatePartition, path, d.partitionOptions(f, shares[f.Name]), &d.caps)
	if err != nil {
		return nil, fmt.Errorf("store: open the partition %s: %w", f.Name, err)
	}
	db.file = f
	db.owner = d

	next := make(map[string]*DB, len(open)+1)
	for name, held := range open {
		next[name] = held
	}
	next[f.Name] = db
	d.parts.open.Store(&next)
	d.parts.layout = f.Layout
	apply(open, shares)
	return db, nil
}

// partitionOptions is the Options one partition's handle is opened with: the
// node's own, with the pool and the cache this partition's share sets and
// the embedding width the node has LEARNED since it was opened.
func (d *DB) partitionOptions(f PartitionFile, share allotment) Options {
	opts := d.opened
	opts.Scratch = false
	opts.MaxOpenConns = share.readers + f.Logs
	opts.pins = f.Logs
	opts.cacheKiB = share.cacheKiB
	opts.EmbeddingDim = int(d.dim.Load())
	return opts
}

// divide is the node's current division, over the files open plus one more
// named at path — or plus none, for a name of "".
func (d *DB) divide(open map[string]*DB, adding, path string) map[string]allotment {
	sizes := make(map[string]int64, len(open)+1)
	for name, held := range open {
		sizes[name] = fileBytes(held.path)
	}
	if adding != "" {
		sizes[adding] = fileBytes(path)
	}
	return divide(sizes, d.opened.poolSize())
}

// apply sets each open partition's pool and page cache to its share.
//
// THE POOL is resized in place — database/sql closes what it no longer
// needs as it is handed back — and THE CACHE is a target each connection
// brings itself to at its next statement ([beginModeConn.sizeCache]), which
// includes the writers the apply loops hold pinned for their lives: a pragma
// sent through the pool would reach whichever connection answered and no
// other.
func apply(open map[string]*DB, shares map[string]allotment) {
	for name, held := range open {
		share, ok := shares[name]
		if !ok {
			continue
		}
		pool := share.readers + held.file.Logs
		held.sql.SetMaxOpenConns(pool)
		held.sql.SetMaxIdleConns(pool)
		held.cache.Store(share.cacheKiB)
	}
}

// fileBytes is a database's size on disk — the file and its write-ahead log,
// which holds pages the file does not have yet — or 0 for one that does not
// exist, which is what a partition about to be created is.
func fileBytes(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal"} {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

// PartitionDB answers the OPEN database for the named partition, or
// [ErrNoEstate].
//
// NAMED FOR WHAT IT ANSWERS rather than `Partition`, and beside
// [DB.PartitionHandle] and [DB.PartitionPath] for the reason every gate that
// reads this tree gives for itself: they match on names, and `Partition` is
// already a method elsewhere in the tree (notify's prompts derive a trigger's
// partition key), so a gate asking who reaches a partition's database could
// not tell the two apart.
//
// NOT OPEN IS A STATE, NOT A BUG, as [ErrNoEstate] says: a partition this node
// does not hold, one it is joining whose file is not installed yet, and one an
// adoption holds closed between its rename and its reopen all answer it, and
// a goroutine already in flight when any of those happened reaches here
// legitimately. Nothing long-lived keeps the handle this returns — see
// [PartitionHandle] for what it keeps instead.
func (d *DB) PartitionDB(name string) (*DB, error) {
	// WITHOUT THE LOCK, which is safe here as it is not for a change: a
	// lookup that races a Close answers either the handle — which the
	// Close then takes down, and which then answers ErrNoEstate itself —
	// or the empty set a closed node keeps.
	if err := d.node("look up a partition"); err != nil {
		return nil, err
	}
	if held := d.parts.held()[name]; held != nil {
		return held, nil
	}
	return nil, fmt.Errorf("%w: the partition %s is not open on this node", ErrNoEstate, name)
}

// OpenPartitions names every partition this node holds open, sorted.
func (d *DB) OpenPartitions() []string {
	if d == nil || d.parts == nil {
		return nil
	}
	names := make([]string, 0, len(d.parts.held()))
	for name := range d.parts.held() {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ClosePartition closes the named partition's handle and stops holding it,
// leaving its file where it is.
//
// ALREADY CLOSED IS NOT AN ERROR: a join that failed between its close and its
// rename unwinds by reopening, and an unwind that had to know how far it got
// would need a record of that which nothing keeps.
//
// THE HANDLE LEAVES THE SET BEFORE IT CLOSES, so a lookup that races the close
// answers [ErrNoEstate] rather than a handle on its way down.
func (d *DB) ClosePartition(name string) error {
	if err := d.node("close a partition"); err != nil {
		return err
	}
	d.parts.mu.Lock()
	defer d.parts.mu.Unlock()
	if err := d.stillOpen("close a partition"); err != nil {
		return err
	}
	held := d.release(name)
	if held == nil {
		return nil
	}
	if err := held.close(); err != nil {
		return fmt.Errorf("store: close the partition %s: %w", name, err)
	}
	return nil
}

// DropPartition stops holding f and DELETES its file, with the file's -wal
// and -shm, under this process's lock on it — the last step of leaving a
// partition, once every loop that wrote it has stopped.
//
// A FILE IT DOES NOT HOLD IS DELETED ALL THE SAME, which is what a leave
// interrupted between its close and its delete needs from its retry. A file
// another process holds is refused naming the holder, and one another handle
// in THIS process holds is refused too — see [discard].
func (d *DB) DropPartition(ctx context.Context, f PartitionFile) error {
	if err := d.node("drop a partition"); err != nil {
		return err
	}
	if err := f.Validate(); err != nil {
		return err
	}
	d.parts.mu.Lock()
	defer d.parts.mu.Unlock()
	if err := d.stillOpen("drop a partition"); err != nil {
		return err
	}
	// ANOTHER FILE UNDER THIS NAME IS REFUSED BEFORE THE SET IS TOUCHED:
	// deleting the one open here would delete what the caller did not ask
	// for, and taking it out of the set to look at it first — then putting
	// it back — would answer ErrNoEstate, for a partition that stayed open
	// throughout, to every lookup in between. The runtime reads that answer
	// as a lost file and restores it.
	if held := d.parts.held()[f.Name]; held != nil && held.file != f {
		return fmt.Errorf("store: the partition %s is open on this node as %+v, "+
			"not %+v — nothing was dropped", f.Name, held.file, f)
	}
	if held := d.release(f.Name); held != nil {
		if err := held.close(); err != nil {
			return fmt.Errorf("store: close the partition %s before dropping it: %w", f.Name, err)
		}
	}
	path := d.PartitionPath(f)
	if err := discard(path); err != nil {
		return fmt.Errorf("store: drop the partition %s at %s: %w", f.Name, path, err)
	}
	return nil
}

// release takes the named partition out of the set and re-divides the node
// across what remains, answering the handle it took, or nil. The caller holds
// d.parts.mu.
func (d *DB) release(name string) *DB {
	open := d.parts.held()
	held := open[name]
	if held == nil {
		return nil
	}
	next := make(map[string]*DB, len(open))
	for other, db := range open {
		if other != name {
			next[other] = db
		}
	}
	d.parts.open.Store(&next)
	apply(next, d.divide(next, "", ""))
	return held
}

// forget takes part out of the set when part is what the set holds under its
// name — the half of closing a partition through its OWN handle that the node
// has to do. See [DB.Close].
func (d *DB) forget(part *DB) {
	d.parts.mu.Lock()
	defer d.parts.mu.Unlock()
	if d.parts.held()[part.file.Name] == part {
		d.release(part.file.Name)
	}
}

// File is the partition this handle is the file of, and the zero value on a
// node's own handle or a copy opened with [OpenEstate].
func (d *DB) File() PartitionFile {
	if d == nil {
		return PartitionFile{}
	}
	return d.file
}

// PartitionHandle is one partition's estate AS A LONG-LIVED HOLDER KEEPS IT:
// the node and the partition's name, resolved to the open file on every call
// and never captured.
//
// # Why it resolves every time
//
// A partition's file is replaced while the node runs: an adoption closes it,
// renames a peer's artefact over it and opens it again, so the *DB is a
// different one afterwards, and a holder that took the old one at boot would
// go on answering from a file no longer at that name. Resolving on every call
// is what makes the window honest instead — while there is no file open, every
// call answers [ErrNoEstate].
//
// # Why a type of its own
//
// It is what the runtime HANDS the code that reads and writes a partition —
// the state log's framework and the domains' appliers and readers — so that
// code never holds the node's handle for the purpose, and cannot confuse the
// two: the node's handle has Read and Tx too, over a file with none of the
// partition's tables in it.
type PartitionHandle struct {
	node *DB
	name string
}

// PartitionHandle is the handle a holder keeps on the named partition. It
// need not be open yet, and it may be closed and reopened under the handle:
// every call resolves it afresh.
func (d *DB) PartitionHandle(name string) PartitionHandle {
	return PartitionHandle{node: d, name: name}
}

// Name is the partition's name.
func (h PartitionHandle) Name() string { return h.name }

// IsZero reports a handle that names no partition: the zero value, which is
// what a holder that was given no estate keeps, and which answers
// [ErrNoEstate] to everything.
func (h PartitionHandle) IsZero() bool { return h.node == nil || h.name == "" }

// DB is the partition's handle as it is open now, or [ErrNoEstate]. For the
// caller that needs the file itself — its path, a backup of it — for the
// length of one operation, and holds it no longer.
func (h PartitionHandle) DB() (*DB, error) {
	if h.node == nil {
		return nil, fmt.Errorf("%w: a partition handle with no node", ErrNoEstate)
	}
	return h.node.PartitionDB(h.name)
}

// Read is [DB.Read] on the partition as it is open now.
func (h PartitionHandle) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	db, err := h.DB()
	if err != nil {
		return err
	}
	return db.Read(ctx, fn)
}

// Tx is [DB.Tx] on the partition as it is open now.
func (h PartitionHandle) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	db, err := h.DB()
	if err != nil {
		return err
	}
	return db.Tx(ctx, fn)
}

// Writer is [DB.Writer] on the partition as it is open now. The writer it
// answers is bound to THAT file, which is why the loop holding it is ended
// before an adoption replaces the file and started again after.
func (h PartitionHandle) Writer(ctx context.Context) (*Writer, error) {
	db, err := h.DB()
	if err != nil {
		return nil, err
	}
	return db.Writer(ctx)
}

// Caps answers the open file's probe, and the zero value while none is open:
// the caller is sizing a statement, not reading state, and [RowsPerInsert]
// reads a zero limit as one row per statement.
func (h PartitionHandle) Caps() Capabilities {
	db, err := h.DB()
	if err != nil {
		return Capabilities{}
	}
	return db.Caps()
}

// Reader is the same partition for a holder that only reads it.
func (h PartitionHandle) Reader() PartitionReader { return PartitionReader{h: h} }

// PartitionReader is a partition's estate for a holder that only READS it —
// a domain's reader, its writer's decide, its gates and fence, the search
// indexer over the documents it indexes, a corpus the embedding duty walks.
// It has no Tx and no Writer, and the one transaction it can open, Read's,
// runs with the engine refusing every write ([DB.Read]) — so a reader cannot
// become a writer, by accident or through a statement inside its read: the
// rule that only an applier writes a partition is then a property of the type
// such a holder is handed, before any gate reads the code, and the gate that
// does read it (internal/store's TestOnlyTheApplierWritesThePartitions) has
// only to find who holds the other type.
type PartitionReader struct{ h PartitionHandle }

// Name is the partition's name.
func (r PartitionReader) Name() string { return r.h.name }

// IsZero reports a reader that names no partition — see
// [PartitionHandle.IsZero].
func (r PartitionReader) IsZero() bool { return r.h.IsZero() }

// Read is [DB.Read] on the partition as it is open now.
func (r PartitionReader) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	return r.h.Read(ctx, fn)
}

// Caps is [PartitionHandle.Caps]: the open file's probe, or the zero value
// while none is open. A reader sizes its own statements by it, which is why a
// read handle carries it.
func (r PartitionReader) Caps() Capabilities { return r.h.Caps() }
