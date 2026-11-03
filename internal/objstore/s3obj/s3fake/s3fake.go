// Package s3fake is an in-process S3 endpoint for tests: the calls the object
// store makes — head bucket; put, get (whole or ranged), head and delete an
// object; delete up to a thousand at once; a paged ListObjectsV2, from a
// start-after or not; a multipart
// upload's start, parts, completion and abandonment; a paged listing of
// the uploads begun and not finished; and S3 Express's CreateSession for a
// directory bucket (`…--x-s3`), reached path-style — over one bucket,
// path-style, counting the requests of each kind it answers
// ([Server.RequestsOf]).
//
// It enforces what S3 itself enforces on a put and what the boot probe and a
// presigned upload rely on: an `x-amz-checksum-sha256` the body does not hash
// to is refused BadDigest, and `If-None-Match: *` on a key it holds is refused
// 412. [Server.IgnoreChecksums] and [Server.IgnoreConditionalPuts] make it a
// provider that silently does neither, and [Server.AllowOnly] a credential
// scoped to one prefix, which may not ask about the bucket itself and has a
// batch deletion judged key by key, as S3 authorises one.
//
// It checks no signature and keeps no versions; it exists so the S3 backend
// runs the object store's conformance suite on a test machine with no network
// and no credentials, against the wire format the SDK actually speaks. A get
// and a head answer the object's ETag, and a get carrying If-Match is refused
// 412 PreconditionFailed once the object no longer carries that tag, as S3
// refuses it — so a reader that holds the later ranges of one read to the
// first one's tag is tested against a bucket that holds it to the tag.
//
// It REFUSES WHAT A CASE ASKS IT TO, with the S3 error the case names: the next
// few requests whatever they are ([Server.FailNext]), or every request for a
// span of time ([Server.FailFor]) — a bucket that throttles or fails for a
// while and recovers — every upload of one part ([Server.FailPart]), every
// session a directory bucket's client asks for ([Server.RefuseSessions]), or
// every request that would store bytes while the bucket has no room
// ([Server.Fill]) — a provider's drives at their floor, a quota spent. And
// it STOPS what a case asks it to: every request of one kind taking only so
// much of its body and then nothing, with no answer ([Server.Hang]), or
// answering its headers and nothing after ([Server.HangAnswer]) — a bucket
// that hung — or reading a request's body at a pace of its own
// ([Server.BodyBytesPerSecond]), fast at first if a case says so
// ([Server.BodyFastBytes]): a bucket whose software reads slower than its
// kernel took the bytes.
//
// It does its OWN WORK AT MEMORY SPEED — a body read into one buffer of its
// declared length, an entity tag computed once per object or part by a
// hardware-accelerated checksum, an assembly sized before it is copied into, a
// listing paged from a sorted index rather than by sorting every key a page —
// so a benchmark run through it measures the node's side of a bucket rather
// than the fake's arithmetic. And it can be made as FAR AWAY as a provider
// ([Server.Latency]) and as NARROW as one ([Server.ConnBytesPerSecond]):
// loopback answers in microseconds and at gigabytes a second, and what bounds
// a node's requests to a real bucket is the round trip and what one
// connection carries, neither of which a measurement of the node's side alone
// ever shows.
//
// # It routes on the query before the method
//
// A multipart upload is spelled with the same methods as everything else — a
// part is a PUT of the key, a completion a POST — and told apart only by its
// query (`?uploads`, `?partNumber=&uploadId=`, `?uploadId=`). A fake that
// routed on the method would store each part as the whole object and pass
// every multipart test while testing none of it, so the query is read first.
//
// # And the real bucket beside it
//
// A benchmark that measures a path through S3 measures it here AND, when the
// CREWLET_BENCH_S3_* variables name one, against a real bucket — the figure
// the fake can only model. [RealBucket] is where those variables are read,
// once for every such benchmark: a second parser of them is a benchmark that
// reads a region, a prefix or a path style differently from the first and
// points its measurement at a bucket nobody meant, so this package's tests
// fail on any other package that spells their names or the stem one is built
// from.
package s3fake

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// defaultPageSize is how many keys one listing page holds when PageSize is
// not positive: small, so a test of a few dozen objects crosses pages.
const defaultPageSize = 7

