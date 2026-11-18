package gitlet

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

// PackReferences declares code_packs: a row per pack bundle a push stored.
// REQUIRED (ADR-0081): a repository's bytes are the company's, audited daily
// and carried by every backup; undeclared in internal/objstore/references,
// the collector would delete every pack a day after it was written.
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
// the code-retention duty's retire_sweep removes its row. RETIRED (ADR-0081);
// its grace, largest object and sweep are reviewed in
// internal/objstore/references.
//
// Only the collector reads it, so it names no Log, Deferred or Records
// ([objstore.ReferenceTable.ValidateLog] asks those of a Required table). A
// restore lacking a retired object loses nothing: no cache downloads a retired
// pack (a cache installs the live rows alone), and the row goes at the next
// sweep.
var RetiredPackReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "code_packs_retired",
	Key: "object", Hash: "hash", Size: "size",
	Owner:    []string{"repo_id", "pack_id", "part"},
	Standing: objstore.Retired,
}

// LayerReferences declares code_layer_parts: the objects a published layer's
// bytes are stored in. DERIVED (ADR-0081): any Gitlet node can rebuild the
// layer from code_packs, so a lost part costs a publication, never data
// (`objects_missing` alarms on lost data): the audit never reads the table,
// and a backup carries none (on nats they ride the bucket's stream snapshot
// uncounted). A store move copies them, or every node of the moved fleet
// would rebuild them. A publication replacing a layer removes its rows with no
// grace: a node whose fetch of a part finds it gone reads the rows again.
// Hence no Log, Deferred or Records, which a backup reads of Required alone.
var LayerReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "code_layer_parts",
	Key: "object", Hash: "hash", Size: "size",
	Owner:    []string{"repo_id", "layer_id", "part"},
	Standing: objstore.Derived,
}

// CheckLogReferences declares code_check_logs: the log a failed check's run
// kept, one object a row names. REQUIRED (ADR-0081): nothing can build it
// again, so it is audited, backed up and copied by a store move; undeclared in
// internal/objstore/references, the collector would delete every log a day
// after it was written. A row goes with its check's (past CheckHistory, or the
// repository's purge), and the collector's next pass takes its object.
var CheckLogReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "code_check_logs",
	Key: "object", Hash: "hash", Size: "size",
	Owner:    []string{"repo_id", "commit_oid", "target_ref", "name", "attempt"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  checkLogRecordReferences,
}

// checkLogRecordReferences is [objstore.ReferenceTable.Records] for
// [CheckLogReferences]: the code_check_logs rows one record writes, keyed in
// Owner order by [objstore.JoinOwner] as the table's walk spells it. A record
// this build cannot read is [objstore.ErrRecordUnreadable] when
// [mayWriteCheckLogs] says it may write one.
//
// An ending that keeps a log answers its row whether or not the join keeps
// that ending: a backup reads a superset of what a restore names, never a
// subset. One that keeps none answers NOTHING, never a removal: had the join
// kept an earlier ending, the removal would lose its log to the backup. A
// horizon, a sweep and a purge remove rows their records do not list, and
// answer none.
func checkLogRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return nil, fmt.Errorf("gitlet: a record no build reads: %w", err)
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
		return nil, fmt.Errorf("gitlet: the %s on %s carries a %T", env.Op, env.Subject, payloadAny)
	}
	if f.Log == nil {
		return nil, nil
	}
	return []objstore.RecordReference{{
		NamedBy: objstore.JoinOwner(f.RepoID, f.OID, f.Target, f.Name, strconv.Itoa(f.Attempt)),
		Object:  f.Log.Content(), Names: true,
	}}, nil
}

// mayWriteCheckLogs reports whether a record this build cannot read may write
// a code_check_logs row, judged from its kind and its writer's scope: every
// object its apply may write and its decide read (scope.go). That superset is
// sound: a path only read can only turn the answer to "may", on which a backup
// refuses rather than guesses, and no path the apply writes is hidden.
//
// An unknown kind may. A known kind may not when every path is a name's claim,
// a repository's own, a change's, the activation fence or the framework's: a
// log row is written only under its repository's checks path, and any other
// path (the domain's, one this build cannot interpret) may. A record that only
// READ the checks (a mergeability, a merge, an enqueue) is refused with the
// rest: the over-refusal a superset allows.
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

// packRecordReferences is [objstore.ReferenceTable.Records] for
// [PackReferences]: the code_packs rows one record writes or removes (a
// removal names nothing once it applies), keyed in Owner order by
// [objstore.JoinOwner] as the table's walk spells it. A record this build
// cannot read is judged by [mayWritePacks], never by this build's applier,
// which says nothing of what a newer build's does.
//
// Every entry and output is answered whether or not this node's applier would
// skip it as applied: a backup reads a superset of what a restore names, never
// a subset. A purge removes rows its record does not list, so it answers none
// (a backup then carries an extra object the collector deletes, never one
// fewer); a sweep of retired packs writes no code_packs row.
func packRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return nil, fmt.Errorf("gitlet: a record no build reads: %w", err)
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
		// THE INPUTS' ROWS LEAVE code_packs, told as removals so a backup
		// reads the restore's rows as naming nothing there (code_packs_retired,
		// which no backup carries, keeps their objects for a grace), each by
		// `pack`, the one part every pack this build stores is. A
		// publication's layer goes in code_layer_parts, which no backup reads.
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
	case ImportChunk:
		// AN IMPORT'S AND A SYNC'S PACKS are live from their record on.
		out = append(out, written(p.RepoID, p.Packs)...)
	case MirrorSync:
		out = append(out, written(p.RepoID, p.Packs)...)
	case Merge:
		// A MERGE'S PACK holds the objects its build wrote; a fast-forward
		// builds none.
		if p.Pack != nil {
			out = append(out, written(p.RepoID, []PackRef{*p.Pack})...)
		}
	case Candidate:
		// A TRAIN'S PACK holds the commits its candidates built; a train of
		// fast-forwards builds none.
		if p.Pack != nil {
			out = append(out, written(p.RepoID, []PackRef{*p.Pack})...)
		}
	case Land:
		// NOTHING OF ITS OWN: a landing's commits are its train's, whose
		// candidate record wrote their pack.
	default:
		return nil, fmt.Errorf("gitlet: the %s on %s carries a %T", env.Op, env.Subject, payloadAny)
	}
	return out, nil
}

// packOps are the ops whose records write or remove code_packs rows they name;
// a purge removes rows its record does not list, so it answers none.
var packOps = []OpKind{OpPush, OpCompact, OpExpungeBegin, OpExpungeCommit, OpImportChunk, OpMirrorSync, OpMerge,
	OpCandidate, OpLand}

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
// code_packs row, judged as [mayWriteCheckLogs] judges a log row, its
// superset argument included. An unknown kind may: this build cannot place
// what it names. A known kind may not when every path is a name's claim, a
// repository's checks, the activation fence or the framework's: a pack row is
// written only under its repository's own path. A record that only READ the
// repository's row (a check's on every arm, a change's on its own subject) is
// refused with the rest, which refuses no check's record anew:
// [mayWriteCheckLogs] already refuses it by its checks path.
func mayWritePacks(env RecordEnvelope) bool {
	if !env.Subject.Kind.Valid() {
		return true
	}
	names := pathDomain + statelog.ScopeSeparator + pathName + statelog.ScopeSeparator
	checks := pathDomain + statelog.ScopeSeparator + pathChecks + statelog.ScopeSeparator
	for _, p := range env.Scope {
		// Any other path may, one this build cannot interpret included.
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
