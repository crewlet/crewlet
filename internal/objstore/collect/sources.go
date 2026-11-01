package collect

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Source is one domain whose rows refer to objects, as the collector reads it.
//
// The engine builds these with [Sources] from the declared tables and never
// by hand: a source written for one table is a second answer to which tables
// name objects, and the day it and the declarations disagree is the day the
// backup carries a file's bytes that the collector has deleted.
type Source interface {
	// Name is the domain, for a log line.
	Name() string

	// Barrier answers a position at or past everything the domain's log
	// had committed when it was called, having waited for this node to
	// apply that far.
	Barrier(ctx context.Context) (statelog.Position, error)

	// Referenced answers which of among any of the domain's declared
	// tables names — a retired or derived one included, since such a row
	// keeps its object from collection as surely as a live one — read no
	// earlier than at (zero: whatever this node holds), and whether the
	// answer is COMPLETE: false while this node holds a record it could not
	// apply that might refer to an object.
	Referenced(ctx context.Context, among []objstore.Key, at statelog.Position) (map[objstore.Key]struct{}, bool, error)

	// Required is a walk of each of the domain's REQUIRED tables — never a
	// retired or derived one's, whose objects the company is not owed
	// (ADR-0074) — read no earlier than at, each in KEY ORDER
	// ([objstore.Key.Compare]): the audit's ([objstore.PassAudit]).
	//
	// IN KEY ORDER because the audit merges every walk into one ordered
	// stream of references, and a store that lists its names in order is
	// then asked about a range of them at once rather than about each.
	//
	// NAMED FOR WHAT IT LEAVES OUT, beside a Referenced that leaves out
	// nothing: a reader seeing a bare Walks would take it for every table.
	Required(at statelog.Position) []objstore.Walk

	// Moved is a walk of each of the domain's tables a STORE MOVE copies
	// (internal/objstore/migrate, [objstore.PassMove]) — its Required ones
	// and its Derived ones, never a retired one's — read no earlier than at,
	// every reference carrying its table's standing: the move owes the
	// store it copies into every object a Required row names, and copies
	// one a Derived row names where the source can give it back whole.
	//
	// NO MERGE, unlike the audit's: a move copies each object on its own, so
	// it needs neither one order across the tables nor a key two rows name
	// handed on once — a second copy finds the first already there.
	Moved(at statelog.Position) []objstore.Walk
}

// References is every source the engine runs.
type References []Source

// Estate is one state-log domain's rows on this node, as the collector reads
// them.
//
// ONLY THE READ, never the statement: which tables to read and how is the
// declaration's ([objstore.ReferenceTable]), so a domain supplies the two
// things only it can — a barrier on its own log, and a read of its own rows
// that reports whether it covers every record.
type Estate interface {
	// Name is the domain, as a declaration's Domain spells it.
	Name() string

	// Barrier answers a position at or past everything the domain's log
	// had committed when it was called, having waited for this node to
	// apply that far.
	Barrier(ctx context.Context) (statelog.Position, error)

	// Read runs fn over this node's rows of the domain, no earlier than at
	// (zero: whatever this node holds), and reports whether they are
	// COMPLETE — false while this node holds a record of the domain it
	// could not apply.
	Read(ctx context.Context, at statelog.Position, fn func(*sql.Tx) error) (bool, error)
}

// Sources is every declared table, read through the estate of the domain it
// names — the one constructor [References] is built with.
//
// Refused rather than degraded, every way the two can disagree: a table whose
// domain has no estate here would read as naming nothing, which is the
// failure that deletes files; an estate no table names is a barrier every
// pass takes for nothing, and a wiring mistake besides; one named twice would
// be read twice under two positions.
//
// Each source keeps its objects from collection through every one of its
// tables; the audit walks only its Required ones, and a store move its
// Required and Derived ones ([objstore.Pass.Reads] decides both). A domain
// whose every table is retired or derived is accepted: the audit's walk of it
// reads nothing and is complete, and nothing about it is wrong.
func Sources(tables []objstore.ReferenceTable, estates ...Estate) (References, error) {
	if len(tables) == 0 {
		return nil, errors.New("objstore/collect: no table is declared as naming objects — " +
			"with none, every object reads as unreferenced")
	}
	byDomain := map[string]*tableSource{}
	var built []*tableSource
	for _, e := range estates {
		if e == nil {
			return nil, errors.New("objstore/collect: a nil estate")
		}
		if _, dup := byDomain[e.Name()]; dup {
			return nil, fmt.Errorf("objstore/collect: the %s estate is given twice", e.Name())
		}
		src := &tableSource{estate: e}
		byDomain[e.Name()] = src
		built = append(built, src)
	}
	for _, t := range tables {
		if err := t.Validate(); err != nil {
			return nil, err
		}
		src, ok := byDomain[t.Domain]
		if !ok {
			return nil, fmt.Errorf("objstore/collect: %s names objects and is written by "+
				"the %s log, which this node reads no estate of — its objects would read "+
				"as unreferenced", t.Table, t.Domain)
		}
		src.tables = append(src.tables, t)
		// AFTER Validate, which has refused a standing that is none of the
		// three: only a Required table's objects are the company's to audit,
		// and a store move copies a Derived table's besides.
		if objstore.PassAudit.Reads(t.Standing) {
			src.required = append(src.required, t)
		}
		if objstore.PassMove.Reads(t.Standing) {
			src.moved = append(src.moved, t)
		}
	}
	out := make(References, 0, len(built))
	for _, src := range built {
		if len(src.tables) == 0 {
			return nil, fmt.Errorf("objstore/collect: the %s estate is given and no "+
				"declared table is in it", src.estate.Name())
		}
		out = append(out, src)
	}
	return out, nil
}

