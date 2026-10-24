package objstore

import (
	"context"
	"database/sql"
	"fmt"
)

// Walking the declared references, and merging the walks into one order.
//
// Two passes read every object a Required table names: the collector's audit
// (internal/objstore/collect), which asks the store about each, and the
// backup (internal/backup), which copies each or asks after it. Both read the
// same declarations in the same keyset pages and both need ONE key order
// across every table — the audit cuts the order into ranges a store lists at
// once, and the backup de-duplicates a key two rows name by comparing it with
// the one before. So the walk, the merge and the page live here, once: the
// backup's own keyset loop beside the audit's was two implementations of one
// walk (ADR-0008), and it held every reference twice besides.

// Reference is one row naming an object: the object as the row records it,
// the row as a person reads it, and the declared table it is a row of.
type Reference struct {
	Object Object

	// NamedBy is the row's owner columns joined by "/" — a project and a
	// path, the file a person would look the object up as.
	NamedBy string

	// Table is the declared table the row is in: two tables may each own
	// a row of one name, and a reader that keys anything on the row has
	// to tell them apart.
	Table string
}

// Walk is one Required table's references in key order ([Key.Compare]), a
// keyset page at a time.
type Walk interface {
	// Next is the walk's next page — at most [WalkPage] references, every
	// key after the last page's, in key order; none once the walk has
	// ended — and whether the read answering it was COMPLETE: false while
	// the reader holds a record it could not apply that might refer to an
	// object.
	//
	// EACH PAGE IS ONE READ, ENDED BEFORE NEXT RETURNS, so nothing a caller
	// does with a page — a question to the store, a copy of an object, round
	// trips a read's connection must not be held across — happens inside a
	// read, and the next page is read only when the caller asks for it.
	Next(ctx context.Context) ([]Reference, bool, error)
}

// RowReader runs fn in ONE read of the rows a walk reads and reports whether
// they are COMPLETE — false while the reader holds a record it could not apply
// that might refer to an object. A copy of an estate is a file and has no such
// record, so a reader over one answers true.
type RowReader func(ctx context.Context, fn func(*sql.Tx) error) (bool, error)

// WalkPage is how many rows one read of a walk returns: a page of the
// declaration's keyset ([ReferenceTable.ReferencesAfter]).
//
// FIVE HUNDRED. A row is a key, a digest, a size and the owner columns — a few
// hundred bytes — so a page is a hundred to two hundred kibibytes in memory, one
// LIMIT on the key column's index per read. Smaller is more reads for no memory
// anybody needed back; larger holds more of every table in a merge that holds a
// page of each ([MergeWalks]).
const WalkPage = 500

// WalkOf is t's keyset walk, each page in one read of its own through read.
//
// ONLY A REQUIRED TABLE IS WALKED: on a retired one the walk's first Next
// answers [ReferenceTable.ReferencesAfter]'s refusal, so a pass that forgot to
// leave a retired table out fails on its first page rather than paging
// somebody about, or copying, an object nobody is owed (ADR-0047).
func (t ReferenceTable) WalkOf(read RowReader) Walk {
	return &tableWalk{table: t, read: read}
}

// tableWalk is one table's keyset walk ([Walk]).
type tableWalk struct {
	table ReferenceTable
	read  RowReader

	// after is the last key handed back, and done set once a page came
	// back short — there is nothing after it.
	after string
	done  bool
}

func (w *tableWalk) Next(ctx context.Context) ([]Reference, bool, error) {
	if w.done {
		return nil, true, nil
	}
	query, err := w.table.ReferencesAfter(WalkPage)
	if err != nil {
		return nil, false, err
	}
	var page []Reference
	whole, err := w.read(ctx, func(tx *sql.Tx) error {
		// FROM EMPTY ON EVERY RUN: a read the store found busy runs fn
		// again (store.DB.Read retries a transient failure), and a page
		// appended to across two runs hands on the rows the first one read
		// twice — and, longer than a page, never reads as the last one.
		page = page[:0]
		rows, qerr := tx.QueryContext(ctx, query, w.after)
		if qerr != nil {
			return fmt.Errorf("read the objects %s names: %w", w.table.Table, qerr)
		}
		defer func() { _ = rows.Close() }()
		return w.table.ScanReferences(rows, func(obj Object, namedBy string) error {
			page = append(page, Reference{Object: obj, NamedBy: namedBy, Table: w.table.Table})
			return nil
		})
	})
	if err != nil {
		return nil, false, err
	}
	if len(page) < WalkPage {
		w.done = true
	}
	if len(page) > 0 {
		w.after = page[len(page)-1].Object.Key.String()
	}
	return page, whole, nil
}

// NamedWalk is one walk of a merge, with the name a failure of it is reported
// under — the domain it reads, or the table.
type NamedWalk struct {
	Name string
	Walk Walk
}

// Merge is every reference of a set of walks in ONE key order, holding one
// page of each — the walks times a page, never the inventory.
//
// TWO WALKS NAMING ONE KEY HAND IT ON ADJACENT: the merge takes the lowest key
// among the walks' heads, so a key two rows name comes out twice in a row. A
// reader that de-duplicates — the backup carries each object once — compares
// a reference with the one before it and holds nothing else.
type Merge struct {
	heads []*mergeHead
	// complete is whether every page read so far was read complete.
	complete bool
}

// mergeHead is one walk's place in the merge: its current page and how far
// into it the merge has handed references on.
type mergeHead struct {
	NamedWalk
	page []Reference
	next int
	done bool
	// last is the last key handed on, which the walk's next must not come
	// before.
	last Key
}

// MergeWalks merges walks into one key order.
func MergeWalks(walks ...NamedWalk) *Merge {
	m := &Merge{complete: true}
	for _, w := range walks {
		m.heads = append(m.heads, &mergeHead{NamedWalk: w})
	}
	return m
}

// Next is the reference with the first key among every walk's next, and false
// once every walk has ended. It reads a walk's next page — one read — only when
// that walk's page is spent, and refuses a walk whose keys go backwards: a
// reader cutting this order into ranges, or de-duplicating across it, would
// otherwise be handed a reference out of the place it relies on.
//
// A SCAN OF THE HEADS rather than a heap: there is one head per Required table
// in the company, a handful, and a scan of a handful is cheaper than keeping a
// heap of them.
func (m *Merge) Next(ctx context.Context) (Reference, bool, error) {
	var best *mergeHead
	for _, h := range m.heads {
		if h.next == len(h.page) && !h.done {
			page, whole, err := h.Walk.Next(ctx)
			if err != nil {
				return Reference{}, false, fmt.Errorf("read %s's references: %w", h.Name, err)
			}
			m.complete = m.complete && whole
			h.page, h.next, h.done = page, 0, len(page) == 0
		}
		if h.next == len(h.page) {
			continue
		}
		if best == nil || h.page[h.next].Object.Key.Compare(best.page[best.next].Object.Key) < 0 {
			best = h
		}
	}
	if best == nil {
		return Reference{}, false, nil
	}
	ref := best.page[best.next]
	best.next++
	if ref.Object.Key.Compare(best.last) < 0 {
		return Reference{}, false, fmt.Errorf("read %s's references: %s came after %s, "+
			"out of the key order a walk reads in", best.Name, ref.Object.Key, best.last)
	}
	best.last = ref.Object.Key
	return ref, true, nil
}

// Complete is whether every page the merge has read so far was read complete.
func (m *Merge) Complete() bool { return m.complete }
