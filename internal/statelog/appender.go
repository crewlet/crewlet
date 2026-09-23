package statelog

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
)

// Appender is the broker, as narrowly as the framework needs it: one
// conditional append, and the one query the rejection path discriminates
// with.
//
// DECLARED HERE because the framework is the caller. It is deliberately not
// the queue's own Publish: that path discards the PubAck, and the sequence
// inside it is the object's new version, the applier's checkpoint and the
// next writer's expectation all at once.
type Appender interface {
	// Append publishes one record.
	//
	// expect is the per-subject last sequence this append is conditional
	// on — nil for no expectation at all, which is the additive pattern
	// and nothing else. A POINTER rather than a sentinel because zero is
	// a real expectation with a specific meaning ("this subject holds
	// nothing") and a sentinel would make the one branch that matters
	// most indistinguishable from the branch that skips arbitration.
	//
	// A record the CLIENT refuses for its size answers [queue.ErrTooLarge]:
	// that refusal is made before the broker sees the record, so it carries
	// no API error for [classify] to read, and without the sentinel it is
	// indistinguishable from no answer at all.
	Append(ctx context.Context, subject, msgID string, expect *uint64, body []byte) (seq uint64, duplicate bool, err error)

	// LastSeq answers the last sequence on a subject, reporting false
	// with a NIL error when the subject holds no message.
	//
	// THE DISCRIMINATOR, and it must be answered authoritatively. It is
	// what tells a trimmed anchor from a lost race, and the two have
	// opposite remedies: one publishes at zero and the other must never.
	//
	// "No message here" is a fact and not a failure, so it is the bool
	// rather than an error: an implementation that returned the broker's
	// not-found as an error would make every caller re-derive the same
	// distinction, and the first one to forget would take the trimmed
	// anchor's branch on a broker that was merely unreachable.
	LastSeq(ctx context.Context, subject string) (seq uint64, found bool, err error)
}

// fault is what a publish attempt actually was: a record stored, a record
// refused, or no answer at all — and refusals differ in their remedy, so each
// remedy is a value of its own.
type fault int

const (
	// faultNone is a PubAck: the record is on the stream.
	faultNone fault = iota

	// faultRejected is the broker's DECISION that this expectation is
	// stale. Somebody wrote first, or the anchor was trimmed.
	faultRejected

	// faultFull is the stream refusing to store the record at all.
	faultFull

	// faultTooLarge is the record refused for its SIZE, and nothing
	// stored: by the NATS client against the max_payload its server
	// announced, or by the broker against the stream's max_msg_size. It is
	// as definitive as faultFull — the same record is refused the same way
	// every time — and apart from it because the remedy is a different
	// setting.
	faultTooLarge

	// faultRefused is every other decision the broker made and named: an
	// API error this framework has no remedy of its own for. It is as
	// definitive as the two above and apart from both, because filed with
	// either it would carry that one's remedy — a byte ceiling to raise, a
	// size limit to lift — for a refusal neither setting causes.
	faultRefused

	// faultUnknown is no answer: the append may or may not have landed,
	// and nothing here can tell which.
	faultUnknown
)

// classify decides what a publish error was.
//
// # Why BOTH rejection codes, and why this is one function
//
// The solo broker answers 10071 and a clustered one answers 10164 — the
// client's own comment calls them "equivalent 'wrong last sequence'
// responses" — and the engine's default topology is the solo embedded broker,
// so a build testing only the clustered code would pass every fleet test and
// mis-read every single-node write.
//
// A design writing directly to a stream does not get the client's own
// revision-mismatch mapping for free: that wrapper lives inside the key-value
// layer. So the raw APIError is classified here, once, against both codes.
//
// # And why the description is never parsed
//
// The server interpolates the sequence it actually found into the rejection's
// description string and exposes it nowhere structured. Reading it back out
// would be a parser against a message that is free to be reworded, for a
// number the discriminator answers authoritatively — so the rejection is
// treated as "stale, cause unknown" and LastSeq is asked.
//
// # And why a size refusal is checked before anything else
//
// The CLIENT refuses a record larger than the max_payload its server announced
// before sending a byte, so that refusal carries no APIError at all — and an
// error with none is the third value below, an append that may or may not have
// landed. Read that way, the write path reads the subject back, finds nothing
// of this write's, and decides again, round after round, to a conflict that
// names neither the size nor the setting. The appender answers it as
// [queue.ErrTooLarge], naming the max_payload that refused it, and those words
// are the detail.
func classify(err error) (fault, string) {
	if err == nil {
		return faultNone, ""
	}
	if errors.Is(err, queue.ErrTooLarge) {
		return faultTooLarge, err.Error()
	}
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode {
		case jetstream.JSErrCodeStreamWrongLastSequence,
			jetstream.JSErrCodeStreamWrongLastSequenceConstant:
			return faultRejected, apiErr.Description
		case codeMessageExceedsMaximum:
			// THE BROKER'S SIZE REFUSAL, of a record larger than the
			// stream's max_msg_size — a setting the engine never makes,
			// so somebody made it on the stream. The server's words name
			// no setting, so the detail does.
			return faultTooLarge, apiErr.Description + " — the stream's " +
				"max_msg_size is smaller than this record and its headers; " +
				"raise it on the stream, or remove it"
		case codeStreamStoreFailed:
			// THE DESCRIPTION TRAVELS RATHER THAN BEING PARSED. The
			// server answers this code for every store limit the
			// record met — the stream's bytes, its message count, a
			// record past the file store's own maximum — and carries
			// which one only in its own words, so the words are
			// handed on verbatim instead of being turned into a
			// second enum this build would have to keep matching.
			return faultFull, apiErr.Description
		}
		// ANY OTHER API ERROR is a decision the server made and named,
		// so it is not ambiguous — and it is not one this framework has
		// a remedy for either, so it is reported in the server's own
		// words rather than retried or filed under a remedy that is not
		// its own.
		return faultRefused, apiErr.Description
	}
	// NO ANSWER IS THE THIRD VALUE. The client retries a no-responder
	// twice on its own before giving up, so reaching here means the
	// append may be on the stream and may not be, and only the ordered
	// classification can say which.
	return faultUnknown, err.Error()
}

// codeStreamStoreFailed is the server's "the stream would not store this"
// code. It is not exported by the client, so it is written down here with
// what it covers rather than left as a literal at the switch.
const codeStreamStoreFailed jetstream.ErrorCode = 10077

// codeMessageExceedsMaximum is the server's "message size exceeds maximum
// allowed", which it answers for a record larger than the stream's own
// max_msg_size. Not exported by the client either.
const codeMessageExceedsMaximum jetstream.ErrorCode = 10054
