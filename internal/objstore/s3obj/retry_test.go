package s3obj_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
)

// soonAfter asks a bucket again a millisecond after it failed, where the
// production backoff waits up to seconds: the backoff decides only WHEN a
// request is asked again, never whether, so a case can afford to ask it again
// five times.
var soonAfter = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) {
	return time.Millisecond, nil
})

// openRetrying is the backend over srv as [s3obj.Open] builds it — the
// environment it reads included — at the parts every case here runs at, but
// asking again a millisecond after a failure, and its gate's time kept by c
// (the zero Clock is the wall clock's).
func openRetrying(t *testing.T, srv *s3fake.Server, prefix string, c s3obj.Clock) *s3obj.Backend {
	t.Helper()
	return openPaced(t, srv, prefix, soonAfter, c)
}

// openPaced is [openRetrying] with the client's attempts paced by backoff.
func openPaced(t *testing.T, srv *s3fake.Server, prefix string, backoff retry.BackoffDelayer,
	c s3obj.Clock) *s3obj.Backend {
	t.Helper()
	b, err := s3obj.OpenRetrying(t.Context(), fakeConfig(srv, prefix), backoff, c)
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	s3obj.CutParts(b, part)
	return b
}

// stepping is the production give-up rule driven deterministically: a backoff
// that waits a millisecond and moves its clock on by the backoff's twenty-second
// ceiling, so a request asked again at the ceiling is judged as the clock
// reads it — and that, where next is set, tells next which retry it is pacing
// (1 for the wait before the second attempt), so a case scripts the bucket's
// next answer from inside the same call.
type stepping struct {
	clock *stoppedClock
	next  func(retry int)
	calls atomic.Int64
}

func (s *stepping) BackoffDelay(int, error) (time.Duration, error) {
	n := s.calls.Add(1)
	s.clock.Advance(retry.DefaultMaxBackoff)
	if s.next != nil {
		s.next(int(n))
	}
	return time.Millisecond, nil
}

// openStepped is the backend over srv with its attempts paced by a [stepping]
// backoff on a clock of its own, and the production give-up rule.
func openStepped(t *testing.T, srv *s3fake.Server, next func(retry int)) (*s3obj.Backend, *stepping) {
	t.Helper()
	s := &stepping{clock: newStoppedClock(), next: next}
	return openPaced(t, srv, "", s, s.clock.Clock()), s
}

// stoppedClock is a gate's time a case moves by hand: its instant moves only
// when the case advances it, and a timer the gate sets is recorded rather
// than set, to be fired when the case chooses.
type stoppedClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []stoppedTimer
}

// stoppedTimer is one timer a gate set on a [stoppedClock]: how long it was set
// for, and what it runs when fired.
type stoppedTimer struct {
	d    time.Duration
	fire func()
}

func newStoppedClock() *stoppedClock {
	return &stoppedClock{now: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)}
}

// Clock is c as a gate keeps time.
func (c *stoppedClock) Clock() s3obj.Clock { return s3obj.Clock{Now: c.Now, After: c.After} }

func (c *stoppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stoppedClock) After(d time.Duration, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timers = append(c.timers, stoppedTimer{d: d, fire: f})
}

// Advance moves c's instant on by d, firing nothing.
func (c *stoppedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Timers is every timer set on c so far, in the order they were set.
func (c *stoppedClock) Timers() []stoppedTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.timers)
}

// answer is the error the client is handed for an attempt the bucket answered
// with status and code, as the SDK's own steps hand it on: the S3 error
// inside the answer's status.
func answer(status int, code string) error {
	return &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      &smithy.GenericAPIError{Code: code},
	}
}

// slowDown is S3's throttle as an attempt is answered with it.
var slowDown = answer(http.StatusServiceUnavailable, "SlowDown")

// A REQUEST THE BUCKET FAILS PAST THREE ATTEMPTS STILL SUCCEEDS once the bucket
// answers it: nothing counts a request's attempts — the SDK's own retryer gave
// up after the third — and each request's end is its deadline. A put, a head
// and a get each go through five server errors, a put's body sent whole again
// every time; a server error is no throttle, so nothing here waits for the
// client's gate.
func TestAFailedRequestPastThreeAttemptsStillSucceeds(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := openRetrying(t, srv, "", s3obj.Clock{})
	const refusals = 5
	data := []byte("stored on the sixth attempt")
	for _, step := range []struct {
		verb string
		do   func() error
	}{
		{"put", func() error { return b.Put(t.Context(), "k", bytes.NewReader(data), objstore.PutMeta{}) }},
		{"head", func() error {
			info, err := b.Stat(t.Context(), "k")
			if err == nil && info.Size != int64(len(data)) {
				err = fmt.Errorf("the object is %d bytes, want %d", info.Size, len(data))
			}
			return err
		}},
		{"get", func() error {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			r, err := b.Get(ctx, "k", 0, int64(len(data)))
			if err != nil {
				return err
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err == nil && !bytes.Equal(got, data) {
				err = fmt.Errorf("it read back %q, want %q", got, data)
			}
			return err
		}},
	} {
		before := srv.Requests()
		srv.FailNext(refusals, http.StatusInternalServerError, "InternalError")
		if err := step.do(); err != nil {
			t.Fatalf("a %s the bucket failed %d times = %v; want it answered on the next attempt",
				step.verb, refusals, err)
		}
		if got := srv.Requests() - before; got != refusals+1 {
			t.Fatalf("a %s the bucket failed %d times was sent %d times, want %d",
				step.verb, refusals, got, refusals+1)
		}
	}
}

// THE ENVIRONMENT CANNOT CAP THE BUCKET'S ATTEMPTS: the SDK reads
// AWS_MAX_ATTEMPTS — and a shared profile's max_attempts — into the client and
// wraps whatever retryer it was given in that count, so an operator's
// environment would silently put back the cap that failed a request a bucket
// would have answered. Under AWS_MAX_ATTEMPTS=3 a request still goes through
// five server errors, and the client's retryer still counts no attempts.
//
// Not parallel: it sets the process's environment.
func TestTheEnvironmentCannotCapTheBucketsAttempts(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "3")
	srv := s3fake.Start(t, "files")
	srv.Put("k", []byte("stored before the bucket failed"))
	b := openRetrying(t, srv, "", s3obj.Clock{})
	srv.FailNext(5, http.StatusInternalServerError, "InternalError")
	if _, err := b.Stat(t.Context(), "k"); err != nil {
		t.Fatalf("under AWS_MAX_ATTEMPTS=3 a head the bucket failed five times = %v; "+
			"want it answered on the sixth attempt", err)
	}
	if got := s3obj.ClientRetryer(b).MaxAttempts(); got != 0 {
		t.Fatalf("under AWS_MAX_ATTEMPTS=3 the client's retryer counts %d attempts, want none (0)", got)
	}
}

