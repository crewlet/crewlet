package boxhelpertest

import (
	"bytes"
	"compress/flate"
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

// inflating is what gzip's one member inflates to, its trailer's CRC
// checked, and the error that stops it, failing t if its header is no gzip's.
func inflating(t *testing.T, gz []byte) ([]byte, error) {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	return io.ReadAll(r)
}

// inflated is gzip's one member, inflated whole.
func inflated(t *testing.T, gz []byte) []byte {
	t.Helper()
	out, err := inflating(t, gz)
	if err != nil {
		t.Fatalf("the tarball does not inflate: %v", err)
	}
	return out
}

// differing is how many bytes of a and b, of one length, differ.
func differing(a, b []byte) int {
	n := 0
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

// integrityOf is body's SHA-512 as a lock writes it.
func integrityOf(body []byte) string {
	sum := sha512.Sum512(body)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// THE STAND-IN REGISTRY SERVES THE LOCK'S BYTES: each path the lock resolves
// a package at serves bytes whose SHA-512 is the lock's integrity, read from
// the npm cache of the suite's tree, which npm checked going in; Corrupt and
// CorruptStream change one byte of the core's, Corrupt's inflating to the
// core's own tarball, CorruptStream's to none; NotFound and Fail answer 404
// and 500; Stall sends half a tarball of declared length; Hang holds a request,
// witnessed as held, until its end; every request is recorded. The install
// cases hold what npm makes of each.
//
// Mutations, each red: serve another content path; Corrupt flipping no byte,
// or one of the deflate data; CorruptStream flipping no byte, or the MTIME's;
// Hang answering at once.
func TestTheStandInRegistryServesTheLocksBytes(t *testing.T) {
	t.Parallel()
	paths := locked(t)
	sorted := slices.Sorted(maps.Keys(paths))
	var asked []string
	for _, p := range sorted {
		asked = append(asked, "GET "+p)
	}

	serve := Registry(t, Serve)
	corrupting := map[string]*StandInRegistry{"Corrupt": Registry(t, Corrupt),
		"CorruptStream": Registry(t, CorruptStream)}
	for _, p := range sorted {
		status, body := get(t, serve, p)
		if got := integrityOf(body); status != http.StatusOK || got != paths[p] {
			t.Errorf("Serve answers %s with %d and bytes of %s, want 200 and the lock's %s", p, status, got, paths[p])
		}
		core := filepath.Base(filepath.Dir(filepath.Dir(p))) == "playwright-core"
		bad := map[string][]byte{}
		for mode, r := range corrupting {
			status, got := get(t, r, p)
			if status != http.StatusOK || (integrityOf(got) == paths[p]) == core || len(got) != len(body) ||
				differing(got, body) > 1 {
				t.Errorf("%s answers %s with %d and %d bytes of %s, want the lock's bytes whole, but for the "+
					"core's, one changed", mode, p, status, len(got), integrityOf(got))
			}
			bad[mode] = got
		}
		if !core {
			continue
		}
		if !bytes.Equal(inflated(t, bad["Corrupt"]), inflated(t, body)) {
			t.Errorf("Corrupt answers %s with bytes that inflate to another tarball, want the core's own, "+
				"so npm's integrity check alone can refuse them", p)
		}
		if out, err := inflating(t, bad["CorruptStream"]); len(out) != 0 || !errors.As(err, new(flate.CorruptInputError)) {
			t.Errorf("CorruptStream answers %s with bytes that inflate to %d bytes (%v), want none, refused as "+
				"corrupt at their start, before npm's integrity check can reach their end", p, len(out), err)
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
