package s3obj

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

// throttleCodes is every error code the bucket's client reads as the bucket
// asking it to slow down: the SDK's own list — S3's `SlowDown` among them —
// MinIO's `SlowDownRead` and `SlowDownWrite`, and `ServiceUnavailable`, S3's
// own code for a request it is too busy to serve and the code the SDK gives a
// 503 whose answer has no body to name one in: every answer to a HEAD, so S3's
// `503 SlowDown` to a Stat reaches the client spelled so.
var throttleCodes = func() map[string]struct{} {
	codes := map[string]struct{}{"SlowDownRead": {}, "SlowDownWrite": {}, "ServiceUnavailable": {}}
	for code := range retry.DefaultThrottleErrorCodes {
		codes[code] = struct{}{}
	}
	return codes
}()

// isThrottle reports whether err is the bucket asking the node to slow down:
// an answer naming one of [throttleCodes], any `429 Too Many Requests` — a
// status that means nothing else, whatever code a gateway names beside it —
// and a 503 whose answer names no code at all (a load balancer's page of its
// own, which the client cannot read as an S3 error).
//
// A 503 NAMING ANOTHER CODE IS NO THROTTLE: it is a gateway's own failure, and
// often one object's — MinIO answers an object it cannot rebuild from its
// drives with `503 XMinioReadQuorum` — so read as a throttle it would cut the
// node's every request at each attempt of the one request asking about that
// object: on the SDK's adaptive mode, which read every 503 so, four loops of
// thirty-two asking about such objects cost the other twenty-eight four
// fifths of their answers (measured in review, 2026-10-15). It is asked again
// as any server error is, and slows nothing else.
func isThrottle(err error) bool {
	if err == nil {
		return false
	}
	answered := status(err)
	if answered == http.StatusTooManyRequests {
		return true
	}
	if api, ok := errors.AsType[smithy.APIError](err); ok {
		_, throttle := throttleCodes[api.ErrorCode()]
		return throttle
	}
	return answered == http.StatusServiceUnavailable
}

// answered reports whether err is the bucket's answer to a request it served:
// none at all, or a client error but a 429 — a key that is not there, a range
// past the end, a tag that no longer matches. What the gate grows by: the
// bucket handled the request, whatever it said of it.
func answered(err error) bool {
	if err == nil {
		return true
	}
	code := status(err)
	return code >= 400 && code < 500 && code != http.StatusTooManyRequests
}

// refusedCertificate reads a TLS handshake whose certificate the node refused
// as FINAL: the SDK takes every failure to send a request as one worth asking
// again, and a certificate this node does not trust — a private authority
// missing from AWS_CA_BUNDLE, a name that does not match — is refused the same
// way every time. With no count of attempts it would be asked again until
// the request's deadline, the boot's check of the bucket included, and the
// operator told only that the deadline passed.
var refusedCertificate = retry.IsErrorRetryableFunc(func(err error) aws.Ternary {
	if _, refused := errors.AsType[*tls.CertificateVerificationError](err); refused {
		return aws.FalseTernary
	}
	return aws.UnknownTernary
})

// newRetryer is how the bucket's client asks a request again: the SDK's
// standard mode with no client-wide quota, no count of attempts and backoff
// pacing the attempts — [fullJitter] in production, a case's own in this
// package's tests.
//
// NO QUOTA ([ratelimit.None]): the SDK's standard retryer holds 500 tokens for
// a whole client and spends five on every retry, so under a sustained
// `SlowDown` its retries ran out after about a hundred and every request
// after that failed at once — the opposite of slowing down. What holds a
// throttled node back is the [gate], which slows every attempt the node sends
// rather than refusing the ones that come after a count; and what bounds the
// requests asking again is who sends them. Every caller in the engine but one
// is a CLOSED LOOP of its own width — the git cache's fetches, the collector's
// audit and deletions, the backup's copy — so a request asking again holds
// the slot it was sent in and never adds one. The one that is not is the
// API's files: a person's upload or download arrives when they ask, and its
// requests are bounded by their own deadlines alone until a throttle engages
// the gate, which holds them as it holds every other. So a bucket failing with
// a server error that is no throttle — the gate's not to answer — is asked
// again by every request open at the time, each at the backoff, until each
// one's deadline.
//
// NO COUNT OF ATTEMPTS: every request's attempts end at its own DEADLINE —
// [callBudget], [sendBudget], [partBudget], [completeBudget], or a read's
// caller's, whose open the store holds to [objstore.ReadStall] too — and the
// SDK's three attempts failed a request a bucket would have answered a few
// seconds later. MaxAttempts answers zero, which the SDK reads as no count,
// and [Open] sets the client's RetryMaxAttempts to zero too, because the SDK
// copies `AWS_MAX_ATTEMPTS`, or a shared profile's `max_attempts`, into the
// client and wraps the retryer in that count after every option has run.
//
// WHAT IS ASKED AGAIN is the SDK's own — a 500, 502, 503 or 504, its throttle
// codes, every failure to send — but a certificate the node does not trust,
// which is final ([refusedCertificate]), and with a 429 besides: the SDK
// retries a gateway's `429 Too Many Requests` by no rule of its own.
func newRetryer(backoff retry.BackoffDelayer) aws.Retryer {
	return retry.AddWithMaxAttempts(retry.NewStandard(func(s *retry.StandardOptions) {
		s.RateLimiter = ratelimit.None
		s.Retryables = append(append([]retry.IsErrorRetryable{refusedCertificate}, s.Retryables...),
			retry.RetryableHTTPStatusCode{Codes: map[int]struct{}{http.StatusTooManyRequests: {}}})
		s.Backoff = backoff
	}), 0)
}

