package artifactsdomain

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Domain is the artifacts log as the state-log framework sees it.
//
// A DECLARATION, not a behaviour: every method answers a question the framework
// asks before it applies anything, and each has a failure mode that is silent if
// the answer is wrong. What tables exist decides what a donated snapshot scrubs
// and what an identity claim compares; which kinds arbitrate decides where an
// anchor row is written; whether a record installs a gate decides whether an
// unknown version stops the applier or is filed for later.
type Domain struct{}

// Name is the register key, the manifest key and the operator's column. NEVER
// renamed.
func (Domain) Name() string { return "artifacts" }

// ArtifactsLogMaxBytes is the ceiling this domain declares for its log, which
// is what the framework's own suites provision the stream with.
//
// A NODE DOES NOT CREATE THE STREAM AT IT: every state log's ceiling is a
// reservation the broker grants in full, so the engine sizes them together from
// Tier A (`stream.artifacts_log_max_bytes`, unset deriving a quarter of the
// tracker log's) inside what the broker can grant. FOUR GIBIBYTES is that
// quarter of the tracker's own declared default.
const ArtifactsLogMaxBytes = 4 << 30

// ArtifactsMaxRecordBytes is the largest record this domain publishes.
//
// TWO MEBIBYTES: the worst version a publish can name is
// [artifacts.MaxFilesPerVersion] entries, each a path of up to
// [artifacts.MaxPathBytes], a served type, a digest, a key, a size and a script
// policy, about 0.85 MiB of JSON, plus [artifacts.MaxSearchTextBytes] of search
// text, then doubled so the base format leaves room for additive fields. The
// gate reserve on this log is sized by it, so a declaration larger than the
// records would hold back room no append could use.
const ArtifactsMaxRecordBytes = 2 << 20

// ArtifactsLogDuplicates is the window the broker collapses a repeated
// operation id in: AN OPTIMISATION, NEVER A MECHANISM (the ledger is what makes
// a retry idempotent). Two minutes, the tracker's and the pages' window,
// because it must outlast a publisher's retry budget and no longer.
const ArtifactsLogDuplicates = 2 * time.Minute

// Stream is the artifacts log, at the domain's whole budget.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:            topics.ArtifactsLogStream,
		Subjects:        []string{topics.ArtifactsLogWildcard},
		SubjectPrefix:   topics.ArtifactsLogPrefix,
		MaxBytes:        ArtifactsLogMaxBytes,
		MaxRecordBytes:  ArtifactsMaxRecordBytes,
		Duplicates:      ArtifactsLogDuplicates,
		Replay:          statelog.ReplayStrict,
		ArbitratedKinds: arbitratedKinds(),
	}
}

// arbitratedKinds is derived from the enum rather than typed again: a list
// written twice is a kind that arbitrates in one place and not the other.
func arbitratedKinds() []string {
	out := make([]string, 0, len(ObjectKinds))
	for _, k := range ObjectKinds {
		if k.Arbitrated() {
			out = append(out, string(k))
		}
	}
	return out
}

// RecordVersion is the record shape this build reads.
func (Domain) RecordVersion() int { return RecordVersion }

// Envelope decodes the half every build can read.
func (Domain) Envelope(payload []byte) (statelog.Envelope, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return statelog.Envelope{}, err
	}
	return frameworkEnvelope(env), nil
}

// frameworkEnvelope is the domain's envelope in the framework's terms: the one
// conversion the domain declaration, the applier's tests and the replay
// harnesses share.
func frameworkEnvelope(env RecordEnvelope) statelog.Envelope {
	return statelog.Envelope{
		V:       env.V,
		Kind:    string(env.Subject.Kind),
		Subject: env.Subject.framework(),
		Op:      string(env.Op),
		OpID:    env.OpID,
		Gen:     env.Gen,
		Scope:   env.Scope.Resolve(env.Subject),
		Writer:  env.Writer,
	}
}

// InstallsGate reports a record whose unknown version must STOP the applier: an
// eviction by its kind and a purge by its op. A deferred purge is a node that
// goes on serving an artifact every other node has destroyed.
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind).InstallsGate() || OpKind(env.Op) == OpPurge
}

// NodeGate reports a node's eviction or readmission, BY ITS KIND, and
// deliberately not a purge: the purge is an ordinary write the gate reserve and
// the fences a node gate is excused must hold.
func (Domain) NodeGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind) == KindEviction
}

