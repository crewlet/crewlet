package search

import (
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Domain is the vector half as the state-log framework sees it.
//
// THE FRAMEWORK'S SECOND DOMAIN, AND ITS FIRST COMPACTED ONE. Every answer
// here differs from the tracker's, and each difference is a branch the
// framework already had and nothing had yet taken: the replay loop that steps
// over holes, the absent operation ledger, the absent arbitration, and the
// identity claim this domain does not make. That is what makes the pair worth
// having — a framework with one consumer is a framework shaped like its
// consumer.
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "vectors" }

const (
	// VectorLogMaxBytes is the compacted stream's ceiling.
	//
	// ONE MESSAGE PER SOURCE, so the stream's size is the corpus rather
	// than its history: at the packed 12 KiB a 3 072-wide vector costs,
	// plus its envelope, 740 000 sources — this engine's declared
	// supported corpus per node — is ≈ 9.1 GB. Sixteen gibibytes is that
	// with room for the width to change under it and for the transition
	// window in which both models' records are on the stream.
	//
	// Crossing it REFUSES an append rather than dropping the oldest
	// record. A dropped vector is a document that silently stops being
	// findable by meaning, with nothing to say so.
	VectorLogMaxBytes = 16 << 30

	// VectorLogMaxAge is how long a message survives.
	//
	// NON-ZERO ONLY BECAUSE THIS DOMAIN IS COMPACTED, and it is what
	// collects the forget tombstones: a withdrawn source leaves a record
	// that exists to tell a replaying node the vector is gone, and once
	// the embed it supersedes is also gone there is nothing left for it to
	// say. Ninety days, because that is comfortably longer than the
	// longest adoption window an operator is asked to plan for, and a node
	// that has been away longer than that is adopting a snapshot rather
	// than replaying.
	//
	// It is safe here for the reason it is refused on a log: the state is
	// durable in SQL on every node, so an aged-out message is not state
	// anyone lost.
	VectorLogMaxAge = 90 * 24 * time.Hour

	// VectorLogDuplicates is the window the broker collapses a repeated
	// operation id in.
	//
	// AN OPTIMISATION AND NOTHING ELSE — this domain keeps no operation
	// ledger, so nothing here relies on it for correctness; what makes a
	// re-published embed harmless is that the apply is idempotent under
	// its version guard. Two minutes, the same window the mutation log
	// uses, because it must outlast a publisher's whole retry budget and
	// no longer.
	VectorLogDuplicates = 2 * time.Minute
)

// Stream is the vector domain's compacted changelog.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:          topics.TrackerVectorsStream,
		Subjects:      []string{topics.TrackerVectorsWildcard},
		SubjectPrefix: topics.TrackerVectorsPrefix,
		MaxBytes:      VectorLogMaxBytes,
		MaxPerSubject: 1,
		MaxAge:        VectorLogMaxAge,
		Duplicates:    VectorLogDuplicates,
		Replay:        statelog.ReplayCompacted,
		// NO ARBITRATED KIND. Exactly one writer publishes on a
		// subject — the fleet's embedding duty, which is a singleton —
		// so there is no expectation to form, and an anchor row here
		// would be a row nothing writes and nothing reads.
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
		Kind:    string(env.Subject.Source),
		Subject: statelog.Subject{Kind: string(env.Subject.Source), ID: env.Subject.ID},
		Op:      string(env.Op),
		OpID:    env.OpID,
		Gen:     env.Gen,
		Scope:   env.Scope,
		Writer:  env.Writer,
	}, nil
}

// InstallsGate reports a record whose unknown version must STOP the applier.
//
// FALSE, ALWAYS, and it is a claim about this domain rather than a default. A
// gate is a rule under which a durable record produces no rows on ANY node —
// a permanent deletion marker, an eviction fence — and this domain has
// neither. A forget is not one: it removes a row rather than licensing every
// later record to be dropped, and a node that defers one until it can read it
// serves a stale vector in the meantime, which is what the whole domain
// tolerates by construction.
func (Domain) InstallsGate(statelog.Envelope) bool { return false }

// Tables is every durable table this domain writes, with its class.
//
// NOTHING IS `Replicated` HERE, and that is the point of the class existing.
// Both row tables are written only by applying a committed record and both
// TRAVEL inside a snapshot — so neither is Local — but neither is in the
// identity claim, because per-node coverage legitimately differs: one node
// joined after a trim, another is mid-fill, and asserting they agree would
// report a healthy fleet as broken.
func (Domain) Tables() map[string]statelog.TableClass {
	return map[string]statelog.TableClass{
		// The vectors themselves: the expensive half, and the one a
		// node cannot recompute alone.
		"kb_vectors": statelog.Divergent,
		// The sign codes: a pure function of the row above, in the same
		// file, rebuildable by one INSERT … SELECT with no network. It
		// travels anyway, because rebuilding half a million of them on
		// an adopting node is work a copy already did.
		"kb_vectors_bin": statelog.Derived,

		"vectors_log_deferred":       statelog.Local,
		"vectors_log_deferred_scope": statelog.Local,
	}
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "vectors_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope path.
func (Domain) ScopeIndex() string { return "vectors_log_deferred_scope" }

// OpsTable is EMPTY, and the pairing that licenses it is asserted rather than
// assumed.
//
// The ledger answers one question: where did this operation land, for a caller
// that is waiting to hear. Nothing waits on an embedding — the duty publishes
// for a source, not for a person — and the apply is a total function under a
// monotone version guard, so a redelivery writes the same rows or none.
func (Domain) OpsTable() string { return "" }

// ReadinessInput reports that this domain's health does NOT gate seat
// admission.
//
// FALSE, and it is the same fact the compaction states from the other side: a
// gap here is a coverage number rather than a fault, so a node behind on
// vectors serves an answer that is less complete and says so. Shedding a
// company's seats for that is the outage the coverage number exists to avoid —
// and it would be shed for the one subsystem every design note calls best
// effort.
func (Domain) ReadinessInput() bool { return false }

// ClaimsIdentity reports that two nodes at one checkpoint are NOT asserted to
// hold the same rows.
//
// FALSE. A compacted stream's per-node coverage differs by construction, and
// the claim exists to be checkable — a claim nobody can check is worse than
// none, because it is the one a fleet report would print.
func (Domain) ClaimsIdentity() bool { return false }
