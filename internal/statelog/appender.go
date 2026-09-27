package statelog

import (
	"context"
	"errors"
	"fmt"

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

// fault is what a publish attempt actually was.
//
// THREE FACTS AT THE CORE — on the stream, refused as stale, or no answer —
// and the refusals that are neither a stale expectation nor an unanswered
// append split three ways further, because each sends a different person to a
// different knob: a log at its ceiling is the operator's retention, a record
// too large for the broker is the writer's change or whichever of three limits
// refused it ([tooLargeDetail]), and anything else the broker refused is the
// broker's own words.
// Folded into one, a record too large was told to raise a ceiling it was
// nowhere near.
//
// AND ONE ANSWER IS NOT A REFUSAL AT ALL although the broker named it: an
// error saying this operation's record may yet land ([faultUnsettled]). The
// broker answered, so it is not the no-answer either — and it is the one
// answer after which a probe finding nothing proves nothing.
type fault int

const (
	// faultNone is a PubAck: the record is on the stream.
	faultNone fault = iota

	// faultRejected is the broker's DECISION that this expectation is
	// stale. Somebody wrote first, or the anchor was trimmed.
	faultRejected

	// faultFull is the log at its byte ceiling, refusing the append
	// rather than dropping a record to make room.
	faultFull

	// faultTooLarge is a record larger than the broker takes in one
	// message. Nothing about the log's size is involved, and nothing a
	// retry does makes the record smaller.
	//
	// THREE LIMITS REACH IT, each moved by a different hand, so its detail
	// names which one refused ([tooLargeDetail]): the NATS server's
	// max_payload, the stream's own max_msg_size, and the file store's
	// per-record limit. Folded into one remedy — raise max_payload — the
	// second sent an operator to a server setting when the stream had been
	// reconfigured under the engine, and the third to a setting they had
	// already turned past the limit that refused the record.
	faultTooLarge

	// faultRefused is any other refusal the broker made and named, and
	// one this framework has no remedy for — so it is reported with the
	// broker's words rather than retried, and rather than dressed as one
	// of the refusals above.
	//
	// FINAL ONLY BECAUSE [faultUnsettled] IS CARVED OUT OF IT FIRST. A
	// named error is a decision about THIS append only when it also says
	// the record will not be stored; the two that say the opposite are
	// classified before anything reaches here.
	faultRefused

	// faultUnsettled is the broker ANSWERING that this operation's record
	// may yet land, which it does in two ways on a clustered stream:
	//
	//   - 10158, "duplicate message id is in process". A clustered leader
	//     stages a record's message id the moment it PROPOSES it, and a
	//     second append under the same id before that proposal applies is
	//     answered with this. The second append is this write's own —
	//     the id is the op id — re-sent after an earlier round's append
	//     went unanswered while its commit was slow, or by a caller
	//     retrying an `unknown` under the same operation, which is the
	//     retry this framework tells every caller to make.
	//   - 10077 whose store error is "store is closed". A clustered leader
	//     answers that when its store closes under an entry raft has
	//     ALREADY committed — a shutdown, a stream reset — so the record
	//     is applied by every member that replays the entry, the leader
	//     included once it comes back.
	//
	// Reported as a refusal, either one told a caller "asking again changes
	// nothing" about a record that committed and applied a moment later —
	// and a caller that believed it and re-filed under a fresh op id wrote
	// the change twice. Resolved like an ambiguous append, then, with one
	// difference that is the whole reason this is not [faultUnknown]: a
	// probe that finds nothing of this write's does NOT mean nothing
	// landed, because the record may be committed and not yet visible. A
	// snapshot retaken there decides against a state about to hold this
	// very write — a create would be told its own record already exists —
	// so what the probe cannot settle is answered `unknown`, whose only
	// retry is the same op id, and that retry gets the broker's duplicate
	// acknowledgement once the record lands.
	faultUnsettled

	// faultUnknown is no answer: the append may or may not have landed,
	// and nothing here can tell which.
	faultUnknown
)

// classify decides which fault a publish error was.
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
// # And why a rejection's description is never parsed
//
// The server interpolates the sequence it actually found into the rejection's
// description string and exposes it nowhere structured. Reading it back out
// would be a parser against a message that is free to be reworded, for a
// number the discriminator answers authoritatively — so the rejection is
// treated as "stale, cause unknown" and LastSeq is asked.
//
// # Why a store failure's description IS compared, whole
//
// 10077 is the server's one code for "the stream would not store this", and
// it covers a log at its ceiling, a message too large for the file store and
// a store that closed under the append alike — the reason travels only as the
// text of the server's own store error. The three have opposite remedies, so
// they are told apart by comparing that text WHOLE against the server's
// declared errors ([storeFailedMaxBytes], [storeFailedTooLarge],
// [storeFailedClosed]), which a test pins to the vendored server's own
// values: never a substring, never a parse. A wording this build does not
// know is [faultRefused] carrying the words verbatim, so a reworded server
// costs a less specific remedy and never a wrong one — which is what reading
// every 10077 as a full log was.
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
			switch apiErr.Description {
			case storeFailedMaxBytes:
				return faultFull, apiErr.Description
			case storeFailedTooLarge:
				return faultTooLarge, tooLargeDetail(limitFileStore, apiErr.Description)
			case storeFailedClosed:
				// A STORE THAT CLOSED UNDER AN ENTRY RAFT HAD
				// COMMITTED, on a clustered leader: every member
				// that replays the entry applies it.
				return faultUnsettled, apiErr.Description
			}
		case codeStreamDuplicateMessageConflict:
			// THIS OPERATION'S OWN RECORD, proposed and not yet
			// applied: the message id is the op id.
			return faultUnsettled, apiErr.Description
		case codeStreamMessageExceedsMaximum:
			// THE STREAM'S OWN PER-MESSAGE LIMIT, a structured code of
			// its own. No state log declares one, so reaching it means
			// the stream was reconfigured under the engine — and it is
			// still a record too large, not a log that is full.
			return faultTooLarge, tooLargeDetail(limitStreamMsgSize, apiErr.Description)
		}
		// ANY OTHER API ERROR is a refusal the server made and named —
		// the two that say the record may yet land are settled above —
		// and it is not one this framework knows how to act on, so it
		// is reported with its code and its words rather than retried
		// or passed off as a full log.
		return faultRefused, fmt.Sprintf("code %d: %s", apiErr.ErrorCode,
			apiErr.Description)
	}
	// TOO LARGE IS A DECISION THE CLIENT MADE, before the append ever
	// reached the broker, so it is not ambiguous and it must not be
	// retried. Without this arm it fell through to the unknown answer
	// below and the publisher re-decided the whole write for all sixteen
	// rounds — then reported "the rows kept changing under this write",
	// which is a sentence about contention and sends a reader looking for
	// a peer that is not there. Nothing about the record changes between
	// rounds; it is too big now and it will be too big in a millisecond.
	if errors.Is(err, nats.ErrMaxPayload) {
		return faultTooLarge, tooLargeDetail(limitMaxPayload, err.Error())
	}
	// NO ANSWER IS THE THIRD VALUE. The client retries a no-responder
	// twice on its own before giving up, so reaching here means the
	// append may be on the stream and may not be, and only the ordered
	// classification can say which.
	return faultUnknown, err.Error()
}

