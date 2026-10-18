package s3obj

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"

	"github.com/crewlet/crewlet/internal/objstore"
)

// CutParts sizes every part of b's multipart uploads but the last at n bytes,
// so a case reaches the multipart path with a body it can afford — see
// [Backend.partBytes].
func CutParts(b *Backend, n int) { b.cutParts(n) }

// Clock is the time a client's gate keeps ([clock]): Now, the instant it
// measures its quiet minute ([throttleQuiet]) from, and After, the timer that
// lifts it when the minute ends. A nil field is the production one —
// time.Now, and time.AfterFunc.
type Clock struct {
	Now   func() time.Time
	After func(d time.Duration, f func())
}

// OpenRetrying is [Open] with the client's attempts paced by backoff — nil is
// the production [fullJitter] — and its gate's time kept by c, so a case asks
// a bucket again in milliseconds rather than seconds, and moves a clock past a
// quiet minute — and fires the timer that ends it — rather than waiting one
// out. Everything else about the client is Open's own: the environment it
// reads included.
func OpenRetrying(ctx context.Context, cfg Config, backoff retry.BackoffDelayer, c Clock) (*Backend, error) {
	if backoff == nil {
		backoff = fullJitter{}
	}
	kept := systemClock
	if c.Now != nil {
		kept.now = c.Now
	}
	if c.After != nil {
		kept.after = c.After
	}
	return open(ctx, cfg, backoff, kept)
}

// SetStall makes d b's one stall bound ([stallBound]) in place of
// [objstore.Stall], and its socket sample d / 12: a request the bucket answers
// with nothing but failures is given up within d — each span's bound drawn
// from d's last third, as the minute's is — and an attempt the bucket stops
// moving is ended d after it last moved, so a case runs both rules in real
// time at a bound it can afford. Set before the case's first request: a watch
// takes the bound as its attempt begins.
func SetStall(b *Backend, d time.Duration) { b.stall = newStallBound(d) }

// Watches is how many attempts of b's are being watched now: none once every
// request has ended, whatever became of it ([watchedClient]).
func Watches(b *Backend) int { return int(b.watched.live.Load()) }

// WatchedClient is [watchedClient], for the cases that drive a watch with a
// transport of their own.
type WatchedClient = watchedClient

// Watching is next with every attempt watched under the stall bound stall.
func Watching(next s3.HTTPClient, stall time.Duration) *WatchedClient {
	bound := newStallBound(stall)
	return watching(next, &bound)
}

// Live is how many of c's attempts are being watched now.
func (c *WatchedClient) Live() int { return int(c.live.Load()) }

// Unacked is [unacked]: c's bytes its far end has not acknowledged, and
// whether they could be read.
func Unacked(c net.Conn) (int, bool) {
	n, err := unacked(c)
	return n, err == nil
}

// BucketProtocols is what the client [bucketClient] builds for bundle speaks:
// HTTP/1.1, and whether HTTP/2 too.
func BucketProtocols(bundle string) (http1, http2 bool, err error) {
	c, err := bucketClient(bundle)
	if err != nil {
		return false, false, err
	}
	t, ok := c.Transport.(*http.Transport)
	if !ok {
		return false, false, fmt.Errorf("the bucket's transport is a %T", c.Transport)
	}
	if t.Protocols == nil {
		return true, t.ForceAttemptHTTP2, nil
	}
	return t.Protocols.HTTP1(), t.Protocols.HTTP2(), nil
}

// Stalled is an attempt the bucket stopped moving ([stalled]): took of its
// body's of bytes taken, its answer begun or not, quiet for d.
func Stalled(took, of int64, answered bool, d time.Duration) error {
	return &stalled{took: took, of: of, answered: answered, quiet: d}
}

// SeedBounds makes b draw each span's bound ([judge.bound]) from a source
// seeded with seed, so a case that derives a figure from the bounds derives
// the same figure on every run. Not safe for concurrent use: a case drives one
// request at a time through it.
func SeedBounds(b *Backend, seed uint64) { b.judge.draw = seeded(seed) }

// GivenUp is [givenUp], for the cases that find a request given up.
type GivenUp = givenUp

