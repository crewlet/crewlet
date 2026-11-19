package s3obj

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
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
// often one object's (MinIO answers an object it cannot rebuild with `503
// XMinioReadQuorum`). Read as a throttle it would cut the node's every
// request at each attempt of the one asking about that object, as the SDK's
// adaptive mode does. It is asked again as any server error is.
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

// unreachableByConstruction reads a dial no attempt can complete as FINAL: an
// address the dialer cannot parse (a port past 65535 or none, an IPv6 zone
// holding a `]`, too many colons) or port 0, which no listener holds. The SDK
// reads any dial error as worth asking again, so such an endpoint held the
// boot's bucket check for the whole give-up window ([judge.giveUpAfter]).
//
// ONLY WHAT NO RETRY CAN CHANGE: a refusal on another port, a timeout, an
// unreachable network and an unresolved name are asked again. And only the
// dialer's own words for an unparsable address ([unparsedAddress]), never
// every *net.AddrError: "no suitable address found" may differ next time.
// Config validation (checkS3Endpoint) refuses these endpoints before boot;
// this covers what it cannot see: the HTTPS_PROXY address.
var unreachableByConstruction = retry.IsErrorRetryableFunc(func(err error) aws.Ternary {
	if addr, ok := errors.AsType[*net.AddrError](err); ok && unparsedAddress[addr.Err] {
		return aws.FalseTernary
	}
	if op, ok := errors.AsType[*net.OpError](err); ok && op.Op == "dial" {
		if tcp, ok := op.Addr.(*net.TCPAddr); ok && tcp.Port == 0 {
			return aws.FalseTernary
		}
	}
	return aws.UnknownTernary
})

// unparsedAddress is every error the dialer gives an address it cannot parse,
// in its own words — net exports none of them as a value, so they are matched
// as it writes them, and TestADialNoAttemptCanCompleteIsNotAskedAgain draws
// each from the dialer itself, so a Go that rewords one fails there rather
// than quietly asking such a dial again for a minute.
var unparsedAddress = map[string]bool{
	"invalid port":               true,
	"missing port in address":    true,
	"too many colons in address": true,
	"unexpected '[' in address":  true,
	"unexpected ']' in address":  true,
}

// newRetryer is how the bucket's client asks a request again: the SDK's
// standard mode with no client-wide quota, no count of attempts and backoff
// pacing the attempts — [fullJitter] in production, a case's own in this
// package's tests.
func newRetryer(backoff retry.BackoffDelayer) aws.Retryer {
	return retry.AddWithMaxAttempts(retry.NewStandard(func(s *retry.StandardOptions) {
		// NO QUOTA ([ratelimit.None]): the SDK's standard retryer holds 500
		// tokens per client, so under a sustained `SlowDown` its retries ran
		// out and every request after failed at once, the opposite of slowing
		// down. The [gate] holds a throttled node back by slowing every
		// attempt. Requests asking again are bounded by who sends them: every
		// caller but the API's files is a CLOSED LOOP of its own width, so a
		// retry holds its slot and adds none, and a person's upload or
		// download is bounded by its deadline and the gate. No node-wide cap
		// goes on top: it would only fail requests that would have been
		// answered.
		s.RateLimiter = ratelimit.None
		// WHAT IS ASKED AGAIN is the SDK's own (500, 502, 503, 504, its
		// throttle codes, every failure to send) except a request given up
		// ([gaveUp]), an untrusted certificate ([refusedCertificate]) and a
		// dial no attempt can complete ([unreachableByConstruction]), each
		// final; plus a 429, which the SDK retries by no rule of its own.
		s.Retryables = append(append([]retry.IsErrorRetryable{gaveUp, refusedCertificate,
			unreachableByConstruction}, s.Retryables...),
			retry.RetryableHTTPStatusCode{Codes: map[int]struct{}{http.StatusTooManyRequests: {}}})
		s.Backoff = backoff
		// NO COUNT OF ATTEMPTS: attempts end at the request's DEADLINE
		// ([callBudget], [sendBudget], [partBudget], [completeBudget], or a
		// read's caller's, held to [objstore.Stall]) or after
		// [judge.giveUpAfter] of nothing but failures; the SDK's three
		// attempts failed requests a bucket would have answered seconds later.
		// The zero below is no count, and [Open] zeroes the client's
		// RetryMaxAttempts too: the SDK copies `AWS_MAX_ATTEMPTS` or a
		// profile's `max_attempts` into the client and wraps the retryer in
		// that count after every option has run.
	}), 0)
}