// fullJitter is how long a request waits before it is asked again: a random
// share of 2^attempt seconds, up to [retry.DefaultMaxBackoff] (twenty
// seconds) — "full jitter", the SDK's own curve up to its ceiling and
// jittered AT the ceiling too.
//
// The SDK's own backoff waits every retry past the fourth exactly twenty
// seconds, with no jitter at all. A count of three attempts never let a
// request reach it; with none, every request a long deadline lets go on
// failing — a read's, a part's sixteen and a half minutes, a completion's
// five — would wait in lockstep with every other that failed when it did,
// all asking again in the same instant once the bucket recovered and none in
// the twenty seconds before it. Jittered, they are spread over the twenty
// seconds and the first are asked again at once.
//
// Under AWS_NEW_RETRIES_2026 the SDK numbers a request's retries from zero
// rather than one, which takes the curve one doubling down: the first retry
// waits up to a second rather than two.
type fullJitter struct{}

func (fullJitter) BackoffDelay(attempt int, _ error) (time.Duration, error) {
	ceiling := retry.DefaultMaxBackoff
	if attempt < 5 {
		ceiling = min(ceiling, time.Second<<max(attempt, 0))
	}
	return rand.N(ceiling), nil
}

// throttleShare is the share of the node's width a throttle leaves the [gate]
// letting out at once: SEVEN TENTHS.
//
// The cut TCP CUBIC makes to its window on a loss (RFC 9438's β, 0.7), and
// the one the SDK's own adaptive mode made to its rate: deep enough that a
// fleet over a prefix's rate is under it within a few throttled rounds — two
// cuts take a node to half its width — and shallow enough that one throttle,
// which on a fleet at the rate's edge is most of what the bucket says, costs a
// node three tenths of one round rather than half of it.
const throttleShare = 0.7

// throttleQuiet is how long the bucket goes without a throttle before the
// [gate] is lifted and the client's attempts are let out as fast as their
// callers send them.
//
// A MINUTE: three of the backoff's twenty-second ceiling ([fullJitter]), so
// a bucket still throttling the requests a node keeps sending is heard
// throttling several times inside it and the limit stays; and long past the
// time the limit takes to climb back over any width the node runs while it
// is being answered — one attempt more each round, a round a first byte, so
// thirty-two cut to twenty-two are back past thirty-two in ten rounds, under
// two seconds at S3's 100 to 200 ms. What lifting it frees is a node that
// went QUIET after a throttle: it gets too few answers to climb, and without
// the lift its next burst — a git node's sync of thirty-two packs — would be
// held to the limit the last throttle left it, the bucket long since ready
// for it.
//
// A LIFTED GATE COSTS A BUCKET STILL THROTTLING ONE ROUND: the attempts in
// flight when its first throttle comes back, which cut the gate once to
// seven tenths of the node's width ([gate.leave]) — the most it has had out
// since its last cut, which a lift leaves as it was — and never to less,
// whether the throttle answers the round's first attempt or the node's one
// lone HEAD between two bursts.
const throttleQuiet = time.Minute

