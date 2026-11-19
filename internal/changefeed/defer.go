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

// The records a node hands back because it has decided nothing about them:
//
//   - DeferNewerRecord: a newer build's record version (future-version arm);
//   - DeferNewerEnvelope: a newer build's envelope;
//   - DeferUnknownKey: sealed under a key this node lacks and a peer may hold;
//   - DeferUntranslatable: the translator failed on a record this build should
//     read, a defect an upgrade reads past.
//
// The first three are DEFERRALS, counted as `crewlet.tracker.feed.deferred`;
// the fourth as `crewlet.tracker.feed.unreadable`, alarmed as `feed_unreadable`.
// All back off alike ([DeferDelay]) and are dead-lettered at [DeadLetterAge].
// A TAMPERED frame is none of them: it is acked.
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

// DeadLetterAge is how old a deferred record grows ([Record.StoredAt]) before
// the feed dead-letters it. WHY A BOUND: a delayed NAK stays pending against
// MaxAckPending (the server's 1000, as the group sets none), and at that limit
// the consumer delivers no NEW record (nats-server v2.15.2, consumer.go:3297-
// 3313, 5019-5022, 697-705); a thousand unreadable records stall the wakes.
//
// WHY AN HOUR, a judgment call: a deferral serves one rolling restart
// (ADR-0109, minutes; the keyring rotation, docs/concepts/secret-store.md). An
// hour is an order of magnitude past either, and 1000 slots in it is an
// unreadable record every 3.6 seconds, which nothing produces
// (docs/guides/fleet.md). A ZERO [Record.StoredAt] IS NEVER DEAD-LETTERED: its
// age is unknown.
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
// A QUARTER KIBIBYTE, because the header must fit beside the payload: a record
// at its log's ceiling (statelog.MaxTransportRecordBytes, 8 MiB less 4 KiB) is
// a frame plus a seal under 300 bytes, kept whole, so the header and the
// transport's framing ([queue.LetterFraming]) share the 3.7 KiB left.
//
// THE WORST CASE IS CONTROL CHARACTERS: the header is ASCII
// ([EncodeDeadLetter]), so a control character, a DEL or a non-UTF-8 byte
// costs six bytes; a wider rune costs less per byte. So the detail is at
// worst a KiB and a half, inside the room for any record a log admits.
const deadLetterDetailBytes = 256

// DeadLetter is what a feed records of one record it stopped handing back
// ([DeadLetterAge]): the HEADER of the entry it keeps beside the record's
// frame ([queue.EventQueue.KeepDeadLetter]), which listings read without it.
//
// SELF-CONTAINED: the entry's body is the frame the broker delivered, so a
// replay re-reads the record without its log (which may have trimmed it) and
// through a delivery's verification ([Records.Reopen]), never trusting what
// the node that gave up made of it. It expires with every dead letter
// (`CREWLET_DLQ`, `stream.event_retention_hours`). THE FRAME IS NOT A FIELD:
// only the replay reads it ([queue.EventQueue.DeadLetterFrame]).
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
// compact JSON in printable ASCII ([queue.LetterHeader]), beside the frame.
//
// ASCII, because JetStream carries it as the message header
// `Crewlet-Dead-Letter`, one line of ASCII. Every rune past the ASCII, and
// DEL, is a JSON escape, a rune above U+FFFF its UTF-16 SURROGATE PAIR (one
// four-digit escape would read back as another rune plus a stray digit);
// [DecodeDeadLetter] reads them back, at the cost [deadLetterDetailBytes]
// bounds. `<`, `>` and `&` stay unescaped. NOT THE FRAME, which is the body,
// raw: in the header, or as base64, a record at its log's ceiling could never
// be kept.
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
