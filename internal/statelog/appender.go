package statelog

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
)

// Appender is the broker, as narrowly as the framework needs it: one
// conditional append, and the one query the rejection path discriminates
// with. Not the queue's own Publish, which discards the PubAck: its sequence
// is the object's new version, the applier's checkpoint and the next writer's
// expectation at once.
type Appender interface {
	// Append publishes one record.
	//
	// expect is the per-subject last sequence this append is conditional
	// on; nil is no expectation, the additive pattern and nothing else. A
	// POINTER, because zero is a real expectation ("this subject holds
	// nothing") that a sentinel would confuse with skipping arbitration.
	Append(ctx context.Context, subject, msgID string, expect *uint64, body []byte) (seq uint64, duplicate bool, err error)

	// LastSeq answers the last sequence on a subject, reporting false
	// with a NIL error when the subject holds no message.
	//
	// THE DISCRIMINATOR, answered authoritatively: it tells a trimmed
	// anchor (publish at zero) from a lost race (never). "No message" is a
	// fact, so a bool: as an error, a caller that forgot to tell it apart
	// would take the trimmed anchor's branch on an unreachable broker.
	LastSeq(ctx context.Context, subject string) (seq uint64, found bool, err error)
}

// fault is what a publish attempt actually was: on the stream, refused as
// stale, or no answer. Other refusals split further, because each sends a
// different person to a different knob: a full log is the operator's
// retention, a record too large the writer's change or whichever limit refused
// it ([tooLargeDetail]), anything else the broker's own words. One named
// answer is no refusal: the record may yet land ([faultUnsettled]).
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
	// message: not the log's size, and no retry makes it smaller. THREE
	// LIMITS REACH IT, each moved by a different hand, so its detail names
	// which refused ([tooLargeDetail]): one remedy for all sends an
	// operator to a setting that cannot help.
	faultTooLarge

	// faultRefused is any other refusal the broker named and this
	// framework has no remedy for: reported in the broker's words, not
	// retried nor dressed as a refusal above. Final only because
	// [faultUnsettled], the named errors saying the record may yet be
	// stored, is classified first.
	faultRefused

	// faultUnsettled is the broker ANSWERING, on a clustered stream, that
	// this operation's record may yet land: 10158 ("duplicate message id is
	// in process": an earlier append under this op id is proposed, not yet
	// applied) or 10077 with "store is closed" (the store closed under an
	// entry raft already committed). As a refusal, a caller re-filing under
	// a fresh op id wrote the change twice. Unlike [faultUnknown], a probe
	// finding nothing of this write's proves nothing (committed, not yet
	// visible; a retaken snapshot would tell a create its own record
	// exists), so what the probe cannot settle is answered `unknown`, whose
	// only retry, the same op id, gets the duplicate acknowledgement.
	faultUnsettled

	// faultUnknown is no answer: the append may or may not have landed,
	// and nothing here can tell which.
	faultUnknown
)

// classify decides which fault a publish error was, from the raw APIError
// (the client's revision-mismatch mapping lives only in its key-value layer).
// Only 10071 is a rejection, the leader's comparison; 10164 decides nothing
// (another write to the subject in flight, internal/jsinflight), so it is no
// answer. A rejection's description, holding the sequence found, is never
// parsed: LastSeq answers that authoritatively. A 10077's store error IS
// compared, whole, with the server's declared errors (a test pins them): a
// full log, a record too large and a closed store have opposite remedies, and
// a wording this build does not know is [faultRefused] in its own words. Only
// the append's own error is handed here: a non-APIError reads as no answer,
// so a refusal this node made first (fence 0's, [Publisher.attempt]) would be
// re-decided as ambiguous until the round budget ran out.
func classify(err error) (fault, string) {
	if err == nil {
		return faultNone, ""
	}
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode {
		case jetstream.JSErrCodeStreamWrongLastSequence:
			return faultRejected, apiErr.Description
		case jetstream.JSErrCodeStreamWrongLastSequenceConstant:
			// ANOTHER WRITE STILL IN FLIGHT, not a comparison, and nothing
			// of this append stored, so the write retakes.
			// internal/queue/jetstream's DomainLog.Append waits it out,
			// so it arrives as a deadline's no-answer.
			return faultUnknown, apiErr.Description
		case codeStreamStoreFailed:
			switch apiErr.Description {
			case storeFailedMaxBytes:
				return faultFull, apiErr.Description
			case storeFailedTooLarge:
				return faultTooLarge, tooLargeDetail(limitFileStore, apiErr.Description)
			case storeFailedClosed:
				// A STORE CLOSED UNDER A COMMITTED RAFT ENTRY: every
				// member that replays the entry applies it.
				return faultUnsettled, apiErr.Description
			}
		case codeStreamDuplicateMessageConflict:
			// THIS OPERATION'S OWN RECORD, proposed and not yet
			// applied: the message id is the op id.
			return faultUnsettled, apiErr.Description
		case codeStreamMessageExceedsMaximum:
			// THE STREAM'S OWN PER-MESSAGE LIMIT, which the publisher
			// checks before sending, so this is a record something else
			// sent or a stream reconfigured under the engine: still too
			// large, not a full log.
			return faultTooLarge, tooLargeDetail(limitStreamMsgSize, apiErr.Description)
		}
		// ANY OTHER API ERROR is a named refusal with no remedy here:
		// reported with its code and words, not retried or passed off as
		// a full log.
		return faultRefused, fmt.Sprintf("code %d: %s", apiErr.ErrorCode,
			apiErr.Description)
	}
	// TOO LARGE IS THE CLIENT'S DECISION, made before the append left, so
	// it is not ambiguous and must not be retried: as no answer, the
	// publisher re-decided the write every round and reported contention.
	if errors.Is(err, nats.ErrMaxPayload) {
		return faultTooLarge, tooLargeDetail(limitMaxPayload, err.Error())
	}
	// NO ANSWER IS THE LAST VALUE: the append may or may not be on the
	// stream (the client already retried a no-responder twice), and only
	// the ordered classification can say which.
	return faultUnknown, err.Error()
}

