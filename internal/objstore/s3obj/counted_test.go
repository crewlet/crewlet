package s3obj_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// newRecorder is a node's recorder, over the whole catalogue.
func newRecorder(t *testing.T) *metrics.Recorder {
	t.Helper()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// openCounted is the backend over srv counting into rec, its attempts paced
// by backoff and its gate's time kept by c — at the parts every case here
// runs at.
func openCounted(t *testing.T, srv *s3fake.Server, rec *metrics.Recorder, backoff retry.BackoffDelayer,
	c s3obj.Clock) *s3obj.Backend {
	t.Helper()
	cfg := fakeConfig(srv, "")
	cfg.Metrics = rec
	b, err := s3obj.OpenRetrying(t.Context(), cfg, backoff, c)
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	s3obj.CutParts(b, part)
	return b
}

// reading is what rec holds of the instrument name: each series by its
// attributes' values in the order the catalogue declares them, joined by `/`
// — `HeadObject/answered`.
func reading(rec *metrics.Recorder, name string) map[string]metrics.Snapshot {
	var declared []string
	for _, inst := range metrics.Catalogue() {
		if inst.Name == name {
			declared = inst.Attributes
		}
	}
	out := map[string]metrics.Snapshot{}
	for _, s := range rec.Read() {
		if s.Name != name {
			continue
		}
		var key []string
		for _, a := range declared {
			key = append(key, s.Attrs[a])
		}
		out[strings.Join(key, "/")] = s
	}
	return out
}

// countsOf is each series of the counter name whose operation is operation,
// by its outcome, at its total.
func countsOf(rec *metrics.Recorder, name, operation string) map[string]uint64 {
	out := map[string]uint64{}
	for key, s := range reading(rec, name) {
		if op, outcome, _ := strings.Cut(key, "/"); op == operation {
			out[outcome] = s.Total
		}
	}
	return out
}

// observedOf is each series of the histogram name whose operation is
// operation, by its outcome, as how many requests it observed and their
// attempts' sum.
func observedOf(rec *metrics.Recorder, name, operation string) map[string][2]float64 {
	out := map[string][2]float64{}
	for key, s := range reading(rec, name) {
		if op, outcome, _ := strings.Cut(key, "/"); op == operation {
			out[outcome] = [2]float64{float64(s.Count), s.Sum}
		}
	}
	return out
}

// EVERY ATTEMPT IS COUNTED BY OPERATION AND OUTCOME, and every request at how
// many attempts it took and how it ended — so an operator reads a bucket's
// throttles, its failures and its retries off the node's own series.
//
// On one object: two 500s and then the object; a `SlowDown` and then the
// object; a missing key; a 403. Seven attempts — two failed, one throttled,
// four answered (two successes, the missing key and the refusal) — and four
// requests answered after 3, 2, 1 and 1 of them. Then a head that never gets
// its turn, waiting at a gate whose every place is held until its deadline:
// a request with no attempt, observed at zero as `unsent`, which is what
// keeps the retries computable — attempts 7, less requests 5, plus unsent 1,
// is the 3 retries the two heads asked again made. And a head the bucket
// fails for its whole span is observed `given_up`, at every attempt it made.
func TestEveryAttemptIsCountedByOperationAndOutcome(t *testing.T) {
	t.Parallel()
	rec := newRecorder(t)
	srv := s3fake.Start(t, "files")
	srv.Put("kept", []byte("an object"))
	b := openCounted(t, srv, rec, soonAfter, s3obj.Clock{})
	ctx := t.Context()

	srv.FailNext(2, http.StatusInternalServerError, "InternalError")
	if _, err := b.Stat(ctx, "kept"); err != nil {
		t.Fatalf("a head through two server errors = %v", err)
	}
	srv.FailNext(1, http.StatusServiceUnavailable, "SlowDown")
	if _, err := b.Stat(ctx, "kept"); err != nil {
		t.Fatalf("a head through a throttle = %v", err)
	}
	if _, err := b.Stat(ctx, "gone"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("a head of a missing key = %v, want ErrNotFound", err)
	}
	srv.FailNext(1, http.StatusForbidden, "AccessDenied")
	if _, err := b.Stat(ctx, "kept"); err == nil || statusOf(err) != http.StatusForbidden {
		t.Fatalf("a head the bucket refuses = %v, want its 403", err)
	}

	if got, want := countsOf(rec, metrics.ObjectsS3Attempts, "HeadObject"),
		map[string]uint64{"failed": 2, "throttled": 1, "answered": 4}; !mapsEqual(got, want) {
		t.Fatalf("the heads' attempts = %v, want %v", got, want)
	}
	if got, want := observedOf(rec, metrics.ObjectsS3RequestAttempts, "HeadObject"),
		map[string][2]float64{"answered": {4, 7}}; !mapsEqual(got, want) {
		t.Fatalf("the heads' requests (count, attempts) = %v, want %v", got, want)
	}
	// THE PROBE AT OPEN (probe.go) is counted like any request through the
	// client: its object stored and deleted through the engine's own path,
	// and deleted again however the probe ends. Its presigned puts are its
	// own HTTP, past the SDK, which no attempt here counts.
	if got, want := countsOf(rec, metrics.ObjectsS3Attempts, "PutObject"),
		map[string]uint64{"answered": 1}; !mapsEqual(got, want) {
		t.Fatalf("the probe's put's attempts = %v, want %v", got, want)
	}
	if got, want := observedOf(rec, metrics.ObjectsS3RequestAttempts, "PutObject"),
		map[string][2]float64{"answered": {1, 1}}; !mapsEqual(got, want) {
		t.Fatalf("the probe's put's requests = %v, want %v", got, want)
	}
	if got, want := countsOf(rec, metrics.ObjectsS3Attempts, "DeleteObject"),
		map[string]uint64{"answered": 2}; !mapsEqual(got, want) {
		t.Fatalf("the probe's deletes' attempts = %v, want %v", got, want)
	}

	// A HEAD THAT NEVER GETS ITS TURN: every place the gate — engaged by the
	// throttle above — has is held, and the head waits out its deadline.
	ended, cancel := context.WithCancel(ctx)
	cancel()
	var held []s3obj.Attempt
	for {
		a, err := s3obj.Enter(ended, b)
		if err != nil {
			break
		}
		held = append(held, a)
	}
	waited, cancelWait := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err := b.Stat(waited, "kept")
	cancelWait()
	for _, a := range held {
		a.Leave(context.Canceled)
	}
	if err == nil {
		t.Fatal("a head at a gate whose every place is held answered before its deadline")
	}
	requests := observedOf(rec, metrics.ObjectsS3RequestAttempts, "HeadObject")
	if got, want := requests["unsent"], [2]float64{1, 0}; got != want {
		t.Fatalf("the head that never got its turn = %v as unsent (count, attempts), want %v; all: %v",
			got, want, requests)
	}

	// THE RETRIES, AS AN OPERATOR COMPUTES THEM from the two series: the
	// attempts, less the requests, plus the requests that made none.
	var attempts, observed, unsent float64
	for _, n := range countsOf(rec, metrics.ObjectsS3Attempts, "HeadObject") {
		attempts += float64(n)
	}
	for outcome, o := range requests {
		observed += o[0]
		if outcome == "unsent" {
			unsent += o[0]
		}
	}
	if retries := attempts - observed + unsent; retries != (3-1)+(2-1) {
		t.Fatalf("the heads' retries read off the series = %v - %v + %v = %v, want the 3 made",
			attempts, observed, unsent, retries)
	}

	// A HEAD THE BUCKET FAILS FOR ITS WHOLE SPAN is given up, and observed
	// so — at every attempt it made, each counted a failure.
	failedBefore := countsOf(rec, metrics.ObjectsS3Attempts, "HeadObject")["failed"]
	s3obj.SetStall(b, 100*time.Millisecond)
	srv.FailFor(time.Hour, http.StatusInternalServerError, "InternalError")
	guard, cancelGuard := context.WithTimeout(ctx, 30*time.Second)
	defer cancelGuard()
	if _, err := b.Stat(guard, "kept"); !isGivenUp(err) {
		t.Fatalf("a head the bucket kept failing = %v, want it given up", err)
	}
	failed := countsOf(rec, metrics.ObjectsS3Attempts, "HeadObject")["failed"] - failedBefore
	if got, want := observedOf(rec, metrics.ObjectsS3RequestAttempts, "HeadObject")["given_up"],
		[2]float64{1, float64(failed)}; got != want || failed < 2 {
		t.Fatalf("the head given up = %v (count, attempts), want %v — one request at its %d failed "+
			"attempts", got, want, failed)
	}
}

// isGivenUp reports whether err holds a request's give-up.
func isGivenUp(err error) bool {
	_, ok := errors.AsType[*s3obj.GivenUp](err)
	return ok
}

func mapsEqual[K comparable, V comparable](a, b map[K]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// EVERY OPERATION THE BACKEND SENDS IS A LABEL — of the closed set, and the
// set holds nothing the backend does not send. Every verb is driven against
// the fake — a put of less than a part and one in parts, a narrow read and a
// read of three ranges, a head, a deletion and a batch of them, a listing
// and a ranged one, the uploads left unfinished and the abandonment of one —
// and a head in a directory bucket, which asks for its S3 Express session:
// the operations the recorder then holds are exactly [s3obj.Operations],
// none of them `other`. A label outside the set would be counted `other`
// and say nothing; one in it that no verb sends is documentation of a request
// that does not exist.
func TestEveryOperationTheBackendSendsIsALabel(t *testing.T) {
	t.Parallel()
	rec := newRecorder(t)
	srv := s3fake.Start(t, "files")
	b := openCounted(t, srv, rec, soonAfter, s3obj.Clock{})
	ctx := t.Context()
	read, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	if err := b.Put(ctx, "small", bytes.NewReader([]byte("small")), objstore.PutMeta{}); err != nil {
		t.Fatalf("a put of less than a part = %v", err)
	}
	large := bytes.Repeat([]byte("three parts "), 3*part/len("three parts ")+1)[:3*part]
	if err := b.Put(ctx, "large", bytes.NewReader(large), objstore.PutMeta{}); err != nil {
		t.Fatalf("a put in parts = %v", err)
	}
	for _, r := range []struct {
		name  string
		off   int64
		n     int64
		bytes int
	}{
		{name: "narrow", off: 0, n: 10, bytes: 10},
		{name: "wide", off: 0, n: -1, bytes: len(large)},
	} {
		rc, err := b.Get(read, "large", r.off, r.n)
		if err != nil {
			t.Fatalf("a %s read = %v", r.name, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || len(got) != r.bytes {
			t.Fatalf("a %s read = %d bytes, %v; want %d", r.name, len(got), err, r.bytes)
		}
	}
	if _, err := b.Stat(ctx, "small"); err != nil {
		t.Fatalf("a head = %v", err)
	}
	if err := b.Delete(ctx, "small"); err != nil {
		t.Fatalf("a deletion = %v", err)
	}
	if err := b.DeleteBatch(ctx, []string{"large"}); err != nil {
		t.Fatalf("a batch deletion = %v", err)
	}
	none := func(objstore.Info) error { return nil }
	if err := b.List(ctx, none); err != nil {
		t.Fatalf("a listing = %v", err)
	}
	if err := b.ListRange(ctx, "", "~", none); err != nil {
		t.Fatalf("a ranged listing = %v", err)
	}
	srv.Begin("left")
	var left []objstore.Pending
	if err := b.Pending(ctx, func(p objstore.Pending) error { left = append(left, p); return nil }); err != nil {
		t.Fatalf("a listing of the unfinished uploads = %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("the unfinished uploads = %v, want the one begun", left)
	}
	if err := b.Abandon(ctx, left[0]); err != nil {
		t.Fatalf("an abandonment = %v", err)
	}
	dir := s3fake.Start(t, "files--usw2-az1--x-s3")
	dir.Put("kept", []byte("an object"))
	if _, err := openCounted(t, dir, rec, soonAfter, s3obj.Clock{}).Stat(ctx, "kept"); err != nil {
		t.Fatalf("a head in a directory bucket = %v", err)
	}

	for _, name := range []string{metrics.ObjectsS3Attempts, metrics.ObjectsS3RequestAttempts} {
		var sent []string
		for key := range reading(rec, name) {
			op, _, _ := strings.Cut(key, "/")
			if !slices.Contains(sent, op) {
				sent = append(sent, op)
			}
		}
		slices.Sort(sent)
		declared := slices.Sorted(slices.Values(s3obj.Operations))
		if !slices.Equal(sent, declared) {
			t.Errorf("%s labels the operations %v; the closed set is %v (a request outside it is "+
				"counted %q)", name, sent, declared, s3obj.OtherOperation)
		}
	}
}

// AN ATTEMPT WHOSE SESSION CANNOT BE HAD IS UNREACHED: a directory bucket's
// head waits for its S3 Express session in GetIdentity, after the gate let it
// out, and the session request — failing to its bound — is counted as its
// own operation, four failed attempts and a request given up; the head is one
// attempt that reached nothing, and a request failed with its session.
func TestAnAttemptWhoseSessionCannotBeHadIsUnreached(t *testing.T) {
	t.Parallel()
	rec := newRecorder(t)
	srv := s3fake.Start(t, "files--use1-az4--x-s3")
	srv.SessionLife = -time.Minute
	srv.Put("k", []byte("an object"))
	s := &stepping{clock: newStoppedClock()}
	b := openCounted(t, srv, rec, s, s.clock.Clock())
	sessionsBefore := countsOf(rec, metrics.ObjectsS3Attempts, "CreateSession")
	srv.RefuseSessions(http.StatusInternalServerError, "InternalError")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := b.Stat(ctx, "k"); err == nil || ctx.Err() != nil {
		t.Fatalf("a head whose session the bucket kept failing = %v (its guard: %v)", err, ctx.Err())
	}
	if got, want := countsOf(rec, metrics.ObjectsS3Attempts, "HeadObject"),
		map[string]uint64{"unreached": 1}; !mapsEqual(got, want) {
		t.Fatalf("the head's attempts = %v, want %v", got, want)
	}
	if got, want := observedOf(rec, metrics.ObjectsS3RequestAttempts, "HeadObject"),
		map[string][2]float64{"failed": {1, 1}}; !mapsEqual(got, want) {
		t.Fatalf("the head's request = %v, want %v", got, want)
	}
	sessions := countsOf(rec, metrics.ObjectsS3Attempts, "CreateSession")
	if failed := sessions["failed"] - sessionsBefore["failed"]; failed != 4 {
		t.Fatalf("the session's failed attempts = %d (all: %v), want the 4 of its minute", failed, sessions)
	}
	if got := observedOf(rec, metrics.ObjectsS3RequestAttempts, "CreateSession")["given_up"]; got !=
		[2]float64{1, 4} {
		t.Fatalf("the session given up = %v (count, attempts), want one request at 4", got)
	}
}

// EVERY ANSWER IS EXACTLY ONE OUTCOME, read by the gate's own predicates: an
// attempt its own context ended is `ended` whatever it was answered; a served
// answer — a success, a refusal — `answered`; a throttle — S3's, a 429, a 503
// naming no code — `throttled`; any other status — a server error, a gateway's
// own 503, a redirect — `failed`; and no status at all — a refused
// connection, a certificate, a session that could not be had — `unreached`.
// And a request: none sent is `unsent` whatever ended it; a give-up is
// `given_up` though its last attempt failed; a served answer `answered`; one
// its deadline ended after an attempt `ended`; anything else `failed`.
func TestEveryAnswerIsExactlyOneOutcome(t *testing.T) {
	t.Parallel()
	live := context.Background()
	gone, cancel := context.WithCancel(live)
	cancel()
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	untrusted := fmt.Errorf("send: %w", &tls.CertificateVerificationError{})
	// AN ANSWER THAT IS NO S3 ERROR — a load balancer's own page — names no
	// code at all, where [answer] always names one.
	noCode := &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusServiceUnavailable}},
		Err:      errors.New("an answer that is no S3 error"),
	}
	noSession := errors.New("get identity: get credentials: operation error S3: CreateSession, " +
		"the bucket failed every attempt of this request for 1m0s (4 attempts), and a request it keeps " +
		"failing is given up 40s to 1m0s into its failures")
	for _, c := range []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"success", live, nil, "answered"},
		{"a success as its context ended", gone, nil, "answered"},
		{"a missing key", live, answer(http.StatusNotFound, "NoSuchKey"), "answered"},
		{"a refusal", live, answer(http.StatusForbidden, "AccessDenied"), "answered"},
		{"S3's throttle", live, slowDown, "throttled"},
		{"a 429", live, answer(http.StatusTooManyRequests, "Whatever"), "throttled"},
		{"a 503 naming no code", live, noCode, "throttled"},
		{"MinIO's write throttle", live, answer(http.StatusServiceUnavailable, "SlowDownWrite"), "throttled"},
		{"a server error", live, answer(http.StatusInternalServerError, "InternalError"), "failed"},
		{"a gateway's own 503", live, answer(http.StatusServiceUnavailable, "XMinioReadQuorum"), "failed"},
		{"a redirect to another region", live, answer(http.StatusMovedPermanently, "PermanentRedirect"), "failed"},
		{"a connection refused", live, refused, "unreached"},
		{"a certificate not trusted", live, untrusted, "unreached"},
		{"a session that could not be had", live, noSession, "unreached"},
		{"a body the bucket stopped taking", live,
			&smithyhttp.RequestSendError{Err: s3obj.Stalled(4<<20, 8<<20, false, time.Minute)}, "stalled"},
		{"an answer that went silent", live, &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusOK}},
			Err:      s3obj.Stalled(8<<20, 8<<20, true, time.Minute)}, "stalled"},
		{"a stall as its context ended", gone, s3obj.Stalled(0, 0, false, time.Minute), "ended"},
		{"a throttle as its context ended", gone, slowDown, "ended"},
		{"a missing key as its context ended", gone, answer(http.StatusNotFound, "NoSuchKey"), "ended"},
		{"a connection refused as its context ended", gone, refused, "ended"},
	} {
		if got := s3obj.AttemptOutcome(c.ctx, c.err); got != c.want {
			t.Errorf("an attempt answered %s = %q, want %q", c.name, got, c.want)
		}
		if !slices.Contains(s3obj.AttemptOutcomes, c.want) {
			t.Errorf("%q is no attempt outcome %v", c.want, s3obj.AttemptOutcomes)
		}
	}
	for _, c := range []struct {
		name     string
		ctx      context.Context
		attempts int
		err      error
		want     string
	}{
		{"ended at the gate", gone, 0, context.Canceled, "unsent"},
		{"refused before it was sent", live, 0, errors.New("invalid input"), "unsent"},
		{"answered", live, 1, nil, "answered"},
		{"refused", live, 2, answer(http.StatusNotFound, "NoSuchKey"), "answered"},
		{"given up", live, 9, s3obj.GiveUp(answer(http.StatusInternalServerError, "InternalError"), 9), "given_up"},
		{"given up as its context ended", gone, 9, s3obj.GiveUp(refused, 9), "given_up"},
		{"ended throttled", gone, 3, slowDown, "ended"},
		{"ended failing", gone, 3, answer(http.StatusInternalServerError, "InternalError"), "ended"},
		{"a certificate not trusted", live, 1, untrusted, "failed"},
		{"its session given up", live, 1, noSession, "failed"},
	} {
		if got := s3obj.RequestOutcome(c.ctx, c.attempts, c.err); got != c.want {
			t.Errorf("a request %s = %q, want %q", c.name, got, c.want)
		}
		if !slices.Contains(s3obj.RequestOutcomes, c.want) {
			t.Errorf("%q is no request outcome %v", c.want, s3obj.RequestOutcomes)
		}
	}
}

