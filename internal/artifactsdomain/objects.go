package artifactsdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE THREE TABLES WHOSE ROWS KEEP OBJECTS ALIVE, collected in
// internal/objstore/references, which the collector, its audit and the backup
// build their statements from. A table missing from that list destroys data:
// its objects read as unreferenced and are collected a day after they land.

// ObjectReferences is every object a retained version of an artifact names,
// each distinct object once per artifact with its reference count.
//
// REQUIRED (ADR-0081): a retained version is something the company holds, so
// its objects are audited and backed up, and the declaration names the log a
// restore replays and how a record names the table's rows (ADR-0082).
var ObjectReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "artifact_objects",
	Key: "object", Hash: "hash", Size: "size",
	Owner:    []string{"artifact_id"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  objectRecordReferences,
}

// AssetReferences is every live asset's object. A removed asset's row names no
// object (NULL), which no statement built from a declaration counts.
//
// REQUIRED for [ObjectReferences]' reason: a live asset is a file the company
// holds.
var AssetReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "artifact_assets",
	Key: "object", Hash: "hash", Size: "size",
	Owner:    []string{"artifact_id", "asset_id"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  assetRecordReferences,
}

// objectRecordReferences is [objstore.ReferenceTable.Records] for
// [ObjectReferences]: the rows one record writes (a create on an artifact or
// source subject, a publish or a rollback: one per distinct object of the
// version's manifest), owned by the artifact id through [objstore.JoinOwner]
// as the table's walk spells it. Every object is answered even where this
// node's applier would skip the record or already holds the object: a backup
// must read a superset of what a restore's rows name, never a subset. A purge,
// and the unlabelled versions a publish retires, remove rows the record does
// not list, so answer none: that costs one extra object the collector
// deletes, never one too few. A record this build cannot read is answered by
// [recordForReferences].
func objectRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	rec, readable, err := recordForReferences(payload)
	if err != nil || !readable {
		return nil, err
	}
	var (
		id    string
		files []objstore.Object
	)
	switch {
	case (rec.Subject.Kind == KindArtifact || rec.Subject.Kind == KindSource) && rec.Op == OpCreate:
		p, err := decodePayload[CreatePayload](rec)
		if err != nil {
			return nil, err
		}
		// THE ARTIFACT THE APPLY FILES IT UNDER: the payload's, and the
		// subject's where it names none, as applySource reads it.
		id = p.Artifact
		if id == "" {
			id = rec.Subject.ID
		}
		files = entriesOf(p.Version)
	case rec.Subject.Kind == KindArtifact && (rec.Op == OpPublish || rec.Op == OpRestoreVersion):
		p, err := decodePayload[PublishPayload](rec)
		if err != nil {
			return nil, err
		}
		id, files = rec.Subject.ID, entriesOf(p.Version)
	default:
		return nil, nil
	}
	seen := make(map[objstore.Key]bool, len(files))
	var out []objstore.RecordReference
	for _, f := range files {
		if seen[f.Key] {
			continue
		}
		seen[f.Key] = true
		out = append(out, objstore.RecordReference{
			NamedBy: objstore.JoinOwner(id), Object: f, Names: true,
		})
	}
	return out, nil
}

// assetRecordReferences is [objstore.ReferenceTable.Records] for
// [AssetReferences]: an asset's create names its object, its removal leaves
// the row naming none, and a purge answers none for [objectRecordReferences]'
// reason.
func assetRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	rec, readable, err := recordForReferences(payload)
	if err != nil || !readable || rec.Subject.Kind != KindAsset {
		return nil, err
	}
	artifact, asset, splitErr := SplitAssetID(rec.Subject.ID)
	if splitErr != nil {
		//nolint:nilerr // THE APPLY REFUSES an asset subject it cannot split,
		// writing no asset row, so the record names nothing here.
		return nil, nil
	}
	row := objstore.JoinOwner(artifact, asset)
	switch rec.Op {
	case OpCreate:
		p, err := decodePayload[AssetPayload](rec)
		if err != nil {
			return nil, err
		}
		return []objstore.RecordReference{{NamedBy: row, Object: p.Object, Names: true}}, nil
	case OpRemove:
		return []objstore.RecordReference{{NamedBy: row}}, nil
	}
	return nil, nil
}

