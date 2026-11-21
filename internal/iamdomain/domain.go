package iamdomain

import (
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Domain is the identity estate as the state-log framework sees it: a
// declaration, each method answering a question the framework asks before it
// applies anything, and a wrong answer failing silently.
//
// Its UNIQUENESS IS DECIDED ON ONE SUBJECT: no unique index exists or can, so
// two people taking one address are kept apart only by the directory subject,
// where every write setting an address, a login or a seat decides and
// contends (package doc, "ONE SUBJECT FOR THE DIRECTORY, and why that is the
// whole design").
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "iam" }

// SessionOpsRetention is how long this node's operation ledger keeps a row for
// a record on a SESSION subject (a sign-in or sign-out); every other subject
// keeps the framework's month ([statelog.OpsRetention]). An hour: the resolve
// budget (seconds, [statelog.DefaultResolveBudget]) plus a margin for retries;
// a sign-in is answered in its request, a close re-decided, never re-asked.
//
// The sweep records this kind's loss apart (replicated migration 0041), so a
// session operation re-asked past the hour is answered `unknown` unpublished
// ([statelog.Result.Unvouched]), never decided twice. Hence none takes a
// derived id ([statelog.DeriveOpID], up to [statelog.MintHorizon] old, so
// always `unknown`): a sign-in's is stepped from the lineage it mints
// ([statelog.StepOpID]), a close's minted fresh or stepped, at its gesture.
const SessionOpsRetention = time.Hour

// IamLogMaxBytes is the ceiling this domain declares for its log, which the
// framework's suites provision; the engine sizes every log together from Tier
// A (`stream.iam_log_max_bytes`). A full log REFUSES an append, never drops
// the oldest: nothing here is derivable, and what it would shed is the
// authentication trail.
//
// Half a GiB, sized by sessions (two records each): docs/guides/retention.md's
// reference company at three sessions a person a day writes ~285 MB a year, so
// this holds ~18 months of a blocked trim, which `trim_blocked` raises long
// before. Not the 64 MiB floor, which that rate fills in three months; not
// [engine.MinDomainCeiling]'s GiB, since the broker reserves a whole ceiling
// before a node can boot. An operator scales the Tier A field to headcount.
const IamLogMaxBytes = 512 << 20

// IamMaxRecordBytes is the largest record this domain publishes
// ([statelog.StreamSpec.MaxRecordBytes]), which sizes this log's gate reserve.
//
// 128 KiB, from the one record that grows: a person's document, republished
// whole by every edit and carried by an enrolment; the rest are a few hundred
// bytes. At every cap ([MaxName], [MaxAddress], [MaxHeldCredentials],
// [MaxTokenLabel]), with every token carrying every grant and a label JSON
// escapes six-fold, it stays under 100 KiB; a test builds that record as the
// writer does, so a cap raised without this fails. Not the transport's 8 MiB:
// the reserve's fleet term is seven peers' largest appends, most of a 64 MiB
// log; at 128 KiB it is under 2 MiB and the ceiling's sixteenth dominates.
const IamMaxRecordBytes = 128 << 10

// IamLogDuplicates is the window the broker collapses a repeated operation id
// in: an optimisation saving a round trip, never the mechanism (the operation
// ledger makes a retry idempotent). Two minutes, as every domain uses: it must
// outlast a publisher's retry budget and no longer, since every message in the
// window costs server memory.
const IamLogDuplicates = 2 * time.Minute

// Stream is the identity estate's log.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:           topics.IamLogStream,
		Subjects:       []string{topics.IamLogWildcard},
		SubjectPrefix:  topics.IamLogPrefix,
		MaxBytes:       IamLogMaxBytes,
		MaxRecordBytes: IamMaxRecordBytes,
		Duplicates:     IamLogDuplicates,
		Replay:         statelog.ReplayStrict,
		// Seven of the nine kinds. A barrier shares one subject across the
		// domain, so an expectation would serialise every linearizable read
		// and write an anchor row per read under the store's only writer;
		// the trim's trimmed record removes anchors, so arbitrated it would
		// write one (ADR-0080).
		ArbitratedKinds: arbitratedKinds(),
	}
}

