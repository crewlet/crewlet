package changefeed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/textcut"
)

// DeferReason is why this node handed a record back rather than settle it: the
// one thing that tells a roll's ordinary traffic from a record no node will
// read.
type DeferReason string

// The records a node hands back because it has decided nothing about them
// (#838):
//
//   - DeferNewerRecord: the envelope decoded and the record is a version a
//     newer build writes — a translator's future-version arm;
//   - DeferNewerEnvelope: the frame opened and the envelope is one a newer
//     build writes;
//   - DeferUnknownKey: the frame is sealed under a key this node does not
//     hold, which a peer that does can open;
//   - DeferUntranslatable: the translator failed on a record this build
//     should read — a defect, which an upgrade past it reads.
//
// The first three are DEFERRALS to a peer that can read the record, counted as
// `crewlet.tracker.feed.deferred` — a roll's ordinary minutes. The fourth is a
// record no build on this node can read, counted as
// `crewlet.tracker.feed.unreadable` and alarmed as `feed_unreadable`. All four
// back off alike ([DeferDelay]) and are dead-lettered alike once a record is
// [DeadLetterAge] old, under the reason that names them.
//
// A TAMPERED frame is none of them: no node should act on a frame this fleet
// did not write, so it is acked as before.
const (
	DeferNewerRecord    DeferReason = "newer_record"
	DeferNewerEnvelope  DeferReason = "newer_envelope"
	DeferUnknownKey     DeferReason = "unknown_key"
	DeferUntranslatable DeferReason = "untranslatable"
)

// Valid reports whether r is one of the reasons this build names.
func (r DeferReason) Valid() bool {
	switch r {
	case DeferNewerRecord, DeferNewerEnvelope, DeferUnknownKey, DeferUntranslatable:
		return true
	}
	return false
}

// Deferral is a record this node cannot read and a peer may: a translator
// answers it (its future-version arm wraps what it met in [Defer]) and an
// opener carries it on the [Message] it met it on ([Message.Deferred]).
//
// NEVER ACKED BARE. Every node pulls a domain's feed from ONE durable
// consumer, so a record this node acks is a record no peer ever sees: the
// wake a newer build, or a node holding the key, would have derived is gone
// fleet-wide (#838). A deferral is returned for redelivery instead — with a
// backoff ([DeferDelay]) — and dead-lettered once it is [DeadLetterAge] old,
// never left to circle for ever.
type Deferral struct {
	Reason DeferReason
	// Err is what this node met, for the log line.
	Err error
}

func (d *Deferral) Error() string {
	if d.Err == nil {
		return "changefeed: deferred to a peer (" + string(d.Reason) + ")"
	}
	return fmt.Sprintf("changefeed: deferred to a peer (%s): %v", d.Reason, d.Err)
}

func (d *Deferral) Unwrap() error { return d.Err }

// Defer is err as a deferral for reason: what a translator answers for a
// record this node cannot read and a peer can.
func Defer(reason DeferReason, err error) error {
	return &Deferral{Reason: reason, Err: err}
}

// deferralOf is the deferral err carries, if any.
func deferralOf(err error) (*Deferral, bool) {
	var d *Deferral
	if errors.As(err, &d) {
		return d, true
	}
	return nil, false
}

// DeferCap is the longest a deferral waits before it comes back: the domain
// consumer's own ack window (internal/queue/jetstream's domainConsumerAckWait,
// 30 s — held equal by its test), so a record handed back for a peer is never
// redelivered more slowly than one nobody answered at all.
const DeferCap = 30 * time.Second

// DeferDelay is how long a record delivered the delivered-th time waits
// before it comes back: [nakDelay] doubled with every delivery before it,
// capped at [DeferCap] — 2, 4, 8, 16, 30, 30 … seconds.
//
// A BACKOFF, NEVER AN IMMEDIATE REDELIVERY: a moment with only older peers
// up — the first node of a roll restarted onto the newer build and drained
// again — would otherwise spin the consumer, every node handing the record
// back to every other as fast as the broker can deliver it. The first wait
// is the feed's ordinary [nakDelay], so a roll's first deferral comes back as
// soon as a failed handle would.
func DeferDelay(delivered uint64) time.Duration {
	d := nakDelay
	for i := uint64(1); i < delivered && d < DeferCap; i++ {
		d *= 2
	}
	return min(d, DeferCap)
}

