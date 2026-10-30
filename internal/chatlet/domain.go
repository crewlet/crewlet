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
// framework asks before it applies anything, and each one has a failure mode
// that is silent if the answer is wrong. What tables exist decides what a
// donated snapshot scrubs and what an identity claim compares; which kinds
// arbitrate decides where an anchor row is written; whether a record installs
// a gate decides whether an unknown version stops the applier or is filed for
// later.
//
// THE FRAMEWORK'S NINTH DOMAIN, and its SEVENTH strictly-ordered,
// identity-claiming one. What it adds over the eight before it is a single log
// carrying TWO DISCIPLINES — a room's state contends on its own subject while
// the messages in it are sequenced and decided by the applier — and the only
// place that
// difference can be stated is [statelog.StreamSpec.ArbitratedKinds], because
// arbitration is a property of the subject kind and a stream has no field for
// it. A domain that declared its messages arbitrated would serialise the
// hottest subject in the company behind itself; one that declared its rooms
// unarbitrated would let two writers rename a room to two different names and
// keep both.
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "chat" }

// ChatLogMaxBytes is the ceiling this domain declares for its log, which is
// what the framework's own suites provision the stream with.
//
// A NODE DOES NOT SIMPLY CREATE THE STREAM AT IT. Every state log's ceiling is
// a reservation the broker grants in full, so the engine sizes them together
// from Tier A (`stream.chat_log_max_bytes`) inside what the broker can
// actually grant — the arrangement the wiki's log was added to after a fixed
// reservation on top of an already-divided budget became the one thing a small
// disk refused. An unset field asks for THIS number, capped by the volume (a
// sixteenth of its free space, `config.DerivedChatLogDivisor`, which says why
// so small) and scaled with the others, which is why it is declared here
// beside the census rather than in Tier A.
//
// Crossing a ceiling REFUSES an append rather than dropping the oldest record.
// On this log the refusal is a message somebody typed and could not send,
// which is the one failure a chat system may not have — and it is still the
// better half of the trade, because the alternative is a year of a company's
// conversation shed silently. Nothing here is derivable from anything else.
//
// AND IT COMES A SIXTEENTH SHORT OF THE CEILING. This log claims identity, so
// it keeps the framework's gate reserve ([statelog.Reservation]): ordinary
// writes — every post, every reaction, every prune — are refused `log_full`
// at fifteen sixteenths of whatever ceiling the node created it at, and the
// last sixteenth is held for the one record that can unpin a log a gone node
// filled, that node's own eviction. At this log's records ([MaxRecordBytes])
// the reservation's fleet term is far under a sixteenth of any ceiling the
// field accepts, so the sixteenth is what governs.
//
// EIGHT GIBIBYTES, an EXCURSION BUDGET rather than a ratio to the tracker's.
// Chat is the highest-VOLUME domain and carries the SMALLEST records —
// [ChatMessagesPerDay] is twenty thousand where the reference company files a
// few thousand work-item commits a day, but a remark is a couple of sentences
// and a routing snapshot, where a task's commit carries a description, a field
// set and the deltas of both. At a mean record near a kibibyte and a half that
// is about 31 MB a day of messages, and the log's barriers add about 1.7 MB
// more ([LinearizableReadsPerSeatDay] × the reference company's 100 seats ×
// the ≈ 140 bytes a signed barrier costs) — about 32 MB a day, a little FASTER
// than the tracker's log grows, so the census ratio the wiki's ceiling keeps
// to the tracker's would put this above the tracker's 16 GiB, not at half of
// it. It is not that ratio because every node's ceilings share one budget, and
// the largest ask in it would take the vector changelog's room for a model
// change (Tier A's field says by how much). It is sized instead for the
// failure a ceiling exists for: a completely blocked trim spends the fifteen
// sixteenths ordinary writes may use in about eight months at the census, and
// in about three and a half at the supported fifty thousand a day — where the
// whole of it is created, which an embedded broker does from about 662 GiB
// free; on a smaller volume the ask is capped and scaled with the others, and
// Tier A's field states what that costs. A healthy trim holds the window at
// days, so a log anywhere near this number is an unmistakable operator failure
// rather than a surprise.
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
// ONE HUNDRED AND TWENTY: thirty turns a seat a day — the reference company's
// 3 000 over its 100 seats, the turns [ChatMessagesPerDay] is derived from —
// times the four chat reads a turn is declared to make, each at the seat's own
// level ([statelog.SurfaceSeat]), so each a barrier: the turn-start thread read
// ([ThreadReader]), the room read a reply is written from, the lookup a room's
// name resolves through, and the visible set a search intersects. Past twice
// it — eight a turn — seats are re-reading rooms in a loop, which is exactly
// the drift worth telling an operator about. A surface that starts reading
// this log at `linearizable` somewhere else brings its own reads into this
// number, in the same change.
//
// ITS OWN FIGURE, not the tracker's [statelog.LinearizableReadsPerSeatDay]
// borrowed: the census does not say how a company's reads split between the
// tracker and the knowledge base, so each of those declares all of them, but
// what a chat turn reads is this domain's own tools and its own thread block,
// counted here.
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
// Answered from the envelope alone, because that is all a node has when it
// cannot decode the payload. An eviction is a gate by its KIND; an erase and a
// prune are gates by their OP, on an ordinary channel subject, because both
// DELETE rows and a deferred deletion is a node still serving what every other
// node destroyed.
//
// IT ASKS [RecordEnvelope.InstallsGate] RATHER THAN RESTATING THE RULE, which
// is the whole of why this method is three lines. The rule has two conditions
// and the framework reaches it through a translated envelope, so a second
// spelling here would be a build that stops on an unreadable erase in one half
// of the package and defers it in the other — and the deferring half is the
// one that runs.
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

