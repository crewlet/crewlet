package boxhelpertest

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Mode is how a stand-in registry answers.
type Mode int

const (
	// Serve answers each locked package's tarball with its bytes.
	Serve Mode = iota
	// NotFound answers every request 404, as a mirror lacking the release.
	NotFound
	// Fail answers every request 500, as a registry failing on its side.
	Fail
	// Corrupt serves playwright-core with one byte of its gzip header's
	// MTIME flipped, every other package whole: bytes the lock refuses
	// that still inflate to the published tarball, so npm's integrity
	// check is the one thing that can refuse them. A byte flipped in the
	// deflate data instead fails the gzip trailer's CRC too, and which of
	// the two npm reports first is a race inside npm (#1929).
	Corrupt
	// Hang reads each request and never answers it.
	Hang
	// Stall answers each request's header, declaring the tarball's whole
	// length, and the first half of its bytes, then nothing more: a body
	// that stops, for every tarball, the smallest (25 KB) included.
	Stall
)

// StandInRegistry is a stand-in for npm's registry on 127.0.0.1, serving the
// lock's tarballs from the npm cache of the suite's tree ([Tree]) at the paths
// the lock resolves them at, and recording every request it is sent. Hang and
// Stall wait on the request's end, never on the time.
type StandInRegistry struct {
	// URL is its address, for NPM_CONFIG_REGISTRY.
	URL string

	mode    Mode
	tarball map[string]string // a locked path → its file in the cache
	mu      sync.Mutex
	got     []string
	// parked witnesses a request held until its end, for the registry's own
	// test; it keeps the first, which is all that test reads, and drops the
	// rest, so no held request waits on it.
	parked chan struct{}
}

// Registry starts a stand-in registry answering as mode says; t's end stops
// it, ending every request it holds.
func Registry(t testing.TB, mode Mode) *StandInRegistry {
	t.Helper()
	tree := Tree(t)
	r := &StandInRegistry{mode: mode, tarball: map[string]string{}, parked: make(chan struct{}, 1)}
	raw, err := os.ReadFile(filepath.Join(tree, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Packages map[string]struct {
			Resolved  string `json:"resolved"`
			Integrity string `json:"integrity"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	for key, p := range lock.Packages {
		if key == "" {
			continue
		}
		at, err := url.Parse(p.Resolved)
		if err != nil {
			t.Fatalf("the lock resolves %s at %q: %v", key, p.Resolved, err)
		}
		r.tarball[at.Path] = contentFile(t, filepath.Join(tree, "npm-cache"), p.Integrity)
	}
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.serve(w, req, done)
	}))
	r.URL = server.URL + "/"
	// Cleanups run last first: the held requests end, then the server stops.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(done) })
	return r
}

// contentFile is where an npm cache keeps the bytes of integrity, a lock's
// sha512-<base64>: cacache's content-v2/sha512/<h[0:2]>/<h[2:4]>/<h[4:]>,
// with h the digest in hex.
func contentFile(t testing.TB, cache, integrity string) string {
	t.Helper()
	sum, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(integrity, "sha512-"))
	if err != nil || !strings.HasPrefix(integrity, "sha512-") {
		t.Fatalf("the lock's integrity %q is not one SHA-512: %v", integrity, err)
	}
	h := hex.EncodeToString(sum)
	return filepath.Join(cache, "_cacache", "content-v2", "sha512", h[:2], h[2:4], h[4:])
}

// serve answers one request as the registry's mode says, recording it first.
func (r *StandInRegistry) serve(w http.ResponseWriter, req *http.Request, done <-chan struct{}) {
	r.mu.Lock()
	r.got = append(r.got, req.Method+" "+req.URL.Path)
	r.mu.Unlock()
	wait := func() {
		select {
		case r.parked <- struct{}{}:
		default:
		}
		select {
		case <-req.Context().Done():
		case <-done:
		}
	}
	switch r.mode {
	case NotFound:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	case Fail:
		http.Error(w, "the stand-in registry fails", http.StatusInternalServerError)
		return
	case Hang:
		wait()
		return
	}
	file, ok := r.tarball[req.URL.Path]
	if !ok {
		http.NotFound(w, req)
		return
	}
	body, err := os.ReadFile(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.mode == Corrupt && strings.Contains(req.URL.Path, "/playwright-core-") {
		if body, err = otherMTime(body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.mode == Stall {
		_, _ = w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush()
		wait()
		return
	}
	_, _ = w.Write(body)
}

// otherMTime is gzip, a tarball as npm publishes it, with its header's MTIME
// changed (RFC 1952 §2.3: bytes 4 to 7, read by no inflater and covered by
// no checksum but FHCRC's, which it refuses).
func otherMTime(gzip []byte) ([]byte, error) {
	const fhcrc = 1 << 1
	if len(gzip) < 10 || gzip[0] != 0x1f || gzip[1] != 0x8b || gzip[2] != 8 || gzip[3]&fhcrc != 0 {
		return nil, fmt.Errorf("the stand-in registry corrupts a gzip member with no header CRC, and the "+
			"tarball begins % x", gzip[:min(len(gzip), 10)])
	}
	other := bytes.Clone(gzip)
	other[4] ^= 1
	return other, nil
}

// Requests is every request the registry was sent, as its method and path,
// in the order it read them.
func (r *StandInRegistry) Requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}
