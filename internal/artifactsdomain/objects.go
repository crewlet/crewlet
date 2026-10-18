package artifactsdomain

import (
	"context"
	"database/sql"
	"errors"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TWO TABLES WHOSE ROWS KEEP OBJECTS ALIVE, declared beside the schema that
// holds them and collected in internal/objstore/references, which the
// collector, its audit and the backup build their statements from. A table that
// names objects and is missing from that list is the one mistake that destroys
// data: its objects read as unreferenced and are collected a day after they
// were written.

// ObjectReferences is every object a retained version of an artifact names,
// each distinct object once per artifact with its reference count.
var ObjectReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "artifact_objects",
	Key: "object", Hash: "hash", Size: "size",
	Owner: []string{"artifact_id"},
}

// AssetReferences is every live asset's object. A removed asset's row names no
// object (NULL), which no statement built from a declaration counts.
var AssetReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "artifact_assets",
	Key: "object", Hash: "hash", Size: "size",
	Owner: []string{"artifact_id", "asset_id"},
}

// ObjectEstate is this domain as the object store's passes read it: a barrier on
// the artifacts log and a read of its rows (internal/objstore/collect's Estate).
type ObjectEstate struct {
	Reader *Reader
}

// Name is the domain, as the declarations spell it.
func (ObjectEstate) Name() string { return Domain{}.Name() }

// Barrier waits until this node has applied everything the log had committed
// when it was called, and answers where that is: a linearizable read of the
// whole domain, which appends the read index's barrier.
func (s ObjectEstate) Barrier(ctx context.Context) (statelog.Position, error) {
	if s.Reader == nil {
		return statelog.Position{}, errNoReader
	}
	served, err := s.Reader.log.Read(ctx, statelog.Query{
		Level: statelog.ReadLinearizable, Scope: DomainScope(), Set: true,
	}, func(*sql.Tx) error { return nil })
	if err != nil {
		return statelog.Position{}, err
	}
	return served.Position, nil
}

// Read runs fn over this node's rows, no earlier than at, and reports whether
// they cover every record of the domain: false while this node holds a record
// it could not apply, which the collector must read as incomplete.
func (s ObjectEstate) Read(ctx context.Context, at statelog.Position, fn func(*sql.Tx) error) (bool, error) {
	if s.Reader == nil {
		return false, errNoReader
	}
	served, err := s.Reader.log.Read(ctx, statelog.Query{
		Level: statelog.ReadStale, Scope: DomainScope(), Set: true, MinPosition: at,
	}, fn)
	if err != nil {
		return false, err
	}
	return served.Complete, nil
}

// errNoReader is an object estate built without the domain's reader.
var errNoReader = errors.New("artifactsdomain: the object estate has no reader")