// NOR CAN IT CHANGE HOW THE GATE COUNTS: under AWS_NEW_RETRIES_2026 the SDK
// asks its retryer for an attempt token at every attempt and gives back only
// the first attempt's, so a gate kept by those tokens would lose a place for
// every retry the variable is set under — until it let nothing out — and
// hear no throttle answered to one. The gate is a step of its own round each
// attempt instead: a head the bucket throttles three times is sent four
// times, every place it took is given back, and the throttles it heard on
// its retries slowed the client.
//
// Not parallel: it sets the process's environment.
func TestTheEnvironmentCannotChangeHowTheGateCounts(t *testing.T) {
	t.Setenv("AWS_NEW_RETRIES_2026", "true")
	srv := s3fake.Start(t, "files")
	srv.Put("k", []byte("an object"))
	b := openRetrying(t, srv, "", s3obj.Clock{})
	srv.FailNext(3, http.StatusServiceUnavailable, "SlowDown")
	before := srv.Requests()
	if _, err := b.Stat(t.Context(), "k"); err != nil {
		t.Fatalf("under AWS_NEW_RETRIES_2026 a head the bucket throttled three times = %v", err)
	}
	if got := srv.Requests() - before; got != 4 {
		t.Fatalf("under AWS_NEW_RETRIES_2026 a head the bucket throttled three times was sent %d "+
			"times, want 4", got)
	}
	if out := s3obj.Out(b); out != 0 {
		t.Fatalf("under AWS_NEW_RETRIES_2026 the gate still counts %d attempts out once every "+
			"request has been answered", out)
	}
	if out := s3obj.LetOut(b, 10); out == 10 {
		t.Fatal("under AWS_NEW_RETRIES_2026 three throttles left the client letting out all ten " +
			"attempts asked for at once")
	}
}

// THE BUCKET CLIENT'S RETRIES NEVER RUN OUT. The SDK's standard retryer holds
// a quota of 500 tokens for a whole client and spends five on every retry, so
// under a sustained SlowDown its retries ran out after about a hundred and
// every request after that failed at once. What slows a throttled node is its
// gate, never a quota: ten thousand retries of a throttled request in a row
// are all granted.
func TestTheBucketClientsRetriesNeverRunOut(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	rt := s3obj.ClientRetryer(openRetrying(t, srv, "", s3obj.Clock{}))
	for i := range 10_000 {
		release, err := rt.GetRetryToken(t.Context(), slowDown)
		if err != nil {
			t.Fatalf("retry %d of a throttled request was refused: %v", i+1, err)
		}
		// THROTTLED AGAIN, as a retry under a sustained SlowDown is: a
		// retry that succeeded would hand its cost back, and a quota
		// would never be seen to run out.
		_ = release(slowDown)
	}
}

// A THROTTLE SLOWS THE WHOLE CLIENT. The moment one answer is a throttle, the
// client's gate holds back every attempt the node sends to the prefix, each
// waiting for a place under its own context: a request the bucket throttles
// once is asked again and answered, and afterwards, of ten attempts asked
// for at once, the client lets out only the few its gate has places for,
// where before the throttle it let out all ten.
//
// A throttle is S3's SlowDown and MinIO's SlowDownRead, a 429, and a 503 to a
// HEAD whatever the bucket meant by it, since a HEAD's answer carries no body
// to name a code in and reaches the client as `ServiceUnavailable`. A server
// error is not, nor is a 503 naming a gateway's own failure — MinIO's
// `XMinioReadQuorum`, one object it cannot rebuild — which slowed every
// request of the node at each attempt of the one that asked about it.
func TestAThrottleSlowsTheWholeClient(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		status   int
		code     string
		head     bool
		throttle bool
	}{
		{name: "SlowDown", status: http.StatusServiceUnavailable, code: "SlowDown", throttle: true},
		{name: "SlowDown to a head", status: http.StatusServiceUnavailable, code: "SlowDown", head: true, throttle: true},
		{name: "SlowDownRead", status: http.StatusServiceUnavailable, code: "SlowDownRead", throttle: true},
		{name: "429", status: http.StatusTooManyRequests, code: "TooManyRequests", throttle: true},
		{name: "a 503 to a head naming nothing", status: http.StatusServiceUnavailable,
			code: "XMinioReadQuorum", head: true, throttle: true},
		{name: "server error", status: http.StatusInternalServerError, code: "InternalError"},
		{name: "a 503 naming a gateway's own failure", status: http.StatusServiceUnavailable,
			code: "XMinioReadQuorum"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := s3fake.Start(t, "files")
			// PUT FROM OUTSIDE, so nothing but the check at open has
			// gone through the client before the throttle.
			srv.Put("k", []byte("an object"))
			b := openRetrying(t, srv, "", s3obj.Clock{})
			if out := s3obj.LetOut(b, 10); out != 10 {
				t.Fatalf("before any throttle the client let out %d of ten attempts at once", out)
			}

			srv.FailNext(1, c.status, c.code)
			before := srv.Requests()
			var err error
			if c.head {
				_, err = b.Stat(t.Context(), "k")
			} else {
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				var r io.ReadCloser
				if r, err = b.Get(ctx, "k", 0, 1); err == nil {
					_, err = io.ReadAll(r)
					_ = r.Close()
				}
			}
			if err != nil {
				t.Fatalf("a request the bucket answered once with %d %s = %v; want it asked "+
					"again and answered", c.status, c.code, err)
			}
			if got := srv.Requests() - before; got != 2 {
				t.Fatalf("a request the bucket answered once with %d %s was sent %d times, want 2",
					c.status, c.code, got)
			}
			out := s3obj.LetOut(b, 10)
			switch {
			case c.throttle && out == 10:
				t.Fatalf("after a throttle with %d %s the client let out all ten attempts asked "+
					"for at once; want its gate holding them back", c.status, c.code)
			case !c.throttle && out != 10:
				t.Fatalf("after %d %s the client let out %d of ten attempts at once; it is no "+
					"throttle, and slows nothing", c.status, c.code, out)
			}
		})
	}
}

