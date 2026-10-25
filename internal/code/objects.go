package code

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// PackReferences declares code_packs: every pack bundle a push stored is named
// by a row here. REQUIRED (ADR-0051): a repository's bytes are the company's,
// audited daily and carried by every backup — and declared in
// internal/objstore/references, or the collector would delete every pack a day
// after it was written.
var PackReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "code_packs",
	Key: "object", Hash: "hash", Size: "size",
	Owner:    []string{"repo_id", "pack_id", "part"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  packRecordReferences,
}

// RetiredPackReferences declares code_packs_retired: the packs a record
// retired, each keeping its object from the collector for [RetireGrace] until
// the code-retention duty's retire_sweep records remove its row — RETIRED
// (ADR-0051), the first table any domain declares so, and reviewed with its
// grace, its largest object and its sweep in internal/objstore/references.
//
// The collector asks it as it asks every declared table; the audit and the
// backup never read it, so it names no Log, Deferred or Records, which
// [objstore.ReferenceTable.ValidateLog] asks of a Required table alone. A
// restore whose store lacks a retired object loses nothing anybody can ask
// for: no cache downloads a retired pack — a cache installs the live rows
// alone — and the row itself goes at the next sweep.
var RetiredPackReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "code_packs_retired",
	Key: "object", Hash: "hash", Size: "size",
	Owner:    []string{"repo_id", "pack_id", "part"},
	Standing: objstore.Retired,
}

// packRecordReferences answers the code_packs rows one record of the code log
// writes, and the object each names once the record applies —
// [objstore.ReferenceTable.Records] for [PackReferences].
//
// A RECORD THIS BUILD READS is answered by the applier's own decoder: only a
// push writes the table, one row per entry carrying a pack. Every entry is
// answered whether or not this node's applier would skip it as already
// applied — a backup reads a superset of what a restore's rows name, never a
// subset. A purge REMOVES rows the record does not list (a part takes the
// oldest rows the applier finds), so it answers none: a backup that carries
// an object a purge later removed carries one more object than a restore
// names, which the collector deletes, and never one fewer.
//
// A RECORD THIS BUILD CANNOT READ — a newer version, or a kind it has never
// heard of — is answered from what EVERY build reads of it ([mayWritePacks]),
// because what this build's applier does with a kind says nothing about what
// a newer build's does: one that may write a pack row is
// [objstore.ErrRecordUnreadable].
//
// THE ROW IS SPELLED IN THE ORDER [PackReferences] LISTS ITS OWNER COLUMNS —
// repo_id, pack_id, part — through [objstore.JoinOwner], as the walk of the
// table spells it.
func packRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return nil, fmt.Errorf("code: a record no build reads: %w", err)
	}
	if env.V > RecordVersion || !env.Subject.Kind.Valid() {
		if !mayWritePacks(env) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: the record on %s is version %d of a kind this build "+
			"%s (it reads version %d), and the scope its writer stated reaches a repository",
			objstore.ErrRecordUnreadable, env.Subject, env.V, knownOrNot(env.Subject.Kind),
			RecordVersion)
	}
	if env.Subject.Kind != KindRepo || env.Op != OpPush {
		return nil, nil
	}
	rec, err := Decode(payload)
	if err != nil {
		return nil, err
	}
	payloadAny, err := DecodeMutation(rec)
	if err != nil {
		return nil, err
	}
	batch, ok := payloadAny.(PushBatch)
	if !ok {
		return nil, fmt.Errorf("code: the push on %s carries a %T", env.Subject, payloadAny)
	}
	var out []objstore.RecordReference
	for _, e := range batch.Pushes {
		if e.Pack == nil {
			continue
		}
		out = append(out, objstore.RecordReference{
			NamedBy: objstore.JoinOwner(batch.RepoID, e.Pack.PackID, "pack"),
			Object:  e.Pack.Content(), Names: true,
		})
	}
	return out, nil
}

// knownOrNot words whether this build knows a kind.
func knownOrNot(k ObjectKind) string {
	if k.Valid() {
		return "knows"
	}
	return "has never heard of"
}

// mayWritePacks reports whether a record this build cannot read may write a
// code_packs row, from what every build reads of it: its kind and the scope
// its WRITER stated as the complete set of objects its apply may write.
//
// AN UNKNOWN KIND MAY: this build cannot place what a kind it has never heard
// of names. A KNOWN KIND may when any path of its scope reaches a repository —
// the domain, a repository's own path, or a path this build cannot interpret,
// which is the domain's reading of it — and may not when every path is a
// name's claim, the activation fence or the framework's own.
func mayWritePacks(env RecordEnvelope) bool {
	if !env.Subject.Kind.Valid() {
		return true
	}
	names := pathDomain + statelog.ScopeSeparator + pathName + statelog.ScopeSeparator
	for _, p := range env.Scope {
		// EVERYTHING BUT THE THREE PATHS THAT REACH NO REPOSITORY may: the
		// domain, a repository's path, and a path this build cannot
		// interpret alike.
		if p == statelog.FrameworkScope || p == ActivationPath() || strings.HasPrefix(p, names) {
			continue
		}
		return true
	}
	return false
}

// ObjectEstate is the code log's rows as the object store's passes read them:
// the collector, its audit and the backup (collect.Estate).
type ObjectEstate struct {
	Reader *Reader
}

// Name is the domain, as a declaration spells it.
func (ObjectEstate) Name() string { return Domain{}.Name() }

// objectScope is the references' closure: the whole domain, because a pack can
// be in any repository and a record this node could not apply may name any of
// them — exactly the case the collector must see as incomplete.
var objectScope = statelog.ScopeSet{Paths: []string{DomainPath()}}

// Barrier waits until this node has applied everything the log had committed
// when it was called, and answers where that is.
func (s ObjectEstate) Barrier(ctx context.Context) (statelog.Position, error) {
	served, err := s.Reader.log.Read(ctx, statelog.Query{
		Level: statelog.ReadLinearizable, Scope: objectScope, Set: true,
	}, func(*sql.Tx) error { return nil })
	if err != nil {
		return statelog.Position{}, err
	}
	return served.Position, nil
}

// Read runs fn over this node's rows, no earlier than at, and reports whether
// they cover every record of the domain.
func (s ObjectEstate) Read(ctx context.Context, at statelog.Position, fn func(*sql.Tx) error) (bool, error) {
	served, err := s.Reader.log.Read(ctx, statelog.Query{
		Level: statelog.ReadStale, Scope: objectScope, Set: true, MinPosition: at,
	}, fn)
	if err != nil {
		return false, err
	}
	return served.Complete, nil
}