// gate is the client's answer to a throttle: how many attempts it lets the
// bucket hold at once — the node's every request to its `files/` prefix,
// whoever sent it — so the first throttle slows the whole node rather than
// the one request it answered. Unlimited until the bucket throttles.
//
// A LIMIT ON ATTEMPTS IN FLIGHT, NOT A RATE. Every caller of the client
// sends its requests in a loop of its own width, so how many attempts are out
// is what the node is doing, known exactly — and a cut is a share of the
// node's WIDTH ([gate.width]): the most attempts it has had out at once since
// the gate last cut it. The SDK's adaptive mode limited a rate instead, which
// it had to measure: a fresh one had measured none, so its first throttle —
// at every boot, and after every quiet spell its limiter was retired over —
// dropped the whole client to the half a request a second its floor allows;
// and a limiter that kept its measurement would have measured a quiet node's
// rate, too low for its next burst.
//
// THE WIDTH, NEVER THE MOMENT. A cut of what was out at the instant a
// throttle came back did to a node between two bursts what the adaptive mode
// did to a fresh one: a fleet at a prefix's rate has its requests throttled
// across the prefix, and one answered to the node's lone HEAD left it ONE
// place, so its next sync of thirty-two climbed back a place a round for the
// minute before the gate lifted. The width is what the node runs at, so a
// throttle costs a burst the same seven tenths wherever in the node's work it
// lands; it is the node's OWN, so one that never ran wider than four is cut
// to two; and a cut sets it to the limit the cut leaves, so a throttle heard
// while the node is quiet takes seven tenths again, as one heard while it
// runs does. A node throttled before it has had more than one attempt out —
// the check at open — has a width of one and climbs from it a place a round
// until the gate lifts: it has no width to keep yet, and one presumed for it
// would leave a node that runs fewer than that unslowed by its first several
// throttles, each of which cuts only seven tenths of the presumption.
//
// Five hundred heads by thirty-two loops against a bucket 150 ms away took
// 2.43 seconds unthrottled; with the burst's first answer a `SlowDown`, 9.6
// to 9.7 on the adaptive mode and 2.72 to 2.74 through this gate; with a lone
// head throttled before the burst, the client having run thirty-two and gone
// quiet, 2.72 to 2.73 where a cut of the moment took 4.99 to 5.00; and with
// the check at open throttled, 5.00 to 5.02 (BenchmarkAThrottledBurst, the
// adaptive arm measured once beside it on the mode as this package configured
// it; four vCPUs of a 2.1 GHz Xeon, 2026-10-15). An attempt holds its place
// until the bucket's answer BEGINS — the headers of a read, the whole body of
// a put — which is what the bucket counts against its rate.
//
// ONE CUT A ROUND: a throttle answered to an attempt let out before the last
// cut is the bucket's reply to the width that cut already answered, and
// cuts nothing more; only a throttle to an attempt let out since cuts again.
// Without it a burst of thirty-two throttled together would cut thirty-two
// times — to nothing — for one round of answers. GROWN BY ONE A ROUND: every
// answer the bucket gives a request it served ([answered]) adds one over the
// limit, one more attempt for every limit's worth of them — TCP's additive
// increase, so a fleet sharing one prefix converges on shares of its rate
// rather than taking turns at it. A server error or a failure to send moves
// it neither way. LIFTED after [throttleQuiet] without a throttle.
//
// A MIDDLEWARE ROUND EACH ATTEMPT, never the retryer's attempt token: the
// SDK asks the retryer for a token at every attempt and, under
// AWS_NEW_RETRIES_2026, gives back only the first attempt's — so a limit
// kept by tokens would lose a place for every retry under that environment,
// and hear no throttle answered to one. Placed directly after the SDK's retry
// step, BEFORE the attempt resolves its identity and is signed, so an attempt
// that waits carries a credential — and a signature — taken when it is sent:
// a credential resolved before the wait could expire during it, and a bucket
// refuses an expired one as final (`ExpiredToken`), never as worth asking
// again.
//
// EXCEPT S3 EXPRESS'S SESSION REQUEST (`CreateSession`), which the SDK sends
// through this same client, under the context of a request that already
// holds a place and waits for it in `GetIdentity`: held here it would wait
// behind its own waiter, and every request needing the session with it, until
// their deadlines or the lift. At one place — a node throttled at its check
// at open starts there — that is every time: a Stat needing a new session
// ended at its two-second deadline with no CreateSession sent, four runs of
// four, where with the session request let through it was answered in 0.8 to
// 1.0 ms (TestTheSessionRequestIsNeverHeldByTheGate against s3fake on
// loopback, 2026-10-18); wider, it is a matter of how many requests wait
// ahead of the session request when the session expires, and an asynchronous
// refresh, which runs under the context of the request that started it, is
// lost whenever that request finishes first. Known by the operation the SDK names in its own context
// ([sessionOperation]), never by URL or bucket — a directory bucket's object
// requests share both and must still wait; it holds no place and tells the
// gate nothing, and the SDK sends at most one at a time per bucket.
//
// FIRST COME, FIRST SERVED: an attempt that has to wait waits in line under
// its own context, and is let out as a place frees or the limit grows — never
// overtaken by one that arrived later, so no request starves behind a stream
// of newer ones until its deadline.
type gate struct {
	// now is the clock the quiet minute is measured on: time.Now, or a
	// case's own.
	now func() time.Time

	mu sync.Mutex
	// limit is how many attempts are let out at once — its whole part,
	// and never fewer than one — or zero for no limit at all.
	limit float64
	// out is how many attempts are out now.
	out int
	// width is the node's width: the most attempts it has had out at once
	// since the gate last cut it, and since a cut never less than the
	// limit that cut left; a lift leaves it as it is. It is never fewer
	// than the attempts out when a throttle that cuts comes back — every
	// attempt let out raises it to what is then out, and one let out since
	// the last cut was let out under the limit the cut set it to — so a cut
	// takes its share of the width alone.
	width float64
	// cuts counts the cuts so far; an attempt carries the count it was
	// let out under ([pass]).
	cuts uint64
	// heard is when the bucket last throttled an attempt.
	heard time.Time
	// queue is the attempts waiting for a place, first come first.
	queue []chan pass
}