// A THROTTLE KEEPS SEVEN TENTHS OF THE NODE'S WIDTH, wherever in the node's
// work it lands. The cut is a share of the most attempts the node has had out
// at once — the width the bucket has seen it run at since its last cut — and
// never of a rate it had to measure, nor of the instant the throttle came
// back. The SDK's adaptive mode had measured no rate on a fresh client, so a
// throttle answered to the first attempt of a burst of thirty-two dropped the
// whole client to half a request a second; and a cut of what was out at that
// instant dropped a node between two bursts — a throttle answered to its one
// HEAD while a fleet sat at the prefix's rate — to ONE place, so its next
// burst of thirty-two climbed back a place a round. Here a throttle at a
// burst's start and one answered to a lone attempt after the node ran a burst
// each leave the gate letting out ⌊0.7 × the width⌋ at once — the node's OWN
// width, so one that never ran wider than four is cut to two rather than to a
// share of a width it never had — and every lone throttle after it takes
// seven tenths again.
func TestAThrottleKeepsSevenTenthsOfTheNodesWidth(t *testing.T) {
	t.Parallel()
	enter := func(t *testing.T, b *s3obj.Backend, n int) []s3obj.Attempt {
		t.Helper()
		held := make([]s3obj.Attempt, n)
		for i := range held {
			a, err := s3obj.Enter(t.Context(), b)
			if err != nil {
				t.Fatal(err)
			}
			held[i] = a
		}
		return held
	}
	// ran is a node that has run a burst of n, every attempt of it
	// answered, and has nothing out now.
	ran := func(t *testing.T, b *s3obj.Backend, n int) {
		t.Helper()
		for _, a := range enter(t, b, n) {
			a.Leave(nil)
		}
	}
	// lone is a throttle answered to one attempt with nothing else out.
	lone := func(t *testing.T, b *s3obj.Backend) {
		t.Helper()
		enter(t, b, 1)[0].Leave(slowDown)
	}
	// burstThrottledFirst is a burst of n whose first answer is a throttle
	// and every other one an answer.
	burstThrottledFirst := func(t *testing.T, b *s3obj.Backend, n int) {
		t.Helper()
		burst := enter(t, b, n)
		burst[0].Leave(slowDown)
		for _, a := range burst[1:] {
			a.Leave(nil)
		}
	}
	share := func(width float64, cuts int) int {
		return int(math.Floor(width * math.Pow(s3obj.ThrottleShare, float64(cuts))))
	}
	for _, c := range []struct {
		name string
		do   func(t *testing.T, b *s3obj.Backend)
		// least and most bound how many the gate then lets out at once.
		least, most int
	}{
		// The thirty-one answers after the throttle grow the gate — still
		// short of the thirty-two the bucket throttled.
		{name: "at a burst's start", do: func(t *testing.T, b *s3obj.Backend) {
			burstThrottledFirst(t, b, 32)
		}, least: share(32, 1), most: 31},
		{name: "between bursts", do: func(t *testing.T, b *s3obj.Backend) {
			ran(t, b, 32)
			lone(t, b)
		}, least: share(32, 1), most: share(32, 1)},
		{name: "between bursts wider than the transport keeps warm", do: func(t *testing.T, b *s3obj.Backend) {
			ran(t, b, 48)
			lone(t, b)
		}, least: share(48, 1), most: share(48, 1)},
		{name: "between bursts of a narrow node", do: func(t *testing.T, b *s3obj.Backend) {
			ran(t, b, 4)
			lone(t, b)
		}, least: share(4, 1), most: share(4, 1)},
		{name: "twice between bursts", do: func(t *testing.T, b *s3obj.Backend) {
			ran(t, b, 32)
			lone(t, b)
			lone(t, b)
		}, least: share(32, 2), most: share(32, 2)},
		// QUIET AFTER A CUT: the width the bucket last saw is the one the
		// cut left, and a lone throttle takes seven tenths of that.
		{name: "between bursts after a cut", do: func(t *testing.T, b *s3obj.Backend) {
			burstThrottledFirst(t, b, 32)
			lone(t, b)
		}, least: share(32, 2), most: share(32, 2)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			b := openRetrying(t, s3fake.Start(t, "files"), "", s3obj.Clock{})
			c.do(t, b)
			// TWICE THE WIDEST BURST ASKED FOR, so a gate the throttle
			// left wider than the node ran cannot pass.
			if out := s3obj.LetOut(b, 96); out < c.least || out > c.most {
				t.Fatalf("the gate lets out %d at once; want %d to %d (%v of the node's width a cut)",
					out, c.least, c.most, s3obj.ThrottleShare)
			}
		})
	}
}

// ONE ROUND OF THROTTLES IS ONE CUT: a throttle answered to an attempt let out
// before the last cut is the bucket's reply to the width that cut already
// answered. Thirty-two attempts throttled together cut the gate once, to
// seven tenths of them — thirty-two cuts would leave it one place — while a
// throttle answered to an attempt let out after the cut is news, and cuts it
// again.
func TestOneRoundOfThrottlesIsOneCut(t *testing.T) {
	t.Parallel()
	b := openRetrying(t, s3fake.Start(t, "files"), "", s3obj.Clock{})
	enter := func(n int) []s3obj.Attempt {
		t.Helper()
		held := make([]s3obj.Attempt, n)
		for i := range held {
			a, err := s3obj.Enter(t.Context(), b)
			if err != nil {
				t.Fatal(err)
			}
			held[i] = a
		}
		return held
	}
	const width = 32
	for _, a := range enter(width) {
		a.Leave(slowDown)
	}
	once := int(math.Floor(s3obj.ThrottleShare * width))
	if out := s3obj.LetOut(b, 2*width); out != once {
		t.Fatalf("%d attempts throttled together left the gate letting out %d at once, want %d: "+
			"one cut", width, out, once)
	}

	second := enter(once)
	second[0].Leave(slowDown)
	for _, a := range second[1:] {
		a.Leave(nil)
	}
	if out := s3obj.LetOut(b, 2*width); out >= once {
		t.Fatalf("a throttle answered to an attempt let out after the cut left the gate letting "+
			"out %d at once, as many as the cut did; want it cut again", out)
	}
}

// THE GATE LETS ITS WAITERS OUT FIRST COME, FIRST SERVED: an attempt that has
// to wait is never overtaken by one that arrived after it, so no request
// starves behind a stream of newer ones until its deadline.
func TestTheGateLetsItsWaitersOutFirstComeFirstServed(t *testing.T) {
	t.Parallel()
	b := openRetrying(t, s3fake.Start(t, "files"), "", s3obj.Clock{})
	held, err := s3obj.Enter(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	// A NODE THAT HAS NEVER HAD MORE THAN ONE OUT — the check at open, then
	// this — is one wide, so the throttle leaves it one place.
	held.Leave(slowDown)
	place, err := s3obj.Enter(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	const waiters = 5
	order := make(chan int, waiters)
	var mu sync.Mutex
	let := make([]s3obj.Attempt, waiters)
	for i := range waiters {
		go func() {
			a, err := s3obj.Enter(t.Context(), b)
			if err != nil {
				return
			}
			mu.Lock()
			let[i] = a
			mu.Unlock()
			order <- i
		}()
		// IN LINE BEFORE THE NEXT ONE COMES: the order they came in is
		// the order they must be let out in.
		for s3obj.Waiting(b) != i+1 {
			time.Sleep(time.Millisecond)
		}
	}
	place.Leave(context.Canceled)
	for want := range waiters {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("waiter %d was let out where waiter %d came first", got, want)
			}
			mu.Lock()
			a := let[got]
			mu.Unlock()
			a.Leave(context.Canceled)
		case <-time.After(10 * time.Second):
			t.Fatalf("waiter %d was never let out", want)
		}
	}
}

