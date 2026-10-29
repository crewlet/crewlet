package code

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
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

// LayerReferences declares code_layer_parts: the objects a published layer's
// bytes are stored in. DERIVED (ADR-0051): any git node can build the layer
// again from the packs code_packs names, so the collector keeps a part while a
// row names it, the audit never reads the table — a lost part costs a
// publication, never data, and `objects_missing` is the alarm for lost data —
// and a backup copies, counts or asks after none (on nats they ride the
// bucket's stream snapshot uncounted), while a store move copies them, since
// every node of a moved fleet would otherwise build them again. A publication
// that replaces a layer removes its rows at once, with no grace: a git node
// whose fetch of a part finds it gone reads the rows again for what replaced
// it. It declares no Log, Deferred or Records, which a backup reads of a
// Required table alone.
var LayerReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "code_layer_parts",
	Key: "object", Hash: "hash", Size: "size",
	Owner:    []string{"repo_id", "layer_id", "part"},
	Standing: objstore.Derived,
}

// CheckLogReferences declares code_check_logs: the log a failed check's run
// kept, one object a row names. REQUIRED (ADR-0051): the log is what a reader
// of a failed check is told to read, and nothing builds it again — the run is
// gone — so it is audited, carried by every backup and copied by a store
// move, and declared in internal/objstore/references, or the collector would
// delete every log a day after it was written. A row goes with its check's —
// past CheckHistory, or with its repository's purge — and the collector's
// next pass takes its object.
var CheckLogReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "code_check_logs",
	Key: "object", Hash: "hash", Size: "size",
	Owner:    []string{"repo_id", "commit_oid", "name", "attempt"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  checkLogRecordReferences,
}

// checkLogRecordReferences answers the code_check_logs rows one record of the
// code log writes, and the object each names once the record applies —
// [objstore.ReferenceTable.Records] for [CheckLogReferences].
//
// A RECORD THIS BUILD READS is answered by the applier's own decoder: an
// ending that keeps a log writes its attempt's row naming it — answered
// whether or not the join keeps that ending, since a backup reads a superset
// of what a restore's rows name, never a subset. An ending that keeps none
// answers NOTHING, never a removal, though the ending a join keeps replaces
// the row an earlier one named: a removal answered for a row the join leaves
// standing — the ending lost — would read the restore's row as naming nothing,
// and its object would be lost to the backup. A horizon, a sweep and a purge
// remove rows their records do not list, and answer none, as a pack's purge
// does.
//
// A RECORD THIS BUILD CANNOT READ — a newer version, or a kind it has never
// heard of — is answered from what EVERY build reads of it ([mayWriteCheckLogs]):
// one that may write a log row is [objstore.ErrRecordUnreadable].
//
// THE ROW IS SPELLED IN THE ORDER [CheckLogReferences] LISTS ITS OWNER COLUMNS
// — repo_id, commit_oid, name, attempt — through [objstore.JoinOwner], as the
// walk of the table spells it.
func checkLogRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return nil, fmt.Errorf("code: a record no build reads: %w", err)
	}
	if env.V > RecordVersion || !env.Subject.Kind.Valid() {
		if !mayWriteCheckLogs(env) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: the record on %s is version %d of a kind this build %s (it reads "+
			"version %d), and the scope its writer stated reaches a repository's checks",
			objstore.ErrRecordUnreadable, env.Subject, env.V, knownOrNot(env.Subject.Kind), RecordVersion)
	}
	if env.Subject.Kind != KindChecks || env.Op != OpCheckFinished {
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
	f, ok := payloadAny.(CheckFinished)
	if !ok {
		return nil, fmt.Errorf("code: the %s on %s carries a %T", env.Op, env.Subject, payloadAny)
	}
	if f.Log == nil {
		return nil, nil
	}
	return []objstore.RecordReference{{
		NamedBy: objstore.JoinOwner(f.RepoID, f.OID, f.Name, strconv.Itoa(f.Attempt)),
		Object:  f.Log.Content(), Names: true,
	}}, nil
}

// mayWriteCheckLogs reports whether a record this build cannot read may write
// a code_check_logs row, from what every build reads of it: its kind and the
// scope its writer stated as the complete set of objects its apply may write.
// AN UNKNOWN KIND MAY. A KNOWN KIND may when any path of its scope reaches a
// repository's checks — the domain, a checks path, or a path this build cannot
// interpret, which is the domain's reading of it — and may not when every
// path is a name's claim, a repository's own, a change's, the activation
// fence or the framework's: a log row is written only under its repository's
// checks path, which a record writing one names.
func mayWriteCheckLogs(env RecordEnvelope) bool {
	if !env.Subject.Kind.Valid() {
		return true
	}
	elsewhere := []string{
		pathDomain + statelog.ScopeSeparator + pathName + statelog.ScopeSeparator,
		pathDomain + statelog.ScopeSeparator + pathRepo + statelog.ScopeSeparator,
		pathDomain + statelog.ScopeSeparator + pathChange + statelog.ScopeSeparator,
	}
	for _, p := range env.Scope {
		if p == statelog.FrameworkScope || p == ActivationPath() ||
			slices.ContainsFunc(elsewhere, func(prefix string) bool { return strings.HasPrefix(p, prefix) }) {
			continue
		}
		return true
	}
	return false
}

