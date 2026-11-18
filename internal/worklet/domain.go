package worklet

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Domain is the tracker as the state-log framework sees it.
//
// A declaration: each method answers a question the framework asks before it
// applies anything, and a wrong answer fails silently. The tables decide what a
// donated snapshot scrubs and what an identity claim compares; the arbitrated
// kinds decide where an anchor row is written; InstallsGate decides whether an
// unknown version stops the applier or is filed for later.
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "tracker" }

// TrackerLogMaxBytes is this log's declared ceiling, which the framework's
// suites provision (a node sizes it from `stream.tracker_log_max_bytes`).
// Crossing it REFUSES an append rather than dropping the oldest record: nothing
// here is derivable, so shedding history is data loss.
//
// Five years of the reference company, measured: its day takes 9.9 MB of this
// log (package doc, "What a day costs the log, measured"), which fills it in
// 4.7 years (TestTheMutationLogsCeilingIsFiveYearsOfAMeasuredDay holds it
// within a tenth of five). Its seats' linearizable reads add 2.2 MB a day of
// barriers, so a completely blocked trim reaches it in nearly four years; the
// operator dial is real because at a three-year-old company whose trim never
// ran the ceiling is under a year away.
const TrackerLogMaxBytes = 16 << 30

// TrackerMaxRecordBytes is the largest record this domain publishes
// ([statelog.StreamSpec.MaxRecordBytes]): the largest the transport carries
// ([statelog.MaxTransportRecordBytes]), since a smaller one buys nothing. The
// declaration sizes only the gate reserve's fleet term, and at the tracker's
// gibibyte floor a sixteenth of the ceiling, 64 MiB, already exceeds the 57
// seven peers' records at this size can overshoot by; what it would otherwise
// bound (a description, a comment body, a batch of fields) has a cap of its
// own. Not the transport's eight mebibytes: that limit is on the signed
// message, so a record declared at it would pass here and be refused by the
// broker.
const TrackerMaxRecordBytes = statelog.MaxTransportRecordBytes

// TrackerLogDuplicates is the window the broker collapses a repeated operation
// id in: an optimisation saving a round trip, never what makes a retry
// idempotent (the operation ledger is). Two minutes outlasts a publisher's
// whole retry budget, and no longer, since every message in the window costs
// server memory.
const TrackerLogDuplicates = 2 * time.Minute

// Stream is the mutation log, at the domain's whole budget.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:           topics.TrackerLogStream,
		Subjects:       []string{topics.TrackerLogWildcard},
		SubjectPrefix:  topics.TrackerLogPrefix,
		MaxBytes:       TrackerLogMaxBytes,
		MaxRecordBytes: TrackerMaxRecordBytes,
		Duplicates:     TrackerLogDuplicates,
		Replay:         statelog.ReplayStrict,
		// Every kind but three (ADR-0080): a turn is additive and races nobody; a
		// barrier's one company-wide subject would serialise every linearizable read
		// and write an anchor row per read inside this store's only writer; and the
		// trim's record removes anchors, so arbitrated it would write one per apply.
		ArbitratedKinds: arbitratedKinds(),
	}
}

// arbitratedKinds is every kind but those three, derived from the enum: a list
// typed twice is a kind arbitrated in one place and not the other, which wedges
// that subject the first time a gate drops a record.
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
		Subject: statelog.Subject{Kind: string(env.Subject.Kind), ID: env.Subject.ID},
		Op:      string(env.Op),
		OpID:    env.OpID,
		Gen:     env.Gen,
		Scope:   env.Scope.Resolve(env.Subject),
		Writer:  env.Writer,
	}, nil
}

// InstallsGate reports a record whose unknown version must STOP the applier
// rather than be filed for later.
//
// Answered from the envelope alone, all a node has when it cannot decode the
// payload: an eviction is a gate by its kind, a purge by its op. A deferred
// gate would license every later record on this node with no inverse to repair
// it.
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind).InstallsGate() || OpKind(env.Op) == OpPurge
}

