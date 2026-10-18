package s3obj_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// caseStall is the stall bound the cases here give a client: two seconds,
// sampled every 167 ms — long against anything loopback makes a request wait
// for, short against the case's own deadline, so a write ended at its stall
// and one that waited for its deadline are told apart.
const caseStall = 2 * time.Second

// openStalling is the backend over srv at the production part size, its stall
// bound [caseStall].
func openStalling(t *testing.T, srv *s3fake.Server) *s3obj.Backend {
	t.Helper()
	b := openFake(t, srv, "")
	s3obj.SetStall(b, caseStall)
	return b
}

// storeOver is the store over b, as every writer in the engine reaches it.
func storeOver(t *testing.T, b objstore.Backend) *objstore.Store {
	t.Helper()
	store, err := objstore.NewStore(b)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// A WRITE THE BUCKET STOPS TAKING ENDS A STALL AFTER IT LAST MOVED, whatever
// its deadline — a put of up to a part (four and a half minutes), a part
// (sixteen and a half), a completion (five) — and is given up, asked once.
//
// THE BUCKET STOPS AT ONCE, so each write's last movement is its start, give
// or take a sample of its socket: each ends between its stall and its stall
// plus a second. The completions' parts are the suite's sixty-four kibibytes,
// so what goes before the stop is milliseconds. NO REQUEST WAS ASKED
// TWICE — the bucket was sent exactly the requests the put makes once each —
// and an upload in parts is abandoned, leaving nothing pending.
func TestAWriteTheBucketStopsTakingEndsAStallAfterItMoved(t *testing.T) {
	t.Parallel()
	const putSize = 8<<20 - 1
	const partsSize = 16 << 20
	const completedSize = 2*part + part/2
	for _, c := range []struct {
		name string
		size int
		// part is the part size the put runs at: zero is the production
		// eight mebibytes.
		part int
		stop func(srv *s3fake.Server)
		// sent is every request the put makes, once each.
		sent map[string]int
	}{
		{"a put the bucket read none of", putSize, 0,
			func(srv *s3fake.Server) { srv.Hang("PutObject", 0) },
			map[string]int{"PutObject": 1}},
		{"a put the bucket read a mebibyte of", putSize, 0,
			func(srv *s3fake.Server) { srv.Hang("PutObject", 1<<20) },
			map[string]int{"PutObject": 1}},
		{"a put the bucket read whole and never answered", putSize, 0,
			func(srv *s3fake.Server) { srv.Hang("PutObject", -1) },
			map[string]int{"PutObject": 1}},
		{"parts the bucket read none of", partsSize, 0,
			func(srv *s3fake.Server) { srv.Hang("UploadPart", 0) },
			map[string]int{"CreateMultipartUpload": 1, "UploadPart": 2, "AbortMultipartUpload": 1}},
		{"a completion never answered", completedSize, part,
			func(srv *s3fake.Server) { srv.Hang("CompleteMultipartUpload", -1) },
			map[string]int{"CreateMultipartUpload": 1, "UploadPart": 3, "CompleteMultipartUpload": 1,
				"AbortMultipartUpload": 1}},
		{"a completion that answered its headers and nothing more", completedSize, part,
			func(srv *s3fake.Server) { srv.HangAnswer("CompleteMultipartUpload") },
			map[string]int{"CreateMultipartUpload": 1, "UploadPart": 3, "CompleteMultipartUpload": 1,
				"AbortMultipartUpload": 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := s3fake.Start(t, "files")
			b := openStalling(t, srv)
			if c.part != 0 {
				s3obj.CutParts(b, c.part)
			}
			c.stop(srv)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			began := time.Now()
			_, err := storeOver(t, b).Put(ctx, bytes.NewReader(bytes.Repeat([]byte("w"), c.size)),
				int64(c.size), objstore.PutMeta{})
			took := time.Since(began)
			if !errors.Is(err, objstore.ErrStalled) {
				t.Fatalf("a write the bucket stopped taking = %v after %v; want objstore.ErrStalled", err, took)
			}
			if !errors.As(err, new(*s3obj.GivenUp)) {
				t.Fatalf("a write the bucket stopped taking = %v; want its request given up", err)
			}
			if took < caseStall || took > caseStall+time.Second {
				t.Fatalf("a write the bucket stopped taking at once ended after %v; want it ended "+
					"between its %v stall and a second past it", took, caseStall)
			}
			for op, want := range c.sent {
				if got := srv.RequestsOf(op); got != want {
					t.Errorf("the bucket was sent %d %s; want %d — the put's own, once each", got, op, want)
				}
			}
			for _, op := range []string{"PutObject", "CreateMultipartUpload", "UploadPart",
				"CompleteMultipartUpload", "AbortMultipartUpload"} {
				if _, ok := c.sent[op]; !ok && srv.RequestsOf(op) != 0 {
					t.Errorf("the bucket was sent %d %s; want none", srv.RequestsOf(op), op)
				}
			}
			if pending := srv.Uploads(); len(pending) != 0 {
				t.Errorf("a stalled upload left %v pending; want it abandoned", pending)
			}
			if n := s3obj.Watches(b); n != 0 {
				t.Errorf("%d attempts are still watched once the put has ended; want none", n)
			}
		})
	}
}

// A STALLED ATTEMPT IS NOT ASKED AGAIN: the bucket has held it a whole stall
// without moving, which a minute of nothing but failures already meets, and
// asked again it would hold the person another stall.
func TestAStalledAttemptIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := openStalling(t, srv)
	srv.Hang("PutObject", 0)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err := b.Put(ctx, "stalled", bytes.NewReader(make([]byte, 4<<20)), objstore.PutMeta{})
	if !errors.Is(err, objstore.ErrStalled) {
		t.Fatalf("a put the bucket stopped taking = %v; want objstore.ErrStalled", err)
	}
	if got := srv.RequestsOf("PutObject"); got != 1 {
		t.Fatalf("a stalled put was sent %d times; want once", got)
	}
	if !strings.Contains(err.Error(), "stopped moving this request for 2s (1 attempt in all)") {
		t.Fatalf("a stalled put = %q; want it to say the bucket stopped moving it, and how often it "+
			"was sent", err)
	}
}

