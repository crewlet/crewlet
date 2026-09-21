package chat

import (
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
// THE FRAMEWORK'S FOURTH DOMAIN, and its THIRD strictly-ordered one. What it
// adds over the three before it is a single log carrying TWO ARBITRATION
// DISCIPLINES — a room's state contends on its own subject while the messages
// in it are additive — and the only place that difference can be stated is
// [statelog.StreamSpec.ArbitratedKinds], because arbitration is a property of
// the subject kind and a stream has no field for it. A domain that declared
// its messages arbitrated would serialise the hottest subject in the company
// behind itself; one that declared its rooms additive would let two writers
// rename a room to two different names and keep both.
type Domain struct{}

// Name is the register key, the manifest key and the operator's own column.
func (Domain) Name() string { return "chat" }

// ChatLogMaxBytes is the ceiling this domain declares for its log, which is
// what the framework's own suites provision the stream with.
//
// A NODE DOES NOT CREATE THE STREAM AT IT. Every state log's ceiling is a
// reservation the broker grants in full, so the engine sizes them together
// from Tier A (`stream.chat_log_max_bytes`) inside what the broker can
// actually grant — the arrangement the wiki's log was added to after a fixed
// reservation on top of an already-divided budget became the one thing a small
// disk refused.
//
// Crossing a ceiling REFUSES an append rather than dropping the oldest record.
// On this log the refusal is a message somebody typed and could not send,
// which is the one failure a chat system may not have — and it is still the
// better half of the trade, because the alternative is a year of a company's
// conversation shed silently. Nothing here is derivable from anything else.
//
// EIGHT GIBIBYTES: half the tracker's, twice the wiki's, and the ratio is the
// census rather than a guess. Chat is the highest-VOLUME domain and carries
// the SMALLEST records — [ChatMessagesPerDay] is twenty thousand where the
// reference company files a few thousand work-item commits a day, but a remark
// is a couple of sentences and a routing snapshot, where a task's commit
// carries a description, a field set and the deltas of both. At a mean record
// near a kibibyte and a half that is about 30 MB a day, so a completely
// blocked trim — the failure a ceiling exists for — reaches this in roughly
// nine months, and at the supported fifty thousand a day in about four. A
// healthy trim holds the window at days, so a log anywhere near this number is
// an unmistakable operator failure rather than a surprise.
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

// Stream is the chat domain's log.
func (Domain) Stream() statelog.StreamSpec {
	return statelog.StreamSpec{
		Name:          topics.ChatLogStream,
		Subjects:      []string{topics.ChatLogWildcard},
		SubjectPrefix: topics.ChatLogPrefix,
		MaxBytes:      ChatLogMaxBytes,
		Duplicates:    ChatLogDuplicates,
		Replay:        statelog.ReplayStrict,
		// TWO OF THE SIX KINDS — see [ObjectKind.Arbitrated] for why
		// each of the other four pays nothing for an expectation it
		// never forms. The messages are the load-bearing absence: an
		// anchor row per post is a write on the domain's hottest
		// subject inside the transaction holding this store's only
		// writer, for an arbitration two people talking never ask for.
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

// DeferredTable is where a record this build cannot decode is retained.
func (Domain) DeferredTable() string { return "chat_log_deferred" }

// ScopeIndex is the deferred record's blast radius, one row per scope term.
func (Domain) ScopeIndex() string { return "chat_log_deferred_scope" }

// OpsTable is contract 3's first layer: what this node has already applied.
func (Domain) OpsTable() string { return "chat_ops" }

// ChatOpsRetention is how long a row in `chat_ops` is kept.
//
// SEVEN DAYS, NOT THE FRAMEWORK'S THIRTY. The ledger's size is a function of
// the domain's OWN COMMIT RATE and of nothing else — one row per applied
// record, on every node, for as long as this says — and [statelog.OpsRetention]
// is sized for the tracker's census of a few thousand commits a day. This
// domain's rate is [ChatMessagesPerDay] plus every edit, reaction and
// membership write beside it, an order of magnitude above that, so inheriting
// the default would buy an order of magnitude more table on every node for a
// horizon nothing asks for: what the ledger resolves is an AMBIGUOUS PUBLISH,
// and a publisher that has not resolved one within a week is a process that
// died long ago.
//
// SEVEN RATHER THAN ONE, because the horizon is bounded by the CLIENT THAT
// RETRIES rather than by the table it costs. A scheduled seat can hold an
// operation id across a weekend with a public holiday behind it, and a ledger
// swept underneath one is the same message posted into a room twice. Seven
// days also clears the floor the framework states — the sweep's own
// fifteen-minute tick, [github.com/crewlet/crewlet/internal/maintenance.Interval]
// — by three orders of magnitude, so the declared number describes the table
// rather than what the sweep would have raised it to.
const ChatOpsRetention = 7 * 24 * time.Hour

// OpsRetention is this domain's own horizon for its operation ledger.
func (Domain) OpsRetention() time.Duration { return ChatOpsRetention }

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
// can assert — it is exactly what this claim is about. Every column this
// domain writes is owned by a record or derived from one inside the same
// transaction, so there is no Divergent table to exclude ([Domain.Tables]).
//
// ASSERTED AND NOT VERIFIED, on the terms
// [github.com/crewlet/crewlet/internal/tracker.Domain.ClaimsIdentity] states:
// a checksum over each node's identity-claimed tables, published and compared,
// is what would turn "should be identical" into something a fleet reports on,
// and nothing builds one.
func (Domain) ClaimsIdentity() bool { return true }
