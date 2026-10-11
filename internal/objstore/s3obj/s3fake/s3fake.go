// Package s3fake is an in-process S3 endpoint for tests: the calls the object
// store makes — head bucket; put, get (whole or ranged), head and delete an
// object; delete up to a thousand at once; a paged ListObjectsV2, from a
// start-after or not; a multipart
// upload's start, parts, completion and abandonment; and a paged listing of
// the uploads begun and not finished — over one bucket, path-style, counting
// the requests of each kind it answers ([Server.RequestsOf]).
//
// It checks no signature and keeps no versions; it exists so the S3 backend
// runs the object store's conformance suite on a test machine with no network
// and no credentials, against the wire format the SDK actually speaks. A get
// and a head answer the object's ETag, and a get carrying If-Match is refused
// 412 PreconditionFailed once the object no longer carries that tag, as S3
// refuses it — so a reader that holds the later ranges of one read to the
// first one's tag is tested against a bucket that holds it to the tag.
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

	// Untagged answers every GetObject and HeadObject with no ETag, as no
	// S3 does — for a case to see what a reader makes of a bucket that
	// gives it nothing to hold an object to. Set it before the first
	// request.
	Untagged bool

	// requests is every request the bucket has been sent ([Server.Requests]).
	requests atomic.Int64

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
	sorted   []string
	uploads  map[string]*upload
	nextID   int
	failPart int32
	// held is the part [Server.HoldPart] holds, or nil.
	held *heldPart
	// undated is the keys whose listings leave the date out.
	undated map[string]bool
	// batchRefusal is what every DeleteObjects answers, while its status is
	// set ([Server.RefuseBatches]); refusedKeys the code a DeleteObjects
	// refuses each key it names with ([Server.RefuseDelete]).
	batchRefusal refusal
	refusedKeys  map[string]string
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
	t.Cleanup(s.Close)
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
	t.Cleanup(s.Close)
	return s
}

func newServer(bucket string) *Server {
	return &Server{bucket: bucket, PageSize: defaultPageSize,
		objects: map[string]object{}, uploads: map[string]*upload{}}
}

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
// "DeleteObject", "DeleteObjects", "ListObjectsV2", "HeadBucket" and the
// multipart calls — so a test can count what a pass cost.
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

// Parts is how many parts key was assembled from: zero for an object stored
// by one put, or one the bucket does not hold.
func (s *Server) Parts(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[key].parts
}

// FailPart makes every upload of part number n fail with a server error, for
// the test that a failed part abandons its upload; zero fails none.
func (s *Server) FailPart(n int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failPart = n
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
	switch {
	case key == "" && r.Method == http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case key == "" && r.Method == http.MethodGet && q.Get("list-type") == "2":
		s.list(w, r)
	case key == "" && r.Method == http.MethodGet && q.Has("uploads"):
		s.listUploads(w, r)
	case key == "" && r.Method == http.MethodPost && q.Has("delete"):
		s.deleteObjects(w, r)
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
		data, err := readBody(r)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "IncompleteBody")
			return
		}
		tag := etag(data)
		s.mu.Lock()
		s.objects[key] = object{data: data, modified: now(),
			contentType: r.Header.Get("Content-Type"), etag: tag}
		s.sorted = nil
		s.mu.Unlock()
		w.Header().Set("ETag", tag)
		w.WriteHeader(http.StatusOK)
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
	if s.failPart == int32(number) {
		s.fail(w, r, http.StatusInternalServerError, "InternalError")
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
		if code := s.refusedKeys[o.Key]; code != "" {
			out.Errors = append(out.Errors, deleteError{Key: o.Key, Code: code, Message: code})
			continue
		}
		delete(s.objects, o.Key)
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