// THE GATE'S GAUGES READ IT ENGAGED UNTIL IT LIFTS, and nothing before. A
// node nothing has throttled sets neither. A throttle at a width of ten
// leaves seven places, and the limit reads 7; three answers grow the limit to
// about 7.43, and it still reads 7, the whole number of places the gate lets
// out. Three attempts waiting at the full gate read 3, and 0 once they are
// let out. And the timer that ends the throttle's minute reads the gate
// lifted: 0.
func TestTheGateGaugeReadsEngagedUntilItLifts(t *testing.T) {
	t.Parallel()
	rec := newRecorder(t)
	clock := newStoppedClock()
	b := openCounted(t, s3fake.Start(t, "files"), rec, soonAfter, clock.Clock())
	gauge := func(name string) (float64, bool) {
		s, ok := reading(rec, name)[""]
		return s.Value, ok
	}
	for _, name := range []string{metrics.ObjectsS3GateLimit, metrics.ObjectsS3GateWaiting} {
		if v, ok := gauge(name); ok {
			t.Fatalf("%s reads %v on a node nothing has throttled, want no series", name, v)
		}
	}
	if n := s3obj.LetOut(b, 10); n != 10 {
		t.Fatalf("a gate nothing slowed let out %d of 10", n)
	}
	cut, err := s3obj.Enter(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	cut.Leave(slowDown)
	if v, _ := gauge(metrics.ObjectsS3GateLimit); v != 7 {
		t.Fatalf("the limit after a throttle at a width of ten reads %v, want 7", v)
	}
	for range 3 {
		a, err := s3obj.Enter(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		a.Leave(nil)
	}
	if v, _ := gauge(metrics.ObjectsS3GateLimit); v != 7 {
		t.Fatalf("the limit three answers later reads %v, want the 7 places it lets out", v)
	}

	var held []s3obj.Attempt
	for range 7 {
		a, err := s3obj.Enter(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, a)
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			a, err := s3obj.Enter(t.Context(), b)
			if err != nil {
				t.Error(err)
				return
			}
			a.Leave(context.Canceled)
		})
	}
	for s3obj.Waiting(b) != 3 {
		time.Sleep(time.Millisecond)
	}
	if v, _ := gauge(metrics.ObjectsS3GateWaiting); v != 3 {
		t.Fatalf("three attempts waiting at the full gate read %v, want 3", v)
	}
	for _, a := range held {
		a.Leave(context.Canceled)
	}
	wg.Wait()
	if v, _ := gauge(metrics.ObjectsS3GateWaiting); v != 0 {
		t.Fatalf("the waiters let out read %v, want 0", v)
	}

	clock.Advance(s3obj.ThrottleQuiet)
	timers := clock.Timers()
	if len(timers) != 1 {
		t.Fatalf("the throttle set %d timers, want the one that lifts the gate", len(timers))
	}
	timers[0].fire()
	if v, _ := gauge(metrics.ObjectsS3GateLimit); v != 0 {
		t.Fatalf("the limit once the throttle's minute has ended reads %v, want 0: the gate is lifted", v)
	}
}