// DeadLetterAge is how old a deferred record grows — from the broker's
// instant for it ([Record.StoredAt]) — before the feed stops handing it back
// and dead-letters it.
//
// WHY A BOUND AT ALL: a deferral is a NAK with a delay, and the broker keeps
// a delayed NAK in the consumer's pending set until it is redelivered
// (nats-server v2.15.2, server/consumer.go:3297-3313), where it counts
// against MaxAckPending, and a consumer at that limit delivers no NEW record
// (consumer.go:5019-5022). The feed's group consumer sets none, so the server
// gives it 1000 (consumer.go:604, 697-705). A record no node will ever read —
// a key retired fleet-wide, bytes corrupt past the MAC — circling for ever
// would hold a slot for ever, and a thousand of them would stall every wake of
// the domain.
//
// WHY AN HOUR — A JUDGMENT CALL, stated as one. The windows a deferral serves
// are each one rolling restart of the fleet: a newer build's roll (ADR-0109
// names that window minutes), and the keyring rotation runbook's, which adds
// a key and restarts every node before anything is sealed under it
// (docs/concepts/secret-store.md, "Rotating the keyring"). An hour is an order
// of magnitude past either, so a roll's records reach a node that reads them
// long before it; and filling 1000 slots inside an hour would take a record no
// node can read every 3.6 seconds for the whole hour — which no retired key or
// corrupt frame produces. docs/guides/fleet.md says it too: a roll finishes
// within the hour, or its deferred wakes are dead-lettered (and replayed by
// the first node that can read them).
//
// A RECORD ITS ESTATE DATES NOTHING FOR (a zero [Record.StoredAt]) IS NEVER
// DEAD-LETTERED: nothing says how long it has been handed back, so it goes on
// being handed back with the backoff. Every state log's delivery carries the
// broker's instant, so only an estate that cannot say meets this.
const DeadLetterAge = time.Hour

// ErrForeignFrame is what [Records.Reopen] answers for a frame this fleet did
// not write — a MAC that fails under a key the node holds, or bytes that are
// not a frame at all. No node will ever act on one, so a replay drops its dead
// letter rather than keeping it for a build or a keyring that cannot help.
var ErrForeignFrame = errors.New("changefeed: the frame is not one this fleet wrote")

// DeadLetterVersion is the version of the [DeadLetter] header this build
// writes, and the newest it reads.
const DeadLetterVersion = 1

// deadLetterDetailBytes is how much of what a node met an entry keeps
// ([DeadLetter.Detail]).
//
// A QUARTER KIBIBYTE, because the header has to stay inside what a stored
// record keeps beside its payload: a record at its log's ceiling
// (statelog.MaxTransportRecordBytes, the transport's eight mebibytes less four
// kibibytes) is a frame of that plus its seal, under three hundred bytes, and
// the entry is that frame whole, so its header and what the transport frames
// it with ([queue.LetterFraming]) share the 3.7 KiB left of the transport's
// ceiling. Every other field is a name, a number or an instant; the detail is
// an error's own words, whose first quarter kibibyte names its cause.
//
// THE WORST CASE IS A DETAIL OF CONTROL CHARACTERS: the header is ASCII
// ([EncodeDeadLetter]), so every byte outside it is escaped, and the costliest
// escape per byte the detail holds is a control character's — or a DEL, or a
// byte that is not UTF-8 — at six bytes each, `\u001b`. A rune past the ASCII
// costs less for each of its bytes: two bytes are one escape of six, three
// bytes one of six, and four bytes — a rune above U+FFFF — a surrogate pair of
// two escapes, twelve bytes, three for each. So the detail is a kibibyte and a
// half at the worst, and the header stays inside the room for any record a
// log admits.
const deadLetterDetailBytes = 256

