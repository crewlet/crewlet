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
// THE FRAMEWORK'S FIFTH DOMAIN, AND ITS SECOND COMPACTED ONE. It answers every
// question the vector domain answers, the same way and for the same reasons —
// a keyed table rather than a log, no arbitration, no operation ledger, no
// identity claim, no seat gating, no generation record ([GenerationRecord]) —
// and differs from it in the one respect that matters to a reader: there is no
// singleton duty behind it. EVERY DATA NODE is a writer, of its own days only,
// which is what the node in the subject buys — and a node without `data` is no
// writer at all, since its spend is counted in its custody keeper's day (see
// the package doc).
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "usage" }

const (
	// History is how far back a node's day is kept: on the stream (its age
	// bound) and in every data node's rows (the apply's own horizon).
	//
	// A HUNDRED AND EIGHTY-ONE DAYS, and the arithmetic is the offered
	// range rather than a round number: a spend window reaches back ninety
	// days and compares itself with the ninety before it, and the day
	// before THAT is the one a window cut on a clock that moved can touch.
	// Anything shorter answers a ninety-day comparison with a silently
	// empty previous half — which is the bug this domain replaced.
	History = 181 * 24 * time.Hour

	// FlushInterval is how often a data node re-derives the days that moved
	// and publishes what changed.
	//
	// FIFTEEN SECONDS, the staleness of "today" in the fleet's rows, and it is
	// the cadence the live budget meters are pushed on: a spend figure that
	// trailed the meter above it by more than one of the meter's own
	// refreshes would read as two different numbers. A tick that finds
	// nothing moved costs one read of the re-derive marks, two reads of the
	// recorded day cuts (usage_day_windows: the cut now falls in, and
	// yesterday's), and, for each of the two days it looks at, two counts:
	// one seeks the day's window per event type and probes the in-flight
	// custody identities' key once per row it counts, the other seeks the
	// dispatch ledger's window. A day that moved adds its derivation, and
	// each object whose derivation changed one read of what the fleet holds
	// of it ([Applied]) beside its publish.
	FlushInterval = 15 * time.Second

	// ReadsPerSeatDay is how many (page, via) entries one seat-day record
	// carries.
	//
	// TWO HUNDRED AND FIFTY-SIX, and what it bounds is ONE RECORD. An
	// entry is a few hundred bytes encoded plus its query, which is at
	// most the 400 bytes a search accepts ([knowledge.MaxQueryBytes]), so
	// a seat-day at the cap is under 200 KiB with a full-length query in
	// every entry and under 1 MiB even where JSON escapes every byte of
	// every one — far under the largest record this log takes
	// ([LogMaxRecordBytes], the transport's own ceiling less what a signed
	// append carries beside it), as TestAFullSeatDayRecordFitsOneMessage
	// measures — and it is past what any person reads about one seat on one
	// day. The most-read entries survive, and what was dropped is counted on
	// the record rather than lost silently.
	//
	// It does NOT bound the stream: [LogMaxBytes] is sized from busy
	// seat-days, and a fleet whose every seat reached this cap every day
	// would outgrow that default, which the log's headroom alarm reports
	// before an append is refused and `stream.usage_log_max_bytes` raises.
	ReadsPerSeatDay = 256

	// ReadersPerRepoDay is how many readers one repo_read record carries —
	// one unit repository's day on one node.
	//
	// A THOUSAND AND TWENTY-FOUR, and what it bounds is ONE RECORD. An entry
	// is a principal of at most 255 bytes, a seat handle of at most 64, two
	// words and three figures, so under 2 KiB even where JSON escapes every
	// byte of the principal, and a record at the cap under 2 MiB — a quarter
	// of the
	// transport's 8 MiB message and far under the largest record this log
	// takes ([LogMaxRecordBytes]), as TestAFullRepoReadRecordFitsOneMessage
	// measures. And it is past the readers any unit's repository has: the
	// seats of its subtree and its leads, the company's persons through
	// their credentials, and its tokens. The most-read survive, and what was
	// dropped is counted on the record, so an answer that lost readers says
	// so.
	ReadersPerRepoDay = 1_024

	// LogMaxBytes is the ceiling this domain declares for its stream, which
	// the framework's own suites provision it with; a node sizes it from
	// Tier A (`stream.usage_log_max_bytes`) instead.
	//
	// ONE GIBIBYTE: the stream holds one message per (node, day, object)
	// for [History], so its size is a census rather than a rate. A
	// three-node fleet of forty seats and twenty schedules, every seat
	// working every day, is 60 subjects × 3 nodes × 181 days ≈ 32 600
	// messages; at a busy seat-day's ≈ 6.5 KiB that is about 217 MB, and a
	// gibibyte is four times it. A unit repository read on a day adds one
	// message for each node that served it, under two hundred bytes a
	// reader beside its envelope: ten unit repositories read every day on
	// three nodes by a dozen readers each are 10 × 3 × 181 ≈ 5 400 messages
	// of about 2.3 KiB, some 13 MB over the history — about 1 % of the
	// gibibyte. Crossing it REFUSES an append and raises the log's headroom
	// alarm rather than dropping the oldest day.
	LogMaxBytes = 1 << 30

	// LogMaxRecordBytes is the largest record this domain publishes
	// ([statelog.StreamSpec.MaxRecordBytes]).
	//
	// THE LARGEST THE TRANSPORT CARRIES ([statelog.MaxTransportRecordBytes]),
	// on the vector domain's precedent and for its reason: this log claims
	// no identity and keeps no gate reserve, so the declaration sizes
	// nothing and only says where the publisher and the broker refuse. A
	// record is bounded far below it by its own caps — a seat-day's reads
	// at [ReadsPerSeatDay], a schedule-day's fires at one per cron minute,
	// 1 440 fires of about 150 bytes, and a repository-day's readers at
	// [ReadersPerRepoDay], under 2 MiB — so a record that does meet the
	// refusal is one no retry places, and the publisher says so once rather
	// than every tick ([Publisher]).
	LogMaxRecordBytes = statelog.MaxTransportRecordBytes

	// LogDuplicates is the window the broker collapses a repeated op id in.
	//
	// An optimisation, as it is for the vectors: nothing here relies on it,
	// because the apply is idempotent under its position guard. Two
	// minutes, the window every state log uses — long enough to outlast a
	// publisher's retry budget and no longer.
	LogDuplicates = 2 * time.Minute
)