// pass is one attempt the gate let out, and the count of cuts it was let out
// under.
type pass struct{ cuts uint64 }

func newGate(now func() time.Time) *gate { return &gate{now: now} }

// enter waits, under ctx, for the gate to let an attempt out.
func (g *gate) enter(ctx context.Context) (pass, error) {
	g.mu.Lock()
	g.liftLocked()
	if len(g.queue) == 0 && g.roomLocked() {
		p := g.takeLocked()
		g.mu.Unlock()
		return p, nil
	}
	let := make(chan pass, 1)
	g.queue = append(g.queue, let)
	g.mu.Unlock()
	select {
	case p := <-let:
		return p, nil
	case <-ctx.Done():
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-let:
		// LET OUT AS ITS CONTEXT ENDED: its place goes to the next in
		// line, telling the gate nothing.
		g.out--
		g.letOutLocked()
	default:
		g.queue = slices.DeleteFunc(g.queue, func(q chan pass) bool { return q == let })
	}
	return pass{}, ctx.Err()
}

// leave gives back the place p held, and tells the gate what the bucket made
// of the attempt: a throttle cuts it to [throttleShare] of the node's width —
// once a round — an answer grows it, and anything else moves it neither way.
func (g *gate) leave(p pass, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.out--
	switch {
	case isThrottle(err):
		g.heard = g.now()
		if p.cuts == g.cuts {
			g.limit = max(1, throttleShare*g.width)
			g.width = g.limit
			g.cuts++
		}
	case answered(err) && g.limit > 0:
		g.limit += 1 / g.limit
	}
	g.liftLocked()
	g.letOutLocked()
}

// roomLocked reports whether the gate has a place for one more attempt.
func (g *gate) roomLocked() bool { return g.limit == 0 || g.out < max(1, int(g.limit)) }

// takeLocked lets one attempt out, the node's width raised to what is then
// out.
func (g *gate) takeLocked() pass {
	g.out++
	g.width = max(g.width, float64(g.out))
	return pass{cuts: g.cuts}
}

// liftLocked lifts a limit the bucket has gone [throttleQuiet] without
// throttling, letting out everyone waiting.
func (g *gate) liftLocked() {
	if g.limit > 0 && g.now().Sub(g.heard) >= throttleQuiet {
		g.limit = 0
		g.letOutLocked()
	}
}

// letOutLocked lets the attempts waiting out, first come first, for as many
// places as the gate has.
func (g *gate) letOutLocked() {
	for len(g.queue) > 0 && g.roomLocked() {
		let := g.queue[0]
		g.queue[0] = nil
		g.queue = g.queue[1:]
		let <- g.takeLocked()
	}
}

// ID implements [middleware.FinalizeMiddleware].
func (g *gate) ID() string { return "crewlet.ThrottleGate" }

