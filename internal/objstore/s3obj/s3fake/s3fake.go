// Package s3fake is an in-process S3 endpoint for tests: the calls the object
// store makes — head bucket; put, get (whole or ranged), head and delete an
// object; a paged ListObjectsV2; a multipart upload's start, parts,
// completion and abandonment; and a paged listing of the uploads begun and
// not finished — over one bucket, path-style.
//
// It checks no signature and keeps no versions; it exists so the S3 backend
// runs the object store's conformance suite on a test machine with no network
// and no credentials, against the wire format the SDK actually speaks.
//
// It does its OWN WORK AT MEMORY SPEED — a body read into one buffer of its
// declared length, an entity tag computed once per part by a
// hardware-accelerated checksum, an assembly sized before it is copied into, a
// listing paged from a sorted index rather than by sorting every key a page —
// so a benchmark run through it measures the node's side of a bucket rather
// than the fake's arithmetic. And it can be made as FAR AWAY as a provider
// ([Server.Latency]): loopback answers in microseconds, and what bounds a
// node's requests to a real bucket is the round trip, which a measurement of
// the node's side alone never shows.
//
// # It routes on the query before the method
//
// A multipart upload is spelled with the same methods as everything else — a
// part is a PUT of the key, a completion a POST — and told apart only by its
// query (`?uploads`, `?partNumber=&uploadId=`, `?uploadId=`). A fake that
// routed on the method would store each part as the whole object and pass
// every multipart test while testing none of it, so the query is read first.
package s3fake

import (
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

	// requests is every request the bucket has been sent ([Server.Requests]).
	requests atomic.Int64

	mu      sync.Mutex
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
	// undated is the keys whose listings leave the date out.
	undated map[string]bool
}

type object struct {
	data        []byte
	modified    time.Time
	contentType string
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

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	if !s.wait(r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != s.bucket {
		s.fail(w, r, http.StatusNotFound, "NoSuchBucket")
		return
	}
	q := r.URL.Query()
	switch {
	case key == "" && r.Method == http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case key == "" && r.Method == http.MethodGet && q.Get("list-type") == "2":
		s.list(w, r)
	case key == "" && r.Method == http.MethodGet && q.Has("uploads"):
		s.listUploads(w, r)
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
		s.mu.Lock()
		s.objects[key] = object{data: data, modified: now(),
			contentType: r.Header.Get("Content-Type")}
		s.sorted = nil
		s.mu.Unlock()
		w.Header().Set("ETag", etag(data))
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

// get answers an object, or the range of it a Range header asks for — and
// refuses ANY range that begins at or past the end, an empty object's every
// range included, as S3 does.
func (s *Server) get(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	obj, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		s.fail(w, r, http.StatusNotFound, "NoSuchKey")
		return
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
	s.objects[key] = object{data: data, modified: u.started,
		contentType: u.contentType, parts: len(req.Parts)}
	s.sorted = nil
	delete(s.uploads, id)
	s.mu.Unlock()
	s.writeXML(w, completeResult{Bucket: s.bucket, Key: key, ETag: etag(data)})
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
	prefix, after := q.Get("prefix"), q.Get("continuation-token")
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
