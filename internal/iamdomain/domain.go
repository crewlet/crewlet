package iamdomain

import (
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Domain is the identity estate as the state-log framework sees it.
//
// A DECLARATION, not a behaviour: every method answers a question the framework
// asks before it applies anything, and each one has a failure mode that is
// silent if the answer is wrong. What tables exist decides what a donated
// snapshot scrubs and what an identity claim compares; which kinds arbitrate
// decides where an anchor row is written; whether a record installs a gate
// decides whether an unknown version stops the applier or is filed for later.
//
// THE FRAMEWORK'S FIFTH DOMAIN, and its fourth strictly-ordered one. What it
// adds over the four before it is a domain whose ARBITRATION IS THE
// CONSTRAINT: there is no unique index anywhere in this estate and there
// cannot be one, so two people taking one address are kept apart by the
// subject they contend on and by nothing else. `subject.go` argues it.
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "iam" }

// IamLogMaxBytes is the ceiling this domain declares for its log, which is
// what the framework's own suites provision the stream with.
//
// A NODE DOES NOT CREATE THE STREAM AT IT. Every state log's ceiling is a
// reservation the broker grants in full, so the engine sizes them together
// from Tier A (`stream.iam_log_max_bytes`) inside what the broker can actually
// grant.
//
// Crossing a ceiling REFUSES an append rather than dropping the oldest record:
// nothing on this stream is derivable from anything else, so shedding history
// is data loss with a tidy name — and here the history that would be shed is
// the authentication trail.
//
// # HALF A GIBIBYTE, and why it is neither the chart's number nor the floor
//
// It is sized from THIS domain's corpus, which is dominated by one thing:
// SESSIONS. People, credentials and invitations are hundreds of records a year
// at any company that fits on one broker — a chart's order of magnitude. A
// session is a record when it opens and a record when it closes, and nothing
// in between, because a rotation id is DERIVED rather than recorded; so the
// domain's write rate is roughly (people × sign-ins a day × 2).
//
// At the reference company — the one docs/guides/retention.md forecasts, whose
// 3 000 turns a day and 50 projects put it at a low hundreds of people — a
// pessimistic three new sessions per person per day is about 220 000 session
// records a year, and at the ~700 bytes a signed record costs on the wire that
// is around 285 MB a year with everything else folded in. Half a gibibyte is
// therefore about eighteen months of a COMPLETELY BLOCKED trim at the
// pessimistic rate and about five years at a realistic one — and a blocked
// trim is not a quiet state, it raises an alarm and holds the backup age
// against the operator from the first tick.
//
// IT IS NOT THE CHART'S 64 MiB, because the chart genuinely does not grow: it
// changes when somebody is hired, moved or promoted. This one grows every
// morning. At 64 MiB the pessimistic rate fills the log in under three months,
// which is a window that could refuse an append before anybody got back from
// leave.
//
// IT IS NOT THE GIBIBYTE [engine.MinDomainCeiling] names either, and the chart
// is why: the broker grants a stream its whole ceiling when it creates it, so
// this number is free space a node must have BEFORE IT CAN BOOT AT ALL, and a
// fifth domain at the framework floor raises that by a gibibyte for a log that
// will not fill one. Half is the smallest power of two that clears a year at
// the pessimistic rate with margin.
//
// An operator running a ten-person company writes a fiftieth of that and sets
// the Tier A field down to its 64 MiB floor; one running a thousand people
// sets it up. That is what the field is for.
const IamLogMaxBytes = 512 << 20

// IamLogDuplicates is the window the broker collapses a repeated operation id
// in.
//
// AN OPTIMISATION, NEVER A MECHANISM: the operation ledger is what actually
// makes a retry idempotent, and this only saves the round trip. Two minutes,
// the same window every other domain uses, because it must outlast a
// publisher's whole retry budget and no longer — every message in the window
// costs memory on the server.
const IamLogDuplicates = 2 * time.Minute

// Stream is the identity estate's log.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:          topics.IamLogStream,
		Subjects:      []string{topics.IamLogWildcard},
		SubjectPrefix: topics.IamLogPrefix,
		MaxBytes:      IamLogMaxBytes,
		Duplicates:    IamLogDuplicates,
		Replay:        statelog.ReplayStrict,
		// NINE OF THE TEN KINDS. A barrier shares one subject across the
		// whole domain, so an expectation there would serialise every
		// linearizable read behind every other one and write an anchor
		// row per read into the transaction holding this store's only
		// writer.
		ArbitratedKinds: arbitratedKinds(),
	}
}