// A WAITER WHOSE CONTEXT ENDS TAKES ITSELF OFF THE GAUGE as it leaves the
// line — the gate starving the node's requests, which is the reading
// `gate.waiting` is published for. Three attempts wait at an engaged gate
// whose every place is held, and their contexts end: the gauge reads 0 once
// they have left, with every place still held, nothing else entering or
// leaving and the throttle's minute not yet over — set where it was, it read
// 3 until the next attempt moved the gate, for as long as nothing did.
func TestAWaiterWhoseContextEndsLeavesTheGateGauge(t *testing.T) {
	t.Parallel()
	rec := newRecorder(t)
	clock := newStoppedClock()
	b := openCounted(t, s3fake.Start(t, "files"), rec, soonAfter, clock.Clock())
	waiting := func() float64 { return reading(rec, metrics.ObjectsS3GateWaiting)[""].Value }
	if n := s3obj.LetOut(b, 10); n != 10 {
		t.Fatalf("a gate nothing slowed let out %d of 10", n)
	}
	cut, err := s3obj.Enter(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	cut.Leave(slowDown)
	var held []s3obj.Attempt
	for range s3obj.LetOut(b, 10) {
		a, err := s3obj.Enter(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, a)
	}
	defer func() {
		for _, a := range held {
			a.Leave(context.Canceled)
		}
	}()

	ctx, giveUp := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if a, err := s3obj.Enter(ctx, b); err == nil {
				a.Leave(context.Canceled)
				t.Error("an attempt was let out of a gate whose every place is held")
			}
		})
	}
	for s3obj.Waiting(b) != 3 {
		time.Sleep(time.Millisecond)
	}
	if v := waiting(); v != 3 {
		t.Fatalf("three attempts waiting at the full gate read %v, want 3", v)
	}
	giveUp()
	wg.Wait()
	if n := s3obj.Waiting(b); n != 0 {
		t.Fatalf("%d attempts still wait at the gate after their contexts ended", n)
	}
	if v := waiting(); v != 0 {
		t.Fatalf("the waiters whose contexts ended read %v once they had left the line, want 0 — every "+
			"place still held, nothing else moving the gate and its minute not over (%d timers set, none "+
			"fired)", v, len(clock.Timers()))
	}
}

// THE CATALOGUE NAMES EVERY OPERATION AND OUTCOME the client counts under,
// because an operator reads what a label means there and nowhere else: the
// attempts counter's entry names every operation of the closed set and every
// attempt outcome, and the request histogram's every request outcome.
func TestTheCatalogueNamesEveryOperationAndOutcome(t *testing.T) {
	t.Parallel()
	shows := map[string]string{}
	for _, inst := range metrics.Catalogue() {
		shows[inst.Name] = inst.Shows
	}
	for _, c := range []struct {
		name   string
		labels []string
	}{
		{metrics.ObjectsS3Attempts, append(slices.Clone(s3obj.Operations), s3obj.AttemptOutcomes...)},
		{metrics.ObjectsS3RequestAttempts, s3obj.RequestOutcomes},
	} {
		text, ok := shows[c.name]
		if !ok {
			t.Fatalf("%s is not catalogued", c.name)
		}
		for _, label := range c.labels {
			if !strings.Contains(text, "`"+label+"`") {
				t.Errorf("%s's entry does not name `%s`, which the client counts under", c.name, label)
			}
		}
	}
}
