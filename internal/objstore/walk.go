package objstore

import (
	"context"
	"database/sql"
	"fmt"
)

// Walking the declared references, and merging the walks into one order.
//
// Three passes read the objects the declared tables name, each the tables of
// the standings it is owed ([Pass]): the collector's audit
// (internal/objstore/collect), which asks the store about every object a
// Required table names; the backup (internal/backup), which copies each of
// those or asks after it; and a store move (internal/objstore/migrate), which
// copies every object a Required or a Derived table names into the store the
// fleet moves to. All three read the same declarations in the same keyset
// pages, and the first two need ONE key order across every table — the audit
// cuts the order into ranges a store lists at once, and the backup
// de-duplicates a key two rows name by comparing it with the one before. So
// the walk, the merge and the page live here, once: the backup's own keyset
// loop beside the audit's was two implementations of one walk (ADR-0008), and
// it held every reference twice besides.

// Pass is one of the passes that walk the declared tables' references, and so
// which standings' tables its walk reads — three columns of [Standing]'s table
// of obligations, held here once rather than restated by each pass.
//
// A WALK IS BUILT FOR ITS PASS ([ReferenceTable.WalkOf]) and refuses, on its
// first page and before it reads anything, a table its pass does not read: a
// pass that forgot to leave such a table out fails at once rather than paging
// somebody about an object nobody has to repair, copying one into every backup
// or carrying a replaced one into the store the fleet moves to.
type Pass string

const (
	// PassAudit is the collector's audit (internal/objstore/collect), which
	// asks the store after every object a Required table names.
	PassAudit Pass = "audit"

	// PassBackup is a backup's object pass (internal/backup), which accounts
	// for every object a Required table names.
	PassBackup Pass = "backup"

	// PassMove is a store move (internal/objstore/migrate), which copies into
	// the store the fleet moves to every object a Required table names AND
	// every one a Derived table names. A derived object left behind is one
	// every node of the moved fleet then builds again, at a cost that grows
	// with what it was built from rather than with anything that changed; the
	// move copies it for its bytes, once, through the same verified read it
	// makes of every object. A retired one stays behind: the move ends with
	// every node restarted onto the new store, so no reader is left that has
	// not caught up with what replaced it.
	PassMove Pass = "move"
)

// passes is every [Pass], for the question whether any of them reads a
// standing.
var passes = [...]Pass{PassAudit, PassBackup, PassMove}

// Valid reports whether p is one of the three passes.
func (p Pass) Valid() bool { return p == PassAudit || p == PassBackup || p == PassMove }

// Reads reports whether p's walk reads a table of standing s: the audit's and
// a backup's read Required tables alone, a store move's Required and Derived
// ones, and no pass reads a Retired table's rows as references.
func (p Pass) Reads(s Standing) bool {
	switch p {
	case PassAudit, PassBackup:
		return s == Required
	case PassMove:
		return s == Required || s == Derived
	}
	return false
}

// reading is what p's walk reads, as a refusal says it.
func (p Pass) reading() string {
	if p == PassMove {
		return "Required and Derived tables"
	}
	return "Required tables alone"
}

// walked reports whether any pass reads a table of standing s.
func walked(s Standing) bool {
	for _, p := range passes {
		if p.Reads(s) {
			return true
		}
	}
	return false
}

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

	// Standing is that table's ([ReferenceTable.Standing]), which a pass
	// walking more than one standing reads: a store move owes the target
	// every object a Required row names, and copies one a Derived row names
	// only where the source can give it back whole.
	Standing Standing
}

// Walk is one declared table's references in key order ([Key.Compare]), a
// keyset page at a time, for one [Pass].
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

// WalkOf is t's keyset walk for pass, each page in one read of its own through
// read.
//
// ONLY A TABLE THE PASS READS IS WALKED ([Pass.Reads]): on any other — a
// retired or derived one for the audit or a backup, a retired one for a store
// move — the walk's first Next answers a refusal naming the table, its
// standing and the pass, before anything is read, so a pass that forgot to
// leave such a table out fails on its first page rather than paging somebody
// about, copying or carrying an object it is not owed (ADR-0074).
func (t ReferenceTable) WalkOf(read RowReader, pass Pass) Walk {
	return &tableWalk{table: t, read: read, pass: pass}
}

// tableWalk is one table's keyset walk ([Walk]).
type tableWalk struct {
	table ReferenceTable
	read  RowReader
	pass  Pass

	// after is the last key handed back, and done set once a page came
	// back short — there is nothing after it.
	after string
	done  bool
}

func (w *tableWalk) Next(ctx context.Context) ([]Reference, bool, error) {
	if w.done {
		return nil, true, nil
	}
	if err := w.table.Validate(); err != nil {
		return nil, false, err
	}
	switch {
	case !w.pass.Valid():
		return nil, false, fmt.Errorf("objstore: a walk of %s for %q, which is no pass "+
			"(objstore.PassAudit, objstore.PassBackup or objstore.PassMove)", w.table.Table, w.pass)
	case !w.pass.Reads(w.table.Standing):
		return nil, false, fmt.Errorf("objstore: %s is declared %s, and the %s pass walks %s: "+
			"its rows name no object this pass is owed", w.table.Table, w.table.Standing,
			w.pass, w.pass.reading())
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
			page = append(page, Reference{Object: obj, NamedBy: namedBy, Table: w.table.Table,
				Standing: w.table.Standing})
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
