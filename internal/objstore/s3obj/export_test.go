package s3obj

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/crewlet/crewlet/internal/objstore"
)

// CutParts sizes every part of b's multipart uploads but the last at n bytes,
// so a case reaches the multipart path with a body it can afford — see
// [Backend.partBytes].
func CutParts(b *Backend, n int) { b.cutParts(n) }

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

// Rebucket points b at another bucket of the same endpoint without opening
// it: the SDK reaches a directory bucket only through S3 Express's
// CreateSession, which the fake does not speak, so a case refusing one's
// ranges stands one up this way.
func Rebucket(b *Backend, name string) { b.bucket = name }

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
