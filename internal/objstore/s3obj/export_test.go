package s3obj

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
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

// OpenRetrying is [Open] with the client's attempts paced by backoff — nil is
// the production [fullJitter] — and its gate's quiet minute ([throttleQuiet])
// measured on now — nil is time.Now — so a case asks a bucket again in
// milliseconds rather than seconds, and moves a clock past a quiet minute
// rather than waiting one out. Everything else about the client is Open's
// own: the environment it reads included.
func OpenRetrying(ctx context.Context, cfg Config, backoff retry.BackoffDelayer, now func() time.Time) (*Backend, error) {
	if backoff == nil {
		backoff = fullJitter{}
	}
	if now == nil {
		now = time.Now
	}
	return open(ctx, cfg, backoff, now)
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
