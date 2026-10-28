package code

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Domain is the code host as the state-log framework sees it.
//
// A DECLARATION, not a behaviour: every method answers a question the
// framework asks before it applies anything, and each has a failure mode that
// is silent if the answer is wrong — what a donated snapshot scrubs, what an
// identity claim compares, where an anchor is written, whether an unknown
// version stops the applier or is filed for later.
//
// THE FRAMEWORK'S EIGHTH DOMAIN, its sixth strict one and its sixth that claims
// identity.
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
		// SIX OF THE EIGHT KINDS — see [ObjectKind.Arbitrated].
		ArbitratedKinds: arbitratedKinds(),
	}
}

// arbitratedKinds is the six, derived from the enum rather than typed again:
// a list written twice is a kind that arbitrates in one place and not the
// other, which wedges that subject the first time a gate drops a record.
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
// their kind, a purge by its op ([RecordEnvelope.InstallsGate]), answered from
// the envelope alone because that is all a node that cannot decode the
// payload has.
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
		return false, fmt.Errorf("code: decode a record from an eviction subject: %w", err)
	}
	mutation, err := DecodeMutation(record)
	if err != nil {
		return false, fmt.Errorf("code: decode the eviction: %w", err)
	}
	eviction, ok := mutation.(Eviction)
	if !ok {
		return false, fmt.Errorf("code: the record on an eviction subject carries a %T", mutation)
	}
	return !eviction.Readmitted, nil
}

// Tables is every durable table this domain writes, with its class — the
// scrub list, the identity claim and the local sweep are all derived from it,
// so it is built from the exported inventories rather than typed a third time.
//
// ONE DIVERGENT TABLE, the operation ledger: it travels with the rows it
// describes ([statelog.Domain.OpsTable]). The knowledge base has a second
// (its skill flag, a build's parser's answer); nothing this domain writes is a
// build's opinion.
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
	return out
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "code_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope path.
func (Domain) ScopeIndex() string { return "code_log_deferred_scope" }

// OpsTable is what this node has already applied.
func (Domain) OpsTable() string { return "code_ops" }

// GatesSeatAdmission reports that NO seat depends on this domain's copy.
//
// FALSE. A wrong or stalled code copy takes only CODE operations out of
// service: they go to peers — the router asks each operation's own domain's
// verdict (ADR-0049) — and git requests are answered from them. A seat whose
// node's code copy stalls still runs, because nothing a seat does reads code
// until seats push, and then a seat's code tools route to peers like any other
// operation. Gating admission on it would shed a company's seats for a fault
// in a subsystem none of them is using.
func (Domain) GatesSeatAdmission() bool { return false }

// ClaimsIdentity reports that every node's Replicated tables must be
// byte-identical: N copies of one SQL state, derived from one ordered log by
// one deterministic applier. Every column this domain writes is owned by a
// record, and every instant it stores is the broker's.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is empty: no wake is derived from a code record. A declared group
// with no consumer would hold the trim's feed term at nothing for ever, so the
// term is ABSENT rather than zero until a feed exists.
func (Domain) FeedGroup() string { return "" }

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
		return nil, fmt.Errorf("code: read the evictions on this log: %w", err)
	}
	return out, nil
}

// evictionsSQL is every eviction on this log, in node order: the whole of
// code_evictions, which holds a row for each node ever evicted from the code
// log — as many as the fleet has evicted, whatever its repositories hold —
// so it is the one table a statement here reads by a scan (plan_test.go
// registers it, and the read gate's scanOK states the bound).
const evictionsSQL = `SELECT node_id, at, by, from_position, readmitted_position
	FROM code_evictions ORDER BY node_id`
