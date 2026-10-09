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
	// tables names — a retired one included, since a retired row keeps its
	// object from collection as surely as a live one — read no earlier than
	// at (zero: whatever this node holds), and whether the answer is
	// COMPLETE: false while this node holds a record it could not apply
	// that might refer to an object.
	Referenced(ctx context.Context, among []objstore.Key, at statelog.Position) (map[objstore.Key]struct{}, bool, error)

	// EachRequired hands every reference the domain holds in a REQUIRED
	// table to visit — never a retired one's, whose object the company is
	// not owed (ADR-0033) — read no earlier than at, and reports whether
	// the walk was COMPLETE. visit is NEVER CALLED INSIDE A READ of the
	// estate: it asks the backend about each, a round trip a read's
	// connection must not be held across.
	//
	// NAMED FOR WHAT IT LEAVES OUT, beside a Referenced that leaves out
	// nothing: a reader seeing a bare Each would take it for every table.
	EachRequired(ctx context.Context, at statelog.Position, visit func(Reference) error) (bool, error)
}

// Reference is one row naming an object: the object as the row records it, and
// the row's owner, as a person reads it.
type Reference struct {
	Object objstore.Object
	// NamedBy is the declaration's owner columns, joined by "/" — a
	// project and a path.
	NamedBy string
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
// tables and walks only its Required ones. A domain whose every table is
// retired is accepted: its walk reads nothing and is complete, and nothing
// about it is wrong.
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
		// AFTER Validate, which has refused a standing that is neither.
		if t.Standing == objstore.Required {
			src.required = append(src.required, t)
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
// from collection; required is those declared objstore.Required, the only ones
// the audit walks.
type tableSource struct {
	estate   Estate
	tables   []objstore.ReferenceTable
	required []objstore.ReferenceTable
}

func (s *tableSource) Name() string { return s.estate.Name() }

func (s *tableSource) Barrier(ctx context.Context) (statelog.Position, error) {
	return s.estate.Barrier(ctx)
}

// Referenced reads every declared table of the domain — required and retired
// alike — in ONE read, so the tables are judged at one position and one
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

// eachPage is how many references one read of a walk reads: a page of the
// declaration's keyset ([objstore.ReferenceTable.ReferencesAfter]), held in
// memory between its read and its visits.
//
// FIVE HUNDRED, the size of the collector's own batch ([judgeBatch]): a page
// is a few hundred bytes a row, and a smaller one is more reads for no memory
// anybody needed back.
const eachPage = judgeBatch

// EachRequired walks every REQUIRED table of the domain a keyset page at a
// time, EACH PAGE IN A READ OF ITS OWN and visited after that read has ended —
// see [Source.EachRequired]. The walk is complete only where every read was:
// each is no earlier than at, so a record the node could not apply at any of
// them is seen. With no required table it reads nothing and is complete.
func (s *tableSource) EachRequired(ctx context.Context, at statelog.Position,
	visit func(Reference) error) (bool, error) {

	complete := true
	for _, t := range s.required {
		query, err := t.ReferencesAfter(eachPage)
		if err != nil {
			return false, err
		}
		for after := ""; ; {
			var page []Reference
			whole, err := s.estate.Read(ctx, at, func(tx *sql.Tx) error {
				rows, err := tx.QueryContext(ctx, query, after)
				if err != nil {
					return fmt.Errorf("read the objects %s names: %w", t.Table, err)
				}
				defer func() { _ = rows.Close() }()
				return t.ScanReferences(rows, func(obj objstore.Object, namedBy string) error {
					page = append(page, Reference{Object: obj, NamedBy: namedBy})
					return nil
				})
			})
			if err != nil {
				return false, err
			}
			complete = complete && whole
			for _, ref := range page {
				if err := visit(ref); err != nil {
					return false, err
				}
			}
			if len(page) < eachPage {
				break
			}
			after = page[len(page)-1].Object.Key.String()
		}
	}
	return complete, nil
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
