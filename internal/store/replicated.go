package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// A node's REPLICATED ESTATE — everything a state log's applier derives — is a
// file of its own beside the node's, and this file is that half of the store:
// the slot the node keeps it open in, and the two handles the runtime hands
// out on it.
//
// # Why it is opened on the node rather than with it
//
// [OpenNode] opens the node's own file and nothing else, and the replicated
// file is opened on the handle it returns ([DB.OpenReplicated]), for three
// reasons that each rule out opening both together:
//
//   - A NODE WITHOUT `data` HOLDS NONE. Its own file is scratch and it runs no
//     applier, so it never opens this one, and a path that reaches for it is
//     told [ErrNoEstate] rather than handed an empty database that reads as a
//     company with nothing in it.
//   - AN ADOPTION REPLACES THE FILE WHILE THE NODE RUNS: it closes it, renames
//     a peer's snapshot over it and opens it again ([DB.CloseReplicated], then
//     [DB.OpenReplicated]). The node's own file stays open throughout, so the
//     two cannot share one lifetime.
//   - ITS POOL IS SIZED BY WHAT RUNS ON IT: one pinned writer per log the
//     engine applies there, which only the engine knows — internal/statelog
//     imports this package, so the count is passed in rather than learned.
//
// What does not change is everything the package doc says about a FILE: it
// is exclusively owned, migrated on open, written through the one FIFO line
// its lock carries, and no transaction spans it and the node's own.

// replicatedReadFloor is the fewest readers the replicated estate's pool
// holds beside the writers its apply loops pin.
//
// TWO, so a long scan — a search reading its candidates, a snapshot being
// taken — never holds the only connection a seat's point read could use. It
// matters only to a node configured with `store.max_open_conns: 1`, whose
// own file keeps the one connection it asked for; the replicated file is read
// by every seat's tools, and one reader there serves them one read at a time
// behind whatever scan is running.
const replicatedReadFloor = 2

// replicatedSlot is the replicated estate a node handle holds open, or none.
type replicatedSlot struct {
	// mu serialises every change to the slot — an open or a close — so two
	// changes cannot each act on a slot the other is changing, and orders
	// both against [DB.Close] (see [DB.stillOpen]).
	mu sync.Mutex

	// open is the replicated estate's handle, or nil while none is open.
	// SWAPPED UNDER mu AND READ WITHOUT IT: [DB.ReplicatedDB] is on every
	// read a domain makes, and the file is opened and closed a handful of
	// times in a node's life.
	open atomic.Pointer[DB]
}

// ErrNotANode is the replicated estate asked of a handle that is not a node's
// own.
//
// The estate is held by the NODE: its handle is what every open, close and
// lookup goes through, so a caller holding the replicated estate's own handle
// — or a copy opened with [OpenEstate] — cannot reach a second replicated
// file through it.
var ErrNotANode = errors.New("store: the replicated estate is held by a node's own handle, and this is not one")

// node refuses a handle that is not a node's own, naming the gesture.
//
// IT DOES NOT JUDGE WHETHER THE NODE IS STILL OPEN, beyond a handle that was
// never opened: that is [DB.stillOpen]'s, asked under the slot's lock by every
// gesture that changes the slot.
func (d *DB) node(gesture string) error {
	switch {
	case d == nil || d.sql == nil:
		return fmt.Errorf("%w: %s: %w", ErrNotANode, gesture, ErrNoEstate)
	case d.slot == nil:
		return fmt.Errorf("%w: %s on the %s handle at %s", ErrNotANode, gesture, d.estate, d.path)
	}
	return nil
}

// stillOpen refuses a gesture on a node that has been closed. A gesture that
// changes the slot asks it holding d.slot.mu.
//
// UNDER THE LOCK, AND ONLY THERE, because that is what orders it against
// [DB.Close]: Close marks the node closed BEFORE it takes the same lock to
// empty the slot, so a gesture that finds the node open here holds the lock
// until its change is in the slot — which Close then takes down — and one
// that comes after finds it closed. Judged before the lock, an open that
// passed the check, waited for the lock while Close emptied the slot, and
// then took it, would open a file into a node nothing will close again: its
// pool, its descriptors and this process's claim on it would outlive the node
// for the life of the process, and another process would be refused the file.
func (d *DB) stillOpen(gesture string) error {
	if d.closed.Load() {
		return fmt.Errorf("store: %s: the node is closed: %w", gesture, ErrNoEstate)
	}
	return nil
}

// ReplicatedFile is where THIS node keeps its replicated estate:
// [ReplicatedPath] for its own file and its store.replicated_path, or the
// file itself asked of the replicated estate's own handle. The estate need
// not be open — an operator reading the path of one an adoption left closed
// needs exactly that answer — and a nil handle answers "".
//
// ASKED OF THE NODE, never derived from a directory alone: the file is
// wherever the node's configuration says, so an answer computed without that
// configuration is right only while the configured name happens to be the
// default — which is the bug `crewlet migrate` had while it derived the path
// itself.
func (d *DB) ReplicatedFile() string {
	switch {
	case d == nil:
		return ""
	case d.estate == EstateReplicated:
		return d.path
	}
	return ReplicatedPath(d.path, d.opened.ReplicatedPath)
}