// DeadLetter is what a feed records of one record it stopped handing back
// ([DeadLetterAge]): what it was and why no node read it — the HEADER of the
// entry it keeps, which the queue keeps beside the record's frame
// ([queue.EventQueue.KeepDeadLetter]) and lists without it.
//
// SELF-CONTAINED: the entry's body is the frame the broker delivered, so a
// replay re-reads the record without its log — which trims it on its own
// schedule, and could have trimmed it before the node able to read it starts —
// and through the same verification a delivery passes ([Records.Reopen]),
// never trusting what the node that gave up made of it. It is kept for as long
// as every dead letter is (`CREWLET_DLQ`, the event stream's
// `stream.event_retention_hours`), and then it expires.
//
// THE FRAME IS NOT A FIELD: everything that reads an entry's header — the
// alarm's reading on every beat of every data node, an operator's listing, a
// discard looking for its entry — reads it without the frame, and only the
// replay, which acts on the record, reads the frame
// ([queue.EventQueue.DeadLetterFrame]).
type DeadLetter struct {
	V int `json:"v"`

	// Source and Group are the feed that kept it ([Source]).
	Source string `json:"source"`
	Group  string `json:"group"`

	// Stream, Seq, Gen and Key are where the record was delivered from: its
	// log, its sequence on it, the log's generation — zero where the node
	// that gave up never read the envelope — and the subject it arrived on.
	Stream string `json:"stream"`
	Seq    uint64 `json:"seq"`
	Gen    uint64 `json:"gen,omitempty"`
	Key    string `json:"key"`

	// OpID is the record's operation id, empty where the envelope was never
	// read.
	OpID string `json:"op_id,omitempty"`

	// Reason is why it was handed back, and Detail what the node that gave
	// up met, cut to a quarter kibibyte.
	Reason DeferReason `json:"reason"`
	Detail string      `json:"detail,omitempty"`

	// StoredAt is the broker's instant for the record, Delivered how many
	// times the broker had delivered it when the feed gave up, and
	// DeadLetteredAt when it did, on that node's clock.
	StoredAt       time.Time `json:"stored_at"`
	Delivered      uint64    `json:"delivered"`
	DeadLetteredAt time.Time `json:"dead_lettered_at"`
}

// at is where the record was delivered from, as [Records.Reopen] is told it:
// what the frame does not carry — and the generation, where the node that gave
// up had read it, which an opener that reads the envelope reads again.
func (d DeadLetter) at() Record {
	return Record{Position: d.Seq, Stream: d.Stream, Gen: d.Gen, Key: d.Key, StoredAt: d.StoredAt,
		Delivered: d.Delivered}
}

// EncodeDeadLetter is d as the header its entry is kept under: one line of
// compact JSON in printable ASCII ([queue.LetterHeader]), which the queue keeps
// beside the frame and every listing answers.
//
// ASCII, because the JetStream backend carries the header as a header of the
// message, `Crewlet-Dead-Letter`, and a NATS header's value is one line of
// ASCII (internal/queue's LetterHeader has the transport's reason). Compact
// JSON already escapes every control character, so no carriage return or line
// feed reaches the line; every rune past the ASCII, and the one character
// below it JSON leaves bare, DEL, is written as its JSON escape too —
// `é` — and a rune above U+FFFF as its UTF-16 SURROGATE PAIR, two escapes,
// `😀`, which is the only spelling JSON has for one: a single `\u`
// escape names four hex digits, so `ὠ0` would be read back as U+1F60 and a
// stray `0`. JSON reads the escapes back as the runes they spell
// ([DecodeDeadLetter]), so a header carries any text it carried before, at the
// cost [deadLetterDetailBytes] bounds.
//
// UNESCAPED `<`, `>` AND `&`, so a subject or a detail holding them costs its
// own byte rather than six.
//
// NOT THE FRAME, which the queue keeps as the entry's body, raw: carried in
// the header — or inside JSON at all, as base64, four bytes for every three —
// a record at its log's ceiling could never be kept, and would circle at the
// head of its consumer for ever.
func EncodeDeadLetter(d DeadLetter) ([]byte, error) {
	d.V = DeadLetterVersion
	d.Detail = textcut.Within(d.Detail, deadLetterDetailBytes)
	var line bytes.Buffer
	enc := json.NewEncoder(&line)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("changefeed: encode the dead letter of %s@%d: %w", d.Stream, d.Seq, err)
	}
	// Encode ends the document with a newline, which no header holds.
	return asciiOnly(bytes.TrimSuffix(line.Bytes(), []byte{'\n'})), nil
}