// A STALLED ATTEMPT IS COUNTED `stalled`, and its request `given_up` at one
// attempt — a body the bucket stopped taking, which reads no status, never
// `unreached`; an answer that went silent, which reads its 200, never
// `failed`.
func TestAStalledAttemptIsCountedStalled(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, op string
		size     int
		stop     func(srv *s3fake.Server)
	}{
		{"a body", "PutObject", part / 2, func(srv *s3fake.Server) { srv.Hang("PutObject", 0) }},
		{"an answer", "CompleteMultipartUpload", 3 * part,
			func(srv *s3fake.Server) { srv.HangAnswer("CompleteMultipartUpload") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := s3fake.Start(t, "files")
			rec := newRecorder(t)
			b := openCounted(t, srv, rec, s3obj.FullJitter, s3obj.Clock{})
			s3obj.SetStall(b, caseStall)
			c.stop(srv)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			if err := b.Put(ctx, "stalled", bytes.NewReader(make([]byte, c.size)), objstore.PutMeta{}); !errors.Is(
				err, objstore.ErrStalled) {
				t.Fatalf("a put = %v; want objstore.ErrStalled", err)
			}
			if got := countsOf(rec, metrics.ObjectsS3Attempts, c.op)["stalled"]; got != 1 {
				t.Errorf("attempts{%s,stalled} = %d; want 1 (all: %v)", c.op, got,
					countsOf(rec, metrics.ObjectsS3Attempts, c.op))
			}
			if got := observedOf(rec, metrics.ObjectsS3RequestAttempts, c.op)["given_up"]; got != [2]float64{1, 1} {
				t.Errorf("request.attempts{%s,given_up} = %v (count, attempts); want one request at one attempt",
					c.op, got)
			}
		})
	}
}

