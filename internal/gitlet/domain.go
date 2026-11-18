package gitlet

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Domain is Gitlet as the state-log framework sees it (ADR-0084): a
// declaration, not a behaviour. Each method answers a question the framework
// asks before it applies anything, and a wrong answer fails silently — what a
// donated snapshot scrubs, what an identity claim compares, where an anchor is
// written, whether an unknown version stops the applier or is filed for later.
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column —
// durable in all three, so never renamed.
func (Domain) Name() string { return "code" }

// Stream is the code log, at the domain's whole budget. STRICT: a hole is a
// lost push.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:           topics.CodeLogStream,
		Subjects:       []string{topics.CodeLogWildcard},
		SubjectPrefix:  topics.CodeLogPrefix,
		MaxBytes:       CodeLogMaxBytes,
		MaxRecordBytes: CodeLogMaxRecordBytes,
		Duplicates:     CodeLogDuplicates,
		Replay:         statelog.ReplayStrict,
		// SEVEN OF THE TEN KINDS — see [ObjectKind.Arbitrated].
		ArbitratedKinds: arbitratedKinds(),
	}
}

// arbitratedKinds is derived from the enum, never typed again: a second list
// could arbitrate a kind in one place and not the other, wedging that subject
// the first time a gate drops a record.
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
	return statelog.Envelope{
		V:       env.V,
		Kind:    string(env.Subject.Kind),
		Subject: env.Subject.statelog(),
		Op:      string(env.Op),
		OpID:    env.OpID,
		Gen:     env.Gen,
		Scope:   env.Scope.Set(),
		Writer:  env.Writer,
	}, nil
}

// InstallsGate reports a record whose unknown version must STOP the applier
// rather than be filed for later — an eviction and the activation fence by
// their kind, a purge by its op ([RecordEnvelope.InstallsGate]) — from the
// envelope alone, all a node that cannot decode the payload has.
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind).InstallsGate() || OpKind(env.Op) == OpPurge
}

// NodeGate reports a node's eviction or readmission — the one record a write
// flagged [statelog.Request.NodeGate] may carry. Narrower than InstallsGate:
// the purge and the activation fence are gates and ordinary writes, held to
// the fences and the reserve a node gate is excused.
func (Domain) NodeGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind) == KindEviction
}

// EvictionSubject is where a node's evictions and readmissions are published
// on this log — the [statelog.EvictionProbe] half: a node the fleet re-anchored
// past never applies an eviction written after its applier stopped.
func (Domain) EvictionSubject(node string) statelog.Subject {
	return EvictionSubject(node).statelog()
}

// Evicts decodes one record from a node's eviction subject: true for an
// eviction, false for the readmission that inverts one.
func (Domain) Evicts(payload []byte) (bool, error) {
	record, err := Decode(payload)
	if err != nil {
		return false, fmt.Errorf("gitlet: decode a record from an eviction subject: %w", err)
	}
	mutation, err := DecodeMutation(record)
	if err != nil {
		return false, fmt.Errorf("gitlet: decode the eviction: %w", err)
	}
	eviction, ok := mutation.(Eviction)
	if !ok {
		return false, fmt.Errorf("gitlet: the record on an eviction subject carries a %T", mutation)
	}
	return !eviction.Readmitted, nil
}

// Tables is every durable table this domain writes, with its class. The scrub
// list, the identity claim and the local sweep all derive from it, so it is
// built from the exported inventories rather than typed again.
//
// TWO DIVERGENT TABLES: the operation ledger travels with the rows it
// describes ([statelog.Domain.OpsTable]), and a change record's notices
// (code_notifications, apply_notices.go) travel in a snapshot but sit outside
// the identity claim — whether a record writes one depends on the batch's
// instant and its epoch's inbox horizon, which two nodes applying it need not
// share (as with the tracker's tracker_notifications).
func (Domain) Tables() map[string]statelog.TableClass {
	out := make(map[string]statelog.TableClass, len(ReproducibleTables)+len(MachineryTables))
	for _, table := range ReproducibleTables {
		out[table] = statelog.Replicated
	}
	for _, table := range MachineryTables {
		out[table] = statelog.Local
	}
	// THE LEDGER TRAVELS — see [statelog.Domain.OpsTable] and
	// [MachineryTables].
	out[Domain{}.OpsTable()] = statelog.Divergent
	out[NoticesTable] = statelog.Divergent
	return out
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "code_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope path.
func (Domain) ScopeIndex() string { return "code_log_deferred_scope" }

// OpsTable is what this node has already applied.
func (Domain) OpsTable() string { return "code_ops" }

// GatesSeatAdmission reports that NO seat depends on this domain's copy: a
// wrong or stalled code copy takes only code operations out of service, and
// those, git requests and a seat's code tools alike, go to peers (the router
// asks each operation's own domain, ADR-0079). Gating admission on it would
// shed a company's seats for a fault in a subsystem none of them is using.
func (Domain) GatesSeatAdmission() bool { return false }

// ClaimsIdentity reports that every node's Replicated tables must be
// byte-identical: N copies of one SQL state, derived from one ordered log by
// one deterministic applier. Every column this domain writes is owned by a
// record, and every instant it stores is the broker's.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is the code log's wake feed ([FeedGroup]): a change's record wakes
// the seats its routing tells (feed.go), so the log's trim waits on the feed's
// acknowledgement (an unacknowledged record has woken no seat yet), and a
// stopped feed holds back this log and no other.
func (Domain) FeedGroup() string { return FeedGroup }

// Evictions is every eviction this log's applied rows hold — what the trim
// reads to stop counting an evicted node on THIS log once its fence window has
// passed. Through the handle, which answers [store.ErrNoEstate] for an estate
// an adoption has closed rather than panicking.
func (Domain) Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, evictionsSQL)
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
			if scanErr := rows.Scan(&e.NodeID, &at, &e.By, &from, &readmitted); scanErr != nil {
				return scanErr
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
		return nil, fmt.Errorf("gitlet: read the evictions on this log: %w", err)
	}
	return out, nil
}

// evictionsSQL is every eviction on this log, in node order: the whole of
// code_evictions, one row per node ever evicted, bounded by the fleet rather
// than the repositories — so the one table read here by a scan (plan_test.go
// registers it, and the read gate's scanOK states the bound).
const evictionsSQL = `SELECT node_id, at, by, from_position, readmitted_position
	FROM code_evictions ORDER BY node_id`
