package doclet

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Domain is the knowledge base as the state-log framework sees it.
//
// A DECLARATION, not a behaviour: every method answers a question the
// framework asks before applying anything, and a wrong answer fails silently.
// Which tables exist decides what a donated snapshot scrubs and an identity
// claim compares; which kinds arbitrate decides where an anchor row is
// written; whether a record installs a gate decides whether an unknown version
// stops the applier or is filed for later.
//
// THE FRAMEWORK'S THIRD DOMAIN, its second strictly-ordered: two STRICT
// domains on one node make the checkpoint, readiness input and identity claim
// per-domain values, which one strict domain cannot distinguish.
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "pages" }

// PagesLogMaxBytes is the ceiling this domain declares for its log, which the
// framework's own suites provision the stream with. A NODE DOES NOT CREATE THE
// STREAM AT IT: each ceiling is a reservation the broker grants in full, so the
// engine sizes the logs together from Tier A (`stream.pages_log_max_bytes`).
//
// Crossing it REFUSES an append rather than dropping the oldest record: nothing
// here is derivable, so shedding is data loss.
//
// FOUR GIBIBYTES, a quarter of the tracker's, from the corpus: the reference
// company files 100 000 tasks and 300 000 comments a year against a few
// thousand pages, mostly SAVES. At a 512 KiB body cap and the modelled edit
// rate a completely blocked trim reaches this in about five years.
const PagesLogMaxBytes = 4 << 30

// PagesMaxRecordBytes is the largest record this domain publishes
// ([statelog.StreamSpec.MaxRecordBytes]).
//
// THE LARGEST THE TRANSPORT CARRIES ([statelog.MaxTransportRecordBytes]), for
// the tracker's reason: at this log's gibibyte floor a sixteenth of the
// ceiling is already the larger term of the gate reserve, so a smaller
// declaration would change no reserve — and a page's body is the largest value
// any domain carries, bounded by its own cap.
const PagesMaxRecordBytes = statelog.MaxTransportRecordBytes

// PagesLogDuplicates is the window the broker collapses a repeated operation
// id in.
//
// AN OPTIMISATION, NEVER A MECHANISM: the operation ledger is what actually
// makes a retry idempotent, and this only saves the round trip. Two minutes,
// the same window the tracker uses, because it must outlast a publisher's
// whole retry budget and no longer — every message in the window costs memory
// on the server.
const PagesLogDuplicates = 2 * time.Minute

// Stream is the knowledge base's log, at the domain's whole budget.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:           topics.PagesLogStream,
		Subjects:       []string{topics.PagesLogWildcard},
		SubjectPrefix:  topics.PagesLogPrefix,
		MaxBytes:       PagesLogMaxBytes,
		MaxRecordBytes: PagesMaxRecordBytes,
		Duplicates:     PagesLogDuplicates,
		Replay:         statelog.ReplayStrict,
		// FIVE OF THE SEVEN KINDS. A barrier shares one subject across
		// the whole domain, so an expectation there would serialise
		// every linearizable read behind every other one and write an
		// anchor row per read into the transaction holding this store's
		// only writer; and the trim's trimmed record removes anchors, so
		// arbitrated it would write one of its own (ADR-0080).
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

// NodeGate reports a node's eviction or readmission — the one record a write
// flagged [statelog.Request.NodeGate] may carry.
//
// BY ITS KIND, which nothing but a node's eviction or readmission is published
// under — and deliberately not InstallsGate: the purge is this log's other
// gate, and it is an ordinary write that the gate reserve and the fences a
// node gate is excused must hold.
func (Domain) NodeGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind) == KindEviction
}

// EvictionSubject is where a node's evictions and readmissions are published
// on this log — the [statelog.EvictionProbe] half, for the tracker's reason: a
// node the fleet re-anchored past never applies an eviction written after its
// applier stopped.
func (Domain) EvictionSubject(node string) statelog.Subject {
	subject := EvictionSubject(node)
	return statelog.Subject{Kind: string(subject.Kind), ID: subject.ID}
}

// Evicts decodes one record from a node's eviction subject: true for an
// eviction, false for the readmission that inverts one.
func (Domain) Evicts(payload []byte) (bool, error) {
	record, err := Decode(payload)
	if err != nil {
		return false, fmt.Errorf("doclet: decode a record from an eviction subject: %w", err)
	}
	mutation, err := DecodeMutation(record)
	if err != nil {
		return false, fmt.Errorf("doclet: decode the eviction: %w", err)
	}
	eviction, ok := mutation.(Eviction)
	if !ok {
		return false, fmt.Errorf("doclet: the record on an eviction subject carries a %T",
			mutation)
	}
	return !eviction.Readmitted, nil
}

// Tables is every durable table this domain writes, with its class.
//
// THE SCRUB LIST, THE IDENTITY CLAIM AND THE LOCAL SWEEP ARE ALL DERIVED FROM
// THIS MAP, so a missing table leaves three lists silently short. It is built
// from the exported inventories, not typed a third time.
//
// ALL FOUR CLASSES ARE PRESENT, and two tables are Divergent. `pages_skills`
// is THIS BUILD'S parser answering about its rules, recomputed on every apply,
// so nodes on different builds legitimately disagree: it travels in a snapshot
// (a joiner wants the answer, not a rebuild) and is excluded from the identity
// claim (a rolling upgrade would report a divergence that resolves itself).
// The operation ledger is the other, as its line below says.
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
	// THE LEDGER TRAVELS — see [statelog.Domain.OpsTable] and
	// [MachineryTables].
	out[Domain{}.OpsTable()] = statelog.Divergent
	return out
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "pages_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope term.
func (Domain) ScopeIndex() string { return "pages_log_deferred_scope" }

// OpsTable is contract 3's first layer: what this node has already applied.
func (Domain) OpsTable() string { return "pages_ops" }

// GatesSeatAdmission reports that a seat depends on this domain's copy.
//
// TRUE, for the tracker's reason: every seat's tools read this log on every
// turn, and a strict replay's stall is a FAULT rather than a coverage number.
// A node behind on the pages log serves a page body that is simply wrong — an
// older paragraph presented as the current one — where a node behind on a
// compacted domain serves an answer that is merely less complete and says so.
// Its copy is judged like every domain's, and a wrong one goes out of service
// for the knowledge base's operations alone — ADR-0079.
func (Domain) GatesSeatAdmission() bool { return true }

// ClaimsIdentity reports that every data node's Replicated tables must be
// byte-identical.
//
// TRUE: N copies of one SQL state from one ordered log and one deterministic
// applier.
//
// ASSERTED AND NOT VERIFIED: nothing builds the published, compared checksum
// that would make "should be identical" something a fleet reports on. See
// [internal/worklet.Domain.ClaimsIdentity] for the argument and the one column
// a naive checksum trips over; every column here is owned by a record, so that
// exclusion does not arise.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is the change feed's own consumer on this log: [FeedGroup], the
// same constant the [Translator] opens, and NOT the tracker's. The trim once
// read the tracker's group on this log too — a consumer that never exists
// here — so its feed term permitted nothing and the knowledge base's log was
// never trimmed at all.
func (Domain) FeedGroup() string { return FeedGroup }