// EvictionSubject is where a node's evictions and readmissions are published
// on this log, for a node the fleet re-anchored past that never applies an
// eviction written after its applier stopped ([statelog.EvictionProbe]).
func (Domain) EvictionSubject(node string) statelog.Subject {
	return EvictionSubject(node).framework()
}

// Evicts decodes one record from a node's eviction subject: true for an
// eviction, false for the readmission that inverts one.
func (Domain) Evicts(payload []byte) (bool, error) {
	record, err := Decode(payload)
	if err != nil {
		return false, fmt.Errorf("artifactsdomain: decode a record from an "+
			"eviction subject: %w", err)
	}
	mutation, err := DecodeMutation(record)
	if err != nil {
		return false, fmt.Errorf("artifactsdomain: decode the eviction: %w", err)
	}
	eviction, ok := mutation.(Eviction)
	if !ok {
		return false, fmt.Errorf("artifactsdomain: the record on an eviction "+
			"subject carries a %T", mutation)
	}
	return !eviction.Readmitted, nil
}

// Evictions is every eviction this log's applied rows hold: the rows the trim
// reads to stop counting an evicted node on THIS log once its fence window has
// passed. THIS DOMAIN'S OWN TABLE, because an eviction is a record on this log.
func (Domain) Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM artifacts_evictions ORDER BY node_id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		out = nil
		for rows.Next() {
			var (
				e          statelog.EvictionRow
				at, from   int64
				readmitted sql.NullInt64
			)
			if err := rows.Scan(&e.NodeID, &at, &e.By, &from, &readmitted); err != nil {
				return err
			}
			e.At = store.DecodeTime(at)
			e.From = uint64(from)
			e.Readmitted = uint64(readmitted.Int64)
			// THE COMPARISON [Fence.Evicted] MAKES, so the trim and the
			// node's own fence never disagree about whether it is back.
			e.Back = readmitted.Valid && readmitted.Int64 > from
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read the evictions on this log: %w", err)
	}
	return out, nil
}

// Tables is every durable table this domain writes, with its class.
//
// THE SCRUB LIST, THE IDENTITY CLAIM AND THE LOCAL SWEEP ARE ALL DERIVED FROM
// THIS MAP, built from the inventories rather than typed a third time. Every
// reproducible table is Replicated: each is written only by applying a record,
// from the record and same-log rows alone, with broker instants, so two nodes at
// one checkpoint hold the same bytes. The ledger is Divergent: it travels so an
// adopter can answer a retry of anything its donor applied, and its
// `applied_at` is each data node's own.
func (Domain) Tables() map[string]statelog.TableClass {
	out := make(map[string]statelog.TableClass,
		len(ReproducibleTables)+len(MachineryTables))
	for _, table := range ReproducibleTables {
		out[table] = statelog.Replicated
	}
	for _, table := range MachineryTables {
		out[table] = statelog.Local
	}
	out[Domain{}.OpsTable()] = statelog.Divergent
	return out
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "artifacts_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope path.
func (Domain) ScopeIndex() string { return "artifacts_log_deferred_scope" }

// OpsTable is the operation ledger: what this node has already applied.
func (Domain) OpsTable() string { return "artifacts_ops" }

// GatesSeatAdmission reports that this domain's health does NOT gate seat
// admission.
//
// FALSE, unlike the tracker's and the pages': a seat reads no artifact on its
// turn path, so a stalled artifacts copy must not stop a node admitting seats.
// The copy is still judged, as every domain's is (ADR-0072), and goes out of
// service for its own readers (a stalled strict replay is a fault, and the
// framework refuses every read of it), and the estate router and the serving
// origin decide through a peer that serves.
func (Domain) GatesSeatAdmission() bool { return false }

// ClaimsIdentity reports that every data node's Replicated tables must be
// byte-identical: N copies of one SQL state from one ordered log.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is EMPTY: no record on this log wakes anybody in this release.
// Comments and watches, which would, arrive with a later build, and empty is
// the answer that makes the trim's feed term absent rather than a consumer
// nobody opens.
func (Domain) FeedGroup() string { return "" }

// BarrierTables is the empty set, DECLARED: the barrier writes no row on any
// node, and stating that is what keeps "this kind wrote nothing" apart from
// "nobody classified this kind".
var BarrierTables = map[string]statelog.TableClass{}