// ReadinessInput reports that this domain's health gates seat admission.
//
// TRUE, for the tracker's and the wiki's reason: a strict replay's stall is a
// FAULT rather than a coverage number. What a node behind on THIS log serves
// is worse than incomplete — it is confidently wrong in both directions. A
// seat woken by a message answers it against a thread whose last hour it
// cannot see, and the room's own sequence is minted from log order, so a node
// that has not applied a post has not minted the number every later post in
// that room depends on. A node behind on a compacted domain answers less
// completely and says so; there is no equivalent here.
func (Domain) ReadinessInput() bool { return true }

// ClaimsIdentity reports that every node's Replicated tables must be
// byte-identical.
//
// TRUE, and here the claim is unusually load-bearing: the per-channel sequence
// is MINTED BY THE APPLIER from log order rather than carried on the record,
// so two nodes agreeing about a room's transcript is not a property any writer
// can assert — it is exactly what this claim is about. Every column of every
// ROW table is owned by a record or derived from one inside the same
// transaction, so what the claim leaves out is the log's own machinery: the
// deferral tables, which are Local — this node's verdict on a record it could
// not read — and the operation ledger, the one Divergent table, whose
// `applied_at` is each node's own clock ([Domain.Tables]).
//
// ASSERTED AND NOT VERIFIED, on the terms
// [github.com/crewlet/crewlet/internal/worklet.Domain.ClaimsIdentity] states:
// a checksum over each node's identity-claimed tables, published and compared,
// is what would turn "should be identical" into something a fleet reports on,
// and nothing builds one.
func (Domain) ClaimsIdentity() bool { return true }

// FeedGroup is the change feed's own consumer on this log: [FeedGroup], the
// same constant the [Translator] opens, and nobody else's. The trim's
// `feed_ack_floor` term waits for it, so answering empty would let the trim
// remove records nobody has been woken for yet — a message whose recipients
// are never told — and answering another domain's group would block this log's
// trim on a consumer nobody opens here.
func (Domain) FeedGroup() string { return FeedGroup }
