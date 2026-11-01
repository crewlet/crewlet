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

// THE THREE TABLES WHOSE ROWS KEEP OBJECTS ALIVE, declared beside the schema that
// holds them and collected in internal/objstore/references, which the
// collector, its audit and the backup build their statements from. A table that
// names objects and is missing from that list is the one mistake that destroys
// data: its objects read as unreferenced and are collected a day after they
// were written.

// ObjectReferences is every object a retained version of an artifact names,
// each distinct object once per artifact with its reference count.
//
// REQUIRED (ADR-0074): a retained version is something the company holds, so
// its objects are audited and every backup carries them — and so the
// declaration names the log a restore replays and how one record of it names
// the table's rows (ADR-0075).
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

// objectRecordReferences answers the artifact_objects rows one record of the
// artifacts log writes, and the object each names once the record applies —
// [objstore.ReferenceTable.Records] for [ObjectReferences].
//
// A RECORD THIS BUILD READS is answered by the applier's own decoder: a create
// (on an artifact or a source subject) and a publish or a rollback write the
// table, one row per distinct object of the version's manifest. Every object
// is answered whether or not this node's applier would find the record
// already applied, or count the object as named before — a backup reads a
// superset of what a restore's rows name, never a subset. A purge, and the
// unlabelled versions a publish retires, REMOVE rows the record does not list,
// so they answer none: a backup carrying an object such a record later
// removed carries one more object than a restore names, which the collector
// deletes, and never one fewer.
//
// A RECORD THIS BUILD CANNOT READ is answered from what EVERY build reads of
// it ([mayWriteObjects]): one that may write either table is
// [objstore.ErrRecordUnreadable].
//
// THE ROW IS SPELLED AS [ObjectReferences] LISTS ITS OWNER — the artifact id —
// through [objstore.JoinOwner], as the walk of the table spells it.
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

// assetRecordReferences answers the artifact_assets row one record of the
// artifacts log writes — [objstore.ReferenceTable.Records] for
// [AssetReferences]: an asset's create names its object, and its removal
// leaves the row naming none. A purge of the artifact removes its asset rows,
// which the record does not list, so it answers none, for
// [objectRecordReferences]' reason.
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
// where it cannot write either referencing table, and
// [objstore.ErrRecordUnreadable] where it may.
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

// mayWriteObjects reports whether a record this build cannot read may write an
// artifact_objects, artifact_assets or artifact_brand_files row, from what
// every build reads of it: its kind and the scope its WRITER stated as the
// complete set of objects its apply may write.
//
// AN UNKNOWN KIND MAY: this build cannot place what a kind it has never heard
// of names. A KNOWN KIND may when any path of its scope reaches an artifact or
// the brand — the domain, an artifact's path or an asset's beneath it, the
// brand's, or a path this build cannot interpret — and may not when every path
// is a source address's hold, one person's own state or the framework's own:
// none of the three tables has a row under any of them.
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

// BrandFileReferences is every file an adoption of the company's brand serves:
// the generated stylesheet, each logo and each face, copied out of the kit's
// version at adoption so the adopted brand outlives the kit's retention, trash
// and purge. A row goes only when its adoption is no longer current and no
// retained version names its stylesheet.
//
// REQUIRED (ADR-0074), for [ObjectReferences]' reason: the adopted brand is
// the company's public face, and a superseded adoption's files are what a
// retained version that pinned its stylesheet still serves — files the
// company holds, so they are audited, every backup carries them, and the
// declaration names the log a restore replays and how one record of it names
// the table's rows (ADR-0075).
var BrandFileReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "artifact_brand_files",
	Key: "object", Hash: "hash", Size: "size",
	Owner:    []string{"css_sha", "slot"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  brandFileRecordReferences,
}

// brandFileRecordReferences answers the artifact_brand_files rows one record of
// the artifacts log writes — [objstore.ReferenceTable.Records] for
// [BrandFileReferences]: an adoption writes one row per file it serves, keyed
// by its stylesheet's hash and the file's slot, each naming the fresh copy the
// adoption uploaded. Every file is answered whether or not this node's applier
// would apply the adoption, for [objectRecordReferences]' reason: a backup
// reads a superset of what a restore's rows name, never a subset. A clear, and
// the prune every adoption and clear runs, REMOVE rows the record does not
// list, so they answer none, for the same reason.
//
// A RECORD THIS BUILD CANNOT READ is answered as [objectRecordReferences]
// answers one ([mayWriteObjects]).
//
// THE ROW IS SPELLED AS [BrandFileReferences] LISTS ITS OWNER — the
// stylesheet's hash, then the slot — through [objstore.JoinOwner].
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
		Level: statelog.ReadStale, Scope: DomainScope(), Set: true, Floors: []statelog.Position{at},
	}, fn)
	if err != nil {
		return false, err
	}
	return served.Complete, nil
}

// errNoReader is an object estate built without the domain's reader.
var errNoReader = errors.New("artifactsdomain: the object estate has no reader")