// Server is one fake bucket.
type Server struct {
	*httptest.Server
	bucket string
	// PageSize is how many keys one listing page holds; zero or less is
	// [defaultPageSize]. Set it before the first request: a benchmark sets
	// S3's own thousand, where a test of a few dozen objects needs a small
	// page to cross pages at all.
	PageSize int

	// Latency is how long the bucket waits before it answers each request —
	// a provider's round trip and first byte, of which loopback has none.
	// Zero answers at once. Set it before the first request.
	//
	// IT CHARGES NO BANDWIDTH: a body moves at loopback's rate once the wait
	// is over, so what it adds is what a round trip costs each request — a
	// multipart upload's every part, a listing's every page, a ranged read's
	// first byte — and nothing a link's throughput would.
	Latency time.Duration

	// ConnBytesPerSecond is the most bytes a second the bucket sends an
	// object's bytes at — the body of every GetObject — once
	// [Server.Latency] is waited out: a provider's ceiling on what one
	// connection carries, about 80–90 MB/s from S3. Zero or less sends at
	// loopback's rate. Set it before the first request.
	//
	// THE TERM [Server.Latency] DOES NOT MODEL: a round trip is paid once a
	// request whatever its size, and a connection's ceiling by every byte
	// of it — which is what decides whether a large read is faster in
	// several ranges at once than in one stream, and a fake answering at
	// loopback's rate could never show.
	//
	// AN OBJECT'S BYTES ONLY, never the few hundred bytes of XML another
	// verb answers, which cross any real link in microseconds at any
	// ceiling: paced at the slow rates a case sets to hold a range in
	// flight, a multipart completion's answer took tens of milliseconds to
	// arrive, and that is long enough for the SDK's early close of the
	// request body to fail the transport's last read of it, which closes
	// the connection under the answer — a race no provider a network round
	// trip away leaves room for.
	ConnBytesPerSecond int64

	// GetHook, where set, is called with the key and the first byte of
	// every GetObject — zero for one with no range — once the bucket has it
	// in hand and before it reads the object to answer it, so a test can
	// hold a range back, or change the object under it, at a moment it
	// chooses. It is called without the bucket's lock, so it may send the
	// bucket requests of its own. Set it before the first request.
	GetHook func(key string, start int64)

	// PartHook, where set, is called with the number of every UploadPart
	// once the bucket has read its body and before it answers it — a part
	// [Server.FailPart] refuses or [Server.HoldPart] holds included — so a
	// test can hold a part's answer back until a moment it chooses. It is
	// called without the bucket's lock. Set it before the first request.
	PartHook func(number int32)

	// Untagged answers every GetObject and HeadObject with no ETag, as no
	// S3 does — for a case to see what a reader makes of a bucket that
	// gives it nothing to hold an object to. Set it before the first
	// request.
	Untagged bool

	// SessionLife is how long a session CreateSession hands out lasts; zero
	// is S3 Express's five minutes; negative hands out one already
	// expired, so every request a client sends asks for a new one — a
	// case's way to make a client need a session now. Set it before the
	// first request.
	//
	// The SDK asks for a session only for a DIRECTORY bucket — one whose
	// name ends in `--x-s3` — and signs that bucket's every request with
	// it; the fake checks no signature, so the session's credentials are
	// handed out and never looked at again.
	SessionLife time.Duration

	// BodyBytesPerSecond is the most bytes a second the bucket reads a
	// request's body at, once its first [Server.BodyFastBytes] are read —
	// each request's on its own, or every request's together where
	// [Server.BodyRateShared] is set. Zero or less reads at loopback's rate.
	// Set it before the first request.
	//
	// THE BUCKET'S SOFTWARE'S PACE, not the link's: the bytes it has not
	// read wait in its kernel's receive buffer and the node's send buffer,
	// which take far more than the software reads while it reads slowly —
	// so the node's socket sees them taken at once and the rest
	// acknowledged as the software reads, which is the case a write's watch
	// has to tell from a bucket that stopped.
	BodyBytesPerSecond int64
	// BodyFastBytes is how many of each request's body bytes the bucket reads
	// at once, before [Server.BodyBytesPerSecond] paces the rest: a bucket
	// that read fast at first and then slowed, the kernel having grown its
	// receive buffer while it read fast. Set it before the first request.
	BodyFastBytes int64
	// BodyRateShared paces every request's body together at
	// [Server.BodyBytesPerSecond] rather than each at its own: one link's
	// worth of the bucket's reading, shared by the parts of an upload. Set
	// it before the first request.
	BodyRateShared bool

	// closing is closed as the case ends, before the server is closed: what
	// every request the bucket holds ([Server.Hang], [Server.HangAnswer])
	// waits for at the latest, since httptest.Server's Close waits for every
	// handler to return.
	closing chan struct{}
	// bodyDue is when the bucket may next read a body's byte under a shared
	// pace ([Server.BodyRateShared]).
	bodyMu  sync.Mutex
	bodyDue time.Time

	// requests is every request the bucket has been sent ([Server.Requests]).
	requests atomic.Int64
	// sessions numbers the sessions CreateSession has handed out, so no two
	// carry the same credentials.
	sessions atomic.Int64

	// parts is the part uploads the bucket has in hand
	// ([Server.MostPartsAtOnce]), and gets its object gets
	// ([Server.GetsInHand], [Server.MostGetsAtOnce]).
	parts, gets inHand

	mu sync.Mutex
	// asked is how many requests of each kind the bucket has answered
	// ([Server.RequestsOf]).
	asked   map[string]int
	objects map[string]object
	// sorted is every key objects holds, in order, for a listing to page
	// through; nil once a write has changed the keys since it was built.
	//
	// BUILT AT MOST ONCE BETWEEN WRITES, where a listing used to collect and
	// sort every key past its marker for each page it answered: a hundred
	// thousand objects listed a thousand at a time sorted five million keys
	// a pass — the fake's cost, which a benchmark would report as the
	// backend's.
	sorted  []string
	uploads map[string]*upload
	nextID  int
	// failPart is the part number [Server.FailPart] refuses, and what with;
	// zero refuses none.
	failPart     int32
	failPartWith refusal
	// failNext is how many requests [Server.FailNext] still refuses, and
	// failNextWith what with.
	failNext     int
	failNextWith refusal
	// failUntil is when the span [Server.FailFor] refuses every request in
	// ends, and failForWith what with.
	failUntil   time.Time
	failForWith refusal
	// sessionRefusal is what every CreateSession answers while its status is
	// set ([Server.RefuseSessions]).
	sessionRefusal refusal
	// fullWith is what every request that would store bytes answers while
	// its status is set ([Server.Fill]).
	fullWith refusal
	// held is the part [Server.HoldPart] holds, or nil.
	held *heldPart
	// undated is the keys whose listings leave the date out.
	undated map[string]bool
	// batchRefusal is what every DeleteObjects answers, while its status is
	// set ([Server.RefuseBatches]); refusedKeys the code a DeleteObjects
	// refuses each key it names with ([Server.RefuseDelete]).
	batchRefusal refusal
	refusedKeys  map[string]string
	// hangs is the hold [Server.Hang] puts on each kind of request, and
	// answerHangs the kinds [Server.HangAnswer] answers headers alone to.
	hangs       map[string]*hang
	answerHangs map[string]bool
	// protocols counts the requests the bucket was sent in each protocol
	// ([Server.Protocols]).
	protocols map[string]int

	// ignoreChecksums and ignoreConditional make the bucket a provider that
	// stores whatever it is sent, whatever the checksum or the condition
	// says; allowOnly, when set, is the one prefix the credential reaches.
	ignoreChecksums   bool
	ignoreConditional bool
	allowOnly         string

	// putHeaders is the headers of the last plain put of each key.
	putHeaders map[string]http.Header
}

// hang is one kind of request [Server.Hang] holds: how much of each body it
// takes, how much it has taken in all, and the news that the case let it go.
type hang struct {
	after    int64
	took     atomic.Int64
	released chan struct{}
	once     sync.Once
}

// refusal is an error answer: a status and the code it carries.
type refusal struct {
	status int
	code   string
}

// heldPart is a part number whose every upload the bucket takes and never
// answers, and the news that a client gave up on one.
type heldPart struct {
	number  int32
	givenUp chan struct{}
	once    sync.Once
}

type object struct {
	data        []byte
	modified    time.Time
	contentType string
	// etag is the entity tag the object was stored with — what a get or a
	// head answers and an If-Match is compared with — kept rather than
	// worked out again from the bytes at every get.
	etag string
	// parts is how many parts the object was assembled from, zero for one
	// stored by a single put.
	parts int
	// checksum is the base64 SHA-256 the put carried and the bucket
	// checked, empty where it carried none.
	checksum string
}

type upload struct {
	key         string
	contentType string
	started     time.Time
	parts       map[int32]part
}

// part is one part of an upload as it arrived: its bytes, and the entity tag
// it was answered with — which a completion is checked against, so the tag is
// kept rather than worked out again from the bytes.
type part struct {
	data []byte
	etag string
}