// Stream is the usage domain's compacted changelog, at the domain's whole
// budget.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:           topics.UsageLogStream,
		Subjects:       []string{topics.UsageLogWildcard},
		SubjectPrefix:  topics.UsageLogPrefix,
		MaxBytes:       LogMaxBytes,
		MaxRecordBytes: LogMaxRecordBytes,
		MaxPerSubject:  1,
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
		V:    env.V,
		Kind: string(env.Subject.Kind),
		// THE WRITER'S IDENTITY for a kind this build does not know — the
		// key a deferred record supersedes on. See [RecordEnvelope.Wire].
		Subject: env.Wire(),
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

// NodeGate reports a node's eviction or readmission: FALSE, ALWAYS. This
// domain claims no identity, so no node is counted on its log and none is
// evicted from it — a record here flagged a node gate is a mistake the
// publisher refuses rather than one it lets past the fences.
func (Domain) NodeGate(statelog.Envelope) bool { return false }

// Tables is every durable table this domain writes, with its class.
//
// NOTHING IS `Replicated`, for the vectors' reason: every row table is written
// only by applying a committed record and travels inside a snapshot, but two
// nodes legitimately hold different days — one joined last month — so none of
// them is in the identity claim.
func (Domain) Tables() map[string]statelog.TableClass {
	return map[string]statelog.TableClass{
		"usage_turns":          statelog.Divergent,
		"usage_tokens":         statelog.Divergent,
		"usage_reads":          statelog.Divergent,
		"usage_schedule_runs":  statelog.Divergent,
		"usage_person_tokens":  statelog.Divergent,
		"usage_repo_read_days": statelog.Divergent,
		"usage_repo_reads":     statelog.Divergent,

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

// GatesSeatAdmission reports that a seat does NOT depend on this domain's
// copy: a node behind on usage answers spend a flush late, which is a coverage
// number and never a reason to withhold or move a company's seats. Its copy is
// still judged, and a wrong one — not one that is behind — goes out of service
// on its own, its seats untouched — ADR-0072.
func (Domain) GatesSeatAdmission() bool { return false }

// ClaimsIdentity reports that two nodes at one checkpoint are NOT asserted to
// hold the same rows, because per-node coverage differs by construction.
func (Domain) ClaimsIdentity() bool { return false }

// FeedGroup is EMPTY: this domain has no wake feed.
//
// A node's day is news to nobody — every turn it counts already woke whoever
// it was going to on its own log — so there is no consumer here for the trim
// to wait on, and its `feed_ack_floor` term reports absent rather than zero.
// Stated rather than inferred from the compaction, as the vectors state it,
// because "is compacted" and "notifies nobody" are two facts that merely
// coincide here.
func (Domain) FeedGroup() string { return "" }

// NewRows builds the publisher's read seam for this domain.
//
// The guards answer FALSE for both, and that is this domain's answer rather
// than an omission: an object is created by its first record and there is no
// deletion marker, since a day leaves by the horizon rather than by a record.
//
// spec is the log the publisher writes, whose checkpoint the seam reads.
func NewRows(db store.ReplicatedReader, spec statelog.StreamSpec) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, spec,
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

// GatedAt implements [statelog.Gates]. The record's body is not read: no
// marker exists that a payload could name.
func (Gates) GatedAt(context.Context, statelog.Subject, string, string, statelog.Position,
	[]byte) (statelog.Reason, bool, error) {
	return "", false, nil
}

// THE FRAMEWORK'S SEAMS, held at compile time: the engine's register wires
// each of these by its interface, so a signature the framework moves under
// them is a build failure here rather than a register entry that no longer
// compiles in a package nobody built.
var (
	_ statelog.Domain            = Domain{}
	_ statelog.Applier           = Applier{}
	_ statelog.Fence             = Fence{}
	_ statelog.Gates             = Gates{}
	_ statelog.GenerationEncoder = GenerationRecord{}
)