// A THROTTLED CLIENT IS LET GO ONCE THE BUCKET IS QUIET. A limit is the answer
// to a throttle, and climbs back only while answers come; a node gone quiet
// after one gets too few to climb, so its next burst would be held to the
// limit the last throttle left. So once the bucket has gone
// [s3obj.ThrottleQuiet] without a throttle the gate is lifted — and not a
// moment before — a request sent then is sent freely, and a throttle heard
// after that engages the gate again at once.
//
// THE LAZY LIFT, which every attempt entering or leaving asks: the clock's
// lift timer is never fired here, so the gate is lifted by an attempt alone.
func TestAThrottledClientIsLetGoOnceTheBucketIsQuiet(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.Put("k", []byte("an object"))
	clock := newStoppedClock()
	advance := clock.Advance
	b := openRetrying(t, srv, "", clock.Clock())
	head := func(what string, sent int64) {
		t.Helper()
		before := srv.Requests()
		if _, err := b.Stat(t.Context(), "k"); err != nil {
			t.Fatalf("%s = %v", what, err)
		}
		if got := srv.Requests() - before; got != sent {
			t.Fatalf("%s was sent %d times, want %d", what, got, sent)
		}
	}
	throttle := func() {
		t.Helper()
		srv.FailNext(1, http.StatusServiceUnavailable, "SlowDown")
		head("a head the bucket throttled once", 2)
	}

	throttle()
	if out := s3obj.LetOut(b, 10); out == 10 {
		t.Fatal("after a throttle the client let out all ten attempts asked for at once")
	}
	advance(s3obj.ThrottleQuiet - time.Second)
	if out := s3obj.LetOut(b, 10); out == 10 {
		t.Fatalf("a second short of %v without a throttle the client was let go", s3obj.ThrottleQuiet)
	}
	advance(time.Second)
	if out := s3obj.LetOut(b, 10); out != 10 {
		t.Fatalf("%v without a throttle the client let out %d of ten attempts at once; "+
			"want its gate lifted", s3obj.ThrottleQuiet, out)
	}
	head("a head once the client was let go", 1)

	throttle()
	if out := s3obj.LetOut(b, 10); out == 10 {
		t.Fatal("a throttle heard after the gate was lifted did not slow the client again")
	}
}

// A THROTTLED GATE LIFTS WHEN ITS MINUTE ENDS, WITH NOTHING SENT. A gate lifted
// only as an attempt entered or left it stayed engaged past
// [s3obj.ThrottleQuiet] whenever every place it had was held by attempts that
// had stopped answering — a part a bucket took the connection of and stopped
// reading, a read whose headers never came — and its waiters, the node's
// closed loops, waited until their own deadlines for a lift nothing triggered.
// The first throttle sets ONE timer for the minute, which lifts the gate with
// its one place still held and lets its waiter out; and a throttle heard while
// that timer runs moves the minute on, so the timer, firing, finds the bucket
// not yet quiet, sets itself again for what is left, and only then lifts.
func TestAThrottledGateLiftsWhenItsMinuteEndsWithNothingSent(t *testing.T) {
	t.Parallel()
	// cut is a gate of one place, cut by one throttle: the node has had only
	// its check at open out.
	cut := func(t *testing.T) (*s3obj.Backend, *stoppedClock) {
		t.Helper()
		clock := newStoppedClock()
		b := openRetrying(t, s3fake.Start(t, "files"), "", clock.Clock())
		a, err := s3obj.Enter(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		a.Leave(slowDown)
		if out := s3obj.LetOut(b, 10); out != 1 {
			t.Fatalf("one throttle left the gate letting out %d at once, want 1", out)
		}
		return b, clock
	}

	t.Run("its places held", func(t *testing.T) {
		t.Parallel()
		b, clock := cut(t)
		timers := clock.Timers()
		if len(timers) != 1 || timers[0].d != s3obj.ThrottleQuiet {
			t.Fatalf("a throttle set timers %v; want one, for %v", timers, s3obj.ThrottleQuiet)
		}
		held, err := s3obj.Enter(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Leave(nil)
		let := make(chan s3obj.Attempt, 1)
		go func() {
			a, err := s3obj.Enter(t.Context(), b)
			if err == nil {
				let <- a
			}
		}()
		for deadline := time.Now().Add(10 * time.Second); s3obj.Waiting(b) != 1; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("an attempt at a gate whose one place is held never waited in line")
			}
		}
		clock.Advance(s3obj.ThrottleQuiet)
		timers[0].fire()
		select {
		case a := <-let:
			defer a.Leave(nil)
		case <-time.After(10 * time.Second):
			t.Fatalf("the minute after the throttle ended and its timer fired, with the gate's one place "+
				"held and nothing entering or leaving, and the waiter is still waiting (%d in line)",
				s3obj.Waiting(b))
		}
		if n := s3obj.Waiting(b); n != 0 {
			t.Fatalf("%d attempts still wait at a lifted gate", n)
		}
		if n := len(clock.Timers()); n != 1 {
			t.Fatalf("a lifted gate set %d timers, want the one", n)
		}
	})

	t.Run("a later throttle moves the minute on", func(t *testing.T) {
		t.Parallel()
		b, clock := cut(t)
		clock.Advance(30 * time.Second)
		a, err := s3obj.Enter(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		a.Leave(slowDown)
		timers := clock.Timers()
		if len(timers) != 1 {
			t.Fatalf("a second throttle while the first one's timer runs left %d timers set, want "+
				"the one: a storm of throttles sets nothing new", len(timers))
		}
		clock.Advance(30 * time.Second)
		timers[0].fire()
		if out := s3obj.LetOut(b, 10); out == 10 {
			t.Fatal("the timer lifted the gate a minute after the first throttle, thirty seconds " +
				"after the second")
		}
		timers = clock.Timers()
		if len(timers) != 2 || timers[1].d != 30*time.Second {
			t.Fatalf("the timer, firing before the bucket went a minute quiet, set timers %v; want a "+
				"second, for the thirty seconds left", timers)
		}
		clock.Advance(30 * time.Second)
		timers[1].fire()
		if out := s3obj.LetOut(b, 10); out != 10 {
			t.Fatalf("a minute after the last throttle, its timer fired, the gate lets out %d of ten "+
				"at once; want it lifted", out)
		}
	})
}

// THE BACKOFF IS JITTERED AT ITS CEILING, as below it: a retry waits a random
// share of 2^attempt seconds, never twenty seconds exactly however many
// attempts a request has failed. The SDK's own backoff waited exactly its
// ceiling from the fifth retry on, so every request a long deadline let go on
// failing asked again in lockstep with the others that failed when it did —
// and none in the twenty seconds before.
func TestTheBackoffIsJitteredAtItsCeiling(t *testing.T) {
	t.Parallel()
	for _, attempt := range []int{0, 1, 2, 3, 4, 5, 10, 50} {
		ceiling := retry.DefaultMaxBackoff
		if attempt < 5 {
			ceiling = min(ceiling, time.Second<<attempt)
		}
		least, most := ceiling, time.Duration(0)
		for range 1000 {
			d, err := s3obj.FullJitter.BackoffDelay(attempt, slowDown)
			if err != nil {
				t.Fatal(err)
			}
			if d < 0 || d >= ceiling {
				t.Fatalf("retry %d waits %v, outside [0, %v)", attempt, d, ceiling)
			}
			least, most = min(least, d), max(most, d)
		}
		if least > ceiling/10 || most < ceiling*9/10 {
			t.Fatalf("a thousand waits of retry %d ran from %v to %v; want them spread over [0, %v)",
				attempt, least, most, ceiling)
		}
	}
}

// A REQUEST ITS DEADLINE ENDS NAMES THE BUCKET'S LAST ANSWER. With no count of
// attempts, a request the bucket keeps failing — or one that cannot reach the
// bucket at all — ends only at its deadline, which the SDK reports as the
// request cancelled; what kept it waiting is the one thing an operator reading
// the error needs. So the last answer is named beside the deadline, as text:
// the error is still the deadline, and nothing reads a server error out of it.
func TestARequestItsDeadlineEndsNamesTheBucketsLastAnswer(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.Put("k", []byte("an object"))
	b := openRetrying(t, srv, "", s3obj.Clock{})
	srv.FailNext(1_000_000, http.StatusInternalServerError, "InternalError")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := b.Stat(ctx, "k")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a head the bucket kept failing = %v; want it ended by its deadline", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("a head the bucket kept failing with a 500 ended %q; want the 500 named", err)
	}
	if _, isAPI := errors.AsType[smithy.APIError](err); isAPI {
		t.Fatalf("a head its deadline ended = %v, which reads as the bucket's own answer; "+
			"want the deadline, the answer only named", err)
	}
}