// A DEADLINE THAT COMES FIRST SAYS HOW LONG ITS ATTEMPT WAS QUIET: a request
// its own deadline ended before the watch's bound reads as that deadline, and
// the clause beside it says the bucket had stopped moving it — not that it ran
// out of time while moving.
//
// FIVE SECONDS against a ten-second stall: the last movement can be dated at
// the first sample of the socket, 833 ms in, so a deadline shorter than two
// samples would leave too little quiet for the clause to be owed.
func TestADeadlineThatComesFirstSaysHowLongItWasQuiet(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := openFake(t, srv, "")
	s3obj.SetStall(b, 10*time.Second)
	srv.Hang("PutObject", 0)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := b.Put(ctx, "quiet", bytes.NewReader(make([]byte, 4<<20)), objstore.PutMeta{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a put its deadline ended = %v; want the deadline", err)
	}
	if !strings.Contains(err.Error(), "its last attempt had moved nothing for") {
		t.Fatalf("a put its deadline ended = %q; want it to say how long its attempt had not moved", err)
	}
}

// A DEADLINE THAT COMES BETWEEN ATTEMPTS SAYS NOTHING OF A QUIET ATTEMPT: the
// clause belongs to an attempt the request's own deadline ended IN FLIGHT,
// before its watch had ended it. A request the bucket answered — throttled, or
// failed — and whose deadline then came while it waited to be asked again had
// no attempt in flight, and its last one moved: it was answered. Told that
// "its last attempt had moved nothing", a person whose upload the bucket kept
// throttling would read a bucket that went quiet.
//
// DETERMINISTIC BY ITS BACKOFF: the bucket answers the first attempt at once,
// and the request then waits longer than its whole deadline to be asked again,
// so the deadline comes in the wait — the case the clause was measured to be
// added to in five runs of six under the production backoff.
func TestADeadlineThatComesBetweenAttemptsSaysNothingOfAQuietAttempt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		status       int
		code         string
		answeredWith string
	}{
		{"a throttle", http.StatusServiceUnavailable, "SlowDown", "SlowDown"},
		{"a server error", http.StatusInternalServerError, "InternalError", "InternalError"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := s3fake.Start(t, "files")
			b := openPaced(t, srv, "", retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) {
				return time.Minute, nil
			}), s3obj.Clock{})
			s3obj.SetStall(b, 10*time.Second)
			srv.FailNext(1, c.status, c.code)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err := b.Put(ctx, "answered", bytes.NewReader([]byte("an object the bucket answered")),
				objstore.PutMeta{})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a put its deadline ended between attempts = %v; want the deadline", err)
			}
			if !strings.Contains(err.Error(), c.answeredWith) {
				t.Fatalf("a put its deadline ended between attempts = %q; want the bucket's last answer, %s, "+
					"beside it", err, c.answeredWith)
			}
			if strings.Contains(err.Error(), "had moved nothing for") {
				t.Fatalf("a put whose last attempt the bucket answered = %q; want no word of a quiet attempt", err)
			}
		})
	}
}

// A WAIT AT THE GATE IS NO STALL: the watch begins where the SDK hands the
// attempt to the HTTP client, after the throttle gate has let it out, so an
// attempt held in line behind others' places is never ended for the wait.
//
// THE CASE HOLDS THE GATE'S ONE PLACE ITSELF, never with an attempt the bucket
// answers late — whose own watch would end it at its stall.
func TestAWaitAtTheGateIsNoStall(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	b := openStalling(t, srv)
	cut, err := s3obj.Enter(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	cut.Leave(slowDown)
	held, err := s3obj.Enter(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	put := make(chan error, 1)
	began := time.Now()
	go func() {
		put <- b.Put(t.Context(), "waited", bytes.NewReader([]byte("an object that waited its turn")),
			objstore.PutMeta{})
	}()
	waitFor(t, 10*time.Second, "the put to wait at the gate", func() bool { return s3obj.Waiting(b) == 1 })
	time.Sleep(caseStall + caseStall/2)
	held.Leave(nil)
	if err := <-put; err != nil {
		t.Fatalf("a put that waited %v at the gate, past its %v stall = %v; want it stored",
			time.Since(began), caseStall, err)
	}
}

// A WAIT ON THE REQUEST'S BODY IS NO STALL: time the transport spends inside a
// read of the body is the body's source's — a person's slow upload — and never
// the bucket's, whatever the bound.
func TestAWaitOnTheRequestBodyIsNoStall(t *testing.T) {
	t.Parallel()
	const stall = time.Second
	c := s3obj.Watching(answering{}, stall)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://bucket.invalid/key",
		&slowBody{wait: 3 * stall, data: []byte("a body its source was slow to hand over")})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("an attempt whose body took %v to read, past its %v stall = %v; want it answered",
			3*stall, stall, err)
	}
	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
}

