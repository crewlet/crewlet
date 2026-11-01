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

// Domain is the executor log as the state-log framework sees it.
//
// A DECLARATION, not a behaviour: every method answers a question the framework
// asks before it applies anything, and each has a failure mode that is silent if
// the answer is wrong.
//
// THE FRAMEWORK'S TENTH DOMAIN, and its EIGHTH strict, identity-claiming one,
// after the tracker, the knowledge base, the identity estate, the artifacts,
// their datastore, the Dashboard Assistant and chat.
type Domain struct{}

// Name is the register key, the manifest key and the operator's column. NEVER
// renamed. `executor` and not `sessions`, because the identity estate already
// keeps a person's sign-in SESSIONS (iamdomain's `session` kind), and an
// operator reading `crewlet retention status` must not have to guess which.
func (Domain) Name() string { return "executor" }

// ExecutorLogMaxBytes is the ceiling this domain declares for its log, which is
// what the framework's own suites provision the stream with.
//
// 512 MiB, the Tier A default, and a node does not create the stream at it:
// every state log's ceiling is a reservation the broker grants in full, so the
// engine sizes them together from Tier A (`stream.executor_log_max_bytes`,
// unset taking config.DefaultExecutorLogMaxBytes, scaled down as far as its
// 64 MiB floor where the broker is short, which states the arithmetic).
const ExecutorLogMaxBytes = 512 << 20

// MaxRecordBytes is the largest record this domain publishes.
//
// 128 KiB, measured rather than guessed ([TestEveryRecordAtEveryCapFitsTheDeclaredBound]).
// The largest record is an `end`: its five prose fields at their caps
// ([CompletedWorkMaxBytes], [BlockedOnMaxBytes], [StopReasonMaxBytes],
// [FailureMaxBytes], [AskExcerptMaxBytes], eleven KiB between them) encode to
// about 66 KiB once JSON escapes every `<`, `>` and `&` to six bytes, and the
// conversation key, the work key, the short fields and the envelope bring the
// whole to about 74 KiB; a `capture` at its caps, its [MaxAbsorbs] absorbed
// turns included, is about 21 KiB. The bound
// keeps the headroom chat's and the assistant's keep over their worst case,
// and the gate reserve on this log is sized by it, so a declaration far above
// the records would hold back room no append could use: at 128 KiB the
// reserve's fleet term is seven 132 KiB appends and a mebibyte, about 1.9 MiB,
// under the sixteenth of even the 64 MiB floor.
const MaxRecordBytes = 128 << 10

// ExecutorLogDuplicates is the window the broker collapses a repeated operation
// id in: AN OPTIMISATION, NEVER A MECHANISM (the ledger is what makes a retry
// idempotent). Two minutes, every strict log's window, because it must outlast
// a publisher's retry budget and no longer.
const ExecutorLogDuplicates = 2 * time.Minute

// LinearizableReadsPerSeatDay is this log's READ MODEL: the linearizable reads
// a day each agent seat's executor launches put on it, which is what
// `census_drift` holds the log to and the barrier term in its ceiling.
//
// SIXTY: thirty turns a seat a day — the reference company's 3 000 over its
// 100 seats — times the two reads a turn makes at the seat's level: its FIRST
// launch reads the conversation's head once ([Reader.Head]), and a RELAUNCH
// (a send-back, an answered question, a lost job carried on) reads the turn's
// own row once ([Reader.Session]), at the reference company's mean of one
// relaunch a turn — a send-back or an answered question on about half of
// turns, and a second on some. Past twice it, four reads a turn, seats are
// relaunching in a loop, which is the drift worth telling an operator about.
const LinearizableReadsPerSeatDay = 60

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
// here installs (a tombstone, an absorption, a release) is about rows inside
// the scope that record declares, so a node that defers it defers every later
// record those rows decide too — see the package doc.
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

// Tables is every durable table this domain writes, with its class.
//
// THE SCRUB LIST, THE IDENTITY CLAIM AND THE LOCAL SWEEP ARE ALL DERIVED FROM
// THIS MAP. Every reproducible table is Replicated: each is written only by
// applying a record, from the record and same-log rows alone, with broker
// instants, so two nodes at one checkpoint hold the same bytes. The ledger is
// Divergent: it travels so an adopter can answer a retry of anything its donor
// applied, and its `applied_at` is each node's own.
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

// ReadinessInput reports that this domain's health does NOT gate seat
// admission, as the artifacts' and the assistant's do not: a launch that cannot
// read this log fails open, to a fresh session with the account of what it
// could not read, so a stalled executor copy must not stop a node admitting
// seats. The copy still goes out of service for its own readers: a stalled
// strict replay is a fault, and the framework refuses every read of it.
func (Domain) ReadinessInput() bool { return false }

// ClaimsIdentity reports that every node's Replicated tables must be
// byte-identical: N copies of one SQL state from one ordered log.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is EMPTY: no record on this log wakes a seat. A turn learns of its
// conversation's head by reading it when it launches, never through a wake.
func (Domain) FeedGroup() string { return "" }
