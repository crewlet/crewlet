package usage

import (
	"context"
	"database/sql"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Domain is the usage domain as the state-log framework sees it.
//
// THE FRAMEWORK'S FOURTH DOMAIN, AND ITS SECOND COMPACTED ONE. It answers every
// question the vector domain answers, the same way and for the same reasons —
// a keyed table rather than a log, no arbitration, no operation ledger, no
// identity claim, no seat gating — and differs from it in the one respect that
// matters to a reader: there is no singleton duty behind it. EVERY node is a
// writer, of its own days only, which is what the node in the subject buys.
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "usage" }

const (
	// History is how far back a node's day is kept: on the stream (its age
	// bound) and in every node's rows (the apply's own horizon).
	//
	// A HUNDRED AND EIGHTY-ONE DAYS, and the arithmetic is the offered
	// range rather than a round number: a spend window reaches back ninety
	// days and compares itself with the ninety before it, and the day
	// before THAT is the one a window cut on a clock that moved can touch.
	// Anything shorter answers a ninety-day comparison with a silently
	// empty previous half — which is the bug this domain replaced.
	History = 181 * 24 * time.Hour

	// FlushInterval is how often a node re-derives the days that moved and
	// publishes what changed.
	//
	// FIFTEEN SECONDS, the staleness of "today" on every node, and it is
	// the cadence the live budget meters are pushed on: a spend figure that
	// trailed the meter above it by more than one of the meter's own
	// refreshes would read as two different numbers. A tick that finds
	// nothing moved costs two indexed counts per day it looks at.
	FlushInterval = 15 * time.Second

	// ReadsPerSeatDay is how many (page, via) entries one seat-day record
	// carries.
	//
	// TWO HUNDRED AND FIFTY-SIX. An entry is about 140 bytes encoded, so
	// the cap bounds the read half of a record near 35 KiB — far under
	// the broker's 1 MiB message ceiling beside a seat's spend cells —
	// and it is past what any person reads about one seat on one day. The
	// most-read entries survive, and what was dropped is counted on the
	// record rather than lost silently.
	ReadsPerSeatDay = 256

	// LogMaxBytes is the ceiling this domain declares for its stream, which
	// the framework's own suites provision it with; a node sizes it from
	// Tier A (`stream.usage_log_max_bytes`) instead.
	//
	// ONE GIBIBYTE: the stream holds one message per (node, day, object)
	// for [History], so its size is a census rather than a rate. A
	// three-node fleet of forty seats and twenty schedules, every seat
	// working every day, is 60 subjects × 3 nodes × 181 days ≈ 32 600
	// messages; at a busy seat-day's ≈ 6.5 KiB that is about 217 MB, and a
	// gibibyte is four times it. Crossing it REFUSES an append and raises
	// the log's headroom alarm rather than dropping the oldest day.
	LogMaxBytes = 1 << 30

	// LogDuplicates is the window the broker collapses a repeated op id in.
	//
	// An optimisation, as it is for the vectors: nothing here relies on it,
	// because the apply is idempotent under its position guard. Two
	// minutes, the window every state log uses — long enough to outlast a
	// publisher's retry budget and no longer.
	LogDuplicates = 2 * time.Minute
)

// Stream is the usage domain's compacted changelog.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:          topics.UsageLogStream,
		Subjects:      []string{topics.UsageLogWildcard},
		SubjectPrefix: topics.UsageLogPrefix,
		MaxBytes:      LogMaxBytes,
		MaxPerSubject: 1,
		// THE STREAM FORGETS A DAY WHEN THE ROWS DO. A message older than
		// the history describes a day no read will ask for, and a node
		// replaying the stream would apply it only for the next record's
		// horizon to delete it.
		MaxAge:     History,
		Duplicates: LogDuplicates,
		Replay:     statelog.ReplayCompacted,
		// NO ARBITRATED KIND: one writer per subject, the node the day
		// belongs to, so there is no race and no expectation to form.
		ArbitratedKinds: nil,
	}
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
		Subject: env.Subject.Wire(),
		OpID:    env.OpID,
		Gen:     env.Gen,
		Scope:   env.Scope,
		Writer:  env.Writer,
	}, nil
}

