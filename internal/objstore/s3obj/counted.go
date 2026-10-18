package s3obj

import (
	"context"
	"errors"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// What the client counts: the labels its attempts and requests are counted
// under, in the node's one [metrics.Recorder] (see the package doc, "What it
// counts").
//
// THE OUTCOMES ARE READ BY THE GATE'S OWN PREDICATES — [isThrottle] and
// [answered] — and nothing of their own, so what the attempts counter calls
// a throttle is exactly what cut the gate, and what it calls answered is
// exactly what grew it: a second reading of the bucket's answers beside the
// gate's would be a counter that disagrees with the limit it explains.

// operations is every S3 operation the client sends — the SDK's own name for
// each, the API's — the one closed set an `operation` label takes, so a
// series per key, bucket or prefix cannot open: a name outside it is counted
// [otherOperation], which the suite holds at none by sending every verb.
var operations = []string{
	"AbortMultipartUpload",
	"CompleteMultipartUpload",
	"CreateMultipartUpload",
	sessionOperation,
	"DeleteObject",
	"DeleteObjects",
	"GetObject",
	"HeadBucket",
	"HeadObject",
	"ListMultipartUploads",
	"ListObjectsV2",
	"PutObject",
	"UploadPart",
}

// otherOperation is the label of an operation outside [operations]: one a
// newer client sends that this list has not named, so the attempt is still
// counted, under a label that says the list is behind.
const otherOperation = "other"

// operationOf is the label an operation the SDK names name is counted under.
func operationOf(name string) string {
	if slices.Contains(operations, name) {
		return name
	}
	return otherOperation
}

// What the bucket made of one attempt, as [attemptOutcome] decides it.
const (
	// attemptEnded: the attempt's own context — the request's deadline or
	// its caller — ended it in flight; it says nothing of the bucket.
	attemptEnded = "ended"
	// attemptStalled: the bucket stopped moving it for the stall bound —
	// took no more of its body, acknowledged none of it at the node's
	// socket and sent none of its answer ([stalled]) — and its request was
	// given up.
	attemptStalled = "stalled"
	// attemptAnswered: served — a success, or a refusal such as a missing
	// key ([answered]). What grows the gate.
	attemptAnswered = "answered"
	// attemptThrottled: asked to slow down ([isThrottle]). What cuts the
	// gate.
	attemptThrottled = "throttled"
	// attemptFailed: any other answer the bucket gave — a server error that
	// is no throttle, or a status no request expects, such as a 301 for a
	// bucket in another region.
	attemptFailed = "failed"
	// attemptUnreached: no answer at all — a connection refused or reset, a
	// name that did not resolve, a TLS failure, or an attempt never sent
	// because its S3 Express session could not be had.
	attemptUnreached = "unreached"
)

// attemptOutcomes is every outcome of an attempt, in the order
// [attemptOutcome] decides them.
var attemptOutcomes = []string{attemptEnded, attemptStalled, attemptAnswered, attemptThrottled, attemptFailed,
	attemptUnreached}

// attemptOutcome is what the bucket made of one attempt that ended with err
// under ctx: exactly one of [attemptOutcomes], decided in that order, so an
// answer that is a throttle is never also a failure and every error lands
// somewhere — a status that is neither served nor a throttle is a failure, no
// status at all is unreached.
//
// A STALL BEFORE EVERY READING OF A STATUS: one whose body stopped reads no
// status at all, and would be counted `unreached` — the bucket never reached,
// when it took megabytes of the body — and one whose answer went silent
// reads its 200, and would be counted `failed`. Its own outcome, after
// `ended`, because the attempt's own context ending is the caller's, which
// the watch's cancellation never is.
func attemptOutcome(ctx context.Context, err error) string {
	switch {
	case err != nil && ctx.Err() != nil:
		return attemptEnded
	case isStalled(err):
		return attemptStalled
	case answered(err):
		return attemptAnswered
	case isThrottle(err):
		return attemptThrottled
	case status(err) != 0:
		return attemptFailed
	}
	return attemptUnreached
}

// How one request ended, as [requestOutcome] decides it.
const (
	// requestAnswered: the bucket's final answer, a success or a refusal.
	requestAnswered = "answered"
	// requestGivenUp: the bucket answered nothing but failures to the end of
	// the request's span's bound, inside a minute, or stopped moving one of
	// its attempts for the stall bound ([givenUp]).
	requestGivenUp = "given_up"
	// requestEnded: its deadline or its caller ended it after at least one
	// attempt.
	requestEnded = "ended"
	// requestUnsent: it ended before its first attempt — its deadline or its
	// caller ended it while it waited at the gate, or it was refused before
	// it was sent — observed at ZERO attempts.
	requestUnsent = "unsent"
	// requestFailed: a failure asking again cannot change — a certificate
	// the node does not trust, or an S3 Express session that could not be
	// had for it.
	requestFailed = "failed"
)

// requestOutcomes is every outcome of a request, in the order
// [requestOutcome] decides them.
var requestOutcomes = []string{requestUnsent, requestGivenUp, requestAnswered, requestEnded, requestFailed}

// requestOutcome is how a request that sent attempts attempts and ended with
// err under ctx ended: exactly one of [requestOutcomes], decided in that
// order.
//
// UNSENT IS ITS OWN OUTCOME, whatever ended the request, because the retries
// an operator reads are the attempts counter's total less the requests that
// made an attempt: a request with none is observed at zero, and the
// histogram's first bucket holds zero and one together, so folded into
// `ended` it would leave that sum a reader cannot compute — attempts less
// requests undercounts by every request that never got its turn, which is
// also the one direct reading of a gate starving the node's requests.
//
// FROM THE REQUEST'S OWN ERROR, never its last attempt's outcome: a request
// given up ends on an attempt that FAILED, and one whose deadline ended it
// may have had its last attempt throttled.
func requestOutcome(ctx context.Context, attempts int, err error) string {
	if attempts == 0 {
		return requestUnsent
	}
	if _, ok := errors.AsType[*givenUp](err); ok {
		return requestGivenUp
	}
	switch {
	case answered(err):
		return requestAnswered
	case ctx.Err() != nil:
		return requestEnded
	}
	return requestFailed
}

// counted is where the client's counts go: the node's recorder, or nowhere.
//
// NIL COUNTS NOTHING — a test of something else, the backup's benchmark —
// and is asked for at every count, so no caller of the client has to build a
// recorder it never reads.
type counted struct{ rec *metrics.Recorder }

// attempt counts one attempt of operation, with outcome.
func (c counted) attempt(operation, outcome string) {
	if c.rec == nil {
		return
	}
	c.rec.Add(metrics.ObjectsS3Attempts, 1, metrics.Attrs{"operation": operation, "outcome": outcome})
}

// request counts one request of operation that ended outcome after attempts
// attempts.
func (c counted) request(operation, outcome string, attempts int) {
	if c.rec == nil {
		return
	}
	c.rec.ObserveValue(metrics.ObjectsS3RequestAttempts, float64(attempts),
		metrics.Attrs{"operation": operation, "outcome": outcome})
}

// gateLimit sets the gauge of how many attempts the gate lets out at once: a
// whole number, at least one while it is engaged, and zero while it is
// lifted.
func (c counted) gateLimit(limit int) {
	if c.rec == nil {
		return
	}
	c.rec.Set(metrics.ObjectsS3GateLimit, float64(limit), nil)
}

// gateWaiting sets the gauge of how many attempts wait in line at the gate.
func (c counted) gateWaiting(waiting int) {
	if c.rec == nil {
		return
	}
	c.rec.Set(metrics.ObjectsS3GateWaiting, float64(waiting), nil)
}
