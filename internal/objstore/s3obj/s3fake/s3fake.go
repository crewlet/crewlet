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
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// [defaultPageSize].
	PageSize int

	mu       sync.Mutex
	objects  map[string]object
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
	parts       map[int32][]byte
}

// Start serves bucket until the test ends.
func Start(t *testing.T, bucket string) *Server {
	t.Helper()
	s := &Server{bucket: bucket, PageSize: defaultPageSize,
		objects: map[string]object{}, uploads: map[string]*upload{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
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
	s.uploads[id] = &upload{key: key, started: now(),
		parts: map[int32][]byte{1: []byte("a part of an upload nobody finished")}}
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
		data, err := io.ReadAll(r.Body)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "IncompleteBody")
			return
		}
		s.mu.Lock()
		s.objects[key] = object{data: data, modified: now(),
			contentType: r.Header.Get("Content-Type")}
		s.mu.Unlock()
		w.Header().Set("ETag", etag(data))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		s.get(w, r, key)
	case r.Method == http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s.fail(w, r, http.StatusNotImplemented, "NotImplemented")
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
		started: now(), parts: map[int32][]byte{}}
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
	data, err := io.ReadAll(r.Body)
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
	u.parts[int32(number)] = data
	w.Header().Set("ETag", etag(data))
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
// checked against the part stored under its number.
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
	var data []byte
	for i, p := range req.Parts {
		part, held := u.parts[p.PartNumber]
		if !held || p.ETag != etag(part) || (i > 0 && p.PartNumber <= req.Parts[i-1].PartNumber) {
			s.mu.Unlock()
			s.fail(w, r, http.StatusBadRequest, "InvalidPart")
			return
		}
		data = append(data, part...)
	}
	s.objects[key] = object{data: data, modified: u.started,
		contentType: u.contentType, parts: len(req.Parts)}
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
func etag(data []byte) string {
	var sum uint32
	for _, b := range data {
		sum = sum*31 + uint32(b)
	}
	return fmt.Sprintf(`"%d-%08x"`, len(data), sum)
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
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	out := listResult{Name: s.bucket, Prefix: prefix, MaxKeys: pageSize}
	for i, k := range keys {
		if i == pageSize {
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
