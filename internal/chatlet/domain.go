package chatlet

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Domain is the company's own chat as the state-log framework sees it.
//
// A DECLARATION, not a behaviour: every method answers a question the
// framework asks before it applies anything, and a wrong answer fails silently.
//
// ONE LOG, TWO DISCIPLINES: a room's state contends on its own subject while
// its messages are sequenced by the applier, stated only in
// [statelog.StreamSpec.ArbitratedKinds]. Arbitrating messages would serialise
// the hottest subject behind itself; leaving rooms unarbitrated would let two
// writers rename a room to two names and keep both.
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "chat" }

// ChatLogMaxBytes is the ceiling this domain declares for its log. A node sizes
// the real stream from Tier A (`stream.chat_log_max_bytes`); unset asks for
// THIS, capped by the volume (`config.DerivedChatLogDivisor`), scaled with the
// other ceilings. A full log REFUSES an append rather than dropping the oldest
// record; the last sixteenth is the gate reserve ([statelog.Reservation]).
//
// EIGHT GIBIBYTES, an EXCURSION BUDGET, not a ratio to the tracker's: about
// 32 MB a day at the census ([ChatMessagesPerDay], [LinearizableReadsPerSeatDay])
// and all ceilings share one budget. A blocked trim fills it in about eight
// months at the census, three and a half at fifty thousand messages a day.
const ChatLogMaxBytes = 8 << 30

// ChatLogDuplicates is the window the broker collapses a repeated operation id
// in.
//
// AN OPTIMISATION, NEVER A MECHANISM: the operation ledger is what actually
// makes a retry idempotent, and this only saves the round trip. Two minutes,
// the window the tracker and the wiki both use, because it must outlast a
// publisher's whole retry budget and no longer — every message in the window
// costs memory on the server, and this is the log with the most of them.
const ChatLogDuplicates = 2 * time.Minute

// LinearizableReadsPerSeatDay is this log's READ MODEL: the linearizable reads
// a day each agent seat's chat reads put on it, which is what `census_drift`
// holds the log to and the barrier term in its ceiling ([ChatLogMaxBytes]).
//
// ONE HUNDRED AND TWENTY: thirty turns a seat a day ([ChatMessagesPerDay]'s
// basis) times four seat-level, so barrier, reads a turn makes: the turn-start
// thread read ([ThreadReader], ONE read even when it falls back during a
// rolling upgrade), the reply's room read, a room-name lookup and a search's
// visible set. Past twice that, seats are re-reading rooms in a loop. A surface
// reading this log at `linearizable` elsewhere adds its reads here. Not
// [statelog.LinearizableReadsPerSeatDay]: the census does not split reads.
const LinearizableReadsPerSeatDay = 120

// Stream is the chat domain's log.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:           topics.ChatLogStream,
		Subjects:       []string{topics.ChatLogWildcard},
		SubjectPrefix:  topics.ChatLogPrefix,
		MaxBytes:       ChatLogMaxBytes,
		MaxRecordBytes: MaxRecordBytes,
		Duplicates:     ChatLogDuplicates,
		Replay:         statelog.ReplayStrict,
		// TWO OF THE SIX KINDS — see [ObjectKind.Arbitrated] for why
		// none of the other four writes an anchor row. A message and a
		// barrier form no expectation at all; an eviction and a
		// generation do, and take it from the subject's last sequence
		// on the broker instead of from a row. The messages are the
		// load-bearing absence: an anchor row per post is a write on
		// the domain's hottest subject inside the transaction holding
		// this store's only writer, for an arbitration two people
		// talking never ask for.
		ArbitratedKinds: arbitratedKinds(),
	}
}

// arbitratedKinds is the two, derived from the enum rather than typed again —
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
// Answered from the envelope alone, all a node has when it cannot decode the
// payload. An eviction is a gate by its KIND; an erase and a prune by their OP,
// because both DELETE rows and a deferred deletion is a node still serving what
// every other node destroyed.
//
// IT ASKS [RecordEnvelope.InstallsGate] RATHER THAN RESTATING THE RULE: a
// second spelling would stop on an unreadable erase in one half of the package
// and defer it in the other.
func (Domain) InstallsGate(env statelog.Envelope) bool {
	return RecordEnvelope{
		Subject: Subject{Kind: ObjectKind(env.Kind), ID: env.Subject.ID},
		Op:      OpKind(env.Op),
	}.InstallsGate()
}

