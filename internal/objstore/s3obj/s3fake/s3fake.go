// Package s3fake is an in-process S3 endpoint for tests: the five calls the
// object store makes — head bucket, put, get, head and delete object, and a
// paged ListObjectsV2 — over one bucket, path-style.
//
// It checks no signature and keeps no versions; it exists so the S3 backend
// runs the object store's conformance suite on a test machine with no network
// and no credentials, against the wire format the SDK actually speaks.
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

// Server is one fake bucket.
type Server struct {
	*httptest.Server
	bucket string
	// PageSize is how many keys one listing page holds, small by default
	// so a test of a few dozen chunks crosses page boundaries.
	PageSize int

	mu      sync.Mutex
	objects map[string]object
}

type object struct {
	data     []byte
	modified time.Time
}

// Start serves bucket until the test ends.
func Start(t *testing.T, bucket string) *Server {
	t.Helper()
	s := &Server{bucket: bucket, PageSize: 7, objects: map[string]object{}}
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

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != s.bucket {
		s.fail(w, r, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch {
	case key == "" && r.Method == http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case key == "" && r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		s.list(w, r)
	case key != "" && r.Method == http.MethodPut:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "IncompleteBody")
			return
		}
		s.mu.Lock()
		s.objects[key] = object{data: data, modified: time.Now().UTC()}
		s.mu.Unlock()
		w.Header().Set("ETag", `"`+strconv.Itoa(len(data))+`"`)
		w.WriteHeader(http.StatusOK)
	case key != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		s.mu.Lock()
		obj, ok := s.objects[key]
		s.mu.Unlock()
		if !ok {
			s.fail(w, r, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Last-Modified", obj.modified.Format(http.TimeFormat))
		w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(obj.data)
		}
	case key != "" && r.Method == http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s.fail(w, r, http.StatusNotImplemented, "NotImplemented")
	}
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
	LastModified string `xml:"LastModified"`
	Size         int    `xml:"Size"`
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix, after := q.Get("prefix"), q.Get("continuation-token")
	s.mu.Lock()
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	out := listResult{Name: s.bucket, Prefix: prefix, MaxKeys: s.PageSize}
	for i, k := range keys {
		if i == s.PageSize {
			out.IsTruncated = true
			out.NextContinuationToken = out.Contents[len(out.Contents)-1].Key
			break
		}
		obj := s.objects[k]
		out.Contents = append(out.Contents, listItem{
			Key: k, Size: len(obj.data),
			LastModified: obj.modified.Format("2006-01-02T15:04:05.000Z"),
		})
	}
	s.mu.Unlock()
	out.KeyCount = len(out.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(out)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = fmt.Fprintf(w, "%s<Error><Code>%s</Code><Message>%s</Message></Error>",
			xml.Header, code, code)
	}
}