// entriesOf is every file object a version's manifest names.
func entriesOf(v VersionPayload) []objstore.Object {
	out := make([]objstore.Object, 0, len(v.Manifest.Files))
	for _, f := range v.Manifest.Files {
		out = append(out, f.Object)
	}
	return out
}

// recordForReferences decodes one record for the references, and reports
// whether this build reads it. A record it cannot read is answered here: nil
// where it cannot write a referencing table ([mayWriteObjects]), else
// [objstore.ErrRecordUnreadable].
func recordForReferences(payload []byte) (MutationRecord, bool, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return MutationRecord{}, false, fmt.Errorf("artifactsdomain: a record no build reads: %w", err)
	}
	if env.V > RecordVersion || !env.Subject.Kind.Valid() || !env.Op.Valid() {
		if !mayWriteObjects(env) {
			return MutationRecord{}, false, nil
		}
		return MutationRecord{}, false, fmt.Errorf("%w: the record on %s is version %d "+
			"(this build reads %d) of a kind or op this build may not know, and the "+
			"scope its writer stated reaches an artifact", objstore.ErrRecordUnreadable,
			env.Subject, env.V, RecordVersion)
	}
	rec, err := Decode(payload)
	if err != nil {
		return MutationRecord{}, false, err
	}
	return rec, true, nil
}

// mayWriteObjects reports whether a record this build cannot read may write a
// row of any of the three tables, from what every build reads of it: its kind
// and the scope its WRITER stated as all its apply may write. An unknown kind
// may, since this build cannot place what it names. A known kind may not only
// when every scope path is a source address's hold, a person's own state or
// the framework's: none of the three tables has a row under those; any other
// path (the domain, an artifact, an asset, the brand, or one this build cannot
// interpret) may.
func mayWriteObjects(env RecordEnvelope) bool {
	if !env.Subject.Kind.Valid() {
		return true
	}
	source := pathDomain + statelog.ScopeSeparator + pathSource + statelog.ScopeSeparator
	reader := pathDomain + statelog.ScopeSeparator + pathReader + statelog.ScopeSeparator
	for _, p := range env.Scope.Resolve(env.Subject).Paths {
		if p == statelog.FrameworkScope || strings.HasPrefix(p, source) ||
			strings.HasPrefix(p, reader) {
			continue
		}
		return true
	}
	return false
}

// BrandFileReferences is every file an adoption of the company's brand serves
// (the stylesheet, each logo and face), copied out of the kit's version so the
// adopted brand outlives the kit's retention, trash and purge. A row goes only
// when its adoption is no longer current and no retained version names its
// stylesheet.
//
// REQUIRED (ADR-0081) for [ObjectReferences]' reason: a superseded adoption's
// files are still served by a retained version that pinned its stylesheet.
var BrandFileReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "artifact_brand_files",
	Key: "object", Hash: "hash", Size: "size",
	Owner:    []string{"css_sha", "slot"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  brandFileRecordReferences,
}

// brandFileRecordReferences is [objstore.ReferenceTable.Records] for
// [BrandFileReferences]: an adoption writes one row per file it serves, owned
// by the stylesheet's hash then the slot ([objstore.JoinOwner]), each naming
// the copy it uploaded. Every file is answered, and a clear or a prune answers
// none, for [objectRecordReferences]' reasons.
func brandFileRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	rec, readable, err := recordForReferences(payload)
	if err != nil || !readable || rec.Subject.Kind != KindBrand || rec.Op != OpAdopt {
		return nil, err
	}
	p, err := decodePayload[BrandAdoptPayload](rec)
	if err != nil {
		return nil, err
	}
	out := make([]objstore.RecordReference, 0, len(p.Files))
	for _, f := range p.Files {
		out = append(out, objstore.RecordReference{
			NamedBy: objstore.JoinOwner(p.CSSSha, f.Slot), Object: f.Object, Names: true,
		})
	}
	return out, nil
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
// whole domain.
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
		Level: statelog.ReadStale, Scope: DomainScope(), Set: true, Floors: []statelog.Position{at},
	}, fn)
	if err != nil {
		return false, err
	}
	return served.Complete, nil
}

// errNoReader is an object estate built without the domain's reader.
var errNoReader = errors.New("artifactsdomain: the object estate has no reader")
