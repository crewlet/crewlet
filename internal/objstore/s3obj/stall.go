package s3obj

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"

	"github.com/crewlet/crewlet/internal/objstore"
)

// stallSamples is how many times a write's socket is read inside one stall
// bound: TWELVE, so the stall is declared within a twelfth of the bound of the
// minute it is owed — 60 to 65 seconds — and a movement only the socket shows
// is dated at most five seconds late, which only ever favours the write.
//
// The cost is one ioctl per attempt in flight every five seconds, measured
// below the noise: thirty-two goroutines of HEADs on loopback ran 63.3–72.5 µs
// a HEAD without the watch and 65.3–77.6 with it, inside the runs' spread.
// Never a knob: there is nothing for an operator to trade.
const stallSamples = 12

// stallBound is the client's one stall bound: how long an attempt the bucket
// has stopped moving is let run ([watchedClient]), and how long a request the
// bucket answers with nothing but failures is asked again ([judge]) — ONE
// value, [objstore.Stall] in production, which the watches and the judge read
// alike, so a case that shortens one shortens both.
type stallBound struct {
	// stall is the bound itself: [objstore.Stall].
	stall time.Duration
	// sample is how often a watched attempt's socket is read: stall /
	// [stallSamples].
	sample time.Duration
}

// newStallBound is the stall bound d, sampled [stallSamples] times.
func newStallBound(d time.Duration) stallBound {
	return stallBound{stall: d, sample: d / stallSamples}
}

// errUncountable is a connection whose count of unacknowledged bytes this
// node cannot read: no TCP socket under it, or a platform without the call.
var errUncountable = errors.New("s3obj: this connection's unacknowledged bytes cannot be read")

// unacked is how many bytes c's socket has sent that the far end has not yet
// acknowledged — and those it has not yet sent: `SIOCOUTQ` on Linux, the
// send buffer's `SO_NWRITE` on darwin, each holding a byte until the far
// end's kernel takes it ([socketUnacked]).
//
// THROUGH THE CONNECTION'S OWN DESCRIPTOR, held open for the call by
// SyscallConn().Control, so a sample racing the connection's close reads an
// error — no change — never another socket's count. A TLS connection is read
// through the TCP socket under it.
func unacked(c net.Conn) (int, error) {
	for {
		inner, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		c = inner.NetConn()
	}
	sc, ok := c.(syscall.Conn)
	if !ok {
		return 0, errUncountable
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		n    int
		nerr error
	)
	if err := raw.Control(func(fd uintptr) { n, nerr = socketUnacked(fd) }); err != nil {
		return 0, err
	}
	return n, nerr
}

// stalled is an attempt the bucket stopped moving for the stall bound: it took
// no more of the request's body, acknowledged none of it at the node's socket
// and sent none of its answer ([watchedClient]).
//
// [objstore.ErrStalled] by errors.Is — the store failing, never a refusal —
// and given up at once by its KIND ([judge]): the bucket has held the request
// a whole bound without moving, which a minute of nothing but failures already
// meets, and asked again it would hold the person another minute.
type stalled struct {
	// took is how many of the body's bytes the transport took, and of the
	// body's length — -1 where the SDK did not say.
	took, of int64
	// answered is whether the bucket's answer had begun.
	answered bool
	// quiet is the bound the attempt went without moving.
	quiet time.Duration
}

func (e *stalled) Error() string {
	switch {
	case e.answered:
		return fmt.Sprintf("the bucket began its answer to this request and sent nothing more of it for %v", e.quiet)
	case e.of > 0 && e.took >= e.of:
		return fmt.Sprintf("the bucket took all %d bytes of this request's body and began no answer for %v",
			e.of, e.quiet)
	case e.took > 0 && e.of > 0:
		return fmt.Sprintf("the bucket took %d of this request's %d body bytes, then nothing more and no "+
			"answer, for %v", e.took, e.of, e.quiet)
	case e.took > 0:
		return fmt.Sprintf("the bucket took %d bytes of this request's body, then nothing more and no "+
			"answer, for %v", e.took, e.quiet)
	case e.of > 0:
		return fmt.Sprintf("the bucket took none of this request's %d body bytes and began no answer for %v",
			e.of, e.quiet)
	}
	return fmt.Sprintf("the bucket began no answer to this request for %v", e.quiet)
}

