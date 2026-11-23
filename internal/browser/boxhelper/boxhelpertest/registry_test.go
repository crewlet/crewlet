package boxhelpertest

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// locked is each package the suite's lock pins, its tarball's path as a
// request names it and its integrity.
func locked(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(Tree(t), "package-lock.json"))
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
	paths := map[string]string{}
	for key, p := range lock.Packages {
		if key == "" {
			continue
		}
		at, err := url.Parse(p.Resolved)
		if err != nil {
			t.Fatal(err)
		}
		paths[at.Path] = p.Integrity
	}
	return paths
}

// get asks the registry for path and answers the response's status and its
// body, failing t if the request fails.
func get(t *testing.T, r *StandInRegistry, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, r.URL+path[1:], nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpxtest.Pool(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// inflated is gzip's one member, inflated whole, its trailer's CRC checked.
func inflated(t *testing.T, gz []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("the tarball does not inflate: %v", err)
	}
	return out
}

// integrityOf is body's SHA-512 as a lock writes it.
func integrityOf(body []byte) string {
	sum := sha512.Sum512(body)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// THE STAND-IN REGISTRY SERVES THE LOCK'S BYTES: each path the lock resolves
// a package at serves bytes whose SHA-512 is the lock's integrity, read from
// the npm cache of the suite's tree, which npm itself checked going in;
// Corrupt serves the core's with one byte changed, still inflating to the
// tarball it was, so integrity is the one check that can refuse it, and the
// others whole;
// NotFound and Fail answer 404 and 500; Stall declares a tarball's whole
// length and sends its first half; Hang holds a request, which it witnesses
// as held, and answers it only with its end; and every request is recorded,
// as its method and path. What npm makes of a hang and a stall, the install
// cases hold.
//
// Mutations, each red: serve another content path; Corrupt flipping no byte;
// Corrupt flipping a byte of the deflate data;
// Hang answering at once (it answers before it holds the request).
func TestTheStandInRegistryServesTheLocksBytes(t *testing.T) {
	t.Parallel()
	paths := locked(t)
	sorted := slices.Sorted(maps.Keys(paths))
	var asked []string
	for _, p := range sorted {
		asked = append(asked, "GET "+p)
	}

	serve, corrupt := Registry(t, Serve), Registry(t, Corrupt)
	for _, p := range sorted {
		status, body := get(t, serve, p)
		if got := integrityOf(body); status != http.StatusOK || got != paths[p] {
			t.Errorf("Serve answers %s with %d and bytes of %s, want 200 and the lock's %s", p, status, got, paths[p])
		}
		status, bad := get(t, corrupt, p)
		core := filepath.Base(filepath.Dir(filepath.Dir(p))) == "playwright-core"
		if status != http.StatusOK || (integrityOf(bad) == paths[p]) == core || len(bad) != len(body) {
			t.Errorf("Corrupt answers %s with %d and %d bytes of %s, want the lock's bytes whole, but for the "+
				"core's, one changed", p, status, len(bad), integrityOf(bad))
		}
		if core && !bytes.Equal(inflated(t, bad), inflated(t, body)) {
			t.Errorf("Corrupt answers %s with bytes that inflate to another tarball, want the core's own, "+
				"so npm's integrity check alone can refuse them", p)
		}
	}
	if got := serve.Requests(); !slices.Equal(got, asked) {
		t.Errorf("the registry recorded %q, want %q", got, asked)
	}

	for mode, want := range map[Mode]int{NotFound: http.StatusNotFound, Fail: http.StatusInternalServerError} {
		if status, _ := get(t, Registry(t, mode), sorted[0]); status != want {
			t.Errorf("mode %d answers %d, want %d", mode, status, want)
		}
	}

	stall := Registry(t, Stall)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, stall.URL+sorted[0][1:], nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpxtest.Pool(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, whole := get(t, serve, sorted[0])
	first := make([]byte, len(whole)/2)
	if _, err := io.ReadFull(resp.Body, first); err != nil || resp.Header.Get("Content-Length") != strconv.Itoa(len(whole)) ||
		!bytes.Equal(first, whole[:len(first)]) {
		t.Errorf("Stall answered %s declaring %s bytes (%v), want the tarball's first %d of its %d", sorted[0],
			resp.Header.Get("Content-Length"), err, len(first), len(whole))
	}
	_ = resp.Body.Close()

	hang := Registry(t, Hang)
	ctx, cancel := context.WithCancel(t.Context())
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, hang.URL+sorted[0][1:], nil)
	if err != nil {
		t.Fatal(err)
	}
	answered := make(chan error, 1)
	client := httpxtest.Pool(t)
	go func() {
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			err = errors.New(resp.Status)
		}
		answered <- err
	}()
	held, stop := context.WithTimeout(t.Context(), time.Minute)
	defer stop()
	select {
	case <-hang.parked:
	case err := <-answered:
		t.Fatalf("Hang answered a request with %v before the request's end", err)
	case <-held.Done():
		t.Fatalf("Hang held no request within a minute")
	}
	cancel()
	if err := <-answered; !errors.Is(err, context.Canceled) {
		t.Errorf("Hang answered a request it holds with %v, want nothing until the request's own end", err)
	}
}