// InstallsGate reports a record whose unknown version must STOP the applier.
//
// FALSE, ALWAYS: a usage record never licenses dropping any other record. A
// node that defers one serves a day that is a flush behind, which is what a
// compacted domain tolerates by construction.
func (Domain) InstallsGate(statelog.Envelope) bool { return false }

// Tables is every durable table this domain writes, with its class.
//
// NOTHING IS `Replicated`, for the vectors' reason: every row table is written
// only by applying a committed record and travels inside a snapshot, but two
// nodes legitimately hold different days — one joined last month — so none of
// them is in the identity claim.
func (Domain) Tables() map[string]statelog.TableClass {
	return map[string]statelog.TableClass{
		"usage_turns":         statelog.Divergent,
		"usage_tokens":        statelog.Divergent,
		"usage_reads":         statelog.Divergent,
		"usage_schedule_runs": statelog.Divergent,

		"usage_log_deferred":       statelog.Local,
		"usage_log_deferred_scope": statelog.Local,
	}
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "usage_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope path.
func (Domain) ScopeIndex() string { return "usage_log_deferred_scope" }

// OpsTable is EMPTY: the apply is a total function under a monotone position
// guard, and nothing waits on a usage record — a node publishes its day for the
// fleet, not for a caller. The framework asserts that pairing.
func (Domain) OpsTable() string { return "" }

// ReadinessInput reports that this domain's health does NOT gate seat
// admission: a node behind on usage answers spend a flush late, which is a
// coverage number and never a reason to move a company's seats.
func (Domain) ReadinessInput() bool { return false }

// ClaimsIdentity reports that two nodes at one checkpoint are NOT asserted to
// hold the same rows, because per-node coverage differs by construction.
func (Domain) ClaimsIdentity() bool { return false }

// NewRows builds the publisher's read seam for this domain.
//
// The guards answer FALSE for both, and that is this domain's answer rather
// than an omission: an object is created by its first record and there is no
// deletion marker, since a day leaves by the horizon rather than by a record.
func NewRows(db *store.DB) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{},
		func(context.Context, *sql.Tx, statelog.Subject) (bool, bool, error) {
			return false, false, nil
		})
}

// Fence is this domain's eviction fence, OPEN BY DECLARATION.
//
// An evicted node's usage is still what its seats did: its turns ran and its
// tokens were spent, and a fence here would erase that history from every
// peer. This domain installs no apply gate, so nothing would drop the record
// anyway, and nothing waits on its acknowledgement.
type Fence struct{}

// NewFence builds it.
func NewFence() Fence { return Fence{} }

// Evicted implements [statelog.Fence]. See the type doc for why it is open.
func (Fence) Evicted(context.Context) (bool, error) { return false, nil }

// ClearForZero implements [statelog.Fence], and is unreachable here: every
// write is additive and carries no expectation, so nothing ever publishes at
// an expectation of zero.
func (Fence) ClearForZero(context.Context, statelog.Position) error { return nil }

// Gates answers whether a durable record produced rows on no node. NOTHING
// GATES A USAGE RECORD, which [Applier.Gated] states from the applier's side.
type Gates struct{}

// NewGates builds it.
func NewGates() Gates { return Gates{} }

// GatedAt implements [statelog.Gates].
func (Gates) GatedAt(context.Context, statelog.Subject, string, string, statelog.Position) (statelog.Reason, bool, error) {
	return "", false, nil
}

// AdoptedAt implements [statelog.Gates], and is unreachable: it qualifies a
// read of the operation ledger, which this domain does not keep.
func (Gates) AdoptedAt(context.Context) (time.Time, bool, error) {
	return time.Time{}, false, nil
}