// THE SESSION REQUEST IS NEVER HELD BY THE GATE. A directory bucket's every
// request is signed with an S3 Express session, which the SDK asks for —
// CreateSession, through the same client — from inside a request that has
// already taken its place at the gate. Held there, the session request waited
// behind its own waiter: at one place, every request needing a new session
// ended at its deadline with no session asked for. Here a request whose
// session has expired, at a gate cut to one place, is answered at once with
// one session asked for — and is itself still gated: with the one place held,
// it waits its turn.
func TestTheSessionRequestIsNeverHeldByTheGate(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files--use1-az4--x-s3")
	// EVERY SESSION ALREADY EXPIRED, so every request needs a new one.
	srv.SessionLife = -time.Minute
	srv.Put("k", []byte("an object"))
	b := openRetrying(t, srv, "", s3obj.Clock{})
	if n := srv.RequestsOf("CreateSession"); n < 1 {
		t.Fatalf("opening the bucket asked for %d sessions; the SDK does not take it for a directory bucket", n)
	}
	// ONE PLACE: the node has had one attempt out (its check at open) — two,
	// were its session request held at the gate beside it, which a throttle
	// cuts to 1.4 — so the throttle leaves one either way.
	cut, err := s3obj.Enter(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	cut.Leave(slowDown)
	if out := s3obj.LetOut(b, 10); out != 1 {
		t.Fatalf("the gate lets out %d at once after the throttle, want 1", out)
	}

	sessions := srv.RequestsOf("CreateSession")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	began := time.Now()
	if _, err := b.Stat(ctx, "k"); err != nil {
		t.Fatalf("a head needing a new session at a gate of one place = %v after %v, %d sessions asked "+
			"for; want it answered — its session request held behind the place it holds itself",
			err, time.Since(began), srv.RequestsOf("CreateSession")-sessions)
	}
	took := time.Since(began)
	if took > time.Second {
		t.Fatalf("a head needing a new session at a gate of one place took %v", took)
	}
	t.Logf("a head needing a new session at a gate of one place was answered in %v", took)
	if n := srv.RequestsOf("CreateSession") - sessions; n != 1 {
		t.Fatalf("a head needing a new session asked for %d sessions, want 1", n)
	}

	// THE REQUEST ITSELF IS STILL GATED: with every place of the gate held —
	// two by now, the head's answer having grown it by one — a head waits in
	// line, and is answered once a place is given back.
	places := s3obj.LetOut(b, 10)
	if places == 10 {
		t.Fatal("the gate was lifted before the control could hold its places")
	}
	var held []s3obj.Attempt
	for range places {
		a, err := s3obj.Enter(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, a)
	}
	answered := make(chan error, 1)
	go func() {
		_, err := b.Stat(t.Context(), "k")
		answered <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); s3obj.Waiting(b) != 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a head at a gate whose every place is held never waited in line")
		}
	}
	select {
	case err := <-answered:
		t.Fatalf("a head at a gate whose every place is held was answered (%v) without waiting", err)
	case <-time.After(50 * time.Millisecond):
	}
	for _, a := range held {
		a.Leave(nil)
	}
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the head let out once the places were given back = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the head was never let out once the places were given back")
	}
}