// fullJitter is how long a request waits before it is asked again: a random
// share of 2^attempt seconds, up to [retry.DefaultMaxBackoff] (twenty
// seconds): "full jitter", the SDK's curve jittered AT its ceiling too.
//
// The SDK's own backoff waits every retry past the fourth exactly twenty
// seconds, unjittered, so with no count of attempts every request that far
// would ask again in lockstep the instant the bucket recovered. Under
// AWS_NEW_RETRIES_2026 the SDK numbers retries from zero, one doubling down,
// and may add up to five seconds from `X-Amz-Retry-After`, past this.
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
	// THE LAST ATTEMPT AT THE SPAN'S OWN BOUND: a request inside its span of
	// failures ([failing]) waits no longer than what is left of the bound the
	// [judge] drew for it ([judge.bound], between forty seconds and the
	// minute), so it is asked once more AT that bound, not up to twenty
	// seconds past it. The bound is its OWN, never one minute for all: else
	// every request that began failing together would make its last attempt in
	// one instant, onto a bucket that may just be recovering, the herd this
	// jitter exists to spread.
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
// A MINUTE: three of the backoff's twenty-second ceiling ([fullJitter]), so a
// bucket still throttling is heard several times inside it; and long past the
// time the limit takes to climb back over the node's width (thirty-two cut to
// twenty-two recover in ten rounds, under two seconds). The lift frees a node
// gone QUIET: it gets too few answers to climb. A lifted gate costs a bucket
// still throttling one round, then cuts again ([gate.leave]). A TIMER the
// first throttle arms ([gate.armLocked]) lifts it, so places held by stalled
// attempts do not hold waiters past this minute.
const throttleQuiet = time.Minute

// gate is the client's answer to a throttle: how many attempts it lets the
// bucket hold at once, across the node's every request to its `files/`
// prefix, so the first throttle slows the whole node. Unlimited until then.
//
// A LIMIT ON ATTEMPTS IN FLIGHT, NOT A RATE: callers send in loops of their
// own width, so what is out is known exactly, and a cut is a share of the
// node's WIDTH ([gate.width]). The SDK's adaptive mode limited a measured
// rate: a fresh one had none, so its first throttle (every boot) dropped to
// half a request a second. An attempt holds its place
// until the bucket's answer BEGINS (a read's headers, a put's whole body).
//
// FIRST COME, FIRST SERVED: waiters are never overtaken, so none starves.
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
	// since the gate last cut it, and since a cut never less than the limit
	// that cut left; a lift leaves it as it is. THE WIDTH, NEVER THE MOMENT:
	// a cut of what was out when a throttle came back would leave a node
	// throttled on its lone HEAD between bursts at ONE place, climbing a
	// place a round for the minute before the lift. A node throttled before
	// it has had more than one attempt out has a width of one: a presumed
	// width would leave a narrower node unslowed by its first throttles.
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
//
// ONE CUT A ROUND: a throttle to an attempt let out before the last cut is
// the bucket's reply to the width that cut already answered and cuts nothing
// more; else a burst of thirty-two throttled together would cut to nothing.
// GROWN BY ONE A ROUND: every answer to a served request ([answered]) adds
// 1/limit, TCP's additive increase, so a fleet sharing a prefix converges on
// shares of its rate. A server error or failure to send moves it neither way.
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
// THE SDK'S OWN NAME FOR WHAT IT IS RUNNING: invokeOperation names the
// operation in a fresh context, so inside the session request it is
// `CreateSession` though called from another operation's `GetIdentity`, and
// inside a directory bucket's object request it is that request's own. A URL
// (`?session`) or bucket (`…--x-s3`) would let object requests through too.
const sessionOperation = "CreateSession"

