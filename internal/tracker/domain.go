package tracker

import (
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Domain is the tracker as the state-log framework sees it.
//
// A DECLARATION, not a behaviour: every method answers a question the
// framework asks before it applies anything, and each one has a failure mode
// that is silent if the answer is wrong. What tables exist decides what a
// donated snapshot scrubs and what an identity claim compares; which kinds
// arbitrate decides where an anchor row is written; whether a record installs
// a gate decides whether an unknown version stops the applier or is filed for
// later.
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "tracker" }

// TrackerLogMaxBytes is the mutation stream's default ceiling.
//
// Crossing it REFUSES an append rather than dropping the oldest record —
// nothing on this stream is derivable from anything else, so shedding history
// is data loss with a tidy name. At the modelled growth rate a completely
// blocked trim reaches it in five years, which is an unmistakable operator
// failure rather than a surprise; the operator dial is real because at a
// three-year-old company the ceiling is two years away.
const TrackerLogMaxBytes = 16 << 30

// TrackerLogDuplicates is the window the broker collapses a repeated
// operation id in.
//
// AN OPTIMISATION, NEVER A MECHANISM: the operation ledger is what actually
// makes a retry idempotent, and this only saves the round trip. Two minutes,
// because it must outlast a publisher's whole retry budget and no longer —
// every message in the window costs memory on the server.
const TrackerLogDuplicates = 2 * time.Minute

// Stream is the mutation domain's log.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:          topics.TrackerLogStream,
		Subjects:      []string{topics.TrackerLogWildcard},
		SubjectPrefix: topics.TrackerLogPrefix,
		MaxBytes:      TrackerLogMaxBytes,
		Duplicates:    TrackerLogDuplicates,
		Replay:        statelog.ReplayStrict,
		// THIRTEEN OF THE FIFTEEN KINDS. A turn is additive and races
		// nobody; a barrier shares one subject across the whole company,
		// so an expectation there would serialise every linearizable
		// read behind every other one and write an anchor row per read
		// into the transaction holding this store's only writer.
		ArbitratedKinds: arbitratedKinds(),
	}
}

// arbitratedKinds is the thirteen, derived from the enum rather than typed
// again — a list written twice is a kind that arbitrates in one place and not
// the other, which wedges that subject the first time a gate drops a record.
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
// Answered from the envelope alone, because that is all a node has when it
// cannot decode the payload. An eviction is a gate by its kind and a purge is
// one by its op — and a deferred gate licenses every later record on this
// node with no inverse that repairs it.
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind).InstallsGate() || OpKind(env.Op) == OpPurge
}

// Tables is every durable table this domain writes, with its class.
//
// THE SCRUB LIST, THE IDENTITY CLAIM AND THE LOCAL SWEEP ARE ALL DERIVED FROM
// THIS MAP, which is why a table missing from it is three lists that are
// silently short rather than one error. It is built from the exported
// inventories rather than typed a fourth time.
func (Domain) Tables() map[string]statelog.TableClass {
	out := make(map[string]statelog.TableClass,
		len(ReproducibleTables)+len(MachineryTables))
	for _, table := range ReproducibleTables {
		out[table] = statelog.Replicated
	}
	// THE ONE DIVERGENT TABLE. It is written only by an applied commit and
	// TRAVELS inside a snapshot — so it is not Local — but it is excluded
	// from the identity claim, because the applier applies the inbox
	// horizon from the epoch it was given and two nodes briefly on
	// different epochs legitimately write different rows.
	out["tracker_notifications"] = statelog.Divergent
	for _, table := range MachineryTables {
		out[table] = statelog.Local
	}
	return out
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "tracker_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope term.
func (Domain) ScopeIndex() string { return "tracker_log_deferred_scope" }

// OpsTable is contract 3's first layer: what this node has already applied.
func (Domain) OpsTable() string { return "tracker_ops" }

// ReadinessInput reports that this domain's health gates seat admission.
//
// TRUE, because a strict replay's stall is a FAULT rather than a coverage
// number: a node behind on the mutation log serves rows that are simply wrong,
// where a node behind on a compacted domain serves an answer that is merely
// less complete and says so.
func (Domain) ReadinessInput() bool { return true }

// ClaimsIdentity reports that every node's Replicated tables must be
// byte-identical.
//
// TRUE, and it is the claim the whole design rests on: N copies of one SQL
// state, derived from one ordered log by one deterministic applier. It is
// checkable — a checksum over the replicated file's own tables — which is
// what turns "should be identical" into something a fleet reports on.
func (Domain) ClaimsIdentity() bool { return true }

// BarrierTables is the empty set, DECLARED.
//
// The barrier writes no row on any node, and stating that explicitly is what
// stops a no-op record slipping through the kind-completeness walk by writing
// nothing: "this kind wrote nothing" and "nobody classified this kind" are the
// same observation, and only one of them is correct.
var BarrierTables = map[string]statelog.TableClass{}
