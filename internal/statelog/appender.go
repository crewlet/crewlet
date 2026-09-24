package statelog

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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

// fault is what a publish attempt actually was — not a success or a failure
// but one of these, because each has its own next step: a rejection is
// discriminated, a full log or an oversized record is refused with a remedy
// that differs from the other's, any other refusal is reported as the
// broker's, and no answer is resolved.
type fault int

const (
	// faultNone is a PubAck: the record is on the stream.
	faultNone fault = iota

	// faultRejected is the broker's DECISION that this expectation is
	// stale. Somebody wrote first, or the anchor was trimmed.
	faultRejected

	// faultFull is the stream refusing to store the record at all.
	faultFull

	// faultTooLarge is a record larger than the broker carries: refused by
	// the client before anything was sent, against the payload limit the
	// server announced, or by the stream against its own message size.
	// Nothing was stored, and the same record is refused every time.
	faultTooLarge

	// faultRefused is any other decision the broker made and named. Nothing
	// was stored, and it is neither a full log nor a lost race.
	faultRefused

	// faultUnknown is no answer: the append may or may not have landed,
	// and nothing here can tell which.
	faultUnknown
)

// classify decides which of these a publish error was.
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
// # And why only the append's own error is ever handed to it
//
// Everything that is not an APIError reads as NO ANSWER here, which is true
// only of what the append itself returned. A refusal this node makes before
// the append — fence 0's — is a decision, not an absence of one, and it ends
// the write where it is made ([Publisher.attempt]): classified, it would send
// a write that never reached the broker down the ambiguous path, which finds
// nothing landed, retakes, is refused again, and reports a conflict once the
// round budget is spent.
func classify(err error) (fault, string) {
	if err == nil {
		return faultNone, ""
	}
	// TOO LARGE BEFORE ANYTHING WAS SENT. The client refuses a message
	// over the payload limit the server announced, locally and as a plain
	// error — so read as "no answer" it went down the ambiguous path,
	// found nothing landed, retook its snapshot, decided the same record
	// and was refused again, sixteen times, and was reported as a
	// colleague editing the object.
	if errors.Is(err, nats.ErrMaxPayload) {
		return faultTooLarge, err.Error()
	}
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode {
		case jetstream.JSErrCodeStreamWrongLastSequence,
			jetstream.JSErrCodeStreamWrongLastSequenceConstant:
			return faultRejected, apiErr.Description
		case codeStreamStoreFailed:
			// THE ONE PLACE THE DESCRIPTION TRAVELS RATHER THAN BEING
			// PARSED. This code covers both "maximum bytes exceeded"
			// and "message too large", the server carries the reason
			// only in its own words, and both are an operator's
			// problem with different remedies — so the words are
			// handed on verbatim instead of being turned into a
			// second enum this build would have to keep matching.
			return faultFull, apiErr.Description
		case codeMessageExceedsMaximum:
			return faultTooLarge, apiErr.Description
		}
		// ANY OTHER API ERROR is a decision the server made and named, so
		// it is not ambiguous — and it is NOT a full log. Reported as one,
		// a stream deleted under the node, a cluster that lost its leader
		// or a request the server would not queue told the caller to raise
		// a byte ceiling, and an operator acting on it spent a maintenance
		// window on a log with room to spare.
		return faultRefused, apiErr.Description
	}
	// NO ANSWER IS THE LAST VALUE. The client retries a no-responder
	// twice on its own before giving up, so reaching here means the
	// append may be on the stream and may not be, and only the ordered
	// classification can say which.
	return faultUnknown, err.Error()
}

// The server's codes this framework acts on and the client does not export,
// written down here with what they cover rather than left as literals at the
// switch.
const (
	// codeStreamStoreFailed is "the stream would not store this".
	codeStreamStoreFailed jetstream.ErrorCode = 10077

	// codeMessageExceedsMaximum is a message over the stream's own size
	// limit (JSStreamMessageExceedsMaximumErr).
	codeMessageExceedsMaximum jetstream.ErrorCode = 10054
)