// HandleFinalize sends one attempt through the gate: it waits for a place,
// sends the attempt, and gives the place back with the bucket's answer — save
// an attempt of S3 Express's session request ([sessionOperation]), which is
// sent at once, holds no place and tells the gate nothing.
//
// A MIDDLEWARE ROUND EACH ATTEMPT, never the retryer's attempt token: under
// AWS_NEW_RETRIES_2026 the SDK gives back only the first attempt's token. It
// sits after the SDK's retry step, BEFORE identity and signing: a credential
// resolved before the wait could expire during it (`ExpiredToken`, final).
// The session request is let through because it runs under a request already
// holding a place, so held here it would wait behind its own waiter
// (TestTheSessionRequestIsNeverHeldByTheGate).
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
// FAILURES is asked again before it is given up ([judge]): a minute from its
// first failure, each span given its own bound inside it ([judge.bound]), the
// client's ONE stall bound ([stallBound]). A FAILURE is a retryable attempt
// that is no throttle, no answer ([answered]) and not ended by its own
// context, NO ANSWER AT ALL included. A minute because it is [objstore.Stall]
// (a read and a write the bucket keeps failing end together), three of the
// backoff's ceiling ([fullJitter]) and [throttleQuiet]; shorter would give up
// on a gateway's failover. A constant: no honest reason makes deployments
// differ. By time, not attempts: a count means 25 to 200 seconds.
func (j *judge) giveUpAfter() time.Duration { return j.stall.stall }

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
	// most the [judge.giveUpAfter] it was judged under.
	span, most time.Duration
	attempts   int
	// stalled is a request given up because its last attempt stopped moving
	// ([stalled]), which no span measures: its sentence says so.
	stalled bool
}

func (g *givenUp) Error() string {
	if g.stalled {
		return fmt.Sprintf("the bucket stopped moving this request for %v (%d %s in all), so it is not asked "+
			"again; %v", g.most, g.attempts, plural(g.attempts, "attempt", "attempts"), g.err)
	}
	return fmt.Sprintf("the bucket failed every attempt of this request for %v (%d attempts), and a request "+
		"it keeps failing is given up %v to %v into its failures, so it is not asked again; its last answer: %v",
		g.span.Round(10*time.Millisecond), g.attempts, g.most-g.most/boundSpread, g.most, g.err)
}

func (g *givenUp) Unwrap() error { return g.err }

// plural is one where n is one, and many otherwise.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

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
	// quiet is how long its latest attempt had moved nothing when the
	// attempt's own context ended it IN FLIGHT, before its watch had
	// ([watchedClient]) — measured as that attempt ended, never later. Zero
	// while an attempt is in flight, and where its latest attempt ended any
	// other way: answered, failed by the transport, or ended by its watch.
	quiet time.Duration
}

// attemptBegins forgets what r's previous attempt left: a deadline that comes
// while this one is in flight is this one's to describe.
func (r *request) attemptBegins() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quiet = 0
}

// endedInFlight records that r's attempt's own context ended it in flight,
// quiet for d.
func (r *request) endedInFlight(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quiet = d
}

