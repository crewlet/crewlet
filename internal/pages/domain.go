package pages

import (
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Domain is the knowledge base as the state-log framework sees it.
//
// A DECLARATION, not a behaviour: every method answers a question the
// framework asks before it applies anything, and each one has a failure mode
// that is silent if the answer is wrong. What tables exist decides what a
// donated snapshot scrubs and what an identity claim compares; which kinds
// arbitrate decides where an anchor row is written; whether a record installs
// a gate decides whether an unknown version stops the applier or is filed for
// later.
//
// THE FRAMEWORK'S THIRD DOMAIN, and its second strictly-ordered one. What it
// adds over the tracker's is the case the pair could not exercise: two
// STRICT domains on one node, so the checkpoint, the readiness input and the
// identity claim are per-domain values rather than the one value a single
// strict domain makes them indistinguishable from.
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "pages" }

// PagesLogMaxBytes is the mutation stream's default ceiling.
//
// Crossing it REFUSES an append rather than dropping the oldest record —
// nothing on this stream is derivable from anything else, so shedding history
// is data loss with a tidy name.
//
// FOUR GIBIBYTES, a quarter of the tracker's, and the ratio is the corpus
// rather than a guess: the reference company files 100 000 tasks and 300 000
// comments a year against a knowledge base of a few thousand pages, and a
// page's records are dominated by SAVES rather than by creates. At a 512 KiB
// body cap and the modelled edit rate a completely blocked trim reaches this
// in about five years, which is the same unmistakable operator failure the
// tracker's ceiling is sized for.
const PagesLogMaxBytes = 4 << 30

// PagesLogDuplicates is the window the broker collapses a repeated operation
// id in.
//
// AN OPTIMISATION, NEVER A MECHANISM: the operation ledger is what actually
// makes a retry idempotent, and this only saves the round trip. Two minutes,
// the same window the tracker uses, because it must outlast a publisher's
// whole retry budget and no longer — every message in the window costs memory
// on the server.
const PagesLogDuplicates = 2 * time.Minute

// Stream is the knowledge base's log.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:          topics.PagesLogStream,
		Subjects:      []string{topics.PagesLogWildcard},
		SubjectPrefix: topics.PagesLogPrefix,
		MaxBytes:      PagesLogMaxBytes,
		Duplicates:    PagesLogDuplicates,
		Replay:        statelog.ReplayStrict,
		// FIVE OF THE SIX KINDS. A barrier shares one subject across
		// the whole domain, so an expectation there would serialise
		// every linearizable read behind every other one and write an
		// anchor row per read into the transaction holding this store's
		// only writer.
		ArbitratedKinds: arbitratedKinds(),
	}
}

// arbitratedKinds is the five, derived from the enum rather than typed again —
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
// one by its op — and a deferred gate licenses every later record on this node
// with no inverse that repairs it.
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind).InstallsGate() || OpKind(env.Op) == OpPurge
}

// Tables is every durable table this domain writes, with its class.
//
// THE SCRUB LIST, THE IDENTITY CLAIM AND THE LOCAL SWEEP ARE ALL DERIVED FROM
// THIS MAP, which is why a table missing from it is three lists that are
// silently short rather than one error. It is built from the exported
// inventories rather than typed a third time.
//
// ALL FOUR CLASSES ARE PRESENT, and the Divergent one is `pages_skills`: the
// tool-skill flag is THIS BUILD'S parser answering about this build's rules,
// recomputed on every apply, so two nodes on different builds legitimately
// disagree about it — it travels inside a snapshot, because a joining node
// wants the answer rather than a rebuild, and it is excluded from the identity
// claim, because a rolling upgrade would otherwise report a fleet-wide
// divergence for a difference that resolves itself.
func (Domain) Tables() map[string]statelog.TableClass {
	out := make(map[string]statelog.TableClass,
		len(ReproducibleTables)+len(MachineryTables)+1)
	for _, table := range ReproducibleTables {
		out[table] = statelog.Replicated
	}
	out["pages_skills"] = statelog.Divergent
	for _, table := range MachineryTables {
		out[table] = statelog.Local
	}
	return out
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "pages_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope term.
func (Domain) ScopeIndex() string { return "pages_log_deferred_scope" }

// OpsTable is contract 3's first layer: what this node has already applied.
func (Domain) OpsTable() string { return "pages_ops" }

// ReadinessInput reports that this domain's health gates seat admission.
//
// TRUE, for the tracker's reason: a strict replay's stall is a FAULT rather
// than a coverage number. A node behind on the pages log serves a page body
// that is simply wrong — an older paragraph presented as the current one —
// where a node behind on a compacted domain serves an answer that is merely
// less complete and says so.
func (Domain) ReadinessInput() bool { return true }

// ClaimsIdentity reports that every node's Replicated tables must be
// byte-identical.
//
// TRUE. N copies of one SQL state, derived from one ordered log by one
// deterministic applier — checkable as a checksum over the replicated file's
// own tables, which is what turns "should be identical" into something a fleet
// reports on.
func (Domain) ClaimsIdentity() bool { return true }

// BarrierTables is the empty set, DECLARED.
//
// The barrier writes no row on any node, and stating that explicitly is what
// stops a no-op record slipping through the kind-completeness walk by writing
// nothing: "this kind wrote nothing" and "nobody classified this kind" are the
// same observation, and only one of them is correct.
var BarrierTables = map[string]statelog.TableClass{}