// Is reads a stall as [objstore.ErrStalled].
func (e *stalled) Is(target error) bool { return target == objstore.ErrStalled }

// isStalled reports whether err holds a [stalled], whatever wraps it.
func isStalled(err error) bool {
	_, ok := errors.AsType[*stalled](err)
	return ok
}

// watchedClient is the bucket's HTTP client as the SDK calls it — once an
// ATTEMPT, after its retry step, the [gate] and the signature — with every
// attempt WATCHED: one that has not MOVED for the stall bound is ended, its
// error a [stalled], whatever its deadline.
//
// MOVED IS COUNTED AT THE TRANSPORT, never estimated from a rate, so a write
// moving at any pace the node's socket can see is never ended, however slow,
// and one the bucket stopped taking is ended a bound after the last thing it
// took. Movement is any of: the transport taking bytes of the request's body
// (a Read of it returning — the time spent INSIDE that Read is the body's
// source's and is never counted against the bucket); the socket's count of
// bytes the far end has not acknowledged changing between two samples
// ([unacked], read at every connection the transport hands the attempt and
// then every [stallBound.sample]); a connection, a `100 Continue` and the
// answer's headers; and every byte of an answer the SDK reads itself — all
// but GetObject's, whose body is handed to the store and read at its reader's
// pace, which the store's own [objstore.Stall] watch bounds from the reader's
// side: a watch here would end a download a slow PERSON paces.
//
// BOTH THE BODY AND THE SOCKET, because the kernel hides a bucket's pace from
// the body's reader: its send buffer and the far end's receive buffer take
// megabytes the bucket's software has not read, so a write moving at the
// floor across a slow link went 84 s (an 8 MiB put) and 335.8 s (8 MiB parts)
// between two reads of its body while those buffers drained, and every
// body-only watch ended writes at the floor that would have landed. The
// socket alone misses an answer and a body still being copied into an empty
// send buffer. Together the longest a write at the floor went with nothing
// moving was 15.0 s — four parts sharing one link — and a bucket that stops
// changes nothing.
//
// THE ONE STOP IT CANNOT SEE is a bucket whose SOFTWARE reads a write slower
// than its kernel took it, or a proxy in front of it that buffers an upload
// before passing it on: what they took and have not read sits acknowledged,
// and nothing the node can see moves while it drains — so a write of which
// they hold more than a stall's reading is ended where it would have landed.
// Measured against the S3 fake: a 16 MiB upload whose bucket read the first
// 4 MiB of each 8 MiB part at once and the rest at the floor was ended at
// 80.3 s with each part at a floor of its own and 85.3 s with the two sharing
// one, where it would have landed at about two and four minutes; an 8 MiB
// put whose bucket read only 2 MiB at once kept less than a minute of it
// unread and landed, at 185.8 s. From the node's side it is a bucket that
// stopped; a slow NETWORK in front of a healthy bucket is never ended.
//
// A CONNECTION WHOSE COUNT CANNOT BE READ ([errUncountable]) is never ended
// here — its deadline still ends it — because a watch blind to the socket
// would end every write at the floor; on the two platforms the engine builds
// for, every connection to the bucket is a TCP socket it can read.
//
// ONE REQUEST TO A SOCKET: the bucket's transport speaks HTTP/1.1 alone
// ([bucketClient]), since over HTTP/2 a socket's acknowledgements would be
// any of the requests it carries, and a request whose stream the bucket
// stopped reading would never be ended while its siblings moved.
type watchedClient struct {
	next  s3.HTTPClient
	bound *stallBound
	// live is how many attempts' watches are running.
	live atomic.Int64
}

// watching is next with every attempt watched under bound.
func watching(next s3.HTTPClient, bound *stallBound) *watchedClient {
	return &watchedClient{next: next, bound: bound}
}