// codeStreamStoreFailed is the server's "the stream would not store this"
// code, codeStreamMessageExceedsMaximum its "larger than this stream's own
// per-message limit", and codeStreamDuplicateMessageConflict its "a record
// under this message id is proposed and not yet applied". The client exports
// none of them.
const (
	codeStreamStoreFailed              jetstream.ErrorCode = 10077
	codeStreamMessageExceedsMaximum    jetstream.ErrorCode = 10054
	codeStreamDuplicateMessageConflict jetstream.ErrorCode = 10158
)

// storeFailedMaxBytes, storeFailedTooLarge and storeFailedClosed are the
// store errors a 10077 carries that this framework reads, spelled exactly as
// the server's ErrMaxBytes, ErrMsgTooLarge and ErrStoreClosed. A test holds
// each against the vendored server, so a bump that rewords one fails the
// build rather than turning it into an unnamed refusal.
const (
	storeFailedMaxBytes = "maximum bytes exceeded"
	storeFailedTooLarge = "message too large"
	storeFailedClosed   = "store is closed"
)

// sizeLimit is which of the three limits refused a record too large.
type sizeLimit int

const (
	// limitMaxPayload is the NATS server's max_payload, which the client
	// checks before the append leaves. A connection held below the
	// contract's ceiling is refused when the queue opens, so a record
	// inside its declaration meets this only after the client RECONNECTED,
	// the one moment it rereads the limit: to a member configured apart, a
	// server restarted with a smaller limit, or across a live reload that
	// lowered it when the write failed before the client read the server's
	// close over the record (read, the close is final and the node stops:
	// internal/queue/jetstream's connectionLoss). Raised on every server,
	// and on the account where that states its own.
	limitMaxPayload sizeLimit = iota

	// limitStreamMsgSize is the stream's own max_msg_size, the declared
	// largest record plus its stored overhead ([StreamSpec.MaxAppendBytes]).
	// The publisher refuses a larger record before sending, so meeting it
	// means a peer that does not ask sent one, or the stream was
	// reconfigured under the engine; max_payload does nothing for it.
	limitStreamMsgSize

	// limitFileStore is the file store's per-record limit, which no
	// setting raises: an external server whose max_payload is above it
	// accepts the message and refuses it at the store, so only a smaller
	// record is stored.
	limitFileStore
)

// tooLargeDetail is a record too large, told by the limit that refused it and
// what moves that limit, beside the words the refusal carried.
//
// ONE SENTENCE PER LIMIT, never a generic one naming all three, which sends a
// reader to try each knob in turn.
func tooLargeDetail(limit sizeLimit, words string) string {
	switch limit {
	case limitStreamMsgSize:
		return fmt.Sprintf("the stream's own max_msg_size refused it (%s) — the "+
			"domain's declared largest record, which no retry and no server "+
			"setting moves: split the change into smaller writes", words)
	case limitFileStore:
		return fmt.Sprintf("the broker's file store refused it as larger than one "+
			"stored record may be (%s) — a limit of the store itself that no "+
			"server setting raises, so split the change into smaller writes", words)
	}
	// NAMING WHEN AS WELL AS THE SETTING: the node started, so its reader
	// may well have set max_payload on the member it started against; the
	// lower limit came with a reconnect ([limitMaxPayload]), of which a
	// live reload is one way, never the only one.
	return fmt.Sprintf("the NATS server's max_payload refused it (%s) — a "+
		"connection held to less than the %d bytes this build sends at most is "+
		"refused at boot, so this is a server this node reconnected to since "+
		"that holds it to less: a cluster member configured apart from the "+
		"others, a server restarted with a smaller limit, or a limit lowered by "+
		"a live reload that this node's connection reconnected across. So raise "+
		"max_payload on every server of the cluster this fleet dials, and on the "+
		"account it signs in to where that states one, or split the change into "+
		"smaller writes", words, queue.MaxPayloadBytes)
}
