package assistantdomain

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Domain is the assistant log as the state-log framework sees it.
//
// A DECLARATION, not a behaviour: every method answers a question the framework
// asks before it applies anything, and each has a failure mode that is silent if
// the answer is wrong.
type Domain struct{}

// Name is the register key, the manifest key and the operator's column. NEVER
// renamed.
func (Domain) Name() string { return "assistant" }

// AssistantLogMaxBytes is the ceiling this domain declares for its log, which
// is what the framework's own suites provision the stream with.
//
// 512 MiB, the Tier A default, and a node does not create the stream at it:
// every state log's ceiling is a reservation the broker grants in full, so
// the engine sizes them together from Tier A (`stream.assistant_log_max_bytes`,
// unset taking config.DefaultAssistantLogMaxBytes, scaled down as far as its
// 64 MiB floor where the broker is short, which states the arithmetic).
const AssistantLogMaxBytes = 512 << 20

// AssistantMaxRecordBytes is the largest record this domain publishes.
//
// TWO MEBIBYTES, measured rather than guessed (spike S4,
// [TestTheLargestRecordFitsItsDeclaration]). The largest record is a proposal:
// a request body of [ProposalBodyMaxBytes], its query of [QueryMaxBytes] and a
// card of [CardMaxBytes], each sealed (an AES-GCM nonce and tag, then base64,
// four thirds of the plaintext) inside one JSON envelope, about 1.05 MiB at the
// worst; a parked turn of [ParkedMaxBytes] is about 0.7 MiB. One mebibyte, the
// first estimate, did not hold the proposal. The framework's own ceiling is the
// transport's eight mebibytes less the four kibibytes a stored record carries
// beside its payload, so no cap had to shrink. The gate reserve on this log is
// sized by this number, so a declaration far above the records would hold back
// room no append could use.
const AssistantMaxRecordBytes = 2 << 20

// AssistantLogDuplicates is the window the broker collapses a repeated
// operation id in: AN OPTIMISATION, NEVER A MECHANISM (the ledger is what makes
// a retry idempotent). Two minutes, the tracker's, the pages' and the
// artifacts' window, because it must outlast a publisher's retry budget and no
// longer.
const AssistantLogDuplicates = 2 * time.Minute

// Stream is the assistant log, at the domain's whole budget.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:            topics.AssistantLogStream,
		Subjects:        []string{topics.AssistantLogWildcard},
		SubjectPrefix:   topics.AssistantLogPrefix,
		MaxBytes:        AssistantLogMaxBytes,
		MaxRecordBytes:  AssistantMaxRecordBytes,
		Duplicates:      AssistantLogDuplicates,
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
// eviction by its kind, a conversation's purge and an owner's erasure by their
// ops. A deferred purge is a node that goes on serving a conversation every
// other node has destroyed; a deferred erasure is one that goes on holding a
// removed person's words.
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind).InstallsGate() || OpKind(env.Op).installsGate()
}

// NodeGate reports a node's eviction or readmission, BY ITS KIND, and
// deliberately not a purge: a purge is an ordinary write the gate reserve and
// the fences a node gate is excused must hold.
func (Domain) NodeGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind) == KindEviction
}

// EvictionSubject is where a node's evictions and readmissions are published
// on this log ([statelog.EvictionProbe]).
func (Domain) EvictionSubject(node string) statelog.Subject {
	return EvictionSubject(node).framework()
}

// Evicts decodes one record from a node's eviction subject: true for an
// eviction, false for the readmission that inverts one.
func (Domain) Evicts(payload []byte) (bool, error) {
	record, err := Decode(payload)
	if err != nil {
		return false, fmt.Errorf("assistantdomain: decode a record from an "+
			"eviction subject: %w", err)
	}
	mutation, err := DecodeMutation(record)
	if err != nil {
		return false, fmt.Errorf("assistantdomain: decode the eviction: %w", err)
	}
	eviction, ok := mutation.(Eviction)
	if !ok {
		return false, fmt.Errorf("assistantdomain: the record on an eviction "+
			"subject carries a %T", mutation)
	}
	return !eviction.Readmitted, nil
}

// Evictions is every eviction this log's applied rows hold: the rows the trim
// reads to stop counting an evicted node on THIS log once its fence window has
// passed.
func (Domain) Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM assistant_evictions ORDER BY node_id`)
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
		return nil, fmt.Errorf("assistantdomain: read the evictions on this log: %w", err)
	}
	return out, nil
}

// Tables is every durable table this domain writes, with its class.
//
// THE SCRUB LIST, THE IDENTITY CLAIM AND THE LOCAL SWEEP ARE ALL DERIVED FROM
// THIS MAP. Every reproducible table is Replicated: each is written only by
// applying a record, from the record and same-log rows alone, with broker
// instants and the writer's sealed bytes, so two nodes at one checkpoint hold
// the same bytes. The ledger is Divergent: it travels so an adopter can answer
// a retry of anything its donor applied, and its `applied_at` is each node's
// own.
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
func (Domain) DeferredTable() string { return "assistant_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope path.
func (Domain) ScopeIndex() string { return "assistant_log_deferred_scope" }

// OpsTable is the operation ledger: what this node has already applied.
func (Domain) OpsTable() string { return "assistant_ops" }

// GatesSeatAdmission reports that no seat depends on this domain's copy: no
// seat reads a conversation on its turn path, so a stalled assistant copy must
// not stop a node admitting seats. The copy is still judged and goes out of
// service for its own readers (ADR-0072): a stalled strict replay is a fault,
// and the framework refuses every read of it.
func (Domain) GatesSeatAdmission() bool { return false }

// ClaimsIdentity reports that every node's Replicated tables must be
// byte-identical: N copies of one SQL state from one ordered log.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is EMPTY: no record on this log wakes a seat. A turn's live frames
// reach the person's browser on their own channel (internal/assistant/live),
// never through a wake.
func (Domain) FeedGroup() string { return "" }