// ONLY THE SESSION REQUEST PASSES THE GATE: of every operation the backend
// sends ([s3obj.Operations], which TestEveryOperationTheBackendSendsIsALabel
// holds to what the verbs send), and of names it does not, an attempt is let
// past a gate whose one place is held only for CreateSession — known by the
// operation the SDK names in its context, so no other request a directory
// bucket's URL shares slips through beside it.
func TestOnlyTheSessionRequestPassesTheGate(t *testing.T) {
	t.Parallel()
	b := openRetrying(t, s3fake.Start(t, "files"), "", s3obj.Clock{})
	cut, err := s3obj.Enter(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	cut.Leave(slowDown)
	held, err := s3obj.Enter(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	// AFTER EVERY SUBTEST, which run once this function has returned.
	t.Cleanup(func() { held.Leave(nil) })
	for _, op := range append(slices.Clone(s3obj.Operations), "other", "") {
		t.Run("operation="+op, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			if ran, want := s3obj.GateLetsOut(ctx, b, op), op == "CreateSession"; ran != want {
				t.Fatalf("an attempt of %q at a gate whose one place is held: let out = %v, want %v",
					op, ran, want)
			}
		})
	}
}

// statusOf is the HTTP status inside err, 0 for none.
func statusOf(err error) int {
	var resp interface{ HTTPStatusCode() int }
	if errors.As(err, &resp) {
		return resp.HTTPStatusCode()
	}
	return 0
}

// A REQUEST FAILED FOR A MINUTE IS GIVEN UP. With nothing counting attempts, a
// request the bucket answered with nothing but failures was asked again until
// its deadline — sixteen and a half minutes for a full part. Now one whose
// failures span its own bound inside [s3obj.GiveUpAfterBound] is not asked
// again: on a clock the backoff moves twenty seconds an attempt, failures at
// 0, 20, 40 and 60 seconds, given up at the fourth whatever bound its span
// drew, since every bound lies past forty seconds and by the minute — its
// error the give-up, naming the minute, with the bucket's last answer inside
// it, never its deadline's. A server error, a gateway's own 503 (asked through
// a GET: a HEAD's 503 carries no code and is a throttle) and a bucket that
// refuses the connection alike.
func TestARequestFailedForAMinuteIsGivenUp(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// fail makes the bucket fail every request from here on.
		fail func(srv *s3fake.Server)
		do   func(ctx context.Context, b *s3obj.Backend) error
		// op is the operation the bucket counts the request's attempts
		// under: empty for one it was sent none of it could count.
		op string
		// last holds the bucket's last answer inside the give-up.
		last func(t *testing.T, err error)
	}{
		{name: "a server error", op: "HeadObject",
			fail: func(srv *s3fake.Server) { srv.FailNext(1_000_000, http.StatusInternalServerError, "InternalError") },
			do:   func(ctx context.Context, b *s3obj.Backend) error { _, err := b.Stat(ctx, "k"); return err },
			last: func(t *testing.T, err error) {
				if _, ok := errors.AsType[smithy.APIError](err); !ok || statusOf(err) != http.StatusInternalServerError {
					t.Fatalf("the give-up %v holds no API error answered 500", err)
				}
			}},
		{name: "a gateway's own 503", op: "GetObject",
			fail: func(srv *s3fake.Server) {
				srv.FailNext(1_000_000, http.StatusServiceUnavailable, "XMinioReadQuorum")
			},
			do: func(ctx context.Context, b *s3obj.Backend) error {
				r, err := b.Get(ctx, "k", 0, 1)
				if err == nil {
					_ = r.Close()
				}
				return err
			},
			last: func(t *testing.T, err error) {
				if api, ok := errors.AsType[smithy.APIError](err); !ok || api.ErrorCode() != "XMinioReadQuorum" {
					t.Fatalf("the give-up %v holds no API error naming XMinioReadQuorum", err)
				}
			}},
		{name: "a connection refused", op: "",
			fail: func(srv *s3fake.Server) { srv.Close() },
			do:   func(ctx context.Context, b *s3obj.Backend) error { _, err := b.Stat(ctx, "k"); return err },
			last: func(t *testing.T, err error) {
				if _, ok := errors.AsType[*net.OpError](err); !ok {
					t.Fatalf("the give-up %v holds no network error", err)
				}
				if _, isAPI := errors.AsType[smithy.APIError](err); isAPI {
					t.Fatalf("the give-up %v holds an API error from a bucket that never answered", err)
				}
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := s3fake.Start(t, "files")
			srv.Put("k", []byte("an object"))
			b, paced := openStepped(t, srv, nil)
			before := srv.RequestsOf(c.op)
			c.fail(srv)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := c.do(ctx, b)
			if err == nil || ctx.Err() != nil {
				t.Fatalf("a request the bucket kept failing = %v (its guard: %v); want it given up "+
					"before its guard", err, ctx.Err())
			}
			given, ok := errors.AsType[*s3obj.GivenUp](err)
			if !ok {
				t.Fatalf("a request the bucket kept failing = %v; want it given up", err)
			}
			for _, want := range []string{"failed every attempt", s3obj.GiveUpAfterBound.String()} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the give-up %q does not name %q", err, want)
				}
			}
			c.last(t, err)
			// FAILURES AT 0, 20, 40 AND 60 SECONDS: the fourth reaches the
			// minute.
			const attempts = 4
			if n := given.Attempts(); n != attempts {
				t.Fatalf("the request was given up after %d attempts, want %d", n, attempts)
			}
			if n := paced.calls.Load(); n != attempts-1 {
				t.Fatalf("the request waited %d times between attempts, want %d", n, attempts-1)
			}
			if c.op != "" {
				if n := srv.RequestsOf(c.op) - before; n != attempts {
					t.Fatalf("the bucket was sent %d attempts, want %d", n, attempts)
				}
			}
		})
	}
}

// A THROTTLE BETWEEN FAILURES STARTS THE MINUTE AGAIN: a bucket that asks the
// node to slow down is up, and a request it throttles is the gate's and its
// deadline's to govern. Answered 500, 500, SlowDown and then 500s on a clock
// the backoff moves twenty seconds an attempt — failures at 0 and 20 seconds,
// the throttle at 40, failures from 60 — the request is given up at its
// seventh attempt, a minute after the failure at 60 that began a span of its
// own (past any bound that span could draw, and short of none before it),
// never at its fourth.
func TestAThrottleBetweenFailuresStartsTheMinuteAgain(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.Put("k", []byte("an object"))
	script := []struct {
		status int
		code   string
	}{
		{http.StatusInternalServerError, "InternalError"},
		{http.StatusInternalServerError, "InternalError"},
		{http.StatusServiceUnavailable, "SlowDown"},
	}
	for range 5 {
		script = append(script, script[0])
	}
	b, _ := openStepped(t, srv, func(retry int) {
		if retry < len(script) {
			srv.FailNext(1, script[retry].status, script[retry].code)
		}
	})
	before := srv.RequestsOf("HeadObject")
	srv.FailNext(1, script[0].status, script[0].code)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := b.Stat(ctx, "k")
	given, ok := errors.AsType[*s3obj.GivenUp](err)
	if !ok {
		t.Fatalf("a head failed, throttled and failed again = %v; want it given up", err)
	}
	if !strings.Contains(err.Error(), s3obj.GiveUpAfterBound.String()) {
		t.Fatalf("the give-up %q does not name the minute", err)
	}
	if n := given.Attempts(); n != 7 {
		t.Fatalf("a head failed, throttled and failed again was given up after %d attempts, want 7", n)
	}
	if n := srv.RequestsOf("HeadObject") - before; n != 7 {
		t.Fatalf("the bucket was sent %d attempts, want 7", n)
	}
}

