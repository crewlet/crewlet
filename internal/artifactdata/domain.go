package artifactdata

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Domain is the datastore log as the state-log framework sees it: a
// declaration, every answer of which has a failure mode that is silent if it is
// wrong.
type Domain struct{}

// Name is the register key, the manifest key and the operator's column. NEVER
// renamed.
func (Domain) Name() string { return "artifactdata" }

// ArtifactDataLogMaxBytes is the ceiling this domain declares for its log,
// which is what the framework's own suites provision the stream with.
//
// A NODE DOES NOT CREATE THE STREAM AT IT: the engine sizes every log together
// from Tier A (`stream.artifact_data_log_max_bytes`, unset deriving a quarter
// of the tracker log's). FOUR GIBIBYTES is that quarter of the tracker's own
// declared default, the artifacts log's figure: the state logs share one pool,
// and a larger derived share would create the vector changelog below the
// corpus a model change republishes.
const ArtifactDataLogMaxBytes = 4 << 30

// ArtifactDataMaxRecordBytes is the largest record this domain publishes.
//
// ONE MEBIBYTE PLUS 64 KiB: a batch's ops are bounded at
// [artifacts.MaxBatchBytes], and the envelope, the stamped authority and the
// framework's signature fit in the rest. The gate reserve on this log is sized
// by it.
const ArtifactDataMaxRecordBytes = artifacts.MaxBatchBytes + 64<<10

// ArtifactDataLogDuplicates is the window the broker collapses a repeated
// operation id in: an optimisation, never the mechanism (the ledger is). Two
// minutes, every strict log's window.
const ArtifactDataLogDuplicates = 2 * time.Minute

// Stream is the datastore log, at the domain's whole budget.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:            topics.ArtifactDataLogStream,
		Subjects:        []string{topics.ArtifactDataLogWildcard},
		SubjectPrefix:   topics.ArtifactDataLogPrefix,
		MaxBytes:        ArtifactDataLogMaxBytes,
		MaxRecordBytes:  ArtifactDataMaxRecordBytes,
		Duplicates:      ArtifactDataLogDuplicates,
		Replay:          statelog.ReplayStrict,
		ArbitratedKinds: arbitratedKinds(),
	}
}

// arbitratedKinds is derived from the enum rather than typed again.
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

// frameworkEnvelope is the domain's envelope in the framework's terms.
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
// eviction by its kind and a purge by its op.
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind).InstallsGate() || OpKind(env.Op) == OpPurge
}

// NodeGate reports a node's eviction or readmission, by its kind, and never a
// purge.
func (Domain) NodeGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind) == KindEviction
}

// EvictionSubject is where a node's evictions and readmissions are published
// on this log.
func (Domain) EvictionSubject(node string) statelog.Subject {
	return EvictionSubject(node).framework()
}

// Evicts decodes one record from a node's eviction subject: true for an
// eviction, false for the readmission that inverts one.
func (Domain) Evicts(payload []byte) (bool, error) {
	record, err := Decode(payload)
	if err != nil {
		return false, fmt.Errorf("artifactdata: decode a record from an eviction subject: %w", err)
	}
	mutation, err := DecodeMutation(record)
	if err != nil {
		return false, fmt.Errorf("artifactdata: decode the eviction: %w", err)
	}
	eviction, ok := mutation.(Eviction)
	if !ok {
		return false, fmt.Errorf("artifactdata: the record on an eviction subject carries a %T",
			mutation)
	}
	return !eviction.Readmitted, nil
}

// Evictions is every eviction this log's applied rows hold, which the trim
// reads to stop counting an evicted node on THIS log.
func (Domain) Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM artifactdata_evictions ORDER BY node_id`)
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
			e.Back = readmitted.Valid && readmitted.Int64 > from
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("artifactdata: read the evictions on this log: %w", err)
	}
	return out, nil
}

// Tables is every durable table this domain writes, with its class. Every
// reproducible table is Replicated: written only by applying a record, from the
// record and same-log rows alone, with broker instants. The ledger is
// Divergent.
func (Domain) Tables() map[string]statelog.TableClass {
	out := make(map[string]statelog.TableClass, len(ReproducibleTables)+len(MachineryTables))
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
func (Domain) DeferredTable() string { return "artifactdata_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope path.
func (Domain) ScopeIndex() string { return "artifactdata_log_deferred_scope" }

// OpsTable is the operation ledger.
func (Domain) OpsTable() string { return "artifactdata_ops" }

// GatesSeatAdmission reports that this domain's health does NOT gate seat
// admission: a seat reads no datastore on its turn path, so a stalled copy
// must not stop a node admitting seats. The copy is still judged, as every
// domain's is (ADR-0072), and goes out of service for its own readers, and the
// estate router decides through a peer that serves.
func (Domain) GatesSeatAdmission() bool { return false }

// ClaimsIdentity reports that every data node's Replicated tables must be
// byte-identical.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is EMPTY: no record on this log wakes anybody in this release.
// Datastore watches, which would, arrive with a later build.
func (Domain) FeedGroup() string { return "" }