// AN ANSWER THE SDK LEAVES OPEN ENDS ITS WATCH: the SDK does not close an
// answer whose body it failed to read — its check of a completion's 200 reads
// the body first and returns the read's error as it is — so a watch ended only
// at the answer's Close would outlive its attempt. Read to its failure and
// never closed, the answer is no longer watched.
func TestAnAnswerTheSDKLeavesOpenEndsItsWatch(t *testing.T) {
	t.Parallel()
	c := s3obj.Watching(answering{fail: errors.New("the connection went away")}, time.Minute)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://bucket.invalid/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(res.Body); err == nil {
		t.Fatal("an answer whose body fails read whole")
	}
	if n := c.Live(); n != 0 {
		t.Fatalf("an answer read to its failure and never closed leaves %d attempts watched; want none", n)
	}
}

// answering is a transport that reads a request's body to its end and answers
// 200 — its body failing with fail part way, where fail is set.
type answering struct{ fail error }

func (a answering) Do(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		_ = req.Body.Close()
	}
	body := io.Reader(strings.NewReader("<Answer/>"))
	if a.fail != nil {
		body = io.MultiReader(strings.NewReader("<Answ"), failingReader{a.fail})
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(body),
		Request: req}, nil
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// slowBody hands over nothing for wait, and then data.
type slowBody struct {
	wait   time.Duration
	data   []byte
	waited bool
}

func (b *slowBody) Read(p []byte) (int, error) {
	if !b.waited {
		b.waited = true
		time.Sleep(b.wait)
	}
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

// A WRITE ACROSS A LINK AT THE FLOOR IS NEVER ENDED — the floor scaled to the
// case's stall: a link carrying a mebibyte a second for every connection
// together, against a two-second stall, is a link at the floor against the
// minute. The node's send buffer then holds seconds of the link — four
// mebibytes, four seconds, as it holds two minutes of the floor — so the
// transport goes that long between two reads of the body while the socket
// drains, and only the socket's count of what the far end acknowledged shows
// the write moving.
func TestAWriteAcrossALinkAtTheFloorIsNeverEnded(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		size int
	}{
		{"two parts sharing the link", 16 << 20},
		{"one put", 8<<20 - 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := s3fake.Start(t, "files")
			link := pacedRelay(t, srv.Listener.Addr().String(), 1<<20)
			cfg := fakeConfig(srv, "")
			cfg.Endpoint = "http://" + link
			b, err := s3obj.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			s3obj.SetStall(b, caseStall)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			began := time.Now()
			if err := b.Put(ctx, "slow", bytes.NewReader(make([]byte, c.size)), objstore.PutMeta{}); err != nil {
				t.Fatalf("%d bytes across a link at a mebibyte a second = %v after %v; want them stored",
					c.size, err, time.Since(began))
			}
		})
	}
}