// Attempts is how many attempts g's request had sent when it was given up.
func (g *GivenUp) Attempts() int { return g.attempts }

// Failing is err as a failure inside a request's span with left of the span's
// bound to go ([failing]), for the case that holds the backoff to it.
func Failing(err error, left time.Duration) error { return &failing{err: err, left: left} }

// SeededJitter is the production backoff ([fullJitter]) drawing its shares
// from a source seeded with seed — so a case that derives a figure from its
// draws derives the same figure on every run. Not safe for concurrent use: a
// case drives one request at a time through it.
func SeededJitter(seed uint64) retry.BackoffDelayer { return fullJitter{draw: seeded(seed)} }

// seeded is a random share of n from a source seeded with seed.
func seeded(seed uint64) func(n time.Duration) time.Duration {
	r := rand.New(rand.NewPCG(seed, seed))
	return func(n time.Duration) time.Duration { return time.Duration(r.Int64N(int64(n))) }
}

// ClientRetryer is the retryer b's client retries every request with, read off
// the client itself — after everything the SDK wraps around the retryer it
// was given.
func ClientRetryer(b *Backend) aws.Retryer { return b.client.Options().Retryer }

// FullJitter is [fullJitter], the production backoff, for the case that holds
// its spread.
var FullJitter retry.BackoffDelayer = fullJitter{}

// ThrottleQuiet and ThrottleShare are [throttleQuiet] and [throttleShare], for
// the cases that lift and cut a gate.
const (
	ThrottleQuiet = throttleQuiet
	ThrottleShare = throttleShare
)

// Attempt is one attempt b's gate let out, held until a case answers it as it
// chooses.
type Attempt struct {
	g *gate
	p pass
}

// Enter waits, under ctx, for b's gate to let an attempt out — as every
// attempt the client sends does.
func Enter(ctx context.Context, b *Backend) (Attempt, error) {
	p, err := b.gate.enter(ctx)
	return Attempt{g: b.gate, p: p}, err
}

// Leave gives the attempt's place back, the bucket having answered it err.
func (a Attempt) Leave(err error) { a.g.leave(a.p, err) }

// LetOut is how many of most attempts b's gate lets out at once, none waiting
// for a place: each is held until all have been asked for, then given back
// as an attempt its context ended, which tells the gate nothing. A gate
// nothing has slowed lets out all of them.
func LetOut(b *Backend, most int) int {
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	var held []pass
	for range most {
		p, err := b.gate.enter(ended)
		if err != nil {
			break
		}
		held = append(held, p)
	}
	for _, p := range held {
		b.gate.leave(p, context.Canceled)
	}
	return len(held)
}

// Out is how many attempts b's gate has out now.
func Out(b *Backend) int {
	b.gate.mu.Lock()
	defer b.gate.mu.Unlock()
	return b.gate.out
}

// Waiting is how many attempts are waiting in line at b's gate now.
func Waiting(b *Backend) int {
	b.gate.mu.Lock()
	defer b.gate.mu.Unlock()
	return len(b.gate.queue)
}

// MaxParts is [maxParts], for the case that pins the part arithmetic.
const MaxParts = maxParts

// PartsInFlight and SpareParts are [partsInFlight] and [spareParts], for the
// cases that hold an upload to them.
const (
	PartsInFlight = partsInFlight
	SpareParts    = spareParts
)

// PartBudget and CallBudget are [partBudget] and [callBudget], for the case
// that holds a part's deadline to its share of the link.
var PartBudget = partBudget

const CallBudget = callBudget

// SparesHeld is how many of the node's spare part buffers b's uploads and
// reads hold now — none, once every upload has ended and every read is
// closed, whatever became of them.
func SparesHeld(b *Backend) int { return len(b.spare) }

// TakeSpares takes every spare part buffer of b's allowance that is free, as
// uploads and reads already under way would hold them, and answers what gives
// them back.
func TakeSpares(b *Backend) (giveBack func()) {
	taken := 0
	for b.takeSpare() {
		taken++
	}
	return func() { b.returnSpares(taken) }
}

// SpanOf is [spanOf], for the case that holds a range's answer to the range
// asked for.
func SpanOf(contentRange string, from, most, size int64) (n, total int64, err error) {
	return spanOf(contentRange, from, most, size)
}