// tableSource is one domain's declared tables, read through its estate:
// tables is every one of them, each of whose rows keeps the object it names
// from collection; required is those the audit walks ([objstore.PassAudit]:
// the Required ones), and moved those a store move walks
// ([objstore.PassMove]: the Required and the Derived ones).
type tableSource struct {
	estate   Estate
	tables   []objstore.ReferenceTable
	required []objstore.ReferenceTable
	moved    []objstore.ReferenceTable
}

func (s *tableSource) Name() string { return s.estate.Name() }

func (s *tableSource) Barrier(ctx context.Context) (statelog.Position, error) {
	return s.estate.Barrier(ctx)
}

// Referenced reads every declared table of the domain — whatever its standing
// — in ONE read, so the tables are judged at one position and one
// completeness.
func (s *tableSource) Referenced(ctx context.Context, among []objstore.Key,
	at statelog.Position) (map[objstore.Key]struct{}, bool, error) {

	out := map[objstore.Key]struct{}{}
	if len(among) == 0 {
		return out, true, nil
	}
	args := make([]any, len(among))
	for i, k := range among {
		args[i] = k.String()
	}
	complete, err := s.estate.Read(ctx, at, func(tx *sql.Tx) error {
		for _, t := range s.tables {
			query, err := t.ObjectsAmong(len(among))
			if err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return fmt.Errorf("read the objects %s names: %w", t.Table, err)
			}
			err = t.ScanObjectsAmong(rows, func(k objstore.Key) error {
				out[k] = struct{}{}
				return nil
			})
			_ = rows.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, complete, nil
}

// Required is a keyset walk of each REQUIRED table of the domain
// ([objstore.ReferenceTable.WalkOf], for [objstore.PassAudit]), each page read
// through the estate no earlier than at, so a record the node could not apply
// at any of them is seen — see [Source.Required]. With no required table it is
// none, and the audit has nothing of this domain to ask about.
func (s *tableSource) Required(at statelog.Position) []objstore.Walk {
	return s.walks(s.required, objstore.PassAudit, at)
}

// Moved is a keyset walk of each table of the domain a store move copies
// ([objstore.PassMove]), read as [tableSource.Required] reads — see
// [Source.Moved].
func (s *tableSource) Moved(at statelog.Position) []objstore.Walk {
	return s.walks(s.moved, objstore.PassMove, at)
}

// walks is a keyset walk of each of tables for pass, each page read through
// the estate no earlier than at.
func (s *tableSource) walks(tables []objstore.ReferenceTable, pass objstore.Pass,
	at statelog.Position) []objstore.Walk {

	read := func(ctx context.Context, fn func(*sql.Tx) error) (bool, error) {
		return s.estate.Read(ctx, at, fn)
	}
	out := make([]objstore.Walk, 0, len(tables))
	for _, t := range tables {
		out = append(out, t.WalkOf(read, pass))
	}
	return out
}

// view is what one pass read the references at: a floor per source.
type view []statelog.Position

// pin takes a barrier on every source, so a pass reads an estate at least as
// new as the moment it began.
func (r References) pin(ctx context.Context) (view, error) {
	out := make(view, len(r))
	for i, s := range r {
		at, err := s.Barrier(ctx)
		if err != nil {
			return nil, fmt.Errorf("objstore/collect: bring %s's view current: %w", s.Name(), err)
		}
		out[i] = at
	}
	return out, nil
}

// referenced is which of among any source refers to, and whether every
// source answered completely, each read no earlier than the view pinned for
// it.
func (r References) referenced(ctx context.Context, among []objstore.Key, v view) (map[objstore.Key]struct{}, bool, error) {
	all := map[objstore.Key]struct{}{}
	complete := true
	for i, s := range r {
		set, whole, err := s.Referenced(ctx, among, v[i])
		if err != nil {
			return nil, false, fmt.Errorf("objstore/collect: read %s's references: %w", s.Name(), err)
		}
		for k := range set {
			all[k] = struct{}{}
		}
		complete = complete && whole
	}
	return all, complete, nil
}