// arbitratedKinds is the nine, derived from the enum rather than typed again —
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
// ORDINARY PERSON SUBJECT, so a reader that keyed on the kind would defer it —
// and a deferred removal here is not a stale row, it is a person the company
// off-boarded still signing in on one node, with no later record that ever
// corrects it because nothing names a removed person again. An eviction does
// have a kind of its own and is answered through the same op, so the two gates
// are one question asked once: it is [RecordEnvelope.InstallsGate], read
// rather than restated.
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
// wrote it, including the sealed ones, because the sealing happens at the
// WRITER before publication. A per-node re-encryption would have made this
// domain's rows legitimately different on every node and its identity claim
// meaningless — which is exactly why it does not happen.
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
func (Domain) DeferredTable() string { return "iam_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope term.
func (Domain) ScopeIndex() string { return "iam_log_deferred_scope" }

// OpsTable is contract 3's first layer: what this node has already applied.
//
// ONE OPS HORIZON FOR THE WHOLE DOMAIN, which is why the table takes the
// framework's four columns and nothing else. A `kind` column and an index over
// (kind, applied_at) would be a second horizon — a claim that a session's op
// id may be swept on a different schedule from an enrolment's — and two
// horizons on one ledger is a retry that resolves against a history half of
// which has been deleted.
//
// The authentication TRAIL has two horizons and this ledger has one, which is
// not a contradiction: the trail is what a person reads, and the ledger is what
// a publisher resolves an ambiguous write against. Their retentions answer
// different questions and are measured against different things — an audit
// obligation, and the longest a client will retry.
func (Domain) OpsTable() string { return "iam_ops" }

// ReadinessInput reports that this domain's health does NOT gate seat
// admission — and it is the FIRST strictly-ordered domain to say so, which is
// why this is the longest answer in the file.
//
// The tracker, the knowledge base and the chart all say true, and the argument
// is the same each time: a node behind on them serves a turn from state that
// is wrong, so admitting a seat there is confident wrong output. The vector
// domain says false because it is COMPACTED and a gap is a coverage number.
// This one says false for a third reason, and stating it is the point.
//
// AN AGENT SEAT NEVER READS THIS DOMAIN. A seat's principal is its own handle,
// its authority is decided by internal/authz from the ORG CHART, and its work
// arrives on its mailbox. Nothing in a turn asks who the people are. So a node
// whose iam applier has stalled runs every seat it holds exactly as correctly
// as a node whose applier is current — and shedding a company's seats because
// a human cannot sign in would be an outage caused by the wrong subsystem, at
// the moment an operator most needs the company still working so they can go
// and fix it.
//
// WHAT THE STALL MUST GATE INSTEAD IS THE REQUEST PATH, and it does, one layer
// up: a node that has not applied a revocation answers a session bearer with
// 503 — never 401, because a browser reads 401 as "sign in again" and one
// stalled applier would stampede the identity provider. That refusal is
// per-request, it is where the staleness actually matters, and it is the one
// place that can tell "this node is behind" from "this person is not allowed".
//
// The pairing the framework asserts is satisfied the other way round: a domain
// with NO operation ledger may not gate seat admission, and this one keeps a
// ledger and does not gate it. That is a legal combination and this is the
// domain that makes it a real one.
func (Domain) ReadinessInput() bool { return false }

// ClaimsIdentity reports that every node's Replicated tables must be
// byte-identical.
//
// TRUE. N copies of one SQL state, derived from one ordered log by one
// deterministic applier — and the sealed columns do not weaken it, because a
// person's name and address are sealed by the WRITER before publication rather
// than by each node on the way in. Every node writes the same ciphertext.
//
// That is a design constraint rather than an observation: per-node encryption
// would have made a determinism comparison impossible to write, and a domain
// whose rows legitimately differ everywhere has no way to notice that one
// node's applier has diverged.
//
// ASSERTED AND NOT VERIFIED, like every other domain's — see
// [internal/tracker.Domain.ClaimsIdentity] for the checksum nobody builds.
func (Domain) ClaimsIdentity() bool { return true }

// BarrierTables is the empty set, DECLARED.
//
// The barrier writes no row on any node, and stating that explicitly is what
// stops a no-op record slipping through the kind-completeness walk by writing
// nothing: "this kind wrote nothing" and "nobody classified this kind" are the
// same observation, and only one of them is correct.
var BarrierTables = map[string]statelog.TableClass{}