// NodeGate reports a node's eviction or readmission, the one record a write
// flagged [statelog.Request.NodeGate] may carry. By kind, and deliberately not
// InstallsGate: the purge, this log's other gate, is an ordinary write that the
// gate reserve and the fences a node gate is excused must hold.
func (Domain) NodeGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind) == KindEviction
}

// EvictionSubject is where a node's evictions and readmissions are published
// on this log — the [statelog.EvictionProbe] half a node the fleet re-anchored
// past reads its peers' standing through, since its applier never reaches an
// eviction written after it stopped.
func (Domain) EvictionSubject(node string) statelog.Subject {
	return wire(EvictionSubject(node))
}

// Evicts decodes one record from a node's eviction subject: true for an
// eviction, false for the readmission that inverts one.
func (Domain) Evicts(payload []byte) (bool, error) {
	record, err := Decode(payload)
	if err != nil {
		return false, fmt.Errorf("worklet: decode a record from an eviction subject: %w", err)
	}
	if record.Subject.Kind != KindEviction {
		return false, fmt.Errorf("worklet: the record on an eviction subject is a %s",
			record.Subject.Kind)
	}
	var eviction Eviction
	if err := decodePayload(record.Mutation, &eviction); err != nil {
		return false, fmt.Errorf("worklet: decode the eviction: %w", err)
	}
	return !eviction.Readmitted, nil
}

// Tables is every durable table this domain writes, with its class.
//
// The scrub list, the identity claim and the local sweep all derive from this
// map, so a missing table silently shortens three lists. It is built from the
// exported inventories rather than typed again.
func (Domain) Tables() map[string]statelog.TableClass {
	out := make(map[string]statelog.TableClass,
		len(ReproducibleTables)+len(MachineryTables))
	for _, table := range ReproducibleTables {
		out[table] = statelog.Replicated
	}
	// The one divergent table: written only by an applied commit and carried in a
	// snapshot (so not Local), but outside the identity claim, because the applier
	// applies the inbox horizon from its epoch and two nodes briefly on different
	// epochs legitimately write different rows.
	out["tracker_notifications"] = statelog.Divergent
	for _, table := range MachineryTables {
		out[table] = statelog.Local
	}
	// The ledger travels: see [statelog.Domain.OpsTable] and [MachineryTables].
	out[Domain{}.OpsTable()] = statelog.Divergent
	return out
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "tracker_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope term.
func (Domain) ScopeIndex() string { return "tracker_log_deferred_scope" }

// OpsTable is contract 3's first layer: what this node has already applied.
func (Domain) OpsTable() string { return "tracker_ops" }

// GatesSeatAdmission reports that a seat depends on this domain's copy: true,
// because every seat's tools read this log every turn, and a node behind on a
// strict replay serves rows that are wrong, not merely less complete as on a
// compacted domain. A wrong copy goes out of service for the tracker's
// operations alone (ADR-0079).
func (Domain) GatesSeatAdmission() bool { return true }

// ClaimsIdentity reports that every data node's Replicated tables must be
// byte-identical: the claim the design rests on, N copies of one SQL state
// derived from one ordered log by one deterministic applier.
//
// It is ASSERTED, not verified: nothing publishes and compares a checksum of
// the claimed tables. One written today would also be wrong:
// `tracker_projects.rank_duplicate_pending` is an applier-local hint each node
// sets from its own probe, cleared only by the fleet-singleton duty's local
// write ([duty.clearProbe]), so nodes legitimately differ on it and a peer's
// copy stays set until it restarts. The claim is really "identical in every
// column a RECORD owns", and a verification must exclude the other columns.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is the change feed's own consumer on this log: [FeedGroup], the
// same constant the [Translator] opens, so the trim waits on the consumer the
// wakes actually advance.
func (Domain) FeedGroup() string { return FeedGroup }
