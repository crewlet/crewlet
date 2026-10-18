package s3obj_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
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
// asking again a millisecond after a failure, and measuring its gate's quiet
// minute on now (nil is time.Now).
func openRetrying(t *testing.T, srv *s3fake.Server, prefix string, now func() time.Time) *s3obj.Backend {
	t.Helper()
	b, err := s3obj.OpenRetrying(t.Context(), fakeConfig(srv, prefix), soonAfter, now)
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	s3obj.CutParts(b, part)
	return b
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
	b := openRetrying(t, srv, "", nil)
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
	b := openRetrying(t, srv, "", nil)
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
	b := openRetrying(t, srv, "", nil)
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
	rt := s3obj.ClientRetryer(openRetrying(t, srv, "", nil))
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
			b := openRetrying(t, srv, "", nil)
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
			b := openRetrying(t, s3fake.Start(t, "files"), "", nil)
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
	b := openRetrying(t, s3fake.Start(t, "files"), "", nil)
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
	b := openRetrying(t, s3fake.Start(t, "files"), "", nil)
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
func TestAThrottledClientIsLetGoOnceTheBucketIsQuiet(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.Put("k", []byte("an object"))
	var (
		mu  sync.Mutex
		now = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
	b := openRetrying(t, srv, "", clock)
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
	b := openRetrying(t, srv, "", nil)
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
	b := openRetrying(t, srv, "", nil)
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

// sends is every operation the backend sends its bucket: the twelve it asks
// of any bucket, and S3 Express's session request for a directory bucket.
var sends = []string{"HeadBucket", "HeadObject", "GetObject", "PutObject", "CreateMultipartUpload",
	"UploadPart", "CompleteMultipartUpload", "AbortMultipartUpload", "DeleteObject", "DeleteObjects",
	"ListObjectsV2", "ListMultipartUploads", "CreateSession"}

// ONLY THE SESSION REQUEST PASSES THE GATE: of every operation the backend
// sends, and of names it does not, an attempt is let past a gate whose one
// place is held only for CreateSession — known by the operation the SDK names
// in its context, so no other request a directory bucket's URL shares slips
// through beside it.
func TestOnlyTheSessionRequestPassesTheGate(t *testing.T) {
	t.Parallel()
	b := openRetrying(t, s3fake.Start(t, "files"), "", nil)
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
	for _, op := range append(slices.Clone(sends), "other", "") {
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