// OpenReplicated opens the replicated estate's file — creating it, and
// migrating it to this binary's schema, when it is absent or behind — under
// this process's exclusive lock, and holds it on this node until
// [DB.CloseReplicated] or [DB.Close]. logs is how many logs' apply loops run
// on it, each of which PINS one writer for the life of the loop ([DB.Writer]),
// so the pool is sized for exactly that many beside its readers.
//
// IDEMPOTENT for the estate already open with the same logs, which answers the
// handle it is open as: a boot and a restore that both open it open what is
// already open. A different count is refused, because the pool open now was
// sized for the other one.
func (d *DB) OpenReplicated(ctx context.Context, logs int) (*DB, error) {
	if err := d.node("open the replicated estate"); err != nil {
		return nil, err
	}
	if logs < 1 {
		return nil, fmt.Errorf("store: open the replicated estate for %d logs: it "+
			"carries at least one, and each pins a writer on the file", logs)
	}
	path := d.ReplicatedFile()
	// REFUSED BEFORE THE LOCK, for the reason [OpenNode] refused it before
	// anything: two exclusive claims on one path do not collide inside this
	// process, so one file for both would carry two migration sequences and
	// every applier would write beside the audit log.
	if path == d.path && path != "" && !strings.HasPrefix(path, ":memory:") {
		return nil, fmt.Errorf("%w: %s", ErrOneFile, path)
	}

	d.slot.mu.Lock()
	defer d.slot.mu.Unlock()
	if err := d.stillOpen("open the replicated estate"); err != nil {
		return nil, err
	}
	if held := d.slot.open.Load(); held != nil {
		// Read without the pin lock: declared is written once, before
		// the handle was published into the slot.
		if held.pins.declared != logs {
			return nil, fmt.Errorf("store: the replicated estate is open on this "+
				"node for %d logs, not %d — close it before opening it for another "+
				"count, because its pool was sized for the first", held.pins.declared, logs)
		}
		return held, nil
	}
	db, err := openEstate(ctx, EstateReplicated, path, d.replicatedOptions(logs))
	if err != nil {
		return nil, fmt.Errorf("store: open the replicated estate: %w", err)
	}
	db.owner = d
	d.slot.open.Store(db)
	return db, nil
}

// replicatedOptions is the Options the replicated estate's handle is opened
// with: the node's own, with a pool of the node's readers — never fewer than
// [replicatedReadFloor] — PLUS one connection per log, and the embedding
// width the node has LEARNED since it was opened.
//
// THE PINS ARE ADDED, NEVER TAKEN OUT OF THE READERS, configured or not: a
// pool bounded at the configured figure with four writers pinned in it leaves
// a read burst queueing behind them — and a figure at or below the pin count
// leaves the last apply loop waiting for a connection none of the others
// will ever give back.
func (d *DB) replicatedOptions(logs int) Options {
	opts := d.opened
	opts.Scratch = false
	opts.MaxOpenConns = max(replicatedReadFloor, d.opened.poolSize()) + logs
	opts.pins = logs
	opts.EmbeddingDim = int(d.dim.Load())
	return opts
}

// ReplicatedDB answers the OPEN replicated estate, or [ErrNoEstate].
//
// NAMED FOR WHAT IT ANSWERS, beside [DB.Replicated] — which answers the
// long-lived handle rather than the file — for the reason every gate that
// reads this tree gives for itself: they match on names, and one name for both
// would leave a gate unable to tell a holder that keeps the open file from one
// that resolves it per call.
//
// NOT OPEN IS A STATE, NOT A BUG, as [ErrNoEstate] says: a node without
// `data`, an adoption holding the file closed between its rename and its
// reopen, and a node after [DB.Close] all answer it, and a goroutine already
// in flight when any of those happened reaches here legitimately. Nothing
// long-lived keeps the handle this returns — see [ReplicatedHandle] for what
// it keeps instead.
func (d *DB) ReplicatedDB() (*DB, error) {
	// WITHOUT THE LOCK, which is safe here as it is not for a change: a
	// lookup that races a Close answers either the handle — which the
	// Close then takes down, and which then answers ErrNoEstate itself —
	// or the empty slot a closed node keeps.
	if err := d.node("look up the replicated estate"); err != nil {
		return nil, err
	}
	if held := d.slot.open.Load(); held != nil {
		return held, nil
	}
	return nil, fmt.Errorf("%w: the replicated estate is not open on this node", ErrNoEstate)
}

// CloseReplicated closes the replicated estate's handle and stops holding it,
// leaving its file where it is.
//
// ALREADY CLOSED IS NOT AN ERROR: an adoption that failed between its close
// and its rename unwinds by reopening, and an unwind that had to know how far
// it got would need a record of that which nothing keeps.
//
// THE HANDLE LEAVES THE SLOT BEFORE IT CLOSES, so a lookup that races the
// close answers [ErrNoEstate] rather than a handle on its way down.
func (d *DB) CloseReplicated() error {
	if err := d.node("close the replicated estate"); err != nil {
		return err
	}
	d.slot.mu.Lock()
	defer d.slot.mu.Unlock()
	if err := d.stillOpen("close the replicated estate"); err != nil {
		return err
	}
	held := d.slot.open.Swap(nil)
	if held == nil {
		return nil
	}
	if err := held.close(); err != nil {
		return fmt.Errorf("store: close the replicated estate: %w", err)
	}
	return nil
}