// Do implements [s3.HTTPClient]: one attempt, watched.
func (c *watchedClient) Do(req *http.Request) (*http.Response, error) {
	parent := req.Context()
	ctx, cancel := context.WithCancelCause(parent)
	w := &watch{
		client: c,
		parent: parent,
		cancel: cancel,
		stall:  c.bound.stall,
		sample: c.bound.sample,
		of:     req.ContentLength,
		last:   time.Now(),
	}
	w.record, _ = middleware.GetStackValue(parent, requestKey{}).(*request)
	if w.record != nil {
		w.record.attemptBegins()
	}
	switch {
	case req.Body == nil || req.Body == http.NoBody:
		w.of = 0
	case req.ContentLength == 0:
		// A BODY OF NO DECLARED LENGTH, as net/http reads a request's zero
		// beside a body.
		w.of = -1
	}
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn:        w.gotConn,
		Got100Continue: w.moved,
	})
	req = req.WithContext(ctx)
	if w.of != 0 {
		req.Body = &watchedBody{ReadCloser: req.Body, w: w}
		if get := req.GetBody; get != nil {
			// A BODY THE TRANSPORT SENDS AGAIN on a second connection is
			// the same attempt's, and watched as it is.
			req.GetBody = func() (io.ReadCloser, error) {
				body, err := get()
				if err != nil {
					return nil, err
				}
				return &watchedBody{ReadCloser: body, w: w}, nil
			}
		}
	}
	c.live.Add(1)
	w.mu.Lock()
	w.timer = time.AfterFunc(w.sample, w.tick)
	w.mu.Unlock()

	res, err := c.next.Do(req)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired != nil {
		// ENDED BY THE WATCH: whatever the transport answered once the
		// attempt's context was cancelled is that cancellation, and the
		// attempt is the stall. An answer that crossed the cancellation is
		// the stall too — its body would read nothing but the cancellation.
		if res != nil {
			_ = res.Body.Close()
		}
		return nil, w.fired
	}
	if err != nil {
		w.endLocked()
		cancel(nil)
		return nil, err
	}
	w.last, w.answered = time.Now(), true
	switch {
	case middleware.GetOperationName(parent) == "GetObject":
		// ITS BODY IS THE STORE'S TO WATCH, at its reader's pace: the
		// watch ends at the headers, the attempt's context at the body's
		// Close, since the body is read under it.
		w.endLocked()
		res.Body = &closingBody{ReadCloser: res.Body, cancel: cancel}
	case res.Body == nil || res.Body == http.NoBody:
		w.endLocked()
		cancel(nil)
	default:
		res.Body = &watchedAnswer{ReadCloser: res.Body, w: w}
	}
	return res, nil
}

// watch is one attempt's: its last movement, and the timer that samples its
// socket and ends it once it has gone the bound without moving.
//
// EVERY MOVEMENT IS RECORDED, AND THE STALL DECIDED AND THE CANCELLATION
// ISSUED, UNDER mu, the decision re-reading the last movement at that
// instant: the timer is a sample, never a deadline fixed when the attempt
// began, so a body byte taken or an answer arriving just before the decision
// is seen by it.
type watch struct {
	client *watchedClient
	// parent is the attempt's own context — the request's deadline and its
	// caller — and cancel ends the child the transport runs under.
	parent context.Context
	cancel context.CancelCauseFunc
	stall  time.Duration
	sample time.Duration
	// record is the request the attempt belongs to, where how long the
	// attempt had been quiet is left when its own context ends it in flight
	// ([request.quiet]); nil outside an operation.
	record *request

	mu sync.Mutex
	// last is the attempt's last movement.
	last time.Time
	// reading is how many reads of the request's body are in progress:
	// the body's source's time, never the bucket's.
	reading int
	// conn is the connection the transport handed the attempt, and count
	// its unacknowledged bytes at the last sample — counted is false until
	// a sample has read it; blind is a connection whose count cannot be
	// read at all.
	conn    net.Conn
	count   int
	counted bool
	blind   bool
	// took is how many bytes of the body the transport took, and of the
	// body's length (-1 unknown, 0 none).
	took, of int64
	// answered is whether the answer's headers arrived.
	answered bool
	timer    *time.Timer
	// ended is whether the watch has ended; fired the stall that ended it.
	ended bool
	fired *stalled
}

