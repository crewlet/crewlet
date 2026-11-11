package atlasdomain

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Domain is the atlas log as the state-log framework sees it: the THIRTEENTH
// domain and the ELEVENTH strict, identity-claiming one.
type Domain struct{}

// Name is the register key, the manifest key and the operator's column. Never
// renamed.
func (Domain) Name() string { return "atlas" }

// AtlasLogMaxBytes is the ceiling this domain declares for its log, which the
// framework's own suites provision the stream with; a node sizes it from Tier A.
// At the reference company the log takes well under a mebibyte a day (a
// person's claim is under a kilobyte), so 128 MiB is months of a trim blocked
// outright, the access log's horizon.
const AtlasLogMaxBytes = 128 << 20

// MaxRecordBytes is the largest record this domain publishes. A person's claim
// is a few hundred bytes; 256 KiB is the access log's bound, kept so a later
// record kind (an integration's read of a page of changes) fits the same
// reserve. The gate reserve this sizes is about 2.7 MiB, under a sixteenth of
// the 64 MiB floor.
const MaxRecordBytes = 256 << 10

// AtlasLogDuplicates is the broker's duplicate window: every strict log's two
// minutes, which outlasts a publisher's retry budget.
const AtlasLogDuplicates = 2 * time.Minute

// LinearizableReadsPerSeatDay is this log's read model: none. A person's edit
// decides from the entry's version in its own snapshot, where the broker
// arbitrates what a stale copy decided, and every read of the atlas is at its
// caller's own level, `session` by default ([DefaultRead]), so no reader takes
// a barrier.
const LinearizableReadsPerSeatDay = 0

// Stream is the atlas log, at the domain's whole budget.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:            topics.AtlasLogStream,
		Subjects:        []string{topics.AtlasLogWildcard},
		SubjectPrefix:   topics.AtlasLogPrefix,
		MaxBytes:        AtlasLogMaxBytes,
		MaxRecordBytes:  MaxRecordBytes,
		Duplicates:      AtlasLogDuplicates,
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
		V: env.V, Kind: string(env.Subject.Kind), Subject: env.Subject.framework(),
		Op: string(env.Op), OpID: env.OpID, Gen: env.Gen,
		Scope: env.Scope.Resolve(env.Subject), Writer: env.Writer,
	}
}

// InstallsGate reports a record whose unknown version must stop the applier:
// a node's eviction by its kind, and a purge by its op. Every other record
// writes inside the scope it declares.
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind).InstallsGate() || OpKind(env.Op) == OpPurge
}

// NodeGate reports a node's eviction or readmission, by its kind, and never a
// purge: a purge is a marker on an object, published like any other write.
func (Domain) NodeGate(env statelog.Envelope) bool { return ObjectKind(env.Kind) == KindEviction }

// EvictionSubject is where a node's evictions are published on this log.
func (Domain) EvictionSubject(node string) statelog.Subject {
	return EvictionSubject(node).framework()
}

// Evicts decodes one record from a node's eviction subject: true for an
// eviction, false for the readmission that inverts one.
func (Domain) Evicts(payload []byte) (bool, error) {
	record, err := Decode(payload)
	if err != nil {
		return false, fmt.Errorf("atlasdomain: decode a record from an eviction subject: %w", err)
	}
	mutation, err := DecodeMutation(record)
	if err != nil {
		return false, fmt.Errorf("atlasdomain: decode the eviction: %w", err)
	}
	eviction, ok := mutation.(Eviction)
	if !ok {
		return false, fmt.Errorf("atlasdomain: the record on an eviction subject carries a %T", mutation)
	}
	return !eviction.Readmitted, nil
}

// Evictions is every eviction this log's applied rows hold, which the trim
// reads to stop counting an evicted node here.
func (Domain) Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM atlas_evictions ORDER BY node_id`)
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
			// The comparison [Fence.Evicted] makes, so the two never disagree.
			e.Back = readmitted.Valid && readmitted.Int64 > from
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("atlasdomain: read the evictions on this log: %w", err)
	}
	return out, nil
}

// Tables is every durable table this domain writes, with its class: the
// scrub list, the identity claim and the local sweep all derive from it.
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
func (Domain) DeferredTable() string { return "atlas_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope path.
func (Domain) ScopeIndex() string { return "atlas_log_deferred_scope" }

// OpsTable is the operation ledger: a person's write answers the caller.
func (Domain) OpsTable() string { return "atlas_ops" }

// GatesSeatAdmission reports that no seat depends on this copy: no seat's turn
// path reads the atlas, and a lagging copy must not withhold every seat claim
// on a node to protect one read. A read that is behind says so.
func (Domain) GatesSeatAdmission() bool { return false }

// ClaimsIdentity reports that every node's Replicated tables are byte-identical.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is empty: no record on this log wakes a seat.
func (Domain) FeedGroup() string { return "" }