// Start serves bucket over plain http until the test ends.
func Start(t testing.TB, bucket string) *Server {
	t.Helper()
	s := newServer(bucket)
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	s.untilTheEnd(t)
	return s
}

// StartTLS serves bucket over https until the test ends, on a certificate of
// the server's own that a client trusts through AWS_CA_BUNDLE.
//
// What real S3 is reached over — and the scheme on which the SDK leaves a
// request's body out of its signature (UNSIGNED-PAYLOAD), where over plain
// http it hashes every byte it sends to sign it. A measurement of the node's
// side of a bucket taken over http alone would be of a cost no deployment
// reaching a bucket over https pays.
func StartTLS(t testing.TB, bucket string) *Server {
	t.Helper()
	s := newServer(bucket)
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	s.untilTheEnd(t)
	return s
}

// StartH2 serves bucket over https OFFERING HTTP/2, as a gateway served by
// Go's own net/http over TLS does by default — for the case that a client
// which must speak HTTP/1.1 to its bucket does so where it is offered more
// ([Server.Protocols] says what it was spoken to in).
func StartH2(t testing.TB, bucket string) *Server {
	t.Helper()
	s := newServer(bucket)
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.EnableHTTP2 = true
	s.StartTLS()
	s.untilTheEnd(t)
	return s
}

// untilTheEnd closes the server as the case ends — every request it holds let
// go first, since closing waits for every handler to return.
func (s *Server) untilTheEnd(t testing.TB) {
	t.Cleanup(s.Close)
	t.Cleanup(func() { close(s.closing) })
}

func newServer(bucket string) *Server {
	return &Server{bucket: bucket, PageSize: defaultPageSize, closing: make(chan struct{}),
		objects: map[string]object{}, uploads: map[string]*upload{}}
}

// Protocols is every protocol the bucket was sent a request in — "HTTP/1.1",
// "HTTP/2.0" — sorted.
func (s *Server) Protocols() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.protocols))
}

// Hang makes every request of op from now on take at most after bytes of its
// body — all of it where after is negative — and then neither read nor answer
// until released, or the case ends: a bucket that took a write and stopped,
// whose kernel still takes what its buffers hold. It answers the release, and
// how many body bytes the held requests took in all.
//
// A HELD REQUEST IS LET GO BY THE CASE, NEVER BY ITS CLIENT: net/http notices a
// client going away only once a handler has read the request to its end
// ([Server.HoldPart]), which one holding part of a body never does. Let go, it
// is cut without an answer, and the requests of op after the release are
// answered as ever.
func (s *Server) Hang(op string, after int64) (release func(), took func() int64) {
	h := &hang{after: after, released: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hangs == nil {
		s.hangs = map[string]*hang{}
	}
	s.hangs[op] = h
	return func() {
		h.once.Do(func() { close(h.released) })
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.hangs[op] == h {
			delete(s.hangs, op)
		}
	}, h.took.Load
}

// HangAnswer makes every request of op from now on be read whole and answered
// its headers — `200 OK` — and then nothing more until its client gives up or
// the case ends: a bucket that began an answer and stopped, as a completion
// assembling for minutes would look without the keep-alive whitespace Amazon
// sends. The request does nothing else: a completion held so assembles no
// object.
func (s *Server) HangAnswer(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answerHangs == nil {
		s.answerHangs = map[string]bool{}
	}
	s.answerHangs[op] = true
}

// hold holds a request of op that [Server.Hang] or [Server.HangAnswer] holds
// and then cuts it, with no answer past what it was held with — never
// returning — and returns at once for any other request.
func (s *Server) hold(w http.ResponseWriter, r *http.Request, op string) {
	s.mu.Lock()
	h, answer := s.hangs[op], s.answerHangs[op]
	s.mu.Unlock()
	switch {
	case h != nil:
		body := io.Reader(r.Body)
		if h.after >= 0 {
			body = io.LimitReader(r.Body, h.after)
		}
		n, _ := io.Copy(io.Discard, body)
		h.took.Add(n)
		select {
		case <-h.released:
		case <-s.closing:
		case <-r.Context().Done():
		}
	case answer:
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-s.closing:
		case <-r.Context().Done():
		}
	default:
		return
	}
	panic(http.ErrAbortHandler)
}

// pacedBody is a request's body as a bucket reading it at
// [Server.BodyBytesPerSecond] reads it: its first [Server.BodyFastBytes] at
// once, the rest a small piece at a time, each read only once the pace allows.
type pacedBody struct {
	io.ReadCloser
	s    *Server
	read int64
	due  time.Time
}

func (b *pacedBody) Read(p []byte) (int, error) {
	s := b.s
	if b.read < s.BodyFastBytes {
		n, err := b.ReadCloser.Read(p[:min(int64(len(p)), s.BodyFastBytes-b.read)])
		b.read += int64(n)
		return n, err
	}
	// A FIFTIETH OF A SECOND'S WORTH AT A TIME, a kibibyte at the least,
	// so the pace is even whatever rate a case sets.
	piece := min(int64(len(p)), max(s.BodyBytesPerSecond/50, 1<<10))
	n, err := b.ReadCloser.Read(p[:piece])
	b.read += int64(n)
	if n == 0 {
		return n, err
	}
	took := time.Duration(float64(n) / float64(s.BodyBytesPerSecond) * float64(time.Second))
	var due time.Time
	if s.BodyRateShared {
		s.bodyMu.Lock()
		s.bodyDue = later(s.bodyDue, time.Now()).Add(took)
		due = s.bodyDue
		s.bodyMu.Unlock()
	} else {
		b.due = later(b.due, time.Now()).Add(took)
		due = b.due
	}
	t := time.NewTimer(time.Until(due))
	defer t.Stop()
	select {
	case <-t.C:
	case <-s.closing:
	}
	return n, err
}

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Bucket is the name of the bucket the fake serves, so a case configures a
// client from the server it started rather than from a second spelling of
// the name.
func (s *Server) Bucket() string { return s.bucket }

// Requests is how many requests the bucket has been sent since it started —
// every verb, a refused one included — which is what a provider's round trip
// multiplies and what it bills by.
func (s *Server) Requests() int64 { return s.requests.Load() }

// MostPartsAtOnce is the most part uploads the bucket has had in hand at once
// — sent and not yet answered — since it started: how many parts of an upload
// a client keeps in flight, which only the bucket's side of the wire can see.
func (s *Server) MostPartsAtOnce() int64 { return s.parts.most.Load() }

// GetsInHand is how many object gets the bucket has in hand now: sent, and
// neither answered to their last byte nor given up on by their client.
func (s *Server) GetsInHand() int64 { return s.gets.now.Load() }