// A BUCKET READING AT THE FLOOR FROM THE START IS NEVER ENDED: one whose
// software reads a write slowly from its first byte never lets its kernel's
// buffer grow, so every byte it reads is one the node's socket sees
// acknowledged — and that, never the body's reads, is what shows the write
// moving once the whole of it sits in the node's send buffer.
func TestABucketReadingAtTheFloorFromTheStartIsNeverEnded(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	srv.BodyBytesPerSecond = 128 << 10
	b := openStalling(t, srv)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	began := time.Now()
	if err := b.Put(ctx, "slow", bytes.NewReader(make([]byte, 1<<20)), objstore.PutMeta{}); err != nil {
		t.Fatalf("a mebibyte the bucket read at 128 KiB a second = %v after %v; want it stored",
			err, time.Since(began))
	}
	if took := time.Since(began); took < 6*time.Second {
		t.Fatalf("a mebibyte the bucket read at 128 KiB a second was stored in %v; the bucket was not "+
			"slow", took)
	}
}

// THE BUCKET IS SPOKEN TO OVER HTTP/1.1, so a socket is one request's to
// watch: a gateway offering HTTP/2 — as one served by Go's own net/http over
// TLS does — is spoken to in HTTP/1.1 all the same, through AWS_CA_BUNDLE's
// client and through the one built with the system's roots alike.
func TestTheBucketIsSpokenToOverHTTP1(t *testing.T) {
	srv := s3fake.StartH2(t, "files")
	t.Setenv("AWS_CA_BUNDLE", trustBundle(t, srv))
	b := openFake(t, srv, "")
	if err := b.Put(t.Context(), "spoken", bytes.NewReader([]byte("over HTTP/1.1")), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Protocols(); !slices.Equal(got, []string{"HTTP/1.1"}) {
		t.Fatalf("the bucket was spoken to in %v; want HTTP/1.1 alone", got)
	}
	http1, http2, err := s3obj.BucketProtocols("")
	if err != nil {
		t.Fatal(err)
	}
	if !http1 || http2 {
		t.Fatalf("the bucket's client with the system's roots speaks HTTP/1.1 %v, HTTP/2 %v; want "+
			"HTTP/1.1 alone", http1, http2)
	}
}

// stallingBackend is the suite's stalling S3 backend: a bucket whose freeze
// stops it reading puts and parts — never the abort, whose own thirty seconds
// would be added to every case — at the case's stall bound.
func stallingBackend(t *testing.T) (objstore.Backend, time.Duration, time.Duration, func() func()) {
	srv := s3fake.Start(t, "files")
	b := open(t, srv, "crewlet/")
	s3obj.SetStall(b, caseStall)
	return b, caseStall, 500 * time.Millisecond, func() func() {
		releasePut, _ := srv.Hang("PutObject", 0)
		releasePart, _ := srv.Hang("UploadPart", 0)
		return func() { releasePut(); releasePart() }
	}
}

// pacedRelay listens on loopback and carries every connection to target, the
// bytes towards it at no more than rate a second for every connection
// together — a link at a pace of a case's choosing — until the case ends. It
// answers the address it listens on.
func pacedRelay(t *testing.T, target string, rate int64) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		due   time.Time
		conns []net.Conn
		shut  bool
	)
	track := func(c net.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		if shut {
			return false
		}
		conns = append(conns, c)
		return true
	}
	// paced copies src to dst a slice of a twentieth of a second at a time,
	// each slice sent once the shared pace allows.
	paced := func(dst io.Writer, src io.Reader) {
		buf := make([]byte, max(rate/20, 1<<10))
		for {
			n, err := src.Read(buf)
			if n > 0 {
				mu.Lock()
				due = later(due, time.Now()).Add(time.Duration(n) * time.Second / time.Duration(rate))
				at := due
				mu.Unlock()
				time.Sleep(time.Until(at))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Go(func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil || !track(in) || !track(out) {
				_ = in.Close()
				if out != nil {
					_ = out.Close()
				}
				continue
			}
			wg.Go(func() {
				done := make(chan struct{})
				go func() {
					_, _ = io.Copy(in, out)
					close(done)
				}()
				paced(out, in)
				_ = in.Close()
				_ = out.Close()
				<-done
			})
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		shut = true
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
