package executordomain

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Domain is the executor log as the state-log framework sees it: a
// declaration, not a behaviour. Each method answers a question the framework
// asks before it applies anything, and a wrong answer fails silently.
type Domain struct{}

// Name is the register key, the manifest key and the operator's column: NEVER
// renamed. Not `sessions`: iamdomain's `session` kind is a person's sign-in,
// and an operator reading `crewlet retention status` must not guess which.
func (Domain) Name() string { return "executor" }

// ExecutorLogMaxBytes is the ceiling this domain declares for its log, which
// the framework's suites provision the stream with: 512 MiB, the Tier A
// default. A node does not create the stream at it: every state log's ceiling
// is a reservation the broker grants in full, so the engine sizes them together
// from Tier A (`stream.executor_log_max_bytes`, unset taking
// config.DefaultExecutorLogMaxBytes, scaled down to its 64 MiB floor where the
// broker is short).
const ExecutorLogMaxBytes = 512 << 20

// MaxRecordBytes is the largest record this domain publishes: 128 KiB, measured
// ([TestEveryRecordAtEveryCapFitsTheDeclaredBound]). The largest is an `end`,
// about 74 KiB whole: its five prose fields at their caps (internal/executorkeep's
// CompletedWorkMaxBytes, BlockedOnMaxBytes, StopReasonMaxBytes and
// FailureMaxBytes, and [AskExcerptMaxBytes], 11 KiB between them) encode to
// about 66 KiB once JSON escapes every `<`, `>` and `&` to six bytes. A
// `capture` at its caps, its [MaxAbsorbs] absorbed turns included, is about
// 21 KiB. The bound keeps chat's and the assistant's headroom over the worst
// case. The gate reserve on this log is sized by it, so a bound far above the
// records holds back room no append can use: at 128 KiB the reserve's fleet
// term (seven 132 KiB appends and a MiB, about 1.9 MiB) stays under a
// sixteenth of even the 64 MiB floor.
const MaxRecordBytes = 128 << 10

// ExecutorLogDuplicates is the window the broker collapses a repeated operation
// id in: AN OPTIMISATION, NEVER A MECHANISM (the ledger makes a retry
// idempotent). Two minutes, every strict log's window: it must outlast a
// publisher's retry budget, and no longer.
const ExecutorLogDuplicates = 2 * time.Minute

// LinearizableReadsPerSeatDay is this log's READ MODEL: the linearizable reads
// a day each agent seat's executor launches put on it, which `census_drift`
// holds the log to and which sizes the barrier term in its ceiling. 150: thirty
// turns a seat a day (the reference company's 3 000 over 100 seats) times five
// reads a turn. A dispatch reads the head ([Reader.Head]); a first launch reads
// the base's row ([Reader.Session]), to take it up only while it names the
// object the read froze (its begin's read-back answers for the turn's own row);
// a relaunch reads the turn's row twice (runner, then launch), at a mean of one
// a turn; list_sessions and read_session
// (internal/agent/builtin) average one read a turn, a judgment without
// production data that this alarm's drift corrects. Past ten a turn, seats are
// relaunching or reading sessions in a loop: the drift worth telling an operator.
const LinearizableReadsPerSeatDay = 150

// Stream is the executor log, at the domain's whole budget.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:            topics.ExecutorLogStream,
		Subjects:        []string{topics.ExecutorLogWildcard},
		SubjectPrefix:   topics.ExecutorLogPrefix,
		MaxBytes:        ExecutorLogMaxBytes,
		MaxRecordBytes:  MaxRecordBytes,
		Duplicates:      ExecutorLogDuplicates,
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

// InstallsGate reports a record whose unknown version must STOP the applier: a
// node's eviction, by its kind, and nothing else. Every other rule a record
// here installs (a tombstone, an absorption, a release) concerns rows inside
// its declared scope, so deferring it defers every later record they decide
// (package doc, "A removed seat stays removed").
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind).InstallsGate()
}

// NodeGate reports a node's eviction or readmission, BY ITS KIND.
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
		return false, fmt.Errorf("executordomain: decode a record from an "+
			"eviction subject: %w", err)
	}
	mutation, err := DecodeMutation(record)
	if err != nil {
		return false, fmt.Errorf("executordomain: decode the eviction: %w", err)
	}
	eviction, ok := mutation.(Eviction)
	if !ok {
		return false, fmt.Errorf("executordomain: the record on an eviction "+
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
			FROM executor_evictions ORDER BY node_id`)
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
		return nil, fmt.Errorf("executordomain: read the evictions on this log: %w", err)
	}
	return out, nil
}

// Tables is every durable table this domain writes, with its class; the scrub
// list, the identity claim and the local sweep all derive from it. A Replicated
// table is written only by applying a record, from the record and same-log rows
// alone, with broker instants, so two nodes at one checkpoint hold the same
// bytes. The ledger is Divergent: it travels so an adopter can answer a retry
// of anything its donor applied, and its `applied_at` is each node's own.
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
func (Domain) DeferredTable() string { return "executor_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope path.
func (Domain) ScopeIndex() string { return "executor_log_deferred_scope" }

// OpsTable is the operation ledger: what this node has already applied.
func (Domain) OpsTable() string { return "executor_ops" }

// GatesSeatAdmission reports that no seat depends on this domain's copy: a
// launch that cannot read this log fails open, to a fresh session with the
// account of what it could not read, so a stalled copy must not stop a node
// admitting seats. The copy still goes out of service for its own readers
// (ADR-0079): a stalled strict replay is a fault, and the framework refuses
// every read of it.
func (Domain) GatesSeatAdmission() bool { return false }

// ClaimsIdentity reports that every node's Replicated tables must be
// byte-identical: N copies of one SQL state from one ordered log.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is EMPTY: no record on this log wakes a seat. A turn learns of its
// conversation's head by reading it when it launches, never through a wake.
func (Domain) FeedGroup() string { return "" }