// Operations, AttemptOutcomes and RequestOutcomes are [operations],
// [attemptOutcomes] and [requestOutcomes] — copies, so no case can change what
// the client counts under — for the cases that hold the counts and their
// documentation to them.
var (
	Operations      = slices.Clone(operations)
	AttemptOutcomes = slices.Clone(attemptOutcomes)
	RequestOutcomes = slices.Clone(requestOutcomes)
)

// OtherOperation is [otherOperation], the label of an operation outside
// [Operations].
const OtherOperation = otherOperation

// AttemptOutcome and RequestOutcome are [attemptOutcome] and
// [requestOutcome], for the case that holds every kind of answer to exactly
// one outcome.
var (
	AttemptOutcome = attemptOutcome
	RequestOutcome = requestOutcome
)

// GiveUp is err as the last failure of a request given up after attempts
// attempts ([givenUp]).
func GiveUp(err error, attempts int) error {
	return &givenUp{err: err, span: objstore.Stall, most: objstore.Stall, attempts: attempts}
}

// GateLetsOut sends one attempt of operation through b's gate under ctx — the
// operation named in the context as the SDK names it — and reports whether the
// gate let it out: whether the step after the gate ran before ctx ended.
func GateLetsOut(ctx context.Context, b *Backend, operation string) (ran bool) {
	next := middleware.FinalizeHandlerFunc(func(context.Context, middleware.FinalizeInput) (
		middleware.FinalizeOutput, middleware.Metadata, error) {
		ran = true
		return middleware.FinalizeOutput{}, middleware.Metadata{}, nil
	})
	_, _, _ = b.gate.HandleFinalize(middleware.WithOperationName(ctx, operation), middleware.FinalizeInput{}, next)
	return ran
}

// LateToLetGo makes every answer b reads take d, once its request has been
// cancelled, to say it has been — as a body whose connection a stalled peer
// still holds open can — so a case can tell a Close that waits for the ranges
// it cancelled from one that only cancels them: without it a cancelled range
// lets go within microseconds, and a Close that did not wait would pass
// whenever every range had let go before anything looked.
func LateToLetGo(b *Backend, d time.Duration) {
	opts := b.client.Options()
	b.client = s3.New(opts, func(o *s3.Options) { o.HTTPClient = lateClient{HTTPClient: opts.HTTPClient, d: d} })
}

// lateClient is the bucket's own client, its answers late to let go.
type lateClient struct {
	s3.HTTPClient
	d time.Duration
}

func (c lateClient) Do(req *http.Request) (*http.Response, error) {
	res, err := c.HTTPClient.Do(req)
	if err == nil {
		res.Body = &lateBody{ReadCloser: res.Body, ctx: req.Context(), d: c.d}
	}
	return res, err
}

// lateBody is an answer whose read, once its request is cancelled, fails only
// after d.
type lateBody struct {
	io.ReadCloser
	ctx context.Context
	d   time.Duration
}

func (b *lateBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && b.ctx.Err() != nil {
		time.Sleep(b.d)
	}
	return n, err
}

// OneStream is b with every read made as ONE GetObject, however large — the
// way a read was made before a large one was read in ranges, and the stream
// the benchmarks hold a ranged read to at the same first byte.
func OneStream(b *Backend) objstore.Backend { return oneStream{b} }

type oneStream struct{ *Backend }

func (o oneStream) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	if err := objstore.CheckRead(ctx, off, n); err != nil {
		return nil, err
	}
	in := &s3.GetObjectInput{Bucket: aws.String(o.bucket), Key: aws.String(o.key(name))}
	switch {
	case n > 0:
		in.Range = aws.String(fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	case off > 0:
		in.Range = aws.String(fmt.Sprintf("bytes=%d-", off))
	}
	out, err := o.client.GetObject(ctx, in)
	switch {
	case missing(err):
		return nil, objstore.ErrNotFound
	case status(err) == http.StatusRequestedRangeNotSatisfiable:
		return io.NopCloser(bytes.NewReader(nil)), nil
	case err != nil:
		return nil, fmt.Errorf("s3obj: get %s: %w", name, err)
	}
	return out.Body, nil
}