// MostGetsAtOnce is the most object gets the bucket has had in hand at once
// since it started: how many ranges of a read a client keeps in flight, which
// only the bucket's side of the wire can see.
func (s *Server) MostGetsAtOnce() int64 { return s.gets.most.Load() }

// inHand counts the requests of one kind the bucket holds — arrived, and not
// yet answered — and the most it has held at once.
type inHand struct{ now, most atomic.Int64 }

// arrive counts a request in hand, answering the call that counts it
// answered.
func (h *inHand) arrive() func() {
	now := h.now.Add(1)
	for most := h.most.Load(); now > most && !h.most.CompareAndSwap(most, now); {
		most = h.most.Load()
	}
	return func() { h.now.Add(-1) }
}

// RequestsOf is how many requests of one kind the bucket has answered — the
// operation's name in the API: "HeadObject", "GetObject", "PutObject",
// "DeleteObject", "DeleteObjects", "ListObjectsV2", "HeadBucket",
// "CreateSession" and the multipart calls — so a test can count what a pass
// cost.
func (s *Server) RequestsOf(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked[op]
}

// count records one request of op.
func (s *Server) count(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.asked == nil {
		s.asked = map[string]int{}
	}
	s.asked[op]++
}

// Keys is every key the bucket holds, sorted.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.objects))
}

// Put stores data under key exactly as one completed put of it would — its
// entity tag, the current time, no content type — for a suite that fills a
// bucket from outside the object store: copying a backup's `objects/` into a
// fresh bucket, as a restore's `aws s3 sync` does (docs/guides/backup.md).
//
// THE BYTES ARE COPIED, so a caller reusing its buffer for the next file
// cannot rewrite the object it already stored.
func (s *Server) Put(key string, data []byte) {
	stored := slices.Clone(data)
	tag := etag(stored)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = object{data: stored, modified: now(), etag: tag}
	s.sorted = nil
}

// Uploads is the key of every multipart upload started and neither completed
// nor abandoned, sorted — what a bucket bills for and no listing shows.
func (s *Server) Uploads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for _, u := range s.uploads {
		keys = append(keys, u.key)
	}
	slices.Sort(keys)
	return keys
}

// Begin starts a multipart upload of key holding one part, and never finishes
// it — what a process killed mid-upload leaves behind — answering its upload
// id.
func (s *Server) Begin(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("upload-%d", s.nextID)
	// THE PART CARRIES THE TAG IT WAS ANSWERED WITH, as every part
	// [Server.putPart] stores does: a completion is checked against the
	// tag held, so a part held with none would take a completion naming an
	// empty tag, which S3 refuses.
	data := []byte("a part of an upload nobody finished")
	s.uploads[id] = &upload{key: key, started: now(),
		parts: map[int32]part{1: {data: data, etag: etag(data)}}}
	return id
}

// Undated makes the bucket's listings leave out the date of key — the upload
// key begins from here on, if it is one, and the object key holds, if it is
// one — as an S3-compatible gateway that omits <Initiated> or <LastModified>
// does. A test reads what the backend makes of an entry nobody dated.
func (s *Server) Undated(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.undated == nil {
		s.undated = map[string]bool{}
	}
	s.undated[key] = true
}

// stamp spells t as a listing does, or nothing for a key [Server.Undated]
// names. The caller holds mu.
func (s *Server) stamp(key string, t time.Time) string {
	if s.undated[key] {
		return ""
	}
	return t.Format("2006-01-02T15:04:05.000Z")
}

// IgnoreChecksums makes the bucket store a put whatever its
// x-amz-checksum-sha256 says, as a provider that does not check one does.
func (s *Server) IgnoreChecksums() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ignoreChecksums = true
}

// IgnoreConditionalPuts makes the bucket overwrite a key whatever the put's
// If-None-Match says, as a provider without conditional writes does.
func (s *Server) IgnoreConditionalPuts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ignoreConditional = true
}

// AllowOnly makes the credential reach only keys under prefix: every request
// for another key, and every request about the bucket itself (a HeadBucket, a
// listing outside the prefix), is refused AccessDenied, as an identity scoped
// to one company's prefix is. A batch deletion is the one request about the
// bucket it is let through, because S3 authorises a DeleteObjects key by key
// (s3:DeleteObject on each): each key outside the prefix is refused within
// the answer, and the keys under it are deleted.
func (s *Server) AllowOnly(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowOnly = prefix
}

// PutHeaders is the headers of the last single put of key, nil for one never
// put that way.
func (s *Server) PutHeaders(key string) http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putHeaders[key].Clone()
}

// Checksum is the base64 SHA-256 the bucket checked key's put against, empty
// where the put carried none.
func (s *Server) Checksum(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[key].checksum
}

// denied reports a request the credential does not reach. The caller does
// not hold mu.
func (s *Server) denied(key string, q map[string][]string) bool {
	s.mu.Lock()
	prefix := s.allowOnly
	s.mu.Unlock()
	if prefix == "" {
		return false
	}
	if key != "" {
		return !strings.HasPrefix(key, prefix)
	}
	if _, batch := q["delete"]; batch {
		// KEY BY KEY, in [Server.deleteObjects]: see [Server.AllowOnly].
		return false
	}
	// A BUCKET-LEVEL REQUEST: a listing confined to the prefix is a
	// right such a credential has; anything else about the bucket is not.
	listed := first(q["prefix"])
	return first(q["list-type"]) != "2" && len(q["uploads"]) == 0 ||
		!strings.HasPrefix(listed, prefix)
}

// Parts is how many parts key was assembled from: zero for an object stored
// by one put, or one the bucket does not hold.
func (s *Server) Parts(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[key].parts
}

// FailPart makes every upload of part number n answer the S3 error status
// and code — for the test that a refused part abandons its upload; zero
// refuses none.
//
// A REFUSAL THE CLIENT TAKES AS FINAL is what such a test wants: a client error
// such as `403 AccessDenied`. A throttle is one the client asks again until its
// deadline, and a server error one it asks again for up to a minute before it
// gives the request up, so a part refused with either for good is a part that
// fails only that much later.
func (s *Server) FailPart(n int32, status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failPart, s.failPartWith = n, refusal{status: status, code: code}
}

// FailNext makes the next n requests the bucket is sent — whatever they are —
// answer the S3 error status and code before anything else is made of them,
// and the requests after them be answered as ever: a bucket that throttles,
// or fails, for a while and then recovers.
//
// A refused request is still a request: [Server.Requests] and
// [Server.RequestsOf] count it, and it waits out [Server.Latency] like any
// other, a provider's refusal costing its round trip as its answer does.
func (s *Server) FailNext(n int, status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext, s.failNextWith = n, refusal{status: status, code: code}
}