// sessionOperation is the operation S3 Express signs a directory bucket's
// requests with, which the [gate] lets straight through: see
// [gate.HandleFinalize].
//
// THE SDK'S OWN NAME FOR WHAT IT IS RUNNING: invokeOperation clears the stack
// values of the context it is handed and names the operation in it, so inside
// the session request's own stack the name is `CreateSession` though it was
// called from another operation's `GetIdentity`, and inside a directory
// bucket's object request it is that request's own — a URL (`?session`) or a
// bucket (`…--x-s3`) would let the object requests through too.
const sessionOperation = "CreateSession"

// HandleFinalize sends one attempt through the gate: it waits for a place,
// sends the attempt, and gives the place back with the bucket's answer — save
// an attempt of S3 Express's session request ([sessionOperation]), which is
// sent at once, holds no place and tells the gate nothing.
func (g *gate) HandleFinalize(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (
	middleware.FinalizeOutput, middleware.Metadata, error) {
	if middleware.GetOperationName(ctx) == sessionOperation {
		return next.HandleFinalize(ctx, in)
	}
	p, err := g.enter(ctx)
	if err != nil {
		// CANCELLED, as the SDK spells it, so its retry step reads the
		// wait's end as final rather than an attempt to ask again.
		return middleware.FinalizeOutput{}, middleware.Metadata{},
			&aws.RequestCanceledError{Err: fmt.Errorf("waiting for the bucket's throttle to let it out: %w", err)}
	}
	out, md, err := next.HandleFinalize(ctx, in)
	g.leave(p, err)
	return out, md, err
}

// lastAnswerKey carries, on an operation's context, the [lastAnswer] its
// attempts leave.
type lastAnswerKey struct{}

// lastAnswer is the answer the bucket gave an operation's latest attempt —
// what [namesLastAnswer] adds to an operation its deadline ends.
type lastAnswer struct {
	mu  sync.Mutex
	err error
}

func (l *lastAnswer) set(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

func (l *lastAnswer) get() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// namesLastAnswer is the step every operation of the bucket's client begins
// with: it gives the operation a [lastAnswer], which [keepsLastAnswer] fills
// at every attempt, and adds the bucket's last answer to an operation that
// ends because its context did.
//
// THE DEADLINE ALONE SAYS NOTHING: with no count of attempts ([newRetryer]) a
// request the bucket keeps throttling or failing, or that cannot reach the
// bucket at all, ends only at its deadline, and the SDK reports that as the
// request cancelled — the `SlowDown`, the 500 or the connection refused that
// kept it waiting are gone. So the last one is named beside it, as TEXT: the
// error is still the deadline, and no caller that judges an error by what it
// is reads a throttle or a server error out of it.
var namesLastAnswer = middleware.InitializeMiddlewareFunc("crewlet.NamesLastAnswer",
	func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
		middleware.InitializeOutput, middleware.Metadata, error) {
		last := &lastAnswer{}
		out, md, err := next.HandleInitialize(context.WithValue(ctx, lastAnswerKey{}, last), in)
		if err != nil && ctx.Err() != nil {
			if answer := last.get(); answer != nil {
				err = fmt.Errorf("%w; the bucket's last answer before then: %s", err, answer.Error())
			}
		}
		return out, md, err
	})

// keepsLastAnswer keeps each failed attempt's answer as its operation's
// [lastAnswer] — every attempt but one its context ended, whose failure is
// the context's rather than the bucket's.
var keepsLastAnswer = middleware.FinalizeMiddlewareFunc("crewlet.KeepsLastAnswer",
	func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (
		middleware.FinalizeOutput, middleware.Metadata, error) {
		out, md, err := next.HandleFinalize(ctx, in)
		if err != nil && ctx.Err() == nil {
			if last, ok := ctx.Value(lastAnswerKey{}).(*lastAnswer); ok {
				last.set(err)
			}
		}
		return out, md, err
	})

// paced adds the client's own steps to every operation's stack: the last
// answer named at its end, and every attempt kept and sent through g — both
// directly after the SDK's retry step, so they run once an attempt.
func paced(g *gate) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		if err := stack.Initialize.Add(namesLastAnswer, middleware.Before); err != nil {
			return err
		}
		if err := stack.Finalize.Insert(g, "Retry", middleware.After); err != nil {
			return err
		}
		return stack.Finalize.Insert(keepsLastAnswer, "Retry", middleware.After)
	}
}
