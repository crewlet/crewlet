// Package httpxtest builds the outbound clients TESTS use, the way
// [github.com/crewlet/crewlet/internal/httpx] builds the ones the engine uses.
//
// It exists because of one block in httptest.Server.Close that the standard
// library's own source introduces as "not part of httptest.Server's
// correctness": after closing its listener, Close reaches for
// http.DefaultTransport and calls CloseIdleConnections on it — the
// PROCESS-GLOBAL pool, not the one belonging to the server being closed. In a
// package whose tests run in parallel, every t.Cleanup(server.Close)
// therefore tears down the keep-alive connections of every OTHER test in the
// binary.
//
// That is a correctness problem rather than a performance one, and the reason
// is narrow enough to be worth writing down. A response with NO BODY — a 204,
// an empty 404 — is handed back to the idle pool by the transport's read loop
// BEFORE the response reaches its caller (net/http's transport.go, the
// `if !hasBody || bodyWritable` branch). For those few instructions one
// connection is both idle and in flight. A close landing in that window wakes
// the round trip on the connection's closech instead of on its own response,
// and the request fails although the server answered it:
//
//	net/http: HTTP/1.x transport connection broken: http: CloseIdleConnections called
//
// internal/sandbox lost a CI run to exactly that, on a DELETE its stub
// answers 204. A request whose response carries a body is never returned to
// the pool early and is not exposed, which is why the failures all look
// arbitrary: they land only on the bodyless calls.
//
// So a double that points at an httptest.Server takes THAT SERVER'S
// transport. Not http.DefaultTransport, which every neighbour's cleanup
// closes, and not [httpx.Transport], which is the engine's own pool — a
// double sharing that would put test traffic in the pool whose settings two
// other tests assert on, and its lifetime is the process rather than the
// server, so a warm connection can outlive the listener it was opened to.
// A started server's transport is its own, and the only CloseIdleConnections
// it ever sees is its own Close, which runs from t.Cleanup after the test
// body has returned.
package httpxtest

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
)

// TB is the part of *testing.T this package uses: a handle to fail with.
//
// Narrower than [testing.TB] because one caller is not one — the
// integration-conformance suite drives its cases through its own three-method
// TB so a recorder can stand in for a test, and a parameter of testing.TB
// would shut that caller out of the one helper it most needs. Kept to what
// this package calls, which is the rule for every interface in this tree.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// Rewriter sends every request to one [httptest.Server] and records the
// address the caller asked for, so a test can assert on an address its
// subject DERIVED rather than on one the test handed it.
type Rewriter struct {
	scheme, host string
	base         http.RoundTripper

	mu    sync.Mutex
	hosts []string
}

// Rewrite returns a round tripper that sends every request to srv.
//
// It takes the SERVER, never its URL, and that is the whole of the type: the
// address and the connection pool become ONE decision. A double built from a
// URL string picks its pool separately, and the two then disagree in the one
// direction nothing notices — the requests arrive, every assertion passes,
// and the pool is the process-global one every parallel neighbour's cleanup
// closes.
func Rewrite(tb TB, srv *httptest.Server) *Rewriter {
	tb.Helper()
	if srv == nil || srv.Client() == nil {
		tb.Fatalf("httpxtest.Rewrite needs a STARTED httptest.Server: an " +
			"unstarted one has no client, so there is no transport of its own to " +
			"borrow and the double would silently fall back to the shared pool")
	}
	u, err := url.Parse(srv.URL)
	if err != nil {
		tb.Fatalf("httpxtest.Rewrite: server URL %q: %v", srv.URL, err)
	}
	return &Rewriter{scheme: u.Scheme, host: u.Host, base: srv.Client().Transport}
}

// RoundTrip sends req to the server this was built from.
func (r *Rewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.hosts = append(r.hosts, req.URL.Scheme+"://"+req.URL.Host)
	r.mu.Unlock()

	// CLONED rather than edited. A RoundTripper may not modify the request
	// it is given, and several callers assert on the address their subject
	// built — rewriting in place would replace the thing under test with
	// the fake's own address.
	routed := req.Clone(req.Context())
	routed.URL.Scheme, routed.URL.Host, routed.Host = r.scheme, r.host, r.host
	return r.base.RoundTrip(routed)
}

// Hosts is every scheme://host the caller addressed, in order.
func (r *Rewriter) Hosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.hosts)
}

// SawHost reports whether the caller ever addressed want.
func (r *Rewriter) SawHost(want string) bool { return slices.Contains(r.Hosts(), want) }

// Pool is a client with a transport of its OWN, for a test with no
// httptest.Server to borrow from — one talking to a real listener the engine
// opened, say.
//
// It exists so that "there is no server here" never becomes a reason to reach
// for http.DefaultClient: the pool is closed when the test ends, so it
// neither outlives the test nor is reachable by anybody else's cleanup.
func Pool(tb testing.TB) *http.Client {
	tb.Helper()
	tr := &http.Transport{}
	tb.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}