// FailFor makes every request the bucket is sent for d from now — whatever it
// is — answer the S3 error status and code before anything else is made of
// it: a bucket that fails for a while, measured in time rather than in
// requests, since how many requests a client sends in that time is the
// backoff's to decide. It is asked before [Server.FailNext]'s count, which a
// request it refuses does not spend.
func (s *Server) FailFor(d time.Duration, status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failUntil, s.failForWith = time.Now().Add(d), refusal{status: status, code: code}
}

// RefuseSessions makes every CreateSession answer the S3 error status and
// code — a directory bucket whose session endpoint fails while its objects
// would answer — until it is called with a status of zero.
func (s *Server) RefuseSessions(status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionRefusal = refusal{status: status, code: code}
}

// Fill makes the bucket one with no room left: every request that would store
// bytes — a PutObject, an UploadPart, a CompleteMultipartUpload — answers the
// S3 error status and code, its body read first and nothing stored, while
// every other request is answered as ever, as a provider refuses writes and
// still serves reads, deletions and abandonments: MinIO at its drives'
// free-space floor (`507 XMinioStorageFull`) or past a bucket's quota (`400
// XMinioAdminBucketQuotaExceeded`), Ceph's gateway out of space (`507
// InsufficientCapacity`) or past a quota (`403 QuotaExceeded`). A status of
// zero makes room again.
//
// AN UPLOAD STARTS ON A FULL BUCKET, because its start stores nothing — so a
// case that fills the bucket reaches the parts and the completion, and one
// that refuses the start does so with [Server.FailNext].
func (s *Server) Fill(status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fullWith = refusal{status: status, code: code}
}

// refuseFull answers true, having answered the request with the refusal, for a
// request op that would store bytes on a bucket [Server.Fill] filled — its
// body read first, so the client hears the refusal rather than a connection
// closed under a body it was still sending.
func (s *Server) refuseFull(w http.ResponseWriter, r *http.Request, op string) bool {
	switch op {
	case "PutObject", "UploadPart", "CompleteMultipartUpload":
	default:
		return false
	}
	s.mu.Lock()
	with := s.fullWith
	s.mu.Unlock()
	if with.status == 0 {
		return false
	}
	_, _ = io.Copy(io.Discard, r.Body)
	s.fail(w, r, with.status, with.code)
	return true
}

// refuseNext answers true, having answered the request with the refusal, for a
// request [Server.FailFor] or [Server.FailNext] still refuses.
//
// ITS BODY IS READ FIRST, so the client hears the refusal rather than a
// connection the server closed under a body it was still sending.
func (s *Server) refuseNext(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	var (
		refuse bool
		with   refusal
	)
	switch {
	case time.Now().Before(s.failUntil):
		refuse, with = true, s.failForWith
	case s.failNext > 0:
		refuse, with = true, s.failNextWith
		s.failNext--
	}
	s.mu.Unlock()
	if !refuse {
		return false
	}
	_, _ = io.Copy(io.Discard, r.Body)
	s.fail(w, r, with.status, with.code)
	return true
}

// HoldPart makes every upload of part number n wait, its body read and no
// answer sent, until its client gives up on it — a bucket that took a part's
// bytes and never answered — and answers a channel closed once a client
// first has. Zero holds none.
//
// The giving up is THE CLIENT'S OWN, seen from the far side of the wire: a
// test that holds a part learns from it the moment the upload the part
// belongs to cancelled its requests, which nothing on the client's side
// exposes.
func (s *Server) HoldPart(n int32) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n == 0 {
		s.held = nil
		return nil
	}
	s.held = &heldPart{number: n, givenUp: make(chan struct{})}
	return s.held.givenUp
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	q := r.URL.Query()
	op := operation(r.Method, key, q)
	// IN HAND FROM ITS ARRIVAL TO THE LAST BYTE OF ITS ANSWER, the latency
	// included: what a client keeps in flight is what the bucket holds.
	switch op {
	case "UploadPart":
		defer s.parts.arrive()()
	case "GetObject":
		defer s.gets.arrive()()
	}
	if !s.wait(r) {
		return
	}
	if op == "GetObject" && s.ConnBytesPerSecond > 0 {
		w = &pacedWriter{ResponseWriter: w, ctx: r.Context(), rate: s.ConnBytesPerSecond}
	}
	if bucket != s.bucket {
		s.fail(w, r, http.StatusNotFound, "NoSuchBucket")
		return
	}
	s.count(op)
	s.mu.Lock()
	if s.protocols == nil {
		s.protocols = map[string]int{}
	}
	s.protocols[r.Proto]++
	s.mu.Unlock()
	if s.BodyBytesPerSecond > 0 {
		r.Body = &pacedBody{ReadCloser: r.Body, s: s}
	}
	s.hold(w, r, op)
	if s.refuseNext(w, r) {
		return
	}
	if s.denied(key, q) {
		s.fail(w, r, http.StatusForbidden, "AccessDenied")
		return
	}
	if s.refuseFull(w, r, op) {
		return
	}
	switch {
	case key == "" && r.Method == http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case key == "" && r.Method == http.MethodGet && q.Get("list-type") == "2":
		s.list(w, r)
	case key == "" && r.Method == http.MethodGet && q.Has("uploads"):
		s.listUploads(w, r)
	case key == "" && r.Method == http.MethodPost && q.Has("delete"):
		s.deleteObjects(w, r)
	case key == "" && r.Method == http.MethodGet && q.Has("session"):
		s.createSession(w, r)
	case key == "":
		s.fail(w, r, http.StatusNotImplemented, "NotImplemented")

	// THE QUERY FIRST — see the package doc.
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.startUpload(w, r, key)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		s.putPart(w, r, q)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		s.completeUpload(w, r, key, q.Get("uploadId"))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		s.abortUpload(w, r, q.Get("uploadId"))

	case r.Method == http.MethodPut:
		s.put(w, r, key)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		s.get(w, r, key)
	case r.Method == http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, key)
		s.sorted = nil
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s.fail(w, r, http.StatusNotImplemented, "NotImplemented")
	}
}