// A BRIEF FAILURE IS RIDDEN THROUGH, and a long one ends a put — single or in
// parts — at the bound rather than at its deadline. In real time, at a bound
// of 300 ms: a bucket failing every request for 100 ms still stores a put and
// an upload in parts; one that refuses part 2 of an upload and then everything
// after it, the abandonment included, ends the upload naming part 2 and the
// abandonment's own failure within a couple of seconds, and then a single put
// likewise — where their deadlines are minutes.
func TestABriefFailureIsRiddenThrough(t *testing.T) {
	t.Parallel()
	const bound = 300 * time.Millisecond
	single := bytes.Repeat([]byte("s"), part-1)
	parts := bytes.Repeat([]byte("p"), 2*part+part/2)
	put := func(t *testing.T, b *s3obj.Backend, name string, data []byte) (time.Duration, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		began := time.Now()
		err := b.Put(ctx, name, bytes.NewReader(data), objstore.PutMeta{})
		if ctx.Err() != nil {
			t.Fatalf("a put of %s ended at its guard: %v", name, err)
		}
		return time.Since(began), err
	}

	t.Run("ridden through", func(t *testing.T) {
		t.Parallel()
		srv := s3fake.Start(t, "files")
		b := openRetrying(t, srv, "", s3obj.Clock{})
		s3obj.GiveUpAfter(b, bound)
		for _, c := range []struct {
			name string
			data []byte
		}{{"one-part", single}, {"in-parts", parts}} {
			srv.FailFor(100*time.Millisecond, http.StatusInternalServerError, "InternalError")
			if _, err := put(t, b, c.name, c.data); err != nil {
				t.Fatalf("a put of %d bytes against a bucket failing for 100ms = %v; want it stored",
					len(c.data), err)
			}
		}
	})

	t.Run("given up", func(t *testing.T) {
		t.Parallel()
		srv := s3fake.Start(t, "files")
		srv.FailPart(2, http.StatusInternalServerError, "InternalError")
		// PART 2 FAILS FIRST, AND EVERYTHING AFTER IT: its first arrival
		// waits for parts 1 and 3 to have arrived — past the refusal
		// they would otherwise meet — and then fails the bucket for good,
		// so part 2 is the request given up and the abandonment fails too.
		var (
			first, third = make(chan struct{}), make(chan struct{})
			once         = map[int32]*sync.Once{1: {}, 2: {}, 3: {}}
		)
		srv.PartHook = func(n int32) {
			switch n {
			case 1:
				once[1].Do(func() { close(first) })
			case 3:
				once[3].Do(func() { close(third) })
			case 2:
				once[2].Do(func() {
					for _, arrived := range []chan struct{}{first, third} {
						select {
						case <-arrived:
						case <-time.After(10 * time.Second):
						}
					}
					srv.FailFor(time.Hour, http.StatusInternalServerError, "InternalError")
				})
			}
		}
		b := openRetrying(t, srv, "", s3obj.Clock{})
		s3obj.GiveUpAfter(b, bound)
		for _, c := range []struct {
			name string
			data []byte
			// names is what the error must name.
			names []string
		}{
			{"in-parts", parts, []string{"part 2", "could not be abandoned"}},
			{"one-part", single, []string{"put one-part"}},
		} {
			took, err := put(t, b, c.name, c.data)
			if _, ok := errors.AsType[*s3obj.GivenUp](err); !ok {
				t.Fatalf("a put of %d bytes against a failing bucket = %v; want it given up", len(c.data), err)
			}
			if took > bound+2*time.Second {
				t.Fatalf("a put of %d bytes against a failing bucket was given up after %v, want at most %v",
					len(c.data), took, bound+2*time.Second)
			}
			for _, want := range c.names {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("a put of %d bytes given up = %q; want it naming %q", len(c.data), err, want)
				}
			}
		}
	})
}

// THE LAST ATTEMPT IS SENT AT THE BOUND: a request inside its span of failures
// waits no longer than what is left of its span's bound before it is asked
// again, so it is asked once more AT the bound rather than up to twenty
// seconds past it — where a wait for any other answer is drawn from the whole
// ceiling.
func TestTheLastAttemptIsSentAtTheBound(t *testing.T) {
	t.Parallel()
	const left = 3 * time.Second
	failure := answer(http.StatusInternalServerError, "InternalError")
	most, atLeft := time.Duration(0), false
	for range 1000 {
		d, err := s3obj.FullJitter.BackoffDelay(10, s3obj.Failing(failure, left))
		if err != nil {
			t.Fatal(err)
		}
		most, atLeft = max(most, d), atLeft || d == left
	}
	if most > left || !atLeft {
		t.Fatalf("a thousand waits with %v of the bound left ran up to %v (one at it: %v); want none "+
			"past it, and some at it", left, most, atLeft)
	}
	most = 0
	for range 1000 {
		d, err := s3obj.FullJitter.BackoffDelay(10, failure)
		if err != nil {
			t.Fatal(err)
		}
		most = max(most, d)
	}
	if most <= left {
		t.Fatalf("a thousand waits for a failure outside any span ran up to %v; want the ceiling's spread", most)
	}
}