// NodeGate reports a node's eviction or readmission — the one record a write
// flagged [statelog.Request.NodeGate] may carry.
//
// BY ITS KIND, which nothing but a node's eviction or readmission is published
// under — and deliberately NOT [Domain.InstallsGate], which is wider here than
// on any other log: an ERASE and a PRUNE install gates too, by their op, on an
// ordinary room's subject. Both are ordinary writes somebody chose to make, and
// the flag excuses the three fences a node gate passes — the passed generation,
// a peer's truncated rows and the gate reserve — so held to the apply gate's
// answer, a prune flagged by mistake would spend the room kept for the eviction
// that unpins a full log, and pass every fence an ordinary write is stopped by.
func (Domain) NodeGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind) == KindEviction
}

// EvictionSubject is where a node's evictions and readmissions are published
// on this log — the [statelog.EvictionProbe] half, for the tracker's reason: a
// node the fleet re-anchored past never applies an eviction written after its
// applier stopped, so the log itself is the only place it can learn of one.
func (Domain) EvictionSubject(node string) statelog.Subject {
	return wire(EvictionSubject(node))
}

// Evicts decodes one record from a node's eviction subject: true for an
// eviction, false for the readmission that inverts one.
func (Domain) Evicts(payload []byte) (bool, error) {
	record, err := Decode(payload)
	if err != nil {
		return false, fmt.Errorf("chatlet: decode a record from an eviction subject: %w", err)
	}
	mutation, err := DecodeMutation(record)
	if err != nil {
		return false, fmt.Errorf("chatlet: decode the eviction: %w", err)
	}
	eviction, ok := mutation.(Eviction)
	if !ok {
		return false, fmt.Errorf("chatlet: the record on an eviction subject carries a %T",
			mutation)
	}
	return !eviction.Readmitted, nil
}

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "chat_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope term.
func (Domain) ScopeIndex() string { return "chat_log_deferred_scope" }

// OpsTable is contract 3's first layer: what this node has already applied.
func (Domain) OpsTable() string { return "chat_ops" }

// GatesSeatAdmission reports that a seat depends on this domain's copy: a
// node admits a seat only once it is established, and a copy out of service
// leaves the node's seats routed through its peers.
//
// TRUE, for Worklet's and Doclet's reason: a strict replay's stall is a
// FAULT rather than a coverage number. What a node behind on THIS log serves
// is worse than incomplete — it is confidently wrong in both directions. A
// seat woken by a message answers it against a thread whose last hour it
// cannot see, and the room's own sequence is minted from log order, so a node
// that has not applied a post has not minted the number every later post in
// that room depends on. A node behind on a compacted domain answers less
// completely and says so; there is no equivalent here.
func (Domain) GatesSeatAdmission() bool { return true }

// ClaimsIdentity reports that every node's Replicated tables must be
// byte-identical.
//
// TRUE, and load-bearing: the per-channel sequence is MINTED BY THE APPLIER
// from log order, so no writer can assert agreement on a transcript. Left out
// are the deferral tables (Local: this node's verdict on a record it could not
// read) and the operation ledger, the one Divergent table ([Domain.Tables]).
//
// ASSERTED AND NOT VERIFIED, on the terms
// [github.com/crewlet/crewlet/internal/worklet.Domain.ClaimsIdentity] states.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is the change feed's own consumer on this log: [FeedGroup], the
// same constant the [Translator] opens, and nobody else's. The trim's
// `feed_ack_floor` term waits for it, so answering empty would let the trim
// remove records nobody has been woken for yet — a message whose recipients
// are never told — and answering another domain's group would block this log's
// trim on a consumer nobody opens here.
func (Domain) FeedGroup() string { return FeedGroup }