// gotConn takes the connection the transport handed the attempt — a second
// one where an idle connection it chose was closed under it — and its count's
// baseline, so one socket's count is never compared with another's.
func (w *watch) gotConn(info httptrace.GotConnInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = time.Now()
	w.conn, w.counted, w.blind = info.Conn, false, false
	switch n, err := unacked(info.Conn); {
	case errors.Is(err, errUncountable):
		w.blind = true
	case err == nil:
		w.count, w.counted = n, true
	}
}

// moved records a movement the transport saw.
func (w *watch) moved() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = time.Now()
}

// tick samples the attempt's socket and ends an attempt that has not moved for
// the bound — unless a read of its body is in progress, or its connection
// cannot be counted — and otherwise samples again a sample later.
func (w *watch) tick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return
	}
	if w.parent.Err() != nil {
		// ITS OWN DEADLINE OR ITS CALLER ended the attempt, which the
		// transport answers as that; a watch has nothing left to watch.
		w.endLocked()
		return
	}
	now := time.Now()
	if w.conn != nil && !w.blind {
		if n, err := unacked(w.conn); err == nil {
			if w.counted && n != w.count {
				w.last = now
			}
			w.count, w.counted = n, true
		}
	}
	if w.reading == 0 && !w.blind && now.Sub(w.last) >= w.stall {
		w.fired = &stalled{took: w.took, of: w.of, answered: w.answered, quiet: w.stall}
		w.endLocked()
		w.cancel(w.fired)
		return
	}
	w.timer.Reset(w.sample)
}

// endLocked ends the watch. Where the attempt's OWN CONTEXT ended it in flight
// — its deadline or its caller, before the watch had — how long it had then
// moved nothing is left in its request's record ([request.quiet]), measured
// at the deadline itself where it has one, since the transport's error or a
// sample may notice the deadline a little after it came. An attempt that
// ended any other way leaves nothing: one answered moved, and one the watch
// ended is the stall.
func (w *watch) endLocked() {
	if w.ended {
		return
	}
	w.ended = true
	if w.timer != nil {
		w.timer.Stop()
	}
	w.client.live.Add(-1)
	if w.record != nil && w.fired == nil && w.parent.Err() != nil {
		end := time.Now()
		if deadline, ok := w.parent.Deadline(); ok && deadline.Before(end) {
			end = deadline
		}
		w.record.endedInFlight(max(end.Sub(w.last), 0))
	}
}

// watchedBody is a request's body as the transport reads it: every read a
// movement, and the time inside one never the bucket's.
type watchedBody struct {
	io.ReadCloser
	w *watch
}

func (b *watchedBody) Read(p []byte) (int, error) {
	b.w.mu.Lock()
	b.w.reading++
	b.w.mu.Unlock()
	n, err := b.ReadCloser.Read(p)
	b.w.mu.Lock()
	b.w.reading--
	b.w.took += int64(n)
	b.w.last = time.Now()
	b.w.mu.Unlock()
	return n, err
}

// watchedAnswer is an answer the SDK reads itself: every byte of it a
// movement, and its watch ended at its end or its first error as well as at
// its Close — the SDK leaves a completion it failed to read unclosed (its
// 200-error check returns the read's error before the deserializer defers the
// close), and a watch ended only at Close would outlive its attempt.
type watchedAnswer struct {
	io.ReadCloser
	w *watch
}

func (a *watchedAnswer) Read(p []byte) (int, error) {
	n, err := a.ReadCloser.Read(p)
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	if a.w.fired != nil && err != nil && !errors.Is(err, io.EOF) {
		// THE WATCH ENDED IT: the read failed on the cancellation, which
		// is the stall.
		return n, a.w.fired
	}
	if n > 0 {
		a.w.last = time.Now()
	}
	if err != nil {
		a.w.endLocked()
		if a.w.fired == nil {
			a.w.cancel(nil)
		}
	}
	return n, err
}

func (a *watchedAnswer) Close() error {
	err := a.ReadCloser.Close()
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	a.w.endLocked()
	if a.w.fired == nil {
		a.w.cancel(nil)
	}
	return err
}

// closingBody is a GetObject's answer, its attempt's context ended at its
// Close.
type closingBody struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (b *closingBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel(nil)
	return err
}