// forget empties the slot when it holds r — the half of closing the
// replicated estate through its OWN handle that the node has to do. See
// [DB.Close].
func (d *DB) forget(r *DB) {
	d.slot.mu.Lock()
	defer d.slot.mu.Unlock()
	d.slot.open.CompareAndSwap(r, nil)
}

// ReplicatedHandle is the replicated estate AS A LONG-LIVED HOLDER KEEPS IT:
// the node, resolved to the open file on every call and never captured.
//
// # Why it resolves every time
//
// The file is replaced while the node runs: an adoption closes it, renames a
// peer's artefact over it and opens it again, so the *DB is a different one
// afterwards, and a holder that took the old one at boot would go on answering
// from a file no longer at that name. Resolving on every call is what makes
// the window honest instead — while there is no file open, every call answers
// [ErrNoEstate].
//
// # Why a type of its own
//
// It is what the runtime HANDS the code that reads and writes the replicated
// estate — the state log's framework and the domains' appliers and readers —
// so that code never holds the node's handle for the purpose, and cannot
// confuse the two: the node's handle has Read and Tx too, over a file with
// none of the estate's tables in it.
type ReplicatedHandle struct{ node *DB }

// Replicated is the handle a holder keeps on this node's replicated estate.
// It need not be open yet, and it may be closed and reopened under the
// handle: every call resolves it afresh.
func (d *DB) Replicated() ReplicatedHandle { return ReplicatedHandle{node: d} }

// IsZero reports a handle on no node: the zero value, which is what a holder
// that was given no estate keeps, and which answers [ErrNoEstate] to
// everything.
func (h ReplicatedHandle) IsZero() bool { return h.node == nil }

// DB is the replicated estate's handle as it is open now, or [ErrNoEstate].
// For the caller that needs the file itself — its path, a backup of it — for
// the length of one operation, and holds it no longer.
func (h ReplicatedHandle) DB() (*DB, error) {
	if h.node == nil {
		return nil, fmt.Errorf("%w: a replicated-estate handle with no node", ErrNoEstate)
	}
	return h.node.ReplicatedDB()
}

// Read is [DB.Read] on the replicated estate as it is open now.
func (h ReplicatedHandle) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	db, err := h.DB()
	if err != nil {
		return err
	}
	return db.Read(ctx, fn)
}

// Tx is [DB.Tx] on the replicated estate as it is open now.
func (h ReplicatedHandle) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	db, err := h.DB()
	if err != nil {
		return err
	}
	return db.Tx(ctx, fn)
}

// Writer is [DB.Writer] on the replicated estate as it is open now. The
// writer it answers is bound to THAT file, which is why the loop holding it
// is ended before an adoption replaces the file and started again after.
func (h ReplicatedHandle) Writer(ctx context.Context) (*Writer, error) {
	db, err := h.DB()
	if err != nil {
		return nil, err
	}
	return db.Writer(ctx)
}

// Caps answers the open file's capabilities ([DB.Caps]), and the zero value
// while none is open: the caller is sizing a statement, not reading state, and
// [RowsPerInsert] reads a zero limit as one row per statement.
func (h ReplicatedHandle) Caps() Capabilities {
	db, err := h.DB()
	if err != nil {
		return Capabilities{}
	}
	return db.Caps()
}

// Reader is the same estate for a holder that only reads it.
func (h ReplicatedHandle) Reader() ReplicatedReader { return ReplicatedReader{h: h} }

// ReplicatedReader is the replicated estate for a holder that only READS it —
// a domain's reader, its writer's decide, its gates and fence, the search
// indexer over the documents it indexes, a corpus the embedding duty walks.
// It has no Tx and no Writer, and the one transaction it can open, Read's,
// runs with the engine refusing every write ([DB.Read]) — so a reader cannot
// become a writer, by accident or through a statement inside its read: the
// rule that only an applier writes the replicated estate is then a property
// of the type such a holder is handed, before any gate reads the code, and
// the gate that does read it (internal/store's
// TestOnlyTheApplierWritesTheReplicatedEstate) has only to find who holds the
// other type.
type ReplicatedReader struct{ h ReplicatedHandle }

// IsZero reports a reader on no node — see [ReplicatedHandle.IsZero].
func (r ReplicatedReader) IsZero() bool { return r.h.IsZero() }

// Read is [DB.Read] on the replicated estate as it is open now.
func (r ReplicatedReader) Read(ctx context.Context, fn func(*sql.Tx) error) error {
	return r.h.Read(ctx, fn)
}

// Caps is [ReplicatedHandle.Caps]: the open file's capabilities, or the zero
// value while none is open. A reader sizes its own statements by it, which is
// why a read handle carries it.
func (r ReplicatedReader) Caps() Capabilities { return r.h.Caps() }