// asciiOnly is compact JSON with every byte outside printable ASCII written as
// the JSON escape of the rune it begins ([EncodeDeadLetter]): only a string
// can hold such a byte, and an escape is what JSON reads back as the rune.
func asciiOnly(doc []byte) []byte {
	out := make([]byte, 0, len(doc))
	for len(doc) > 0 {
		if b := doc[0]; b >= ' ' && b <= '~' {
			out = append(out, b)
			doc = doc[1:]
			continue
		}
		// encoding/json writes valid UTF-8 — a byte that is not is already
		// `�` — so this is a whole rune; one that is not decodes as
		// the replacement rune, which is what JSON would have written.
		r, size := utf8.DecodeRune(doc)
		doc = doc[size:]
		if r > 0xffff {
			hi, lo := utf16.EncodeRune(r)
			out = fmt.Appendf(out, `\u%04x\u%04x`, hi, lo)
			continue
		}
		out = fmt.Appendf(out, `\u%04x`, r)
	}
	return out
}

// DecodeDeadLetter reads an entry's header back, refusing one a newer build
// wrote — whose header this build cannot be sure it reads whole — and bytes
// that are not a header: an entry a subscription's backend kept, whose header
// is empty, among them.
func DecodeDeadLetter(header []byte) (DeadLetter, error) {
	if len(header) == 0 {
		return DeadLetter{}, errors.New("changefeed: the entry was kept with no header, so it is " +
			"no dead letter of a feed")
	}
	var version struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(header, &version); err != nil {
		return DeadLetter{}, fmt.Errorf("changefeed: read a dead letter's header: %w", err)
	}
	switch {
	case version.V < 1:
		return DeadLetter{}, errors.New("changefeed: the entry's header states no version, so it is no dead letter")
	case version.V > DeadLetterVersion:
		return DeadLetter{}, fmt.Errorf("changefeed: the dead letter is version %d, and this build reads "+
			"up to %d: a newer build kept it", version.V, DeadLetterVersion)
	}
	var d DeadLetter
	if err := json.Unmarshal(header, &d); err != nil {
		return DeadLetter{}, fmt.Errorf("changefeed: read a dead letter's header: %w", err)
	}
	return d, nil
}

// deadLetterHead is the topic every feed's dead letters are kept under:
// [DeadLetterTopic]'s first segment, and the head of [DeadLettersUnder].
const deadLetterHead = "changefeed"

// DeadLetterTopic is the topic half of a feed's dead-letter subject: its
// source's name under `changefeed.`.
//
// CHOSEN SO `dlq.changefeed.>` IS EVERY FEED'S DEAD LETTERS AND NOTHING ELSE'S:
// a subscription's are under the topic it subscribed to, every one of which is
// the engine's own `crewlet.` namespace or a test's, never this one. And the
// head an operator greps names the estate whose wake it is — the feed's
// [Source.Name], `work`, `page`, `chat` or `code`, the source column every
// published wake carries.
func DeadLetterTopic(src Source) string { return deadLetterHead + "." + src.Name }

// DeadLetterSubject is where a feed keeps what it dead-letters, and where its
// start replays them from: [topics.DeadLetter] of its [DeadLetterTopic] and its
// group. The group is the fleet's one position on a domain's log, declared
// once per domain and never shared, so one subject is every entry of one
// domain's feed, whichever node kept it.
func DeadLetterSubject(src Source) string {
	return topics.DeadLetter(DeadLetterTopic(src), src.Group)
}

// DeadLettersUnder is the subtree every feed's dead-letter subject lies under
// ([topics.DeadLettersUnder] of the feeds' head): what the alarm's reading
// counts on every beat ([queue.EventQueue.DeadLetterMarks]) to tell whether any
// feed's entries moved, in one request whatever they hold.
func DeadLettersUnder() string { return topics.DeadLettersUnder(deadLetterHead) }