// packRecordReferences answers the code_packs rows one record of the code log
// writes, and the object each names once the record applies —
// [objstore.ReferenceTable.Records] for [PackReferences].
//
// A RECORD THIS BUILD READS is answered by the applier's own decoder: a push
// writes the table, one row per entry carrying a pack, and a COMPACTION writes
// a row per output it adds and removes the rows of the packs it retires,
// which it answers as rows that name nothing once it applies (their objects
// are kept for a grace by code_packs_retired, which no backup carries) — a
// publication's repack among them; the layer a publication adds names its
// objects in code_layer_parts, a Derived table no backup reads. An EXPUNGE'S
// BEGIN writes a row per pack it keeps and per pack it leaves dropping — a
// dropping pack is a row of code_packs, naming its object as a live one does,
// until its commit retires it — and removes its inputs'; its COMMIT writes a
// row per rescue pack and removes its dropping packs'; a request writes none.
// Every entry and output is answered whether or not this node's applier would skip
// it as already applied — a backup reads a superset of what a restore's rows
// name, never a subset. A purge REMOVES rows the record does not list (a part
// takes the oldest rows the applier finds), so it answers none: a backup that
// carries an object a purge later removed carries one more object than a
// restore names, which the collector deletes, and never one fewer. A sweep of
// retired packs writes nothing of code_packs and answers none either.
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
	if env.Subject.Kind != KindRepo || !slices.Contains(packOps, env.Op) {
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
	var out []objstore.RecordReference
	switch p := payloadAny.(type) {
	case PushBatch:
		for _, e := range p.Pushes {
			if e.Pack == nil {
				continue
			}
			out = append(out, objstore.RecordReference{
				NamedBy: objstore.JoinOwner(p.RepoID, e.Pack.PackID, "pack"),
				Object:  e.Pack.Content(), Names: true,
			})
		}
	case Compaction:
		out = append(out, written(p.RepoID, p.Add)...)
		// THE INPUTS' ROWS LEAVE code_packs: told as removals, so a
		// backup's walk past its copy reads the restore's rows as naming
		// nothing there — each by `pack`, the one part every pack this
		// build stores is.
		out = append(out, removed(p.RepoID, p.Retire)...)
	case ExpungeBegin:
		// A DROPPING PACK NAMES ITS OBJECT as a live one does: it is a row
		// of code_packs, Required, until its commit retires it.
		out = append(out, written(p.RepoID, p.Add)...)
		out = append(out, written(p.RepoID, p.Dropping)...)
		out = append(out, removed(p.RepoID, p.Retire)...)
	case ExpungeCommit:
		out = append(out, written(p.RepoID, p.Add)...)
		out = append(out, removed(p.RepoID, p.Retire)...)
	default:
		return nil, fmt.Errorf("code: the %s on %s carries a %T", env.Op, env.Subject, payloadAny)
	}
	return out, nil
}

// packOps are the ops whose records write or remove rows of code_packs that
// a record names — a push's, a compaction's, an expunge's begin's and its
// commit's; a purge removes rows its record does not list, and answers none.
var packOps = []OpKind{OpPush, OpCompact, OpExpungeBegin, OpExpungeCommit}

// written is the rows of code_packs refs write, each naming its object.
func written(repoID string, refs []PackRef) []objstore.RecordReference {
	out := make([]objstore.RecordReference, len(refs))
	for i, r := range refs {
		out[i] = objstore.RecordReference{NamedBy: objstore.JoinOwner(repoID, r.PackID, "pack"),
			Object: r.Content(), Names: true}
	}
	return out
}

// removed is the rows of code_packs ids leave, each naming nothing once the
// record applies.
func removed(repoID string, ids []string) []objstore.RecordReference {
	out := make([]objstore.RecordReference, len(ids))
	for i, id := range ids {
		out[i] = objstore.RecordReference{NamedBy: objstore.JoinOwner(repoID, id, "pack")}
	}
	return out
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
// name's claim, a repository's checks, the activation fence or the
// framework's own: a pack row is written only under its repository's own
// path, which a record writing one names, and a check's record never names it.
func mayWritePacks(env RecordEnvelope) bool {
	if !env.Subject.Kind.Valid() {
		return true
	}
	names := pathDomain + statelog.ScopeSeparator + pathName + statelog.ScopeSeparator
	checks := pathDomain + statelog.ScopeSeparator + pathChecks + statelog.ScopeSeparator
	for _, p := range env.Scope {
		// EVERYTHING BUT THE FOUR PATHS THAT REACH NO REPOSITORY'S PACKS may:
		// the domain, a repository's path, and a path this build cannot
		// interpret alike.
		if p == statelog.FrameworkScope || p == ActivationPath() || strings.HasPrefix(p, names) ||
			strings.HasPrefix(p, checks) {
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
		Level: statelog.ReadStale, Scope: objectScope, Set: true, Floors: []statelog.Position{at},
	}, fn)
	if err != nil {
		return false, err
	}
	return served.Complete, nil
}