// codeStreamStoreFailed is the server's "the stream would not store this"
// code, codeStreamMessageExceedsMaximum its "larger than this stream's own
// per-message limit", and codeStreamDuplicateMessageConflict its "a record
// under this message id is proposed and not yet applied". None is exported by
// the client, so each is written down here with what it covers rather than
// left as a literal at the switch.
const (
	codeStreamStoreFailed              jetstream.ErrorCode = 10077
	codeStreamMessageExceedsMaximum    jetstream.ErrorCode = 10054
	codeStreamDuplicateMessageConflict jetstream.ErrorCode = 10158
)

// storeFailedMaxBytes, storeFailedTooLarge and storeFailedClosed are the
// store errors a 10077 carries that this framework reads, spelled exactly as
// the server declares them (ErrMaxBytes, ErrMsgTooLarge and ErrStoreClosed in
// its store). A test holds each against the vendored server, so a bump that
// rewords one fails the build instead of turning a full log — or a record
// that may yet land — into an unnamed refusal.
const (
	storeFailedMaxBytes = "maximum bytes exceeded"
	storeFailedTooLarge = "message too large"
	storeFailedClosed   = "store is closed"
)

// sizeLimit is which of the three limits refused a record too large.
type sizeLimit int

const (
	// limitMaxPayload is the NATS server's max_payload, which the client
	// refuses against before the append leaves the process. An operator
	// raises it on an external server; the embedded broker's is the
	// contract's own ceiling.
	limitMaxPayload sizeLimit = iota

	// limitStreamMsgSize is the stream's own max_msg_size. No state log
	// declares one, so meeting it means somebody reconfigured the stream
	// under the engine, and restoring it is the remedy — raising the
	// server's max_payload changes nothing about it.
	limitStreamMsgSize

	// limitFileStore is the file store's per-record limit, which no
	// setting raises: an external server whose max_payload is above it
	// accepts the message on the wire and refuses it at the store, so the
	// operator has already turned the one knob a max_payload refusal
	// names, and only a smaller record is stored.
	limitFileStore
)

// tooLargeDetail is a record too large, told by the limit that refused it and
// what moves that limit, beside the words the refusal carried.
//
// ONE SENTENCE PER LIMIT, and never a generic one naming all three: a
// refusal that lists every knob sends a reader to try each in turn, which is
// the wrong-remedy bug with the right remedy somewhere in it.
func tooLargeDetail(limit sizeLimit, words string) string {
	switch limit {
	case limitStreamMsgSize:
		return fmt.Sprintf("the stream's own max_msg_size refused it (%s) — no "+
			"state log declares one, so the stream was reconfigured under the "+
			"engine: restore its max_msg_size to unlimited (-1), or split the "+
			"change into smaller writes", words)
	case limitFileStore:
		return fmt.Sprintf("the broker's file store refused it as larger than one "+
			"stored record may be (%s) — a limit of the store itself that no "+
			"server setting raises, so split the change into smaller writes", words)
	}
	return fmt.Sprintf("the NATS server's max_payload refused it (%s), so split "+
		"the change into smaller writes, or raise max_payload on the NATS server "+
		"this fleet dials", words)
}
