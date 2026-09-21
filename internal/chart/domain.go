package chart

import (
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Domain is the org chart as the state-log framework sees it.
//
// A DECLARATION, not a behaviour: every method answers a question the framework
// asks before it applies anything, and each one has a failure mode that is
// silent if the answer is wrong. What tables exist decides what a donated
// snapshot scrubs and what an identity claim compares; which kinds arbitrate
// decides where an anchor row is written; whether a record installs a gate
// decides whether an unknown version stops the applier or is filed for later.
//
// THE FRAMEWORK'S FOURTH DOMAIN, and its third strictly-ordered one. What it
// adds over the three before it is a domain whose arbitration is not uniform:
// one subject carries the whole STRUCTURE while every object's CONTENT carries
// its own, so [Stream]'s arbitrated-kind list is the first one where two kinds
// on one log mean genuinely different things by "the object I contend on".
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "chart" }

// ChartLogMaxBytes is the ceiling this domain declares for its log, which is
// what the framework's own suites provision the stream with.
//
// A NODE DOES NOT CREATE THE STREAM AT IT. Every state log's ceiling is a
// reservation the broker grants in full, so the engine sizes them together from
// Tier A (`stream.chart_log_max_bytes`) inside what the broker can actually
// grant.
//
// Crossing a ceiling REFUSES an append rather than dropping the oldest record:
// nothing on this stream is derivable from anything else, so shedding history
// is data loss with a tidy name.
//
// ONE GIBIBYTE, which is the framework's own minimum domain ceiling and the
// smallest number the engine's scaling will hold a log at. It is sized from the
// CORPUS rather than by analogy with its neighbours, and the corpus is the
// company's own shape: a chart is hundreds of objects where the tracker is
// hundreds of thousands of items, and it changes when somebody is hired, moved
// or promoted rather than on every comment. At the reference company's rate —
// a few hundred structural records and a few thousand content records a year,
// each a few kilobytes — a COMPLETELY BLOCKED trim reaches this in well over a
// century. There is no number below it worth having, so the honest choice is
// the floor rather than a smaller one that buys nothing and a larger one that
// reserves disk no chart will ever use.
const ChartLogMaxBytes = 1 << 30

// ChartLogDuplicates is the window the broker collapses a repeated operation id
// in.
//
// AN OPTIMISATION, NEVER A MECHANISM: the operation ledger is what actually
// makes a retry idempotent, and this only saves the round trip. Two minutes,
// the same window the tracker and the knowledge base use, because it must
// outlast a publisher's whole retry budget and no longer — every message in the
// window costs memory on the server.
const ChartLogDuplicates = 2 * time.Minute

// Stream is the org chart's log.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:          topics.ChartLogStream,
		Subjects:      []string{topics.ChartLogWildcard},
		SubjectPrefix: topics.ChartLogPrefix,
		MaxBytes:      ChartLogMaxBytes,
		Duplicates:    ChartLogDuplicates,
		Replay:        statelog.ReplayStrict,
		// SIX OF THE SEVEN KINDS. A barrier shares one subject across
		// the whole domain, so an expectation there would serialise
		// every linearizable read behind every other one and write an
		// anchor row per read into the transaction holding this store's
		// only writer.
		ArbitratedKinds: arbitratedKinds(),
	}
}

// arbitratedKinds is the six, derived from the enum rather than typed again —
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
// cannot decode the payload.
//
// ON THE OP AND NOT ON A KIND, and a removal is why. A removal rides the
// ORDINARY STRUCTURE SUBJECT, so a reader that keyed on the kind would defer
// it — and a deferred removal is a node that goes on serving a unit every
// other node has dropped, with no inverse that repairs it, because nothing
// ever names a removed object again. An eviction does have a kind of its own
// and is answered through the same op, so the two gates are one question
// asked once: it is [RecordEnvelope.InstallsGate], read rather than restated,
// because a rule written twice is a record that installs a gate on the write
// side and is deferred on the read side.
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return RecordEnvelope{Op: OpKind(env.Op)}.InstallsGate()
}

// Tables is every durable table this domain writes, with its class.
//
// THE SCRUB LIST, THE IDENTITY CLAIM AND THE LOCAL SWEEP ARE ALL DERIVED FROM
// THIS MAP, which is why a table missing from it is three lists that are
// silently short rather than one error. It is built from the exported
// inventories rather than typed a third time.
//
// THERE IS NO DIVERGENT TABLE HERE, and that is a statement rather than an
// omission: every column this domain writes is a function of the record that
// wrote it, so there is nothing a per-node or per-epoch value reaches. The
// knowledge base's `pages_skills` is the counter-example — a flag THIS BUILD's
// parser derives — and the chart has no equivalent, because nothing here is
// re-derived at apply time from anything but the payload.
func (Domain) Tables() map[string]statelog.TableClass {
	out := make(map[string]statelog.TableClass,
		len(ReproducibleTables)+len(MachineryTables))
	for _, table := range ReproducibleTables {
		out[table] = statelog.Replicated
	}
	for _, table := range MachineryTables {
		out[table] = statelog.Local
	}
	return out
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "chart_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope term.
func (Domain) ScopeIndex() string { return "chart_log_deferred_scope" }

// OpsTable is contract 3's first layer: what this node has already applied.
//
// ONE OPS HORIZON FOR THE WHOLE DOMAIN, which is why the table takes the
// framework's four columns and nothing else. A `kind` column and an index over
// (kind, applied_at) would be a second horizon — a claim that a structural
// record's op id may be swept on a different schedule from a content record's —
// and two horizons on one ledger is a retry that resolves against a history
// half of which has been deleted.
func (Domain) OpsTable() string { return "chart_ops" }

// ReadinessInput reports that this domain's health gates seat admission.
//
// TRUE, and here it is the strongest case of the three strict domains. A node
// behind on the tracker serves a task row that is wrong; a node behind on the
// pages log serves an old paragraph. A node behind on the CHART does not know
// who reports to whom — so it routes an escalation to a manager who no longer
// holds the seat, hands a turn a roster of people who have moved, and admits a
// seat the company has removed. The chart is what every other answer is scoped
// by, so serving turns from a stale one is not degraded output, it is confident
// wrong output.
func (Domain) ReadinessInput() bool { return true }

// ClaimsIdentity reports that every node's Replicated tables must be
// byte-identical.
//
// TRUE. N copies of one SQL state, derived from one ordered log by one
// deterministic applier.
//
// ASSERTED AND NOT VERIFIED. A checksum over each node's identity-claimed
// tables, published and compared, is what would turn "should be identical" into
// something a fleet reports on, and nothing builds one — see
// [internal/tracker.Domain.ClaimsIdentity], which carries the argument and the
// one column a naive checksum would trip over. Every column this domain writes
// is owned by a record, so the exclusion that domain needs does not arise here.
func (Domain) ClaimsIdentity() bool { return true }

// BarrierTables is the empty set, DECLARED.
//
// The barrier writes no row on any node, and stating that explicitly is what
// stops a no-op record slipping through the kind-completeness walk by writing
// nothing: "this kind wrote nothing" and "nobody classified this kind" are the
// same observation, and only one of them is correct.
var BarrierTables = map[string]statelog.TableClass{}