// quietAtEnd is how long r's latest attempt had moved nothing when its own
// context ended it in flight, or zero ([request.quiet]).
func (r *request) quietAtEnd() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.quiet
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
// ONE STEP PER CLIENT, built with the client's recorder and stall bound as the
// gate and the judge are: a step shared by every client could count into none.
func follows(count counted, bound *stallBound) middleware.InitializeMiddleware {
	return middleware.InitializeMiddlewareFunc("crewlet.FollowsRequest",
		func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
			middleware.InitializeOutput, middleware.Metadata, error) {
			operation := operationOf(middleware.GetOperationName(ctx))
			r := &request{}
			out, md, err := next.HandleInitialize(middleware.WithStackValue(ctx, requestKey{}, r), in)
			attempts := r.attemptsSent()
			count.request(operation, requestOutcome(ctx, attempts, err), attempts)
			// THE DEADLINE ALONE SAYS NOTHING: a request the bucket keeps
			// throttling, or fails for less than [judge.giveUpAfter], ends
			// cancelled with the `SlowDown` or 500 that kept it waiting gone,
			// so the last answer is named as TEXT (the error stays the
			// deadline; no caller reads a throttle out of it). So is how long
			// the last attempt had not moved, ONLY where the deadline ended
			// it IN FLIGHT and it had been quiet past a sample of the bound
			// ([stallBound], [request.quiet]): a deadline with no attempt in
			// flight ended nothing the bucket went quiet on.
			if err != nil && ctx.Err() != nil {
				if answer := r.lastAnswer(); answer != nil {
					err = fmt.Errorf("%w; the bucket's last answer before then: %s", err, answer.Error())
				}
				if quiet := r.quietAtEnd(); quiet > bound.sample {
					err = fmt.Errorf("%w; its last attempt had moved nothing for %v", err,
						quiet.Round(100*time.Millisecond))
				}
			}
			return out, md, err
		})
}

// judge is the step that reads each attempt's answer into its [request], and
// gives up a request the bucket has answered with nothing but failures to the
// end of its span's bound ([judge.bound]): a failure at or past it is handed
// on as [givenUp] (final, [gaveUp]), any before it as [failing].
// THE SPAN IS WALL TIME BETWEEN THE REQUEST'S OWN FAILURES, gate waits
// included. Only a THROTTLE ANSWERED TO THE REQUEST ITSELF ends it: one
// answered to ANOTHER never does, or one object's failure (MinIO's
// `XMinioReadQuorum`) beside a throttled bucket would be asked until its
// deadline. A HEAD's 503 is a throttle by [isThrottle], so a Stat is never
// given up on 503s. It runs AFTER THE GATE, so it sees only attempts let out
// (a wait ended there is no attempt) and S3 Express's session request; a waiter's error carries that failure as TEXT, so nothing
// there reads as [givenUp]. Every attempt is counted before it is judged.
type judge struct {
	now     func() time.Time
	retryer aws.Retryer
	// stall is the client's one stall bound ([stallBound]), which
	// [judge.giveUpAfter] reads.
	stall *stallBound
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
	if isStalled(err) {
		// GIVEN UP AT ONCE, BY ITS KIND, whatever wraps it, never by dating
		// its last movement against the span: the watch's bound is real time
		// on the transport's side and the span is on the judge's clock (a
		// case's own in stepped tests). An attempt the watch ended has gone a
		// whole bound with nothing moving; asked again, it would hold the
		// person another minute for a bucket that just showed it hangs.
		return &givenUp{err: err, stalled: true, most: j.giveUpAfter(), attempts: r.attempts}
	}
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
		return &givenUp{err: err, span: span, most: j.giveUpAfter(), attempts: r.attempts}
	}
	return &failing{err: err, left: r.bound - span}
}

// boundSpread is the share of [judge.giveUpAfter] a span's bound is drawn
// from, at its end: the last THIRD — twenty seconds of the minute, the
// twenty-second ceiling the backoff spreads every other attempt over
// ([fullJitter]).
const boundSpread = 3

// bound is the bound a span of failures beginning now is given: an even
// share of the last [boundSpread]th of [judge.giveUpAfter] short of it — forty
// seconds to a minute.
//
// DRAWN FOR EACH SPAN, never one bound for every request: the span's last
// attempt is sent at its bound, and one bound sent the last attempt of every
// request that began failing in one instant in one instant too. Drawn as the
// span begins, so the whole span's waits are cut to it; and again for a span
// a throttle restarted, which is a span of its own.
func (j *judge) bound() time.Duration {
	return j.giveUpAfter() - shareOf(j.draw, j.giveUpAfter()/boundSpread)
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