// wait holds a request for [Server.Latency] before it is answered, and
// answers false — the request then answered nothing at all — for one whose
// client gave up meanwhile.
func (s *Server) wait(r *http.Request) bool {
	if s.Latency <= 0 {
		return true
	}
	t := time.NewTimer(s.Latency)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

// pacedWriter writes a get's body no faster than rate bytes a second,
// measured from its first byte ([Server.ConnBytesPerSecond]), and stops
// writing once its client has given up on it.
type pacedWriter struct {
	http.ResponseWriter
	ctx   context.Context
	rate  int64
	start time.Time
	sent  int64
}

func (p *pacedWriter) Write(b []byte) (int, error) {
	if p.start.IsZero() {
		p.start = time.Now()
	}
	// FIVE MILLISECONDS' WORTH AT A TIME, so the pace is even at whatever
	// rate a case sets — a kibibyte at the least, so a slow rate is not a
	// write of a few bytes, and a quarter of a mebibyte at the most — and
	// each flushed, so the pace is the wire's rather than a buffer's.
	piece := int(min(max(p.rate/200, 1<<10), 256<<10))
	written := 0
	for len(b) > 0 {
		n, err := p.ResponseWriter.Write(b[:min(len(b), piece)])
		written += n
		p.sent += int64(n)
		b = b[n:]
		if err != nil {
			return written, err
		}
		if f, ok := p.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		due := p.start.Add(time.Duration(float64(p.sent) / float64(p.rate) * float64(time.Second)))
		if wait := time.Until(due); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-p.ctx.Done():
				t.Stop()
				return written, p.ctx.Err()
			}
		}
	}
	return written, nil
}

// operation names a request as the API does, reading the query first for the
// same reason [Server.serve] routes on it.
func operation(method, key string, q map[string][]string) string {
	has := func(k string) bool { _, ok := q[k]; return ok }
	switch {
	case key == "" && method == http.MethodHead:
		return "HeadBucket"
	case key == "" && has("uploads"):
		return "ListMultipartUploads"
	case key == "" && method == http.MethodPost && has("delete"):
		return "DeleteObjects"
	case key == "" && method == http.MethodGet && has("session"):
		return "CreateSession"
	case key == "":
		return "ListObjectsV2"
	case method == http.MethodPost && has("uploads"):
		return "CreateMultipartUpload"
	case method == http.MethodPut && has("uploadId"):
		return "UploadPart"
	case method == http.MethodPost && has("uploadId"):
		return "CompleteMultipartUpload"
	case method == http.MethodDelete && has("uploadId"):
		return "AbortMultipartUpload"
	}
	return map[string]string{http.MethodPut: "PutObject", http.MethodGet: "GetObject",
		http.MethodHead: "HeadObject", http.MethodDelete: "DeleteObject"}[method]
}

// put stores a single put's body, refusing it as S3 does where its checksum
// does not match the body or its If-None-Match condition finds the key held.
func (s *Server) put(w http.ResponseWriter, r *http.Request, key string) {
	data, err := readBody(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "IncompleteBody")
		return
	}
	declared := r.Header.Get("X-Amz-Checksum-Sha256")
	sum := sha256.Sum256(data)
	actual := base64.StdEncoding.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if declared != "" && declared != actual && !s.ignoreChecksums {
		s.fail(w, r, http.StatusBadRequest, "BadDigest")
		return
	}
	if _, held := s.objects[key]; held && r.Header.Get("If-None-Match") == "*" && !s.ignoreConditional {
		s.fail(w, r, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}
	checked := ""
	if declared != "" && !s.ignoreChecksums {
		checked = declared
	}
	tag := etag(data)
	s.objects[key] = object{data: data, modified: now(),
		contentType: r.Header.Get("Content-Type"), etag: tag, checksum: checked}
	s.sorted = nil
	if s.putHeaders == nil {
		s.putHeaders = map[string]http.Header{}
	}
	s.putHeaders[key] = r.Header.Clone()
	w.Header().Set("ETag", tag)
	w.WriteHeader(http.StatusOK)
}

// get answers an object, or the range of it a Range header asks for — and
// refuses ANY range that begins at or past the end, an empty object's every
// range included, as S3 does. An If-Match naming another tag than the
// object's is refused before the range is read, as S3 judges a precondition
// before a range.
func (s *Server) get(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method == http.MethodGet && s.GetHook != nil {
		s.GetHook(key, rangeStart(r.Header.Get("Range")))
	}
	s.mu.Lock()
	obj, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		s.fail(w, r, http.StatusNotFound, "NoSuchKey")
		return
	}
	if match := r.Header.Get("If-Match"); match != "" && match != "*" && match != obj.etag {
		s.fail(w, r, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}
	if !s.Untagged {
		w.Header().Set("ETag", obj.etag)
	}
	body, status := obj.data, http.StatusOK
	if spec := r.Header.Get("Range"); spec != "" {
		start, end, ok := parseRange(spec, int64(len(obj.data)))
		if !ok {
			s.fail(w, r, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
			return
		}
		body, status = obj.data[start:end], http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(obj.data)))
	}
	if obj.checksum != "" && r.Header.Get("X-Amz-Checksum-Mode") == "ENABLED" && status == http.StatusOK {
		w.Header().Set("X-Amz-Checksum-Sha256", obj.checksum)
		w.Header().Set("X-Amz-Checksum-Type", "FULL_OBJECT")
	}
	w.Header().Set("Last-Modified", obj.modified.Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	contentType := obj.contentType
	if contentType == "" {
		contentType = "binary/octet-stream" // what S3 itself answers for an object stored with none
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// rangeStart is the first byte a Range header asks for: zero for no header, or
// for one that does not parse, which the get then refuses on its own.
func rangeStart(spec string) int64 {
	from, _, _ := strings.Cut(strings.TrimPrefix(spec, "bytes="), "-")
	start, err := strconv.ParseInt(from, 10, 64)
	if err != nil {
		return 0
	}
	return start
}

// parseRange reads `bytes=a-b` or `bytes=a-` against an object of size bytes,
// answering the half-open [start, end) it covers, or false for one that is
// not satisfiable.
func parseRange(spec string, size int64) (start, end int64, ok bool) {
	from, to, found := strings.Cut(strings.TrimPrefix(spec, "bytes="), "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(from, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end = size
	if to != "" {
		last, err := strconv.ParseInt(to, 10, 64)
		if err != nil || last < start {
			return 0, 0, false
		}
		end = min(last+1, size)
	}
	return start, end, true
}

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

func (s *Server) startUpload(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("upload-%d", s.nextID)
	s.uploads[id] = &upload{key: key, contentType: r.Header.Get("Content-Type"),
		started: now(), parts: map[int32]part{}}
	s.mu.Unlock()
	s.writeXML(w, initiateResult{Bucket: s.bucket, Key: key, UploadID: id})
}

func (s *Server) putPart(w http.ResponseWriter, r *http.Request, q map[string][]string) {
	id := first(q["uploadId"])
	number, err := strconv.ParseInt(first(q["partNumber"]), 10, 32)
	if err != nil || number < 1 {
		s.fail(w, r, http.StatusBadRequest, "InvalidArgument")
		return
	}
	data, err := readBody(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "IncompleteBody")
		return
	}
	if s.PartHook != nil {
		s.PartHook(int32(number))
	}
	s.mu.Lock()
	held := s.held
	s.mu.Unlock()
	if held != nil && held.number == int32(number) {
		// THE BODY IS READ FIRST, so the server is already watching the
		// connection for its client to close it: until a handler has read
		// the request to its end, net/http does not notice a client going
		// away, and this would wait for ever.
		<-r.Context().Done()
		held.once.Do(func() { close(held.givenUp) })
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPart != 0 && s.failPart == int32(number) {
		s.fail(w, r, s.failPartWith.status, s.failPartWith.code)
		return
	}
	u, ok := s.uploads[id]
	if !ok {
		s.fail(w, r, http.StatusNotFound, "NoSuchUpload")
		return
	}
	tag := etag(data)
	u.parts[int32(number)] = part{data: data, etag: tag}
	w.Header().Set("ETag", tag)
	w.WriteHeader(http.StatusOK)
}

type completeRequest struct {
	Parts []struct {
		ETag       string `xml:"ETag"`
		PartNumber int32  `xml:"PartNumber"`
	} `xml:"Part"`
}

type completeResult struct {
	XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
	Bucket  string   `xml:"Bucket"`
	Key     string   `xml:"Key"`
	ETag    string   `xml:"ETag"`
}

// completeUpload assembles the parts the request names, in its order, each
// checked against the tag the part stored under its number was answered with.
//
// THE OBJECT'S LastModified IS WHEN ITS UPLOAD STARTED, as Amazon documents
// for an object assembled from parts — not when it was completed.
func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, key, id string) {
	var req completeRequest
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, r, http.StatusBadRequest, "MalformedXML")
		return
	}
	s.mu.Lock()
	u, ok := s.uploads[id]
	if !ok || u.key != key {
		s.mu.Unlock()
		s.fail(w, r, http.StatusNotFound, "NoSuchUpload")
		return
	}
	size := 0
	for i, p := range req.Parts {
		held, ok := u.parts[p.PartNumber]
		if !ok || p.ETag != held.etag || (i > 0 && p.PartNumber <= req.Parts[i-1].PartNumber) {
			s.mu.Unlock()
			s.fail(w, r, http.StatusBadRequest, "InvalidPart")
			return
		}
		size += len(held.data)
	}
	// SIZED ONCE AND COPIED INTO, where appending part after part grew the
	// object through a reallocation and a copy of everything so far at
	// each doubling.
	data := make([]byte, 0, size)
	for _, p := range req.Parts {
		data = append(data, u.parts[p.PartNumber].data...)
	}
	tag := etag(data)
	s.objects[key] = object{data: data, modified: u.started,
		contentType: u.contentType, parts: len(req.Parts), etag: tag}
	s.sorted = nil
	delete(s.uploads, id)
	s.mu.Unlock()
	s.writeXML(w, completeResult{Bucket: s.bucket, Key: key, ETag: tag})
}

