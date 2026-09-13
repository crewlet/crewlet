package statelog

import (
	"context"
	"errors"

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

// fault is what a publish attempt actually was, which is three facts and not
// two.
type fault int

const (
	// faultNone is a PubAck: the record is on the stream.
	faultNone fault = iota

	// faultRejected is the broker's DECISION that this expectation is
	// stale. Somebody wrote first, or the anchor was trimmed.
	faultRejected

	// faultFull is the stream refusing to store the record at all.
	faultFull

	// faultUnknown is no answer: the append may or may not have landed,
	// and nothing here can tell which.
	faultUnknown
)

// classify decides which of the three a publish error was.
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
func classify(err error) (fault, string) {
	if err == nil {
		return faultNone, ""
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
		}
		// Any other API error is a decision the server made and named,
		// so it is not ambiguous — but it is not one this framework
		// knows how to act on either, so it is reported rather than
		// retried.
		return faultFull, apiErr.Description
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