// arbitratedKinds is the seven, derived from the enum: a list written twice is
// a kind arbitrated in one place and not the other, which wedges that subject
// the first time a gate drops a record.
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
// rather than be filed for later, answered from the envelope alone, since that
// is all a node has when it cannot decode the payload.
//
// Keyed on the op, not the kind: a removal rides the directory subject beside
// every enrolment, and a deferred removal is an off-boarded person still
// signing in on one node, which no later record corrects. Every gate is one
// question, [RecordEnvelope.InstallsGate], read rather than restated.
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return RecordEnvelope{Op: OpKind(env.Op)}.InstallsGate()
}

// Tables is every durable table this domain writes, with its class, built from
// the exported inventories. The scrub list, the identity claim and the local
// sweep all derive from it, so a missing table silently shortens all three.
//
// The one Divergent table is the operation ledger: every other column is a
// function of its record (sealing happens at the writer, never per node), but
// the ledger's `applied_at` is this node's own instant. So it stays out of the
// identity claim yet travels in a snapshot: an adopter without its donor's
// rows would decide again what the donor applied: a second enrolment, grant
// change or epoch bump.
func (Domain) Tables() map[string]statelog.TableClass {
	out := make(map[string]statelog.TableClass,
		len(ReproducibleTables)+len(MachineryTables))
	for _, table := range ReproducibleTables {
		out[table] = statelog.Replicated
	}
	for _, table := range MachineryTables {
		out[table] = statelog.Local
	}
	// The ledger travels: see [statelog.Domain.OpsTable], [MachineryTables].
	out[Domain{}.OpsTable()] = statelog.Divergent
	return out
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "iam_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope term.
func (Domain) ScopeIndex() string { return "iam_log_deferred_scope" }

// OpsTable is contract 3's first layer: what this node has already applied.
// The framework's columns only, and two horizons: a session subject's rows go
// after [SessionOpsRetention], every other kind's after the framework's month.
// The hourly sweep ranges over the stored subject, which carries the kind, and
// records that kind's loss apart from the table's (replicated migration 0041),
// so the publisher holds each kind to its own horizon: one hourly watermark
// would answer any other operation older than an hour `unknown`, and an
// unmarked sweep would let a session operation be decided again. The
// authentication trail's horizons are not these: the trail answers an audit
// obligation, the ledger the longest a client retries.
func (Domain) OpsTable() string { return "iam_ops" }

// GatesSeatAdmission reports that this domain's health does NOT gate seat
// admission; its copy is still judged on its own and goes out of service for
// its own operations (ADR-0079). The tracker and knowledge base say true (a
// node behind on them serves wrong turns), the vector domain false (compacted:
// a gap is coverage). This one says false because an agent seat never reads
// it: its authority comes from the org chart through internal/authz, so a
// stalled iam applier would stop the company's work for the wrong subsystem.
//
// The request path is gated instead: a node that has not applied a revocation
// answers a session bearer 503, never 401, which a browser reads as "sign in
// again" for everybody at once. Keeping a ledger without gating is legal: only
// a domain with NO operation ledger is barred from gating seat admission.
func (Domain) GatesSeatAdmission() bool { return false }

// ClaimsIdentity reports that every data node's Replicated tables must be
// byte-identical. True: one ordered log, one deterministic applier, and the
// sealed columns are sealed by the WRITER before publication, so every node
// writes the same ciphertext; per-node encryption would make a diverged
// applier undetectable. Asserted, not verified, like every domain's
// ([internal/worklet.Domain.ClaimsIdentity]).
func (Domain) ClaimsIdentity() bool { return true }

// BarrierTables is the empty set, DECLARED: the barrier writes no row, and
// declaring it lets the kind-completeness walk tell "this kind wrote nothing"
// from "nobody classified this kind".
var BarrierTables = map[string]statelog.TableClass{}
