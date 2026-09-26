package upkeep

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Source is one domain whose rows refer to chunks, as the passes read it.
//
// The engine builds these with [Sources] from the declared tables and never
// by hand: a source written for one table is a second answer to which tables
// name chunks, and the day it and the declarations disagree is the day the
// backup carries a file's bytes that the collector has deleted.
type Source interface {
	// Name is the domain, for a log line.
	Name() string

	// Barrier answers a position at or past everything the domain's log
	// had committed when it was called, having waited for this node to
	// apply that far.
	Barrier(ctx context.Context) (statelog.Position, error)

	// Referenced answers every chunk the domain refers to in one
	// placement group, read no earlier than at (zero: whatever this node
	// holds), and whether the answer is COMPLETE — false while this node
	// holds a record it could not apply that might refer to a chunk.
	Referenced(ctx context.Context, pg int, at statelog.Position) (map[objstore.Hash]struct{}, bool, error)
}

// References is every source the engine runs.
type References []Source

// Estate is one state-log domain's rows on this node, as a pass reads them.
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
func Sources(tables []objstore.ReferenceTable, estates ...Estate) (References, error) {
	if len(tables) == 0 {
		return nil, errors.New("objstore/upkeep: no table is declared as naming chunks — " +
			"with none, every chunk reads as unreferenced")
	}
	byDomain := map[string]*tableSource{}
	var built []*tableSource
	for _, e := range estates {
		if e == nil {
			return nil, errors.New("objstore/upkeep: a nil estate")
		}
		if _, dup := byDomain[e.Name()]; dup {
			return nil, fmt.Errorf("objstore/upkeep: the %s estate is given twice", e.Name())
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
			return nil, fmt.Errorf("objstore/upkeep: %s names chunks and is written by "+
				"the %s log, which this node reads no estate of — its chunks would read "+
				"as unreferenced", t.Table, t.Domain)
		}
		src.tables = append(src.tables, t)
	}
	out := make(References, 0, len(built))
	for _, src := range built {
		if len(src.tables) == 0 {
			return nil, fmt.Errorf("objstore/upkeep: the %s estate is given and no "+
				"declared table is in it", src.estate.Name())
		}
		out = append(out, src)
	}
	return out, nil
}

// tableSource is one domain's declared tables, read through its estate.
type tableSource struct {
	estate Estate
	tables []objstore.ReferenceTable
}

func (s *tableSource) Name() string { return s.estate.Name() }

func (s *tableSource) Barrier(ctx context.Context) (statelog.Position, error) {
	return s.estate.Barrier(ctx)
}

// Referenced reads every declared table of the domain in ONE read, so the
// tables are judged at one position and one completeness.
func (s *tableSource) Referenced(ctx context.Context, pg int,
	at statelog.Position) (map[objstore.Hash]struct{}, bool, error) {

	out := map[objstore.Hash]struct{}{}
	complete, err := s.estate.Read(ctx, at, func(tx *sql.Tx) error {
		for _, t := range s.tables {
			query, err := t.ChunksIn()
			if err != nil {
				return err
			}
			if err := collect(ctx, tx, t, query, pg, out); err != nil {
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

// collect adds every chunk one table names in one group to out.
func collect(ctx context.Context, tx *sql.Tx, t objstore.ReferenceTable, query string,
	pg int, out map[objstore.Hash]struct{}) error {

	rows, err := tx.QueryContext(ctx, query, pg)
	if err != nil {
		return fmt.Errorf("read the chunks %s names in group %d: %w", t.Table, pg, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		h, err := objstore.ParseHash(raw)
		if err != nil {
			return fmt.Errorf("%s.%s in group %d: %w", t.Table, t.Column, pg, err)
		}
		out[h] = struct{}{}
	}
	return rows.Err()
}

// view is what one pass read the references at: a floor per source.
type view []statelog.Position

// pin takes a barrier on every source, so a collection pass reads an estate
// at least as new as the moment it began.
func (r References) pin(ctx context.Context) (view, error) {
	out := make(view, len(r))
	for i, s := range r {
		at, err := s.Barrier(ctx)
		if err != nil {
			return nil, fmt.Errorf("objstore/upkeep: bring %s's view current: %w", s.Name(), err)
		}
		out[i] = at
	}
	return out, nil
}

// referenced is the union of every source's chunks in one group, and whether
// every source answered completely. A nil view reads whatever each source
// holds.
func (r References) referenced(ctx context.Context, pg int, v view) (map[objstore.Hash]struct{}, bool, error) {
	all := map[objstore.Hash]struct{}{}
	complete := true
	for i, s := range r {
		var at statelog.Position
		if v != nil {
			at = v[i]
		}
		set, whole, err := s.Referenced(ctx, pg, at)
		if err != nil {
			return nil, false, fmt.Errorf("objstore/upkeep: read %s's references in group %d: %w",
				s.Name(), pg, err)
		}
		for h := range set {
			all[h] = struct{}{}
		}
		complete = complete && whole
	}
	return all, complete, nil
}
