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

	"github.com/crewlet/crewlet/internal/objstore"
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
// again by every request open at the time, each at the backoff, for no longer
// than a minute after its first failure ([giveUpAfter]) or its deadline,
// whichever is first: nine attempts a request at the median and twelve at
// the 95th percentile ([giveUpAfter] says where those figures come from), then
// it stops, where a part asked a failing bucket again for its whole sixteen
// and a half minutes, about a hundred. No node-wide cap is put on top: what is
// left is the retry of a 500 for a minute that AWS recommends, which a cap
// would shorten only by failing requests that would have been answered.
//
// NO COUNT OF ATTEMPTS: every request's attempts end at its own DEADLINE —
// [callBudget], [sendBudget], [partBudget], [completeBudget], or a read's
// caller's, whose open the store holds to [objstore.Stall] too — or
// inside a minute of nothing but failures ([judge]), and the SDK's three
// attempts failed a request a bucket would have answered a few seconds later.
// MaxAttempts answers zero, which the SDK reads as no count, and [Open] sets
// the client's RetryMaxAttempts to zero too, because the SDK copies
// `AWS_MAX_ATTEMPTS`, or a shared profile's `max_attempts`, into the client and
// wraps the retryer in that count after every option has run.
//
// WHAT IS ASKED AGAIN is the SDK's own — a 500, 502, 503 or 504, its throttle
// codes, every failure to send — but a request given up ([gaveUp]) and a
// certificate the node does not trust ([refusedCertificate]), both final, and
// with a 429 besides: the SDK retries a gateway's `429 Too Many Requests` by
// no rule of its own.
func newRetryer(backoff retry.BackoffDelayer) aws.Retryer {
	return retry.AddWithMaxAttempts(retry.NewStandard(func(s *retry.StandardOptions) {
		s.RateLimiter = ratelimit.None
		s.Retryables = append(append([]retry.IsErrorRetryable{gaveUp, refusedCertificate}, s.Retryables...),
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
// request reach it; with none, every request asked again past its fourth
// retry — one the bucket keeps throttling, until its deadline (a part's
// sixteen and a half minutes, a completion's five), and one it keeps failing,
// until its span's bound inside the minute ([giveUpAfter]), half of them nine
// attempts or more — would wait in lockstep with every other that was
// answered when it was, all asking again in the same instant once the bucket
// recovered and none in the twenty seconds before it. Jittered, they are
// spread over the twenty seconds and the first are asked again at once.
//
// Under AWS_NEW_RETRIES_2026 the SDK numbers a request's retries from zero
// rather than one, which takes the curve one doubling down: the first retry
// waits up to a second rather than two — and it may add up to five seconds
// a bucket's `X-Amz-Retry-After` asks for, past what this answers.
//
// THE LAST ATTEMPT AT THE SPAN'S OWN BOUND: a request inside its span of
// failures ([failing]) waits no longer than what is left of the bound the
// [judge] drew for that span — a point in the last third of [giveUpAfter],
// between forty seconds and the minute ([judge.bound]) — so it is asked once
// more AT that bound rather than at a random point up to twenty seconds past
// it: the give-up comes one attempt after the bound, and a failure shorter
// than it is ridden through whenever an attempt lands after the failure. A
// bound OF ITS OWN, never the minute for every request: cut to one bound,
// these waits put the last attempt of every request that began failing in one
// instant ON that instant's minute, all within a round trip of each other —
// every write open on every node, onto a bucket that may just be recovering —
// the herd this jitter exists to spread.
type fullJitter struct {
	// draw is a random share of n: math/rand/v2's, or a case's own seeded one
	// so a figure the docs quote is the same on every run.
	draw func(n time.Duration) time.Duration
}

func (j fullJitter) BackoffDelay(attempt int, err error) (time.Duration, error) {
	ceiling := retry.DefaultMaxBackoff
	if attempt < 5 {
		ceiling = min(ceiling, time.Second<<max(attempt, 0))
	}
	d := shareOf(j.draw, ceiling)
	if f, ok := errors.AsType[*failing](err); ok {
		d = min(d, f.left)
	}
	return d, nil
}

// shareOf is a random share of n drawn by draw, or by math/rand/v2 where
// draw is nil.
func shareOf(draw func(time.Duration) time.Duration, n time.Duration) time.Duration {
	if draw == nil {
		return rand.N(n)
	}
	return draw(n)
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
//
// LIFTED BY A TIMER the first throttle arms ([gate.armLocked]), so a gate
// whose places are all held by attempts that have stopped answering — a part
// on a bucket that took its connection and stopped reading, a read whose
// headers never come — is still lifted on time: lifted only when an attempt
// next entered or left it, it held its waiters until their own deadlines, past
// the minute this promises.
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
// it neither way. LIFTED after [throttleQuiet] without a throttle, by a timer
// the first throttle arms, so a gate whose places are all held by attempts
// that have stopped answering is still lifted on time — lifted only when an
// attempt next entered or left, it held its waiters until their own
// deadlines.
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
	// now is the clock the quiet minute is measured on, and after the timer
	// that ends it ([clock]): the wall clock's, or a case's own.
	now   func() time.Time
	after func(d time.Duration, f func())

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
	// armed is whether the timer that lifts the gate is set: ONE at a time,
	// set by the throttle that engages the gate and set again only by its
	// own firing ([gate.wake]), so a storm of throttles arms nothing new.
	armed bool
	// queue is the attempts waiting for a place, first come first.
	queue []chan pass

	// count is where the gate's two gauges go ([gate.reportLocked]), and
	// shownLimit and shownWaiting what it last set them to.
	count        counted
	shownLimit   int
	shownWaiting int
}

// pass is one attempt the gate let out, and the count of cuts it was let out
// under.
type pass struct{ cuts uint64 }

func newGate(c clock, count counted) *gate { return &gate{now: c.now, after: c.after, count: count} }

// enter waits, under ctx, for the gate to let an attempt out.
func (g *gate) enter(ctx context.Context) (pass, error) {
	g.mu.Lock()
	g.liftLocked()
	if len(g.queue) == 0 && g.roomLocked() {
		p := g.takeLocked()
		g.reportLocked()
		g.mu.Unlock()
		return p, nil
	}
	let := make(chan pass, 1)
	g.queue = append(g.queue, let)
	g.reportLocked()
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
	g.reportLocked()
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
		g.armLocked()
	case answered(err) && g.limit > 0:
		g.limit += 1 / g.limit
	}
	g.liftLocked()
	g.letOutLocked()
	g.reportLocked()
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
//
// IDEMPOTENT, and asked whenever an attempt enters or leaves as well as when
// the lift timer fires: the timer is what guarantees the minute, never the
// only path to it.
func (g *gate) liftLocked() {
	if g.limit > 0 && g.now().Sub(g.heard) >= throttleQuiet {
		g.limit = 0
		g.letOutLocked()
	}
}

// armLocked sets the timer that lifts an engaged gate when the minute after
// the bucket's last throttle ends — unless one is already set, which will
// find a later throttle and set itself again for what is left ([gate.wake]).
func (g *gate) armLocked() {
	if g.armed || g.limit == 0 {
		return
	}
	g.armed = true
	g.after(throttleQuiet-g.now().Sub(g.heard), g.wake)
}

// wake is the lift timer firing: it lifts the gate if the bucket has gone the
// minute without a throttle, and otherwise — a later throttle moved the
// minute on — sets itself again for what is left of it.
func (g *gate) wake() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = false
	g.liftLocked()
	g.armLocked()
	g.reportLocked()
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

// reportLocked sets the gate's gauges to what it holds now, each only when it
// has changed since the gate last set it — so a node nothing throttles sets
// neither, and a gate under load sets one at a cut, at a whole place grown, at
// the lift and as its line moves, never at every attempt.
//
// THE WHOLE NUMBER OF PLACES, never the limit itself: the limit grows by a
// fraction of a place an answer, and what holds the node back is how many
// attempts it lets out, [gate.roomLocked]'s figure — at least one while it is
// engaged, and zero while it is lifted, which a quiet node reads within the
// minute after its last throttle ([gate.armLocked]).
func (g *gate) reportLocked() {
	limit := 0
	if g.limit > 0 {
		limit = max(1, int(g.limit))
	}
	if limit != g.shownLimit {
		g.shownLimit = limit
		g.count.gateLimit(limit)
	}
	if waiting := len(g.queue); waiting != g.shownWaiting {
		g.shownWaiting = waiting
		g.count.gateWaiting(waiting)
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

// giveUpAfter is how long a request the bucket answers with nothing but
// FAILURES is asked again, at the most, before it is given up ([judge]): a
// minute from its first failure — each span of failures given a bound of its
// own inside it, between forty seconds and the minute ([judge.bound]).
//
// A FAILURE is an attempt the retryer would ask again that is neither a
// throttle ([isThrottle]) nor answered ([answered]) nor ended by its own
// context: a 500, 502, 503 naming a gateway's own fault, 504, an S3
// `InternalError`-class code — and NO ANSWER AT ALL, a connection refused or
// reset, a name that does not resolve for now, a TLS handshake that failed for
// any reason but the certificate ([refusedCertificate], final already). No
// answer counts as failing, though only server errors were asked about: a
// bucket that refuses connections holds a person's part exactly as long as
// one answering 500, and both are the bucket failing rather than slowing the
// node down.
//
// A MINUTE, and one minute three ways: it is [objstore.Stall], the minute
// a READ already waits on its backend before it ends as stalled — "a bucket
// that keeps failing the request it asks again" is in its own doc — so a read
// and a write the bucket keeps failing end at the same point, and a person
// reads the same minute in both; it is three of the backoff's twenty-second
// ceiling ([fullJitter]), so a failing request is asked again several times
// at the ceiling; and it is [throttleQuiet], the minute the gate itself takes
// a bucket's silence to mean a throttle is over. Shorter (a [callBudget])
// would give up inside the ceiling's first two waits and on a gateway's
// ordinary failover; longer buys a person nothing. A constant, never
// configuration: no honest reason makes two deployments differ, and the one
// value that might, Stall, it follows.
//
// EACH SPAN'S OWN BOUND, drawn evenly from the minute's last third at the
// span's first failure ([judge.bound]), is where its last attempt is sent
// ([fullJitter]): so the requests that began failing in one instant — every
// write open on every node when the bucket fell over — send their last
// attempts across twenty seconds rather than all in one instant at the
// minute, onto a bucket that may just be recovering, which is the burst the
// backoff's jitter exists to spread. THE TRADE: a request is given up as soon
// as forty seconds into a failure, and rides through one that ends in the
// minute's last twenty seconds only where its own bound lies past it — where
// one bound of a minute for every request rode through any failure shorter
// than the minute, and sent every last attempt at once.
//
// WHAT A FAILING REQUEST SENDS inside its bound is the backoff's to decide, so
// it is a distribution, not a figure: at S3's 100 ms first byte, a median of
// nine attempts, twelve at the 95th percentile and at most fourteen in two
// thousand requests, given up fifty seconds into the failure at the median
// (TestAFailingRequestsLastAttemptsAreSpreadOverTheirBounds: the production
// backoff and bounds seeded, the bucket's answers on a clock of its own).
//
// PER REQUEST AND BY TIME. Per request rather than a node-wide judgement of
// the bucket, which would fail a request that has had one failure because
// others had many, and would be a second health model beside the gate; by
// time rather than a count of attempts, because a person waits on time and the
// backoff makes a count mean anything from twenty-five seconds to two hundred
// — and the SDK's count is what [newRetryer] removed.
//
// WHAT IT CHANGES, caller by caller: a READ — a person's download, a git sync,
// the backup — whose open the bucket keeps failing ends at its span's bound,
// forty seconds to a minute in, where [objstore.Stall] ended it a minute
// after the open; a request on [callBudget] — the collector, the check at
// open, an upload's start and abandonment — nothing, its thirty seconds come
// first; a WRITE — a put of up to a part (four and a half minutes to a minute
// at the most), a part ([partBudget]'s sixteen and a half to one), a
// completion (five to one). Every writer is somebody waiting.
const giveUpAfter = objstore.Stall

// gaveUp reads a request its [judge] gave up ([givenUp]) as final, ahead of
// every other rule: whatever the bucket's last answer was, the request is not
// asked again.
var gaveUp = retry.IsErrorRetryableFunc(func(err error) aws.Ternary {
	if _, ok := errors.AsType[*givenUp](err); ok {
		return aws.FalseTernary
	}
	return aws.UnknownTernary
})

// givenUp is a request's last failure once the bucket has answered it with
// nothing but failures to the end of its span's bound ([judge.bound]): what
// the request's caller is told, the bucket's last answer inside it — so
// errors.As still finds the bucket's own error and status() its status, and a
// batch deletion given up is still the bucket failing, never
// [objstore.ErrNoBatch].
//
// IT NAMES HOW LONG THE BUCKET FAILED THE REQUEST, and the range a span's
// bound is drawn from: a request is given up anywhere from forty seconds into
// a failure, and a message saying a minute would overstate it.
type givenUp struct {
	err error
	// span is the time from the request's first failure to its last, and
	// most the [giveUpAfter] it was judged under.
	span, most time.Duration
	attempts   int
}

func (g *givenUp) Error() string {
	return fmt.Sprintf("the bucket failed every attempt of this request for %v (%d attempts), and a request "+
		"it keeps failing is given up %v to %v into its failures, so it is not asked again; its last answer: %v",
		g.span.Round(10*time.Millisecond), g.attempts, g.most-g.most/boundSpread, g.most, g.err)
}

func (g *givenUp) Unwrap() error { return g.err }

// failing is a failure inside a request's failing span, and what is left of
// the span's bound ([judge.bound]): transparent — its text and its chain are
// the failure's own — save to [fullJitter], which waits no longer than what
// is left, so the request's last attempt is sent AT its bound.
type failing struct {
	err  error
	left time.Duration
}

func (f *failing) Error() string { return f.err.Error() }

func (f *failing) Unwrap() error { return f.err }

// requestKey carries, among an operation's stack values, the [request] its
// attempts are judged on — a STACK value, which the SDK clears at the start of
// every operation, so an operation another one starts — S3 Express's session
// request, from inside a waiting request's GetIdentity — never judges its
// attempts on its waiter's record.
type requestKey struct{}

// request is one operation of the client's, as its attempts have left it.
type request struct {
	mu sync.Mutex
	// last is the bucket's answer to the latest attempt its own context
	// did not end — what an operation its deadline ends is told.
	last error
	// attempts is how many attempts it has sent.
	attempts int
	// failingSince is when its current span of failures began: zero while
	// it has none, and set to zero again by a throttle answered to it; and
	// bound is that span's own bound, drawn as it began ([judge.bound]).
	failingSince time.Time
	bound        time.Duration
}

// lastAnswer is the bucket's answer to r's latest attempt its own context did
// not end, or nil.
func (r *request) lastAnswer() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// attemptsSent is how many attempts r has sent.
func (r *request) attemptsSent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts
}

// follows is the step every operation of the bucket's client begins with: it
// gives the operation a [request], which the [judge] fills at every attempt;
// counts the request into count as it ends — by its operation, how it ended
// ([requestOutcome]) and how many attempts it sent; and adds the bucket's last
// answer to an operation that ends because its context did.
//
// THE DEADLINE ALONE SAYS NOTHING: a request the bucket keeps throttling, or
// one it fails for less than [giveUpAfter] before the deadline comes, ends at
// its deadline, and the SDK reports that as the request cancelled — the
// `SlowDown`, the 500 or the connection refused that kept it waiting are gone.
// So the last one is named beside it, as TEXT: the error is still the
// deadline, and no caller that judges an error by what it is reads a throttle
// or a server error out of it.
//
// ONE STEP PER CLIENT, built with the client's recorder as the gate and the
// judge are: a step shared by every client could count into none.
func follows(count counted) middleware.InitializeMiddleware {
	return middleware.InitializeMiddlewareFunc("crewlet.FollowsRequest",
		func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
			middleware.InitializeOutput, middleware.Metadata, error) {
			operation := operationOf(middleware.GetOperationName(ctx))
			r := &request{}
			out, md, err := next.HandleInitialize(middleware.WithStackValue(ctx, requestKey{}, r), in)
			attempts := r.attemptsSent()
			count.request(operation, requestOutcome(ctx, attempts, err), attempts)
			if err != nil && ctx.Err() != nil {
				if answer := r.lastAnswer(); answer != nil {
					err = fmt.Errorf("%w; the bucket's last answer before then: %s", err, answer.Error())
				}
			}
			return out, md, err
		})
}

// judge is the step that reads each attempt's answer into its [request], and
// gives up a request the bucket has answered with nothing but failures to the
// end of its span's bound ([judge.bound], inside [giveUpAfter]): the failure
// at or past the bound is handed on as [givenUp], which the retryer reads as
// final ([gaveUp]). Every failure before then is handed on as [failing],
// carrying what is left of the bound.
//
// THE SPAN IS WALL TIME BETWEEN THE REQUEST'S OWN FAILURES, a wait at the gate
// included: a request that failed once, then waited out its bound at a gate
// other requests' throttles hold, and failed again, heard nothing but
// failures for that long, which is what the rule judges — and a gate held
// that long is a node far over its prefix's rate. Only a THROTTLE ANSWERED TO
// THE REQUEST ITSELF ends the span: the bucket is up and asking the node to
// slow down, which the gate and the request's own deadline govern. A throttle
// answered to ANOTHER request never does, or a failure of one object — MinIO's
// `XMinioReadQuorum` — beside a bucket throttling the rest would be asked
// until its deadline. A HEAD's 503 carries no code and is a throttle by
// [isThrottle]'s rule, so a Stat is never given up on 503s: telling a gateway's
// 503 from a throttle there would need a body S3 does not send.
//
// AFTER THE GATE, so it sees only attempts the gate let out — a wait whose
// context ended there is no attempt, and is the gate's own cancellation — and
// S3 Express's session request, which the gate lets straight through: a
// session the bucket keeps failing is given up like any request, and the
// request waiting for it in GetIdentity fails with it — as TEXT, since the SDK
// flattens the session's error (`get credentials: %v`), so nothing in the
// waiter's error is a [givenUp] or reads as worth asking again. The SDK's own
// text match for a reset connection is the one exception: a session whose last
// answer names one leaves its waiter a failure by this rule, asked again, and
// given up at the first of its sessions' give-ups that its own span, measured
// from the first of them, reaches its bound at — bounded, a minute or two in.
//
// IT COUNTS EVERY ATTEMPT into count, by its operation and what the bucket
// made of it ([attemptOutcome]) — a success, the session request and an
// attempt whose identity could not be had included — from the attempt's own
// answer, before it is judged: what the counter reads as a throttle or an
// answer is exactly what the gate, just before it, cut or grew by.
type judge struct {
	now         func() time.Time
	retryer     aws.Retryer
	giveUpAfter time.Duration
	// draw is a random share of n for each span's bound ([judge.bound]):
	// math/rand/v2's where nil, or a case's own seeded one.
	draw  func(n time.Duration) time.Duration
	count counted
}

// ID implements [middleware.FinalizeMiddleware].
func (j *judge) ID() string { return "crewlet.JudgesAttempt" }

// HandleFinalize sends one attempt and judges its answer.
func (j *judge) HandleFinalize(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (
	middleware.FinalizeOutput, middleware.Metadata, error) {
	out, md, err := next.HandleFinalize(ctx, in)
	j.count.attempt(operationOf(middleware.GetOperationName(ctx)), attemptOutcome(ctx, err))
	if r, ok := middleware.GetStackValue(ctx, requestKey{}).(*request); ok {
		err = j.judged(ctx, r, err)
	}
	return out, md, err
}

// judged is the answer err to one of r's attempts as r's caller and the
// retryer are to read it.
func (j *judge) judged(ctx context.Context, r *request, err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if err == nil || ctx.Err() != nil {
		// ENDED BY ITS OWN CONTEXT: the failure is the context's, not the
		// bucket's, and says nothing of the bucket.
		return err
	}
	r.last = err
	switch {
	case isThrottle(err):
		r.failingSince = time.Time{}
		return err
	case answered(err) || !j.retryer.IsErrorRetryable(err):
		return err
	}
	now := j.now()
	if r.failingSince.IsZero() {
		r.failingSince, r.bound = now, j.bound()
	}
	span := now.Sub(r.failingSince)
	if span >= r.bound {
		return &givenUp{err: err, span: span, most: j.giveUpAfter, attempts: r.attempts}
	}
	return &failing{err: err, left: r.bound - span}
}

// boundSpread is the share of [giveUpAfter] a span's bound is drawn from, at
// its end: the last THIRD — twenty seconds of the minute, the twenty-second
// ceiling the backoff spreads every other attempt over ([fullJitter]).
const boundSpread = 3

// bound is the bound a span of failures beginning now is given: an even
// share of the last [boundSpread]th of [giveUpAfter] short of it — forty
// seconds to a minute.
//
// DRAWN FOR EACH SPAN, never one bound for every request: the span's last
// attempt is sent at its bound, and one bound sent the last attempt of every
// request that began failing in one instant in one instant too. Drawn as the
// span begins, so the whole span's waits are cut to it; and again for a span
// a throttle restarted, which is a span of its own.
func (j *judge) bound() time.Duration {
	return j.giveUpAfter - shareOf(j.draw, j.giveUpAfter/boundSpread)
}

// paced adds the client's own steps to every operation's stack: the request
// followed from its start by f ([follows]), and every attempt sent through g
// and judged by j — directly after the SDK's retry step, in that order, so
// both run once an attempt.
func paced(f middleware.InitializeMiddleware, g *gate, j *judge) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		if err := stack.Initialize.Add(f, middleware.Before); err != nil {
			return err
		}
		if err := stack.Finalize.Insert(g, "Retry", middleware.After); err != nil {
			return err
		}
		return stack.Finalize.Insert(j, g.ID(), middleware.After)
	}
}