// THE LAST ATTEMPTS OF REQUESTS THAT BEGAN FAILING TOGETHER ARE SPREAD over the
// minute's last third, each at its own span's bound — where one bound of a
// minute for every request sent every last attempt in one instant, on the
// minute: every write open on every node when the bucket fell over, onto a
// bucket that may just be recovering. And what a failing request sends before
// it is given up is a distribution the backoff decides, derived here once for
// the docs to quote.
//
// Two thousand heads, one at a time, against a bucket failing every request,
// through the production backoff and bounds — each seeded, so every run draws
// alike — on a clock of the client's own that the backoff moves by each wait
// and by a modelled first byte of 100 ms (S3's, at its lower end): every head
// is given up forty seconds to a minute after its first failure (and its last
// attempt's first byte), no tenth of a second holding more than one in fifty
// of their last attempts; and their attempts run to a median of nine, twelve
// at the 95th percentile and fourteen at the most — the figures giveUpAfter's
// doc, newRetryer's and docs/concepts/object-store.md quote, so a change to
// the backoff or the rule that moves them fails here.
func TestAFailingRequestsLastAttemptsAreSpreadOverTheirBounds(t *testing.T) {
	t.Parallel()
	const (
		requests  = 2000
		firstByte = 100 * time.Millisecond
		window    = 100 * time.Millisecond
	)
	srv := s3fake.Start(t, "files")
	srv.Put("k", []byte("an object"))
	clock := newStoppedClock()
	b := openPaced(t, srv, "", &onTheClock{clock: clock, backoff: s3obj.SeededJitter(1), firstByte: firstByte},
		clock.Clock())
	s3obj.SeedBounds(b, 2)
	srv.FailFor(time.Hour, http.StatusInternalServerError, "InternalError")
	least := s3obj.GiveUpAfterBound - s3obj.GiveUpAfterBound/3
	var (
		lasts    []time.Duration
		attempts []int
	)
	for range requests {
		began := clock.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		_, err := b.Stat(ctx, "k")
		cancel()
		given, ok := errors.AsType[*s3obj.GivenUp](err)
		if !ok {
			t.Fatalf("a head the bucket kept failing = %v; want it given up", err)
		}
		// GIVEN UP at its first failure at or past its span's bound, a first
		// byte after that attempt was sent — which a wait cut to the bound
		// sends on it, and one drawn short of the bound up to a first byte
		// before it. The first failure is the first attempt's answer, which
		// the clock read as the head began.
		took := clock.Now().Sub(began)
		if took < least || took > s3obj.GiveUpAfterBound+firstByte {
			t.Fatalf("a head the bucket kept failing was given up %v after its first failure (%d attempts); "+
				"want it inside [%v, %v], its own bound and its last attempt's first byte", took,
				given.Attempts(), least, s3obj.GiveUpAfterBound+firstByte)
		}
		lasts, attempts = append(lasts, took-firstByte), append(attempts, given.Attempts())
	}

	slices.Sort(lasts)
	most, at := 0, time.Duration(0)
	for i, from := range lasts {
		n, _ := slices.BinarySearch(lasts[i:], from+window)
		if n > most {
			most, at = n, from
		}
	}
	// EVEN, a tenth of a second of the bounds' twenty holds one in two
	// hundred — ten here, and the fullest of two hundred such windows about
	// twice that; four times it is no spread at all.
	if most > requests/50 {
		t.Fatalf("%d of %d last attempts were sent within %v of each other, %v after the first failure; "+
			"want them spread over the minute's last third", most, requests, window, at)
	}

	slices.Sort(attempts)
	median, p95, worst := attempts[requests/2], attempts[requests*95/100], attempts[requests-1]
	t.Logf("attempts a request: median %d, 95th percentile %d, most %d; last attempts at most %d of %d "+
		"in a %v window", median, p95, worst, most, requests, window)
	if median != 9 || p95 != 12 || worst != 14 {
		t.Fatalf("a failing request's attempts ran to a median of %d, %d at the 95th percentile and %d "+
			"at the most; the docs quote 9, 12 and 14 — measure again and restate them", median, p95, worst)
	}
}

// onTheClock is a backoff that waits on clock rather than in real time: it
// moves clock on by the wait backoff draws and by firstByte, the time the
// attempt it leads to then takes to be answered, and waits for nothing — so
// a request's attempts are judged where a bucket a first byte away would
// have them, as fast as the fake answers.
type onTheClock struct {
	clock     *stoppedClock
	backoff   retry.BackoffDelayer
	firstByte time.Duration
}

func (o *onTheClock) BackoffDelay(attempt int, err error) (time.Duration, error) {
	d, err := o.backoff.BackoffDelay(attempt, err)
	o.clock.Advance(d + o.firstByte)
	return 0, err
}

// A GIVEN-UP REQUEST IS STILL THE BUCKET'S FAILURE: the give-up holds the
// bucket's last answer, so a batch deletion given up on 500s is the bucket
// failing — never [objstore.ErrNoBatch], which would send its caller to ask
// for every name on its own — and its 500 is still found inside it.
func TestAGivenUpRequestIsStillTheBucketsFailure(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b, _ := openStepped(t, srv, nil)
	srv.FailNext(1_000_000, http.StatusInternalServerError, "InternalError")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := b.DeleteBatch(ctx, []string{"files/a", "files/b"})
	if _, ok := errors.AsType[*s3obj.GivenUp](err); !ok {
		t.Fatalf("a batch deletion the bucket kept failing = %v; want it given up", err)
	}
	if errors.Is(err, objstore.ErrNoBatch) {
		t.Fatalf("a batch deletion given up on 500s = %v, which reads as a bucket without the verb", err)
	}
	if code := statusOf(err); code != http.StatusInternalServerError {
		t.Fatalf("a batch deletion given up on 500s = %v, holding status %d; want the 500", err, code)
	}
}

// A SESSION GIVEN UP GIVES UP ITS WAITER. A directory bucket's request waits
// in GetIdentity for its S3 Express session, which is a request of its own: a
// session endpoint failing for a minute is given up like any request, and the
// request waiting for it fails with it — never sent itself — after exactly the
// session's four attempts. The SDK hands the waiter the session's failure as
// TEXT, so the waiter holds no give-up of its own, and is final because
// nothing reads that text as worth asking again: should a later SDK keep the
// chain, this case says so.
func TestASessionGivenUpGivesUpItsWaiter(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files--use1-az4--x-s3")
	srv.SessionLife = -time.Minute
	srv.Put("k", []byte("an object"))
	b, _ := openStepped(t, srv, nil)
	srv.RefuseSessions(http.StatusInternalServerError, "InternalError")
	sessions := srv.RequestsOf("CreateSession")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := b.Stat(ctx, "k")
	if err == nil || ctx.Err() != nil {
		t.Fatalf("a head whose session the bucket kept failing = %v (its guard: %v); want it failed "+
			"with its session", err, ctx.Err())
	}
	for _, want := range []string{"CreateSession", "failed every attempt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("a head whose session was given up = %q; want it naming %q", err, want)
		}
	}
	if _, ok := errors.AsType[*s3obj.GivenUp](err); ok {
		t.Fatalf("a head whose session was given up holds a give-up in its chain (%v); the SDK used "+
			"to flatten the session's error to text, and the waiter's reading of it is now worth "+
			"restating", err)
	}
	if n := srv.RequestsOf("CreateSession") - sessions; n != 4 {
		t.Fatalf("the session was asked for %d times, want the 4 of its minute", n)
	}
	if n := srv.RequestsOf("HeadObject"); n != 0 {
		t.Fatalf("the head waiting for its session was sent %d times, want none", n)
	}
}