// maxDeleteKeys is the most keys one DeleteObjects takes.
const maxDeleteKeys = 1000

type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
	Quiet bool `xml:"Quiet"`
}

type deleteResult struct {
	XMLName xml.Name      `xml:"DeleteResult"`
	Deleted []deletedKey  `xml:"Deleted"`
	Errors  []deleteError `xml:"Error"`
}

type deletedKey struct {
	Key string `xml:"Key"`
}

type deleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// deleteObjects removes up to a thousand keys, as DeleteObjects does: each
// that is not there answered deleted, each [Server.RefuseDelete] names refused
// by its code, and a quiet request answered with its refusals alone. It
// requires the body's Content-MD5, as the API long did and S3-compatible
// gateways still do, so a client that sends none is refused here too.
func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	refused := s.batchRefusal
	s.mu.Unlock()
	if refused.status != 0 {
		s.fail(w, r, refused.status, refused.code)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "IncompleteBody")
		return
	}
	sum := md5.Sum(body)
	switch got := r.Header.Get("Content-MD5"); {
	case got == "":
		s.fail(w, r, http.StatusBadRequest, "MissingContentMD5")
		return
	case got != base64.StdEncoding.EncodeToString(sum[:]):
		s.fail(w, r, http.StatusBadRequest, "BadDigest")
		return
	}
	var req deleteRequest
	if err := xml.Unmarshal(body, &req); err != nil || len(req.Objects) > maxDeleteKeys {
		s.fail(w, r, http.StatusBadRequest, "MalformedXML")
		return
	}
	var out deleteResult
	s.mu.Lock()
	for _, o := range req.Objects {
		if s.allowOnly != "" && !strings.HasPrefix(o.Key, s.allowOnly) {
			out.Errors = append(out.Errors, deleteError{Key: o.Key, Code: "AccessDenied", Message: "AccessDenied"})
			continue
		}
		if code := s.refusedKeys[o.Key]; code != "" {
			out.Errors = append(out.Errors, deleteError{Key: o.Key, Code: code, Message: code})
			continue
		}
		if _, ok := s.objects[o.Key]; ok {
			// THE LISTING'S INDEX FORGETS IT TOO, as every other write
			// that changes the keys makes it: built before this request,
			// it would go on naming a key no read can find.
			delete(s.objects, o.Key)
			s.sorted = nil
		}
		if !req.Quiet {
			out.Deleted = append(out.Deleted, deletedKey{Key: o.Key})
		}
	}
	s.mu.Unlock()
	s.writeXML(w, out)
}

// RefuseBatches makes every DeleteObjects answer status with code, removing
// nothing — what a gateway that does not offer the verb answers; a status of
// zero offers it again.
func (s *Server) RefuseBatches(status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchRefusal = refusal{status: status, code: code}
}

// RefuseDelete makes a DeleteObjects that names key refuse it with code,
// deleting the rest — what S3 answers a key it would not remove; an empty code
// removes it again.
func (s *Server) RefuseDelete(key, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refusedKeys == nil {
		s.refusedKeys = map[string]string{}
	}
	if code == "" {
		delete(s.refusedKeys, key)
		return
	}
	s.refusedKeys[key] = code
}

func (s *Server) abortUpload(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	_, ok := s.uploads[id]
	delete(s.uploads, id)
	s.mu.Unlock()
	if !ok {
		s.fail(w, r, http.StatusNotFound, "NoSuchUpload")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sessionLife is how long a session lasts where [Server.SessionLife] is zero:
// S3 Express's own five minutes.
const sessionLife = 5 * time.Minute

type createSessionResult struct {
	XMLName     xml.Name           `xml:"CreateSessionResult"`
	Credentials sessionCredentials `xml:"Credentials"`
}

type sessionCredentials struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

// createSession answers S3 Express's CreateSession: credentials that last
// [Server.SessionLife] from now, a session nothing here checks again — or the
// refusal [Server.RefuseSessions] set.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	refused := s.sessionRefusal
	s.mu.Unlock()
	if refused.status != 0 {
		s.fail(w, r, refused.status, refused.code)
		return
	}
	life := s.SessionLife
	if life == 0 {
		life = sessionLife
	}
	id := s.sessions.Add(1)
	s.writeXML(w, createSessionResult{Credentials: sessionCredentials{
		AccessKeyID:     fmt.Sprintf("SESSIONKEY%d", id),
		SecretAccessKey: fmt.Sprintf("session-secret-%d", id),
		SessionToken:    fmt.Sprintf("session-token-%d", id),
		Expiration:      time.Now().Add(life).UTC().Format("2006-01-02T15:04:05Z"),
	}})
}

// now is the instant an object is dated by: to the second, as S3 keeps it —
// so a listing and a head, which spell it differently, agree on it.
func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// etag is a stand-in entity tag: quoted, as S3 sends one, and different for
// different bytes of different lengths, which is all a test needs of it.
//
// A CRC-32C, which the CPU computes at memory speed, where the byte-at-a-time
// arithmetic it replaced ran over every byte of every part at upload, again
// at completion and once more over the assembled object — a cost a benchmark
// through the fake would have reported as the backend's.
func etag(data []byte) string {
	return fmt.Sprintf(`"%d-%08x"`, len(data), crc32.Checksum(data, castagnoli))
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// readBody reads a request's body: into ONE buffer of its declared length
// where it declares one, as every request the SDK sends does, and grown as it
// arrives where it does not.
//
// io.ReadAll alone grows its buffer through a dozen reallocations and copies
// on the way to an eight-mebibyte part — the fake's cost, not the backend's.
// A body shorter than its declared length fails here, as it does at S3.
func readBody(r *http.Request) ([]byte, error) {
	if r.ContentLength < 0 {
		return io.ReadAll(r.Body)
	}
	data := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, data); err != nil {
		return nil, err
	}
	return data, nil
}

type listResult struct {
	XMLName               xml.Name   `xml:"ListBucketResult"`
	Name                  string     `xml:"Name"`
	Prefix                string     `xml:"Prefix"`
	KeyCount              int        `xml:"KeyCount"`
	MaxKeys               int        `xml:"MaxKeys"`
	IsTruncated           bool       `xml:"IsTruncated"`
	NextContinuationToken string     `xml:"NextContinuationToken,omitempty"`
	Contents              []listItem `xml:"Contents"`
}

type listItem struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified,omitempty"`
	Size         int    `xml:"Size"`
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// A CONTINUATION TOKEN WINS OVER start-after, as S3 has it: the token
	// carries the place the first page's start-after began from.
	prefix, after := q.Get("prefix"), q.Get("continuation-token")
	if after == "" {
		after = q.Get("start-after")
	}
	pageSize := s.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	s.mu.Lock()
	if s.sorted == nil {
		s.sorted = slices.Sorted(maps.Keys(s.objects))
	}
	keys := s.sorted
	// THE FIRST KEY PAST THE MARKER THAT CARRIES THE PREFIX: each half holds
	// of every key from some point of the sorted order on, so the two
	// together do too, and the page starts there.
	from := sort.Search(len(keys), func(i int) bool { return keys[i] > after && keys[i] >= prefix })
	out := listResult{Name: s.bucket, Prefix: prefix, MaxKeys: pageSize}
	for _, k := range keys[from:] {
		if !strings.HasPrefix(k, prefix) {
			break
		}
		if len(out.Contents) == pageSize {
			out.IsTruncated = true
			out.NextContinuationToken = out.Contents[len(out.Contents)-1].Key
			break
		}
		obj := s.objects[k]
		out.Contents = append(out.Contents, listItem{
			Key: k, Size: len(obj.data),
			LastModified: s.stamp(k, obj.modified),
		})
	}
	s.mu.Unlock()
	out.KeyCount = len(out.Contents)
	s.writeXML(w, out)
}

type uploadsResult struct {
	XMLName            xml.Name       `xml:"ListMultipartUploadsResult"`
	Bucket             string         `xml:"Bucket"`
	Prefix             string         `xml:"Prefix"`
	KeyMarker          string         `xml:"KeyMarker"`
	UploadIDMarker     string         `xml:"UploadIdMarker"`
	NextKeyMarker      string         `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string         `xml:"NextUploadIdMarker,omitempty"`
	MaxUploads         int            `xml:"MaxUploads"`
	IsTruncated        bool           `xml:"IsTruncated"`
	Uploads            []uploadedItem `xml:"Upload"`
}

type uploadedItem struct {
	Key       string `xml:"Key"`
	UploadID  string `xml:"UploadId"`
	Initiated string `xml:"Initiated,omitempty"`
}

// listUploads answers the uploads begun under a prefix and not finished, in
// (key, upload id) order, [Server.PageSize] to a page — paged by the two
// markers S3 pages them by, so a backend reading only the first page is
// caught.
func (s *Server) listUploads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix, afterKey, afterID := q.Get("prefix"), q.Get("key-marker"), q.Get("upload-id-marker")
	pageSize := s.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	type entry struct{ key, id string }
	s.mu.Lock()
	var all []entry
	started := map[string]string{}
	for id, u := range s.uploads {
		if !strings.HasPrefix(u.key, prefix) {
			continue
		}
		if afterKey != "" && (u.key < afterKey || (u.key == afterKey && id <= afterID)) {
			continue
		}
		all = append(all, entry{u.key, id})
		started[id] = s.stamp(u.key, u.started)
	}
	s.mu.Unlock()
	slices.SortFunc(all, func(a, b entry) int {
		if c := strings.Compare(a.key, b.key); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	out := uploadsResult{Bucket: s.bucket, Prefix: prefix, KeyMarker: afterKey,
		UploadIDMarker: afterID, MaxUploads: pageSize}
	for i, e := range all {
		if i == pageSize {
			last := out.Uploads[len(out.Uploads)-1]
			out.IsTruncated, out.NextKeyMarker, out.NextUploadIDMarker = true, last.Key, last.UploadID
			break
		}
		out.Uploads = append(out.Uploads, uploadedItem{Key: e.key, UploadID: e.id,
			Initiated: started[e.id]})
	}
	s.writeXML(w, out)
}

func (s *Server) writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = fmt.Fprintf(w, "%s<Error><Code>%s</Code><Message>%s</Message></Error>",
			xml.Header, code, code)
	}
}
